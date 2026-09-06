package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// Time helpers
// ---------------------------------------------------------------------------
//
// Every timestamp stored in the schema is the RFC3339 UTC form with a fixed
// fractional part. The format is chosen so that lexicographic order matches
// chronological order — it lets the schema use TEXT for time, gets a stable
// textual sort, and keeps SQLite's index scans correct without needing
// datetime() conversions.

const timeLayout = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// parseTime accepts both the canonical fractional layout and the older
// layout without milliseconds that some imports / older rows may carry. Any
// other form is rejected with a domain validation error so a malformed
// timestamp does not silently become the zero value.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(timeLayout, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("store: unparseable timestamp %q", s)
}

// parseTimePtr returns nil for NULL/empty and the parsed time otherwise.
func parseTimePtr(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// nullableTime turns a *time.Time into a NULL-or-string cell. Empty strings
// never reach this function because parseTime will have rejected them.
func nullableTime(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(*t), Valid: true}
}

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

func encodeJSON(v any) (string, error) {
	if v == nil {
		return "[]", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("store: encode json: %w", err)
	}
	// json.Encoder always appends a newline; strip it for compact storage.
	return strings.TrimRight(buf.String(), "\n"), nil
}

func decodeTags(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("store: decode tags: %w", err)
	}
	return out, nil
}

func decodeAcceptance(s string) ([]domain.AcceptanceItem, error) {
	if s == "" {
		return nil, nil
	}
	var out []domain.AcceptanceItem
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("store: decode acceptance: %w", err)
	}
	return out, nil
}

func decodeMetadata(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("store: decode metadata: %w", err)
	}
	return out, nil
}

func decodeScopes(s string) (domain.Scopes, error) {
	if s == "" {
		return nil, nil
	}
	var raw []string
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("store: decode scopes: %w", err)
	}
	out := make(domain.Scopes, len(raw))
	for i, r := range raw {
		out[i] = domain.Scope(r)
	}
	return out, nil
}

func decodeStrings(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("store: decode string list: %w", err)
	}
	return out, nil
}

func decodePayload(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("store: decode payload: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Constraint / sqlite error mapping
// ---------------------------------------------------------------------------
//
// modernc.org/sqlite does not currently expose an ExtendedCode() method on
// its *sqlite.Error, so we cannot rely on SQLITE_CONSTRAINT_UNIQUE etc.
// The driver does, however, format the error message in a stable
// human-readable form. We assert on substrings ("UNIQUE constraint failed",
// "FOREIGN KEY constraint failed") which are stable in the upstream
// SQLite library and have been so for years.

// IsUniqueViolation reports whether err is a UNIQUE constraint failure.
func IsUniqueViolation(err error) bool {
	if sqliteCode(err) != 19 {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// IsForeignKeyViolation reports whether err is a FOREIGN KEY constraint
// failure.
func IsForeignKeyViolation(err error) bool {
	if sqliteCode(err) != 19 {
		return false
	}
	return strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// IsCheckViolation reports whether err is a CHECK constraint failure.
func IsCheckViolation(err error) bool {
	if sqliteCode(err) != 19 {
		return false
	}
	return strings.Contains(err.Error(), "CHECK constraint failed")
}

func sqliteCode(err error) int {
	var sErr *sqlite.Error
	if errors.As(err, &sErr) {
		return sErr.Code()
	}
	return 0
}

// ---------------------------------------------------------------------------
// Actor context
// ---------------------------------------------------------------------------
//
// The store interface has no Actor parameter — the service layer is the
// only caller, and it knows the actor. For filters that depend on identity
// (ClaimedMine, ClaimedOther) the service layer puts the actor name on the
// context; the store pulls it out and fails loud if it is missing. This is
// a non-breaking addition: callers that never set the actor get a clear
// error, not a silent miscount.

type ctxKey int

const ctxKeyActor ctxKey = 1

// WithActor returns a context carrying the actor name; only service-layer
// code calls this. Repos read it with actorFromContext.
func WithActor(ctx context.Context, actor string) context.Context {
	if actor == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyActor, actor)
}

func actorFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyActor).(string)
	return v, ok && v != ""
}

// injectActor attaches the tx's actor context onto a derived context, so
// repos that need identity info can read it without the Tx interface
// growing a new method. (Used by BuildTaskView and the task filter; the
// caller is always a repository method whose Tx is in fact a *txWrap.)
func actorFromTx(t Tx) (string, bool) {
	tw, ok := t.(*txWrap)
	if !ok {
		return "", false
	}
	return actorFromContext(tw.ctx())
}

// ---------------------------------------------------------------------------
// BuildTaskView — populate a TaskView from a Task and the live data the
// store holds (column, project, open blockers, blocks, subtask tally).
//
// The store layer returns TaskViews only when a higher layer explicitly
// asks for one — primarily when populating a Conflict payload so the caller
// can merge without a second round trip. Most of the time callers see
// *domain.Task, with the view-computing done in service or mcp.
// ---------------------------------------------------------------------------

// buildTaskView hydrates a TaskView. It is safe to call inside an open tx;
// every query is a point read on the reader-writer side that issued the
// call.
func buildTaskView(t Tx, task *domain.Task) (*domain.TaskView, error) {
	if task == nil {
		return nil, nil
	}
	view := &domain.TaskView{Task: *task}
	// Project key
	var projectKey string
	if err := t.(*txWrap).tx.QueryRow(
		"SELECT key FROM projects WHERE id = ?", task.ProjectID,
	).Scan(&projectKey); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: project key: %w", err)
	}
	view.ProjectKey = projectKey
	// Column info
	var colName, colKind string
	if err := t.(*txWrap).tx.QueryRow(
		"SELECT name, kind FROM columns WHERE id = ?", task.ColumnID,
	).Scan(&colName, &colKind); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: column: %w", err)
	}
	view.ColumnName = colName
	view.ColumnKind = domain.Kind(colKind)

	// Open blockers (sort enforced at the SQL level for determinism).
	open, err := linksOpenBlockers(t, task.ID)
	if err != nil {
		return nil, err
	}
	view.BlockedBy = open
	// Blocks (outgoing)
	blocks, err := linksBlocks(t, task.ID)
	if err != nil {
		return nil, err
	}
	view.Blocks = blocks
	// Subtask tally
	var subDone, subTotal int
	row := t.(*txWrap).tx.QueryRow(
		"SELECT "+
			"COALESCE(SUM(CASE WHEN archived_at IS NULL AND column_id IN (SELECT id FROM columns WHERE kind='done') THEN 1 ELSE 0 END),0),"+
			"COUNT(*) "+
			"FROM tasks WHERE parent_id = ?", task.ID)
	if err := row.Scan(&subDone, &subTotal); err != nil {
		return nil, fmt.Errorf("store: subtask tally: %w", err)
	}
	view.SubDone = subDone
	view.SubTotal = subTotal
	// Ready + lease remaining
	now := t.Now()
	view.LeaseRemain = domain.LeaseRemaining(&view.Task, now)
	view.Ready = subTotal == 0 && len(open) == 0 && domain.ClaimableBy(&view.Task, "", now) && view.ColumnKind == domain.KindBacklog
	return view, nil
}

// linksOpenBlockers is the workhorse for OpenBlockers. It returns the keys
// of every open blocker for taskID, sorted. Open = not archived AND not in
// a done column.
func linksOpenBlockers(t Tx, taskID string) ([]string, error) {
	const q = `
SELECT t.key
FROM links l
JOIN tasks t  ON t.id = l.blocker_id
JOIN columns c ON c.id = t.column_id
WHERE l.blocked_id = ?
  AND l.type = 'blocks'
  AND t.archived_at IS NULL
  AND c.kind <> 'done'
ORDER BY t.key ASC, t.id ASC`
	rows, err := t.(*txWrap).tx.Query(q, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: open blockers: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("store: scan blocker key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// linksBlocks returns the keys this task blocks.
func linksBlocks(t Tx, taskID string) ([]string, error) {
	const q = `
SELECT t.key
FROM links l
JOIN tasks t ON t.id = l.blocked_id
WHERE l.blocker_id = ?
  AND l.type = 'blocks'
ORDER BY t.key ASC, t.id ASC`
	rows, err := t.(*txWrap).tx.Query(q, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: blocks: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("store: scan blocked key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// naturalKeyLess orders PROJ-N keys by project then numeric sequence. "BMB-9"
// comes after "BMB-10" only inside a numeric comparison — and we want
// "BMB-10" after "BMB-9", so we split on the LAST dash (matching
// domain.ParseTaskKey).
func naturalKeyLess(a, b string) bool {
	pa, sa, errA := splitKey(a)
	pb, sb, errB := splitKey(b)
	if errA != nil || errB != nil {
		return a < b
	}
	if pa != pb {
		return pa < pb
	}
	return sa < sb
}

func splitKey(s string) (proj string, seq int, err error) {
	i := strings.LastIndex(s, "-")
	if i <= 0 {
		return s, 0, fmt.Errorf("no dash")
	}
	n, convErr := parseSeq(s[i+1:])
	if convErr != nil {
		return s, 0, convErr
	}
	return s[:i], n, nil
}

func parseSeq(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not digits")
		}
		n = n*10 + int(r-'0')
	}
	if len(s) == 0 {
		return 0, fmt.Errorf("empty")
	}
	return n, nil
}

// sortNatural sorts a slice of task keys in place, project alphabetical,
// then numeric.
func sortNatural(keys []string) {
	sort.Slice(keys, func(i, j int) bool { return naturalKeyLess(keys[i], keys[j]) })
}

// ---------------------------------------------------------------------------
// Small utilities used across repos
// ---------------------------------------------------------------------------

// nullString returns sql.NullString{Valid:false} for nil inputs, otherwise
// a valid cell. Used for optional text columns that distinguish "not set"
// from "".
func nullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// makeInClause builds "?,?,?,?" for a slice. The caller must ensure
// len(ids) > 0.
func makeInClause(ids []string) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = strings.TrimRight(placeholders, ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return placeholders, args
}

// intPtr is a tiny helper for setting *int in tests and for the rare
// repository case where the schema has a NULL int column.
func intPtr(i int) *int { return &i }

// wrapf returns a clone of e with its Message extended by formatted args.
// We cannot define methods on a non-local type (*domain.Error), so the
// constructor lives in this package and the domain type stays clean.
func wrapf(e *domain.Error, format string, args ...any) *domain.Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Message = e.Message + ": " + fmt.Sprintf(format, args...)
	return &clone
}

// missingRef builds a CodeNotFound error for a foreign-key violation, where
// the row that is missing is one of two the statement referenced and SQLite
// does not say which. domain.NotFound takes a single identifier, so these
// call sites used to pass a fabricated noun ("parent or column") in the key
// slot — a message that names nothing the caller can look up. msg is a whole
// sentence instead, and it must name every id involved.
func missingRef(msg, remediation string) *domain.Error {
	return &domain.Error{
		Code:        domain.CodeNotFound,
		Message:     msg,
		Remediation: remediation,
	}
}

// idempotencyMismatch builds a fresh CodeIdempotencyMismatch error without
// touching the shared ErrIdempotencyMismatch sentinel.
func idempotencyMismatch(format string, args ...any) *domain.Error {
	return &domain.Error{
		Code:        domain.CodeIdempotencyMismatch,
		Message:     fmt.Sprintf(format, args...),
		Remediation: "Use a new idempotency key, or replay the original request to receive the original response.",
	}
}
