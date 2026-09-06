package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// The product's correctness rests on exactly one serving process owning a
// database file: the in-process events bus, the single-connection writer pool
// and the migration runner all assume it, and none of them can enforce it
// alone. This file turns that assumption into an OS-enforced fact: an
// exclusive byte-range lock on a file next to the database, held for the
// lifetime of whoever owns the data directory.
//
// Why a byte-range lock and not a PID file: the OS releases byte-range locks
// (flock on Unix, LockFileEx on Windows) when the owning process dies, even on
// kill -9. A PID file whose presence means "occupied" bricks the install after
// a crash; a PID file whose *content* is checked races. The lock file created
// here is only the lock target — its contents are advisory diagnostics and are
// never parsed for a decision.

// ownerLockName is the lock file, kept beside the database so the whole data
// directory is the unit of ownership (backups directory included).
const ownerLockName = "kanban.lock"

// dbFileName is the database file inside the data directory. Declared once so
// the lock error message and the actual open path can never drift apart.
const dbFileName = "kanban.db"

// databasePath is the single derivation of the database path from a data dir.
func databasePath(dataDir string) string { return filepath.Join(dataDir, dbFileName) }

// ErrOwnerHeld reports that another process already owns the data directory.
// Callers branch on it to decide between refusing (the server) and attaching
// without ownership (short-lived CLI commands); it never means corruption.
var ErrOwnerHeld = errors.New("app: data directory already owned by another process")

// errLockHeld is the platform-neutral translation of "the OS refused the lock
// because someone holds it". Platform files map their EWOULDBLOCK /
// ERROR_LOCK_VIOLATION onto it.
var errLockHeld = errors.New("lock held elsewhere")

// OwnerHeldError is what a second serving process receives: a complete,
// operator-facing refusal naming the lock, the database and the way out.
type OwnerHeldError struct {
	LockPath string
	DBPath   string
	Inner    error
}

// Error prints the full refusal. It follows the "refusing to start: …" shape
// internal/config uses for startup refusals so the CLI renders one consistent
// voice, and it says what to do instead of only what went wrong.
func (e *OwnerHeldError) Error() string {
	return fmt.Sprintf(
		"refusing to start: another kanban process already owns this data directory "+
			"(lock %s, database %s). One serving process per database file is a design rule: "+
			"live board updates and schema migrations are process-local. "+
			"Stop the other `kanban serve` first (check this machine and any machine sharing "+
			"the directory), or start with a different --data. "+
			"Agents on other machines must connect to the running server over HTTP "+
			"(`kanban mcp --url http://host:port`), never open the database directly.",
		e.LockPath, e.DBPath)
}

// Unwrap exposes the underlying OS error for diagnostics.
func (e *OwnerHeldError) Unwrap() error { return e.Inner }

// Is makes errors.Is(err, ErrOwnerHeld) work through the wrapper.
func (e *OwnerHeldError) Is(target error) bool { return target == ErrOwnerHeld }

// OwnerLock is an acquired ownership claim on a data directory. Release it
// exactly once with Release; dropping the value without releasing keeps the
// process locked out until it exits (the OS reclaims the lock at exit).
type OwnerLock struct {
	file *os.File
	path string
}

// AcquireOwnerLock takes the exclusive ownership lock for dataDir without
// blocking. It creates the lock file if absent — a leftover file from a killed
// process is inert, because correctness lives in the OS lock, not the file's
// existence or content.
func AcquireOwnerLock(dataDir string) (*OwnerLock, error) {
	path := filepath.Join(dataDir, ownerLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("app: open lock file %s: %w", path, err)
	}
	if err := lockExclusive(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errLockHeld) {
			return nil, &OwnerHeldError{LockPath: path, DBPath: databasePath(dataDir), Inner: err}
		}
		return nil, fmt.Errorf("app: lock %s: %w", path, err)
	}
	// Advisory only: which PID owns the directory right now. Never parsed for
	// a decision — a stale PID here must not change behaviour.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte("pid="+strconv.Itoa(os.Getpid())+"\n"), 0)
	return &OwnerLock{file: f, path: path}, nil
}

// Release drops the claim. Closing the descriptor releases the OS lock on
// every supported platform; it is safe to call after a failed Release.
func (l *OwnerLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("app: release %s: %w", l.path, err)
	}
	return nil
}

// cliStore pairs an attached store with the owner lock it took, so a
// short-lived command that opened the directory while it was free holds
// ownership for exactly as long as it holds the database open.
type cliStore struct {
	store.Store
	lock *OwnerLock
}

// Close releases the database and, with it, the ownership claim. Order
// matters: the store must stop writing before the lock that protected those
// writes goes away.
func (c *cliStore) Close() error {
	return errors.Join(c.Store.Close(), c.lock.Release())
}

// OpenCLIStore opens the database for a short-lived CLI command (token,
// backup, doctor, task purge, migrate) under a deliberate, uniform policy:
//
//   - No server owns the directory: take the owner lock, open WITH migrations
//     (a fresh install must be bootable from `kanban token create` alone) and
//     hold both until Close. This also serialises two concurrent CLI commands
//     through a schema upgrade.
//   - A server owns the directory: attach WITHOUT migrations. Refusing
//     `kanban token create` while the server runs would be hostile — minting
//     a token beside a live server is a normal operation — and steady-state
//     writes are safe because SQLite serialises them across processes
//     (BEGIN IMMEDIATE + busy_timeout). The one unsafe window, a schema
//     upgrade, cannot happen: schema evolution belongs to the owner, and the
//     owner applied all migrations before it started serving.
//
// The rule for operators is one sentence: servers own the schema, CLI
// commands never migrate under a live server.
func OpenCLIStore(ctx context.Context, dataDir string) (store.Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("app: create data dir: %w", err)
	}
	lock, err := AcquireOwnerLock(dataDir)
	if err != nil {
		if errors.Is(err, ErrOwnerHeld) {
			noMigrations := false
			st, err := store.Open(ctx, store.Config{
				Path:            databasePath(dataDir),
				ApplyMigrations: &noMigrations,
			})
			if err != nil {
				return nil, fmt.Errorf("app: attach to %s: %w", databasePath(dataDir), err)
			}
			return st, nil
		}
		return nil, err
	}
	st, err := store.Open(ctx, store.Config{Path: databasePath(dataDir)})
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	return &cliStore{Store: st, lock: lock}, nil
}
