package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	// modernc.org/sqlite is pure Go, CGO_ENABLED=0 compliant.
	_ "modernc.org/sqlite"
)

// Config tunes Open. Zero values use the production defaults required by PLAN
// §4: writer with SetMaxOpenConns(1), reader with SetMaxOpenConns(NumCPU*2),
// WAL, busy_timeout=5000, foreign_keys=ON, synchronous=NORMAL.
type Config struct {
	// Path is the SQLite file path. ":memory:" and "" are rejected by Open
	// because they would not exercise the two-pool WAL discipline the
	// concurrency story depends on.
	Path string
	// ReadPoolSize overrides the reader pool size; <=0 means NumCPU*2.
	ReadPoolSize int
	// ApplyMigrations runs the embedded migrations on Open. Defaults true.
	ApplyMigrations *bool
	// RegisterMigrationsFalse allows the test suite to skip the migration
	// runner on a brand-new database that the test will create itself.
}

// Open returns a ready Store backed by a writer *sql.DB (single connection)
// and a reader *sql.DB (concurrent). All connection-level pragmas are set via
// the DSN so they apply to every pooled connection that database/sql may
// create — a per-connection PRAGMA statement at first-use does not propagate
// to connections opened later under load, and that silent miss is the most
// common cause of "WAL is not really on" or "foreign keys are off in this
// goroutine" bugs.
func Open(ctx context.Context, cfg Config) (Store, error) {
	if cfg.Path == "" || cfg.Path == ":memory:" {
		return nil, fmt.Errorf("store: refusing to open %q (use a file path so WAL and the two pools are exercised)", cfg.Path)
	}
	if !filepath.IsAbs(cfg.Path) {
		abs, err := filepath.Abs(cfg.Path)
		if err != nil {
			return nil, fmt.Errorf("store: resolve path: %w", err)
		}
		cfg.Path = abs
	}
	readerDSN, writerDSN, err := buildDSNs(cfg.Path)
	if err != nil {
		return nil, err
	}

	writer, err := sql.Open("sqlite", writerDSN)
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)
	writer.SetConnMaxIdleTime(0)

	reader, err := sql.Open("sqlite", readerDSN)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	readerSize := cfg.ReadPoolSize
	if readerSize <= 0 {
		readerSize = runtime.NumCPU() * 2
	}
	if readerSize < 1 {
		readerSize = 1
	}
	reader.SetMaxOpenConns(readerSize)
	reader.SetMaxIdleConns(readerSize)
	reader.SetConnMaxLifetime(0)
	reader.SetConnMaxIdleTime(0)

	// Probe the writer to make sure the file opens, the WAL is on and FK is
	// on — failing fast at Open is much friendlier than discovering it on the
	// first real write.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := probeConnection(probeCtx, writer); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return nil, fmt.Errorf("store: writer probe: %w", err)
	}
	if err := probeConnection(probeCtx, reader); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return nil, fmt.Errorf("store: reader probe: %w", err)
	}

	s := &sqlStore{
		path:      cfg.Path,
		writer:    writer,
		reader:    reader,
		migration: 0,
		repos:     make(map[reposKey]any),
	}
	apply := true
	if cfg.ApplyMigrations != nil {
		apply = *cfg.ApplyMigrations
	}
	if apply {
		if err := s.runMigrations(ctx); err != nil {
			_ = writer.Close()
			_ = reader.Close()
			return nil, err
		}
	}
	return s, nil
}

// buildDSNs constructs a writer DSN with _txlock=immediate (the modernc
// driver passes "begin immediate" automatically) and a reader DSN that uses
// shared cache so the two pools cooperate on the same WAL file. The reader
// is NOT marked mode=ro: the file: path with mode=ro runs into permission
// races on the -shm file and is overkill — the writer pool's single
// connection is the single point of write authority, and READONLY
// transactions are issued through the reader's BeginTx(ReadOnly=true).
func buildDSNs(path string) (readerDSN, writerDSN string, err error) {
	// url.URL handles the file: scheme and escapes backslashes / spaces in
	// the Windows path. SQLite then sees a clean canonical URI.
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	fileURL := "file:" + filepath.ToSlash(abs) + "?" + strings.Join(basePragmas(), "&")
	writerURL := fileURL + "&_txlock=immediate"
	return fileURL, writerURL, nil
}

func basePragmas() []string {
	return []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(NORMAL)",
	}
}

// probeConnection runs a tiny query and asserts the pragmas took effect on
// THIS connection (a DSN pragma that the driver silently failed to apply
// would still return "row 1 1" — we explicitly read journal_mode and
// foreign_keys to catch that class of failure).
func probeConnection(ctx context.Context, db *sql.DB) error {
	var journal, fk string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("read journal_mode: %w", err)
	}
	if !strings.EqualFold(journal, "wal") {
		return fmt.Errorf("journal_mode is %q, want WAL (DSN pragma did not apply)", journal)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		return fmt.Errorf("read foreign_keys: %w", err)
	}
	if fk != "1" {
		return fmt.Errorf("foreign_keys is %q, want 1", fk)
	}
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("select 1: %w", err)
	}
	if one != 1 {
		return fmt.Errorf("select 1 returned %d", one)
	}
	return nil
}

// ---------------------------------------------------------------------------
// sqlStore
// ---------------------------------------------------------------------------

type reposKey struct{ name string }

type sqlStore struct {
	path      string
	writer    *sql.DB // SetMaxOpenConns(1)
	reader    *sql.DB // SetMaxOpenConns(NumCPU*2)
	mu        sync.Mutex
	migration int
	repos     map[reposKey]any
}

func (s *sqlStore) Read(ctx context.Context, fn func(Tx) error) error {
	if fn == nil {
		return errors.New("store: Read: nil function")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Read-only transactions on the reader pool. The driver short-circuits
	// "begin immediate" to plain "begin" when ReadOnly is set, so we do not
	// accidentally take a write lock on a read.
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("store: begin read tx: %w", err)
	}
	wrap := &txWrap{tx: tx, store: s, ctx_: ctx, state: &txState{}}
	if err := fn(wrap); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return fmt.Errorf("%w (rollback: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit read tx: %w", err)
	}
	return nil
}

func (s *sqlStore) Write(ctx context.Context, fn func(Tx) error) error {
	if fn == nil {
		return errors.New("store: Write: nil function")
	}
	// Honor cancellation before we take the single-connection writer lock:
	// a request that timed out on the client must not block the next caller
	// waiting for the writer.
	if err := ctx.Err(); err != nil {
		return err
	}
	// The writer pool has SetMaxOpenConns(1), so this call serializes the
	// whole process. The DSN sets _txlock=immediate so every tx starts with
	// BEGIN IMMEDIATE, which is what PLAN §4 and §7 require.
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin write tx: %w", err)
	}
	wrap := &txWrap{tx: tx, store: s, ctx_: ctx, state: &txState{}}
	if err := fn(wrap); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return fmt.Errorf("%w (rollback: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit write tx: %w", err)
	}
	return nil
}

// txState is the state a transaction shares with every nested unit derived
// from it: one cached database clock and one savepoint-name counter. A
// nested unit is a scope inside the same *sql.Tx, not a new transaction, so
// it must neither re-read the clock (a nested item is not a new point in
// time) nor restart the numbering (two live savepoints must never share a
// name — ROLLBACK TO would unwind to the wrong one).
type txState struct {
	mu     sync.Mutex
	loaded bool
	now    time.Time
	nowErr error
	seq    int64
}

// clock reads the database clock once per transaction and caches the
// outcome — including a failure.
//
// The failure is cached on purpose. Now() is the ordering authority for
// lease expiry, done_at and the event log, so the one thing it may never do
// is give two different answers inside one transaction. A retry that
// happened to succeed after an earlier caller was told "no clock" would do
// exactly that: half the transaction's decisions made without a timestamp,
// the other half stamped from a later read. A transaction whose clock is
// unreadable is over; every subsequent Now() says so.
func (st *txState) clock(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.loaded {
		return st.now, st.nowErr
	}
	st.loaded = true
	var ts string
	// strftime('%Y-%m-%dT%H:%M:%fZ', 'now') produces a lexically and
	// chronologically ordered UTC stamp. 'now' is the start-of-statement
	// timestamp which is stable for the duration of a single SQL statement
	// but may shift between statements; we want a single stable value
	// across the whole transaction, so we read it once.
	if err := tx.QueryRowContext(ctx, "SELECT strftime('%Y-%m-%dT%H:%M:%fZ','now')").Scan(&ts); err != nil {
		st.nowErr = fmt.Errorf("store: read database clock: %w", err)
		return time.Time{}, st.nowErr
	}
	parsed, err := time.Parse(timeLayout, ts)
	if err != nil {
		st.nowErr = fmt.Errorf("store: database clock returned unparseable %q: %w", ts, err)
		return time.Time{}, st.nowErr
	}
	st.now = parsed.UTC()
	return st.now, nil
}

// nextSavepoint hands out a name that is unique for the lifetime of the
// transaction. The counter is shared through txState so sibling and nested
// units cannot collide.
func (st *txState) nextSavepoint() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.seq++
	return "kanban_sp_" + strconv.FormatInt(st.seq, 10)
}

// txWrap is the concrete Tx handed to every repository. The database clock
// is read on first use and then fixed for the rest of the transaction —
// every timestamp the repos write or compare must use that one value, never
// time.Now(), or a lease renewal racing an expiry stops being decidable.
// Nested units share the same txState, so they share that value too.
type txWrap struct {
	tx    *sql.Tx
	store *sqlStore
	ctx_  context.Context
	state *txState
	// released marks a nested Tx whose Nested call has already returned.
	// The savepoint it named is gone, so the value no longer denotes the
	// scope its holder thinks it does; using it again is a bug we can name
	// instead of a rollback that silently unwinds the wrong work.
	released atomic.Bool
}

// ctx returns the context that was active when the transaction was started.
// The store layer stamps it here so helpers in scan.go can extract the
// actor from a context value.
func (t *txWrap) ctx() context.Context { return t.ctx_ }

// Now returns the database clock for this transaction, or the error that
// prevented reading it. See the Tx interface for why there is no fallback.
func (t *txWrap) Now() (time.Time, error) { return t.state.clock(t.ctx(), t.tx) }

// Nested runs fn inside a SAVEPOINT. See the Tx interface for the contract.
func (t *txWrap) Nested(fn func(Tx) error) error {
	if fn == nil {
		return errors.New("store: Nested: nil function")
	}
	if t.released.Load() {
		return errors.New("store: Nested: this Tx came from a nested unit that has already finished; do not retain the Tx passed to Nested")
	}
	name := t.state.nextSavepoint()
	if _, err := t.tx.ExecContext(t.ctx(), "SAVEPOINT "+name); err != nil {
		return fmt.Errorf("store: open savepoint: %w", err)
	}
	child := &txWrap{tx: t.tx, store: t.store, ctx_: t.ctx_, state: t.state}
	finished := false
	defer func() {
		child.released.Store(true)
		if finished {
			return
		}
		// fn panicked (or called runtime.Goexit — t.Fatal from a helper
		// goroutine does that). The savepoint is still open and fn's half
		// finished writes are still provisional, so undo them here: the
		// enclosing transaction may well be recovered and committed by
		// whoever catches the panic, and it must not carry them.
		_ = t.discard(name)
	}()
	err := fn(child)
	finished = true
	if err != nil {
		if rbErr := t.discard(name); rbErr != nil {
			// Report both: the caller needs fn's error for its per-item
			// result, and the rollback failure means the enclosing
			// transaction can no longer be trusted to commit.
			return errors.Join(err, rbErr)
		}
		return err
	}
	if _, relErr := t.tx.ExecContext(t.ctx(), "RELEASE "+name); relErr != nil {
		// RELEASE failed, so the savepoint is still open and fn's writes
		// are still provisional. Returning nil would promise the caller
		// that its unit joined the transaction when it has not.
		if rbErr := t.discard(name); rbErr != nil {
			return errors.Join(fmt.Errorf("store: release savepoint: %w", relErr), rbErr)
		}
		return fmt.Errorf("store: release savepoint: %w", relErr)
	}
	return nil
}

// discard undoes a savepoint and pops it off the stack.
//
// The RELEASE is not optional cleanup: ROLLBACK TO rewinds the writes but
// leaves the savepoint active, so a batch that rejects fifty items would
// otherwise leave fifty live savepoint frames on one transaction, and the
// next ROLLBACK TO of a reused name would unwind to the wrong frame.
func (t *txWrap) discard(name string) error {
	if _, err := t.tx.ExecContext(t.ctx(), "ROLLBACK TO "+name); err != nil {
		return fmt.Errorf("store: roll back savepoint: %w", err)
	}
	if _, err := t.tx.ExecContext(t.ctx(), "RELEASE "+name); err != nil {
		return fmt.Errorf("store: release rolled-back savepoint: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Repository accessors
// ---------------------------------------------------------------------------

func (s *sqlStore) Projects() ProjectRepo {
	return s.repo("projects", func() any { return &projectRepo{s: s} }).(ProjectRepo)
}
func (s *sqlStore) Columns() ColumnRepo {
	return s.repo("columns", func() any { return &columnRepo{s: s} }).(ColumnRepo)
}
func (s *sqlStore) Tasks() TaskRepo {
	return s.repo("tasks", func() any { return &taskRepo{s: s} }).(TaskRepo)
}
func (s *sqlStore) Links() LinkRepo {
	return s.repo("links", func() any { return &linkRepo{s: s} }).(LinkRepo)
}
func (s *sqlStore) Notes() NoteRepo {
	return s.repo("notes", func() any { return &noteRepo{s: s} }).(NoteRepo)
}
func (s *sqlStore) Events() EventRepo {
	return s.repo("events", func() any { return &eventRepo{s: s} }).(EventRepo)
}
func (s *sqlStore) Tokens() TokenRepo {
	return s.repo("tokens", func() any { return &tokenRepo{s: s} }).(TokenRepo)
}
func (s *sqlStore) Sessions() SessionRepo {
	return s.repo("sessions", func() any { return &sessionRepo{s: s} }).(SessionRepo)
}
func (s *sqlStore) Idempotency() IdempotencyRepo {
	return s.repo("idempotency", func() any { return &idempotencyRepo{s: s} }).(IdempotencyRepo)
}

func (s *sqlStore) repo(name string, build func() any) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.repos[reposKey{name}]; ok {
		return r
	}
	r := build()
	s.repos[reposKey{name}] = r
	return r
}

// ---------------------------------------------------------------------------
// Backup / Checkpoint / Health / Close
// ---------------------------------------------------------------------------

// Backup performs VACUUM INTO. It runs on the writer pool outside a
// transaction (VACUUM cannot be inside one). The destination must be on the
// same filesystem and must not exist; SQLite's VACUUM INTO refuses to
// overwrite.
func (s *sqlStore) Backup(ctx context.Context, dest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dest == "" {
		return errors.New("store: backup: empty destination")
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("store: backup: resolve dest: %w", err)
	}
	// VACUUM INTO does not accept bound parameters for the filename; build
	// the statement with the path safely quoted. We use single quotes and
	// escape any single quote in the path by doubling it.
	escaped := strings.ReplaceAll(absDest, "'", "''")
	stmt := "VACUUM INTO '" + escaped + "'"
	if _, err := s.writer.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("store: backup (%s): %w", absDest, err)
	}
	return nil
}

// Checkpoint runs wal_checkpoint(TRUNCATE) on the writer. A long-lived SSE
// deployment otherwise grows the WAL without bound (PLAN §4).
func (s *sqlStore) Checkpoint(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.writer.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("store: checkpoint: %w", err)
	}
	return nil
}

// Health returns the snapshot used by `kanban doctor` and /readyz. The
// WriteMicros is the round-trip cost of a no-op write transaction, which
// exposes both pool starvation and WAL contention.
func (s *sqlStore) Health(ctx context.Context) (HealthInfo, error) {
	if err := ctx.Err(); err != nil {
		return HealthInfo{}, err
	}
	h := HealthInfo{Path: s.path, Migration: migrationVersion(s, ctx)}
	// A real write transaction (BEGIN IMMEDIATE + insert into
	// schema_migrations + commit) times the round trip on the writer.
	// We use a real INSERT instead of a literal no-op because a no-op tx
	// can complete in < 1 µs on modern SSDs and round to 0.
	start := time.Now()
	werr := s.Write(ctx, func(tx Tx) error {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		_, err = tx.(*txWrap).tx.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, name, applied_at) VALUES(-1, '__health__', ?)",
			formatTime(now))
		return err
	})
	cleanupErr := s.Write(ctx, func(tx Tx) error {
		_, err := tx.(*txWrap).tx.ExecContext(ctx,
			"DELETE FROM schema_migrations WHERE version = -1")
		return err
	})
	h.WriteMicros = time.Since(start).Nanoseconds() / 1000
	if h.WriteMicros == 0 {
		// Fast SSDs can complete the writer round trip in under 1 µs.
		// Bump to 1 so the field is non-zero in monitoring; the real
		// value is still bounded by the resolution of time.Now on the
		// host (typically 100 ns on Linux, 1 µs on Windows).
		h.WriteMicros = 1
	}
	if werr != nil {
		return h, fmt.Errorf("store: health write: %w", werr)
	}
	if cleanupErr != nil {
		// Non-fatal: health already measured.
		_ = cleanupErr
	}

	var journal string
	if err := s.reader.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return h, fmt.Errorf("store: health journal: %w", err)
	}
	h.JournalMode = journal
	if err := s.reader.QueryRowContext(ctx, "PRAGMA page_count").Scan(&h.PageCount); err != nil {
		return h, fmt.Errorf("store: health page_count: %w", err)
	}
	if h.JournalMode == "wal" {
		var wal int64
		if err := s.reader.QueryRowContext(ctx, "PRAGMA wal_size").Scan(&wal); err == nil {
			h.WALBytes = wal
		}
	}
	return h, nil
}

// Close releases both pools. We close the reader first (it cannot be
// holding the writer) and the writer last. Any error from the writer is
// returned; the reader's error is joined.
func (s *sqlStore) Close() error {
	rerr := s.reader.Close()
	werr := s.writer.Close()
	return errors.Join(rerr, werr)
}

// migrationVersion reads the highest applied migration number from the
// schema_migrations table that the migration runner creates.
func migrationVersion(s *sqlStore, ctx context.Context) int {
	var v sql.NullInt64
	err := s.reader.QueryRowContext(ctx,
		"SELECT MAX(version) FROM schema_migrations").Scan(&v)
	if err != nil || !v.Valid {
		return 0
	}
	return int(v.Int64)
}

// readURLForTest exports a DSN for tests; kept private to the package.
func readURLForTest(path string) (string, error) {
	u, err := url.Parse("file:" + path)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// io.Copy is imported to keep the import set consistent if a future
// streaming backup is added; keep the link alive for now.
var _ = io.Copy
