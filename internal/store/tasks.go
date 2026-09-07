package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

type taskRepo struct{ s *sqlStore }

// ---------------------------------------------------------------------------
// Create / Update
// ---------------------------------------------------------------------------

// Create inserts a new task. The caller must have assigned t.ID, t.Key
// and t.ProjectID; the store stamps CreatedAt/UpdatedAt/ColumnEnteredAt
// and Version. Metadata and Acceptance are JSON-encoded here so callers
// see Go types.
func (r *taskRepo) Create(tx Tx, t *domain.Task) error {
	if t == nil {
		return errors.New("store: task.Create: nil task")
	}
	if t.ID == "" || t.Key == "" || t.ProjectID == "" || t.ColumnID == "" {
		return domain.Invalid("task", "id, key, project_id and column_id are required",
			"Set those before calling task Create.")
	}
	if !t.Type.Valid() {
		return domain.Invalid("type", fmt.Sprintf("task type %q is invalid", t.Type),
			"Use one of task, bug, feat, chore, doc, perf, research.")
	}
	if !t.Priority.Valid() {
		return domain.Invalid("priority", fmt.Sprintf("priority %d is invalid", t.Priority),
			"Use 0..4 (none..critical).")
	}
	now, err := tx.Now()
	if err != nil {
		return err
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	if t.ColumnEnteredAt.IsZero() {
		t.ColumnEnteredAt = now
	}
	if t.Version == 0 {
		t.Version = 1
	}
	tagsJSON, err := encodeJSON(t.Tags)
	if err != nil {
		return err
	}
	acceptanceJSON, err := encodeJSON(t.Acceptance)
	if err != nil {
		return err
	}
	metadataJSON, err := encodeJSON(t.Metadata)
	if err != nil {
		return err
	}
	if t.CreatedBy == "" {
		t.CreatedBy = "system"
	}
	t.UpdatedBy = t.CreatedBy
	tw := tx.(*txWrap)
	_, err = tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO tasks(
			id, key, project_id, column_id, parent_id, rank,
			title, body, type, priority, estimate, actual, tags, assignee, reviewer,
			outcome, conclusion,
			claimed_by, claimed_at, claim_expires_at,
			acceptance, due_at, column_entered_at, started_at, done_at,
			version, metadata, created_at, updated_at, created_by, updated_by, archived_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Key, t.ProjectID, t.ColumnID, nullableIDPtr(t.ParentID), t.Rank,
		t.Title, t.Body, string(t.Type), int(t.Priority), nullableFloat(t.Estimate), nullableFloat(t.Actual), tagsJSON, nullString(t.Assignee), nullString(t.Reviewer),
		string(defaultOutcome(t.Outcome)), t.Conclusion,
		nullString(t.ClaimedBy), nullableTime(t.ClaimedAt), nullableTime(t.ClaimExpiresAt),
		acceptanceJSON, nullableTime(t.DueAt), formatTime(t.ColumnEnteredAt), nullableTime(t.StartedAt), nullableTime(t.DoneAt),
		t.Version, metadataJSON,
		formatTime(t.CreatedAt), formatTime(t.UpdatedAt), t.CreatedBy, t.UpdatedBy, nullableTime(t.ArchivedAt),
	)
	if err != nil {
		if IsProjectLocalityViolation(err) {
			return crossProjectEdge("parent")
		}
		if IsUniqueViolation(err) {
			return wrapf(domain.Conflict(nil, 0, 0), "task key %q already exists", t.Key)
		}
		if IsCheckViolation(err) {
			return domain.Invalid("task", "task fields failed a CHECK constraint",
				"Verify type, priority and (column_id) project/column match.")
		}
		if IsForeignKeyViolation(err) {
			parent := "none"
			if t.ParentID != nil {
				parent = *t.ParentID
			}
			return missingRef(
				fmt.Sprintf("task %q references a column (%s) or a parent (%s) that does not exist",
					t.Key, t.ColumnID, parent),
				"Create the column with project_upsert, or check the parent task key, then retry.")
		}
		return fmt.Errorf("store: insert task: %w", err)
	}
	return nil
}

// Update writes content changes: it always bumps version, and when
// ifVersion is supplied it does optimistic concurrency. The current row
// is loaded and attached to a domain.Conflict on mismatch.
//
// Reparenting is part of the contract — t.ParentID is written here when
// set (including a nil pointer, which clears the parent and makes the
// task top-level). The two invariants from PLAN §5 are enforced inside
// this transaction:
//   - MaxSubtaskDepth: a subtask (depth=1) may not itself have a
//     child, i.e. the chosen parent must currently be depth=0.
//   - Self-descendant: t may not be assigned a parent whose chain
//     already includes t.
func (r *taskRepo) Update(tx Tx, t *domain.Task, ifVersion *int) error {
	if t == nil || t.ID == "" {
		return errors.New("store: task.Update: nil or missing id")
	}
	tw := tx.(*txWrap)
	now, err := tx.Now()
	if err != nil {
		return err
	}
	if ifVersion != nil {
		var current int
		err = tw.tx.QueryRowContext(tw.ctx(),
			"SELECT version FROM tasks WHERE id = ?", t.ID,
		).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotFound("task", t.ID)
		}
		if err != nil {
			return fmt.Errorf("store: read task version: %w", err)
		}
		if current != *ifVersion {
			cur, _ := r.GetByID(tx, t.ID)
			return domain.Conflict(cur, *ifVersion, current)
		}
	}
	// Reparent validation: only when the caller actually set ParentID
	// to something (a non-nil pointer OR an explicit clear). We
	// distinguish "leave alone" (caller passes a fresh Task from
	// GetByID which contains the current parent — same pointer or same
	// id) from "set" (a new id) and "clear" (nil pointer).
	if err := r.validateReparent(tx, t); err != nil {
		return err
	}
	tagsJSON, err := encodeJSON(t.Tags)
	if err != nil {
		return err
	}
	acceptanceJSON, err := encodeJSON(t.Acceptance)
	if err != nil {
		return err
	}
	metadataJSON, err := encodeJSON(t.Metadata)
	if err != nil {
		return err
	}
	t.UpdatedAt = now
	res, err := tw.tx.ExecContext(tw.ctx(), `
		UPDATE tasks SET
			title=?, body=?, type=?, priority=?, estimate=?, actual=?, tags=?, assignee=?, reviewer=?,
			outcome=?, conclusion=?,
			parent_id=?, acceptance=?, due_at=?, metadata=?,
			version=version+1, updated_at=?, updated_by=?
		WHERE id = ?`,
		t.Title, t.Body, string(t.Type), int(t.Priority), nullableFloat(t.Estimate), nullableFloat(t.Actual),
		tagsJSON, nullString(t.Assignee), nullString(t.Reviewer),
		string(defaultOutcome(t.Outcome)), t.Conclusion,
		nullableIDPtr(t.ParentID),
		acceptanceJSON, nullableTime(t.DueAt), metadataJSON,
		formatTime(t.UpdatedAt), t.UpdatedBy, t.ID,
	)
	if err != nil {
		if IsProjectLocalityViolation(err) {
			return crossProjectEdge("parent")
		}
		return fmt.Errorf("store: update task: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.NotFound("task", t.ID)
	}
	t.Version++
	return nil
}

// validateReparent enforces the parent-chain rules for an Update. It is
// a no-op when the new parent equals the current parent or when both
// are nil. A non-nil new parent must be (a) top-level (depth 0), and
// (b) not a descendant of the task being reparented.
func (r *taskRepo) validateReparent(tx Tx, t *domain.Task) error {
	tw := tx.(*txWrap)
	// Read current parent_id. If it matches the new value, nothing to
	// validate.
	var currentParent sql.NullString
	err := tw.tx.QueryRowContext(tw.ctx(),
		"SELECT parent_id FROM tasks WHERE id = ?", t.ID,
	).Scan(&currentParent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotFound("task", t.ID)
		}
		return fmt.Errorf("store: read current parent: %w", err)
	}
	newParent := ""
	if t.ParentID != nil {
		newParent = *t.ParentID
	}
	if currentParent.Valid && currentParent.String == newParent {
		return nil
	}
	if !currentParent.Valid && newParent == "" {
		return nil
	}
	// Clearing the parent: always allowed (becomes top-level).
	if newParent == "" {
		return nil
	}
	if newParent == t.ID {
		return domain.Invalid("parent", "a task may not be its own parent",
			"Pass a different task or leave parent unset.")
	}
	// Self-descendant check first: a cycle is the more fundamental
	// violation, and several cycle configurations also violate the
	// depth cap (subtask-of-subtask). Reporting the cycle is the
	// correct shape — the user needs to break the chain, not pick a
	// different top-level parent.
	if _, ok, err := parentChain(tw, newParent, t.ID); err != nil {
		return err
	} else if ok {
		return domain.Cycle([]string{t.Key})
	}
	// Depth check: the new parent must currently be depth 0 (top-level).
	// MaxSubtaskDepth=2 means "a task may have a subtask, but a subtask
	// may not have a child" — so the new parent must have depth 0.
	depth, err := parentChainDepth(tw, newParent)
	if err != nil {
		return err
	}
	if depth >= domain.MaxSubtaskDepth-1 {
		return domain.Invalid("parent",
			fmt.Sprintf("parent is already a subtask (depth %d); subtasks may not have children (max depth %d)", depth, domain.MaxSubtaskDepth),
			"Pick a top-level task as the parent; subtasks are leaves.")
	}
	// Foreign key sanity: the parent row must exist.
	var exists bool
	if err := tw.tx.QueryRowContext(tw.ctx(),
		"SELECT 1 FROM tasks WHERE id = ?", newParent,
	).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotFound("task", newParent)
		}
		return fmt.Errorf("store: check parent exists: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

func (r *taskRepo) GetByKey(tx Tx, key string) (*domain.Task, error) {
	if key == "" {
		return nil, domain.Invalid("key", "task key is empty", "Pass the task key (e.g. BMB-14).")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(),
		`SELECT `+taskColumns+` FROM tasks WHERE key = ? COLLATE NOCASE`, key)
	t, err := scanTaskRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("task", key)
	}
	return t, err
}

func (r *taskRepo) GetByID(tx Tx, id string) (*domain.Task, error) {
	if id == "" {
		return nil, domain.Invalid("id", "task id is empty", "Pass the task UUID.")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(),
		`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	t, err := scanTaskRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("task", id)
	}
	return t, err
}

// GetManyByKeys resolves several task keys in one round trip. Keys match
// case-insensitively, the same way GetByKey does, and the returned map is
// keyed by the uppercase key so a caller that asked for "bmb-14" can still
// find the row. Keys with no row are simply absent — this is a lookup, not
// an assertion that every key exists.
func (r *taskRepo) GetManyByKeys(tx Tx, keys []string) (map[string]*domain.Task, error) {
	if len(keys) == 0 {
		return map[string]*domain.Task{}, nil
	}
	tw := tx.(*txWrap)
	norm := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		norm = append(norm, k)
	}
	if len(norm) == 0 {
		return map[string]*domain.Task{}, nil
	}
	placeholders, args := makeInClause(norm)
	// COLLATE binds to the expression on its left, so the trailing
	// "key IN (?,?) COLLATE NOCASE" this used to carry applied the collation
	// to the comparison's boolean result — a no-op dressed up as a rule.
	// The case-insensitive match actually comes from the column (tasks.key
	// is declared COLLATE NOCASE in 0001_init.sql); stating it on the left
	// operand makes the query mean what it says and keeps this lookup
	// agreeing with GetByKey if that declaration ever changes.
	q := `SELECT ` + taskColumns + ` FROM tasks WHERE key COLLATE NOCASE IN (` + placeholders + `)`
	rows, err := tw.tx.QueryContext(tw.ctx(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: get many tasks: %w", err)
	}
	defer rows.Close()
	out := make(map[string]*domain.Task, len(norm))
	for rows.Next() {
		t, err := scanTaskRows(rows)
		if err != nil {
			return nil, err
		}
		out[strings.ToUpper(t.Key)] = t
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func (r *taskRepo) List(tx Tx, f TaskFilter) ([]*domain.Task, error) {
	actor, _ := actorFromTx(tx)
	clauses, args := buildTaskFilter(f, actor)
	q := `SELECT ` + taskColumns + ` FROM tasks`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY project_id, column_id, rank, key"
	if f.Limit > 0 {
		q += " LIMIT ? OFFSET ?"
		args = append(args, f.Limit, f.Offset)
	}
	tw := tx.(*txWrap)
	rows, err := tw.tx.QueryContext(tw.ctx(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	defer rows.Close()
	var out []*domain.Task
	for rows.Next() {
		t, err := scanTaskRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *taskRepo) Children(tx Tx, parentID string) ([]*domain.Task, error) {
	if parentID == "" {
		return nil, domain.Invalid("parent_id", "parent id is empty", "Pass the task UUID.")
	}
	tw := tx.(*txWrap)
	rows, err := tw.tx.QueryContext(tw.ctx(),
		`SELECT `+taskColumns+` FROM tasks WHERE parent_id = ? ORDER BY rank, key`, parentID)
	if err != nil {
		return nil, fmt.Errorf("store: children: %w", err)
	}
	defer rows.Close()
	var out []*domain.Task
	for rows.Next() {
		t, err := scanTaskRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Archive / Delete
// ---------------------------------------------------------------------------

func (r *taskRepo) Archive(tx Tx, id string, archived bool, actor string) error {
	if id == "" {
		return domain.Invalid("id", "task id is empty", "Pass the task UUID.")
	}
	tw := tx.(*txWrap)
	now, err := tx.Now()
	if err != nil {
		return err
	}
	// archived_at is TEXT in the schema and every reader parses it with
	// parseTime, so it has to be written in the canonical layout — handing
	// the driver a time.Time lets it choose its own encoding.
	var arg any
	if archived {
		arg = formatTime(now)
	} else {
		arg = nil
	}
	res, err := tw.tx.ExecContext(tw.ctx(), `
		UPDATE tasks SET archived_at = ?, version = version + 1, updated_at = ?, updated_by = ?
		WHERE id = ?`, arg, formatTime(now), actor, id)
	if err != nil {
		return fmt.Errorf("store: archive task: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.NotFound("task", id)
	}
	return nil
}

func (r *taskRepo) Delete(tx Tx, id string) error {
	if id == "" {
		return domain.Invalid("id", "task id is empty", "Pass the task UUID.")
	}
	tw := tx.(*txWrap)
	res, err := tw.tx.ExecContext(tw.ctx(), "DELETE FROM tasks WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: delete task: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.NotFound("task", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Claim / Release
// ---------------------------------------------------------------------------

// Claim is the single-statement CAS from PLAN §6.7. It does NOT bump
// version, and an expired former owner cannot renew after another actor
// already took the lease because the WHERE clause checks both expiry
// and ownership.
func (r *taskRepo) Claim(tx Tx, id, actor string, ttl time.Duration) (bool, *domain.Task, error) {
	if id == "" {
		return false, nil, domain.Invalid("id", "task id is empty", "Pass the task UUID.")
	}
	if actor == "" {
		return false, nil, domain.Invalid("actor", "actor is empty", "Service layer must pass the token name.")
	}
	ttl = domain.ClampClaimTTL(ttl)
	now, err := tx.Now()
	if err != nil {
		return false, nil, err
	}
	expires := now.Add(ttl)
	tw := tx.(*txWrap)
	res, err := tw.tx.ExecContext(tw.ctx(), `
		UPDATE tasks
		SET claimed_by = ?, claimed_at = ?, claim_expires_at = ?
		WHERE id = ?
		  AND archived_at IS NULL
		  AND (claimed_by IS NULL OR claim_expires_at < ? OR claimed_by = ?)`,
		actor, formatTime(now), formatTime(expires), id, formatTime(now), actor,
	)
	if err != nil {
		return false, nil, fmt.Errorf("store: claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil, fmt.Errorf("store: claim rows: %w", err)
	}
	task, terr := r.GetByID(tx, id)
	if terr != nil {
		return false, nil, terr
	}
	return n == 1, task, nil
}

// Release is idempotent. If the task is unclaimed, the operation is a
// no-op success. To release someone else's live lease, force must be
// true.
func (r *taskRepo) Release(tx Tx, id, actor string, force bool) (bool, error) {
	if id == "" {
		return false, domain.Invalid("id", "task id is empty", "Pass the task UUID.")
	}
	if actor == "" {
		return false, domain.Invalid("actor", "actor is empty", "Service layer must pass the token name.")
	}
	tw := tx.(*txWrap)
	var claimedBy sql.NullString
	err := tw.tx.QueryRowContext(tw.ctx(),
		"SELECT claimed_by FROM tasks WHERE id = ?", id,
	).Scan(&claimedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, domain.NotFound("task", id)
	}
	if err != nil {
		return false, fmt.Errorf("store: read claim: %w", err)
	}
	if !claimedBy.Valid {
		// Idempotent: nothing to release.
		return true, nil
	}
	holder := claimedBy.String
	if !force && holder != actor {
		return false, nil
	}
	if _, err := tw.tx.ExecContext(tw.ctx(), `
		UPDATE tasks
		SET claimed_by = NULL, claimed_at = NULL, claim_expires_at = NULL
		WHERE id = ?`, id); err != nil {
		return false, fmt.Errorf("store: release: %w", err)
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Move / NeighbourRanks / RenumberColumn
// ---------------------------------------------------------------------------

// Move sets the column, resets ColumnEnteredAt and maintains StartedAt /
// DoneAt. The service layer is expected to have called domain.CheckMove
// (or its admin Force variant) BEFORE Move inside the same transaction.
func (r *taskRepo) Move(tx Tx, id, columnID string, rank int64, actor string) error {
	if id == "" || columnID == "" {
		return domain.Invalid("task", "id and column_id are required", "Pass both.")
	}
	tw := tx.(*txWrap)
	now, err := tx.Now()
	if err != nil {
		return err
	}

	var kind domain.Kind
	if err := tw.tx.QueryRowContext(tw.ctx(),
		"SELECT kind FROM columns WHERE id = ?", columnID,
	).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotFound("column", columnID)
		}
		return fmt.Errorf("store: read column kind: %w", err)
	}
	if !kind.Valid() {
		return domain.Invalid("column", fmt.Sprintf("column kind %q is invalid", kind),
			"Use backlog, active or done.")
	}

	var curColumn string
	var startedAt, doneAt sql.NullString
	if err := tw.tx.QueryRowContext(tw.ctx(),
		"SELECT column_id, started_at, done_at FROM tasks WHERE id = ?", id,
	).Scan(&curColumn, &startedAt, &doneAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotFound("task", id)
		}
		return fmt.Errorf("store: read task state: %w", err)
	}

	newStarted := startedAt
	newDone := doneAt
	switch kind {
	case domain.KindActive:
		if !startedAt.Valid {
			newStarted = sql.NullString{String: formatTime(now), Valid: true}
		}
		if doneAt.Valid {
			newDone = sql.NullString{}
		}
	case domain.KindDone:
		if !doneAt.Valid {
			newDone = sql.NullString{String: formatTime(now), Valid: true}
		}
	default: // KindBacklog
		if doneAt.Valid {
			newDone = sql.NullString{}
		}
	}

	res, err := tw.tx.ExecContext(tw.ctx(), `
		UPDATE tasks SET
			column_id = ?, rank = ?, column_entered_at = ?,
			started_at = ?, done_at = ?,
			version = version + 1, updated_at = ?, updated_by = ?
		WHERE id = ?`,
		columnID, rank, formatTime(now),
		nullableOrNull(newStarted), nullableOrNull(newDone),
		formatTime(now), actor, id,
	)
	if err != nil {
		return fmt.Errorf("store: move task: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.NotFound("task", id)
	}
	return nil
}

// NeighbourRanks returns the ranks bracketing a requested position. The
// service picks a sparse rank strictly between before and after. When the
// gap is exhausted (no integer between before and after), the column is
// renumbered in the same transaction and the recomputed brackets are
// returned.
func (r *taskRepo) NeighbourRanks(tx Tx, columnID string, position RankPosition) (int64, int64, error) {
	if columnID == "" {
		return 0, 0, domain.Invalid("column_id", "column id is empty", "Pass the column UUID.")
	}
	tw := tx.(*txWrap)

	// The top sentinel is 0; the bottom sentinel is math.MaxInt64 so the
	// service can pick any rank below the largest one without overflow.
	const bottomSentinel = int64(9223372036854775807)

	if position == RankTop {
		var minRank sql.NullInt64
		err := tw.tx.QueryRowContext(tw.ctx(),
			"SELECT MIN(rank) FROM tasks WHERE column_id = ? AND archived_at IS NULL", columnID,
		).Scan(&minRank)
		if err != nil {
			return 0, 0, fmt.Errorf("store: min rank: %w", err)
		}
		if !minRank.Valid {
			return 0, domain.RankStep, nil
		}
		before := int64(0)
		after := minRank.Int64
		if after-before <= 1 {
			if err := r.renumberColumnTx(tw, columnID); err != nil {
				return 0, 0, err
			}
			return r.NeighbourRanks(tx, columnID, position)
		}
		return before, after, nil
	}

	var maxRank sql.NullInt64
	err := tw.tx.QueryRowContext(tw.ctx(),
		"SELECT MAX(rank) FROM tasks WHERE column_id = ? AND archived_at IS NULL", columnID,
	).Scan(&maxRank)
	if err != nil {
		return 0, 0, fmt.Errorf("store: max rank: %w", err)
	}
	if !maxRank.Valid {
		return 0, domain.RankStep, nil
	}
	before := maxRank.Int64
	after := bottomSentinel
	if after-before <= 1 {
		if err := r.renumberColumnTx(tw, columnID); err != nil {
			return 0, 0, err
		}
		return r.NeighbourRanks(tx, columnID, position)
	}
	return before, after, nil
}

func (r *taskRepo) RenumberColumn(tx Tx, columnID string) error {
	if columnID == "" {
		return domain.Invalid("column_id", "column id is empty", "Pass the column UUID.")
	}
	tw := tx.(*txWrap)
	return r.renumberColumnTx(tw, columnID)
}

// renumberColumnTx assigns new ranks spaced by RankStep, ordered by the
// current rank then key. The renumber is a maintenance operation; it
// does not bump version because the relative order is unchanged.
func (r *taskRepo) renumberColumnTx(tw *txWrap, columnID string) error {
	rows, err := tw.tx.QueryContext(tw.ctx(), `
		SELECT id FROM tasks WHERE column_id = ?
		ORDER BY rank ASC, key ASC`, columnID)
	if err != nil {
		return fmt.Errorf("store: renumber select: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("store: renumber scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	ts, err := tw.Now()
	if err != nil {
		return err
	}
	now := formatTime(ts)
	for i, id := range ids {
		newRank := int64(i+1) * domain.RankStep
		if _, err := tw.tx.ExecContext(tw.ctx(), `
			UPDATE tasks SET rank = ?, updated_at = ? WHERE id = ?`,
			newRank, now, id,
		); err != nil {
			return fmt.Errorf("store: renumber write: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Task SQL & scanning
// ---------------------------------------------------------------------------

// taskColumns is the canonical SELECT list. New columns go here and
// scanTaskRow / scanTaskRows together.
const taskColumns = `id, key, project_id, column_id, parent_id, rank,
title, body, type, priority, estimate, actual, tags, assignee, reviewer,
outcome, conclusion,
claimed_by, claimed_at, claim_expires_at,
acceptance, due_at, column_entered_at, started_at, done_at,
version, metadata, created_at, updated_at, created_by, updated_by, archived_at`

// scanTemps holds the SQL-bound temporaries. Threads by pointer through
// the scan and the decoder so the public domain.Task stays clean.
type scanTemps struct {
	parent          sql.NullString
	estimate        sql.NullFloat64
	actual          sql.NullFloat64
	assignee        sql.NullString
	reviewer        sql.NullString
	claimedBy       sql.NullString
	claimedAt       sql.NullString
	claimExpiresAt  sql.NullString
	dueAt           sql.NullString
	columnEnteredAt string
	startedAt       sql.NullString
	doneAt          sql.NullString
	createdAt       string
	updatedAt       string
	archivedAt      sql.NullString
	tags            string
	acceptance      string
	metadata        string
}

func taskScanArgs(t *domain.Task, s *scanTemps) []any {
	return []any{
		&t.ID, &t.Key, &t.ProjectID, &t.ColumnID, &s.parent, &t.Rank,
		&t.Title, &t.Body, &t.Type, &t.Priority, &s.estimate, &s.actual, &s.tags, &s.assignee, &s.reviewer,
		&t.Outcome, &t.Conclusion,
		&s.claimedBy, &s.claimedAt, &s.claimExpiresAt,
		&s.acceptance, &s.dueAt, &s.columnEnteredAt, &s.startedAt, &s.doneAt,
		&t.Version, &s.metadata, &s.createdAt, &s.updatedAt, &t.CreatedBy, &t.UpdatedBy, &s.archivedAt,
	}
}

func fillTask(t *domain.Task, s *scanTemps) error {
	if s.parent.Valid {
		v := s.parent.String
		t.ParentID = &v
	}
	if s.estimate.Valid {
		v := s.estimate.Float64
		t.Estimate = &v
	}
	if s.actual.Valid {
		v := s.actual.Float64
		t.Actual = &v
	}
	if s.assignee.Valid {
		v := s.assignee.String
		t.Assignee = &v
	}
	if s.reviewer.Valid {
		v := s.reviewer.String
		t.Reviewer = &v
	}
	if s.claimedBy.Valid {
		v := s.claimedBy.String
		t.ClaimedBy = &v
	}
	if s.claimedAt.Valid {
		v, err := parseTime(s.claimedAt.String)
		if err != nil {
			return err
		}
		t.ClaimedAt = &v
	}
	if s.claimExpiresAt.Valid {
		v, err := parseTime(s.claimExpiresAt.String)
		if err != nil {
			return err
		}
		t.ClaimExpiresAt = &v
	}
	if s.dueAt.Valid {
		v, err := parseTime(s.dueAt.String)
		if err != nil {
			return err
		}
		t.DueAt = &v
	}
	ct, err := parseTime(s.columnEnteredAt)
	if err != nil {
		return err
	}
	t.ColumnEnteredAt = ct
	if s.startedAt.Valid {
		v, err := parseTime(s.startedAt.String)
		if err != nil {
			return err
		}
		t.StartedAt = &v
	}
	if s.doneAt.Valid {
		v, err := parseTime(s.doneAt.String)
		if err != nil {
			return err
		}
		t.DoneAt = &v
	}
	ct2, err := parseTime(s.createdAt)
	if err != nil {
		return err
	}
	t.CreatedAt = ct2
	ct3, err := parseTime(s.updatedAt)
	if err != nil {
		return err
	}
	t.UpdatedAt = ct3
	if s.archivedAt.Valid {
		v, err := parseTime(s.archivedAt.String)
		if err != nil {
			return err
		}
		t.ArchivedAt = &v
	}
	if tgs, err := decodeTags(s.tags); err != nil {
		return err
	} else {
		t.Tags = tgs
	}
	if acc, err := decodeAcceptance(s.acceptance); err != nil {
		return err
	} else {
		t.Acceptance = acc
	}
	if md, err := decodeMetadata(s.metadata); err != nil {
		return err
	} else {
		t.Metadata = md
	}
	return nil
}

// scanTaskRow handles *sql.Row.Scan; ErrNoRows is left for callers to map
// to a domain.NotFound.
func scanTaskRow(row *sql.Row) (*domain.Task, error) {
	var t domain.Task
	var s scanTemps
	if err := row.Scan(taskScanArgs(&t, &s)...); err != nil {
		return nil, err
	}
	if err := fillTask(&t, &s); err != nil {
		return nil, err
	}
	return &t, nil
}

// scanTaskRows handles *sql.Rows.Scan.
func scanTaskRows(rows *sql.Rows) (*domain.Task, error) {
	var t domain.Task
	var s scanTemps
	if err := rows.Scan(taskScanArgs(&t, &s)...); err != nil {
		return nil, fmt.Errorf("store: scan task: %w", err)
	}
	if err := fillTask(&t, &s); err != nil {
		return nil, err
	}
	return &t, nil
}

// nullableOrNull returns an any suitable for Exec: the literal nil for
// a non-valid NullString, the inner string otherwise. It exists because
// passing sql.NullString directly to Exec binds the struct fields as
// individual values, not the underlying string-or-NULL contract.
func nullableOrNull(s sql.NullString) any {
	if !s.Valid {
		return nil
	}
	return s.String
}

// nullableFloat mirrors nullableOrNull for *float64.
func nullableFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// defaultOutcome guards the NOT NULL / CHECK on the outcome column: a task
// value the service left as the zero string would otherwise violate the CHECK.
// Persisting is the last line of defence, so an unset outcome becomes "open".
func defaultOutcome(o domain.Outcome) domain.Outcome {
	if o == "" {
		return domain.OutcomeOpen
	}
	return o
}

// normalizeKeysForLookup trims and upper-cases caller-supplied keys for
// use with the NOCASE comparison. Empty keys are dropped — they would
// match the empty key (no row) and add nothing to the lookup.
func normalizeKeysForLookup(in []string) []string {
	out := make([]string, 0, len(in))
	for _, k := range in {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out = append(out, strings.ToUpper(k))
	}
	return out
}
