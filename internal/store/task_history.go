package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// Task lifecycle journal (KANB-30)
// ---------------------------------------------------------------------------
//
// Every entry is appended by the store inside the SAME transaction as the
// mutation it describes. The service layer must not append here: a rule that
// lives one layer above the write is a rule the next new write path silently
// skips, and a skipped lifecycle entry does not fail a test — it shows up
// months later as a kink in a chart nobody can explain.
//
// See migrations/0006_task_history.sql for the schema, the retention rule
// (forever) and why the baseline is seeded events rather than a snapshot.

// TaskHistoryKind names what a journal entry records. See the migration for
// the full prose; the short version is one constant per lifecycle change the
// charts are built from.
type TaskHistoryKind string

const (
	// HistoryExists is the baseline seed: "this card already existed at the
	// origin instant", carrying its whole state. Only migration 0006 writes it.
	HistoryExists TaskHistoryKind = "exists"
	// HistoryCreated records a task creation, carrying the same whole state.
	HistoryCreated TaskHistoryKind = "created"
	// HistoryMoved records a column change, with the KIND of both columns.
	HistoryMoved TaskHistoryKind = "moved"
	// HistoryArchived records an archival.
	HistoryArchived TaskHistoryKind = "archived"
	// HistoryRestored records an un-archival.
	HistoryRestored TaskHistoryKind = "restored"
	// HistoryEstimate records an estimate change, old and new.
	HistoryEstimate TaskHistoryKind = "estimate"
	// HistoryParent records a parent change, old and new — it moves the set of
	// leaves, which the estimate rollup is computed over.
	HistoryParent TaskHistoryKind = "parent"
	// HistoryColumnKind records a COLUMN's kind change. No task row is touched
	// by that edit, yet every card in the column changes bucket.
	HistoryColumnKind TaskHistoryKind = "column_kind"
)

// TaskHistoryEntry is one row of the journal. Pointer fields are the ones the
// schema leaves NULL for kinds that do not carry them; a nil pointer means
// "not applicable to this kind", never "zero".
type TaskHistoryEntry struct {
	ID        int64
	TS        time.Time
	Actor     string
	ProjectID string
	// TaskID is nil only for HistoryColumnKind, which describes a column.
	TaskID *string
	Kind   TaskHistoryKind
	// Reconstructed is true only for the baseline seed written by the
	// migration: state asserted at origin, not observed as it happened.
	Reconstructed bool

	// Archived is the archived flag AFTER the entry, for exists/created.
	Archived    *bool
	FromColumn  *string
	FromKind    *domain.Kind
	ToColumn    *string
	ToKind      *domain.Kind
	OldEstimate *float64
	NewEstimate *float64
	OldParent   *string
	NewParent   *string
}

// TaskHistoryRepo reads and appends the lifecycle journal. There is no Prune,
// no Delete and no Update: the table is append-only and kept forever.
type TaskHistoryRepo interface {
	// Append writes one entry. Callers are store-internal mutation paths.
	Append(tx Tx, e *TaskHistoryEntry) error
	// ListByProject returns the project's whole journal, oldest first, ordered
	// by (ts, id). Entries with a nil TaskID (column kind changes) of that
	// project are included: they change which bucket its cards fall into.
	ListByProject(tx Tx, projectID string) ([]TaskHistoryEntry, error)
	// Origin returns the instant from which the journal is trustworthy — the
	// moment migration 0006 ran. Nothing before it may be drawn as a line.
	Origin(tx Tx) (time.Time, error)
}

type taskHistoryRepo struct{ s *sqlStore }

func (r *taskHistoryRepo) Append(tx Tx, e *TaskHistoryEntry) error {
	if e == nil {
		return errors.New("store: task history: nil entry")
	}
	if e.Kind == "" {
		return domain.Invalid("task_history", "kind is required", "Set the entry kind before Append.")
	}
	if e.ProjectID == "" {
		return domain.Invalid("task_history", "project_id is required", "Journal entries are scoped to a project.")
	}
	if e.Kind != HistoryColumnKind && (e.TaskID == nil || *e.TaskID == "") {
		return domain.Invalid("task_history", "task_id is required", "Only column_kind entries may omit the task.")
	}
	if e.TS.IsZero() {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		e.TS = now
	}
	tw := tx.(*txWrap)
	res, err := tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO task_history(
			ts, actor, project_id, task_id, kind, reconstructed,
			archived, from_column, from_kind, to_column, to_kind,
			old_estimate, new_estimate, old_parent, new_parent)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		formatTime(e.TS), e.Actor, e.ProjectID, nullableIDPtr(e.TaskID), string(e.Kind), boolToInt(e.Reconstructed),
		nullableBool(e.Archived), nullableIDPtr(e.FromColumn), nullableKind(e.FromKind),
		nullableIDPtr(e.ToColumn), nullableKind(e.ToKind),
		nullableFloat(e.OldEstimate), nullableFloat(e.NewEstimate),
		nullableIDPtr(e.OldParent), nullableIDPtr(e.NewParent),
	)
	if err != nil {
		return fmt.Errorf("store: append task history: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: task history last insert id: %w", err)
	}
	e.ID = id
	return nil
}

func (r *taskHistoryRepo) ListByProject(tx Tx, projectID string) ([]TaskHistoryEntry, error) {
	if projectID == "" {
		return nil, domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	tw := tx.(*txWrap)
	rows, err := tw.tx.QueryContext(tw.ctx(), `
		SELECT id, ts, actor, project_id, task_id, kind, reconstructed,
		       archived, from_column, from_kind, to_column, to_kind,
		       old_estimate, new_estimate, old_parent, new_parent
		FROM task_history WHERE project_id = ? ORDER BY ts ASC, id ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: read task history: %w", err)
	}
	defer rows.Close()
	var out []TaskHistoryEntry
	for rows.Next() {
		var (
			e          TaskHistoryEntry
			ts, kind   string
			recon      int
			taskID     sql.NullString
			archived   sql.NullInt64
			fromCol    sql.NullString
			fromKind   sql.NullString
			toCol      sql.NullString
			toKind     sql.NullString
			oldEst     sql.NullFloat64
			newEst     sql.NullFloat64
			oldP, newP sql.NullString
		)
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.ProjectID, &taskID, &kind, &recon,
			&archived, &fromCol, &fromKind, &toCol, &toKind,
			&oldEst, &newEst, &oldP, &newP); err != nil {
			return nil, fmt.Errorf("store: scan task history: %w", err)
		}
		t, err := parseTime(ts)
		if err != nil {
			return nil, err
		}
		e.TS = t
		e.Kind = TaskHistoryKind(kind)
		e.Reconstructed = recon != 0
		e.TaskID = ptrFromNullString(taskID)
		if archived.Valid {
			b := archived.Int64 != 0
			e.Archived = &b
		}
		e.FromColumn = ptrFromNullString(fromCol)
		e.ToColumn = ptrFromNullString(toCol)
		e.FromKind = kindPtr(fromKind)
		e.ToKind = kindPtr(toKind)
		if oldEst.Valid {
			v := oldEst.Float64
			e.OldEstimate = &v
		}
		if newEst.Valid {
			v := newEst.Float64
			e.NewEstimate = &v
		}
		e.OldParent = ptrFromNullString(oldP)
		e.NewParent = ptrFromNullString(newP)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *taskHistoryRepo) Origin(tx Tx) (time.Time, error) {
	tw := tx.(*txWrap)
	var ts string
	err := tw.tx.QueryRowContext(tw.ctx(), "SELECT started_at FROM task_history_origin WHERE id = 1").Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, errors.New("store: task history origin row is missing")
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: read task history origin: %w", err)
	}
	return parseTime(ts)
}

// ---------------------------------------------------------------------------
// Write helpers used by the task and column mutation paths
// ---------------------------------------------------------------------------

// taskLifecycleState is the whole lifecycle-relevant state of one task at one
// instant: what a 'created' (or seeded 'exists') entry carries.
type taskLifecycleState struct {
	columnID string
	kind     domain.Kind
	estimate *float64
	parent   *string
	archived bool
}

// recordWholeState appends a 'created'-style entry carrying the entire
// lifecycle-relevant state of one task.
func (r *taskHistoryRepo) recordWholeState(tx Tx, kind TaskHistoryKind, projectID, taskID, actor string, st taskLifecycleState) error {
	colKind := st.kind
	archived := st.archived
	return r.Append(tx, &TaskHistoryEntry{
		Actor:       actor,
		ProjectID:   projectID,
		TaskID:      &taskID,
		Kind:        kind,
		Archived:    &archived,
		ToColumn:    &st.columnID,
		ToKind:      &colKind,
		NewEstimate: st.estimate,
		NewParent:   st.parent,
	})
}

func nullableBool(b *bool) any {
	if b == nil {
		return nil
	}
	return boolToInt(*b)
}

func nullableKind(k *domain.Kind) any {
	if k == nil {
		return nil
	}
	return string(*k)
}

func ptrFromNullString(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

func kindPtr(ns sql.NullString) *domain.Kind {
	if !ns.Valid {
		return nil
	}
	k := domain.Kind(ns.String)
	return &k
}

// floatPtrEqual reports whether two optional estimates are the same value,
// treating nil (unestimated) as distinct from any number.
func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// strPtrEqual reports whether two optional ids are the same, treating nil
// (no parent) as distinct from any id.
func strPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// lifecycleBefore reads the lifecycle-relevant state of one task as it stands
// right now — the "old" half of every entry a mutation path records. It is a
// single indexed row read on the writer connection, inside the caller's own
// transaction, so the before/after pair can never straddle another writer.
func (r *taskHistoryRepo) lifecycleBefore(tx Tx, taskID string) (projectID string, st taskLifecycleState, err error) {
	tw := tx.(*txWrap)
	var (
		estimate   sql.NullFloat64
		parent     sql.NullString
		archivedAt sql.NullString
	)
	err = tw.tx.QueryRowContext(tw.ctx(), `
		SELECT t.project_id, t.column_id, c.kind, t.estimate, t.parent_id, t.archived_at
		FROM tasks t JOIN columns c ON c.id = t.column_id
		WHERE t.id = ?`, taskID,
	).Scan(&projectID, &st.columnID, &st.kind, &estimate, &parent, &archivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", st, domain.NotFound("task", taskID)
	}
	if err != nil {
		return "", st, fmt.Errorf("store: read task lifecycle state: %w", err)
	}
	if estimate.Valid {
		v := estimate.Float64
		st.estimate = &v
	}
	st.parent = ptrFromNullString(parent)
	st.archived = archivedAt.Valid
	return projectID, st, nil
}

// recordCreated journals a task creation. The column kind is read here rather
// than taken from the caller: Create is handed a domain.Task, which knows its
// column id but not that column's kind, and guessing it would be the first
// place the journal and the board could disagree.
func (r *taskHistoryRepo) recordCreated(tx Tx, t *domain.Task) error {
	tw := tx.(*txWrap)
	var kind domain.Kind
	if err := tw.tx.QueryRowContext(tw.ctx(),
		"SELECT kind FROM columns WHERE id = ?", t.ColumnID,
	).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotFound("column", t.ColumnID)
		}
		return fmt.Errorf("store: read column kind for journal: %w", err)
	}
	return r.recordWholeState(tx, HistoryCreated, t.ProjectID, t.ID, t.CreatedBy, taskLifecycleState{
		columnID: t.ColumnID,
		kind:     kind,
		estimate: t.Estimate,
		parent:   t.ParentID,
		archived: t.ArchivedAt != nil,
	})
}

// recordContentChange journals the estimate and parent halves of an Update.
// Nothing is written when neither actually moved: the journal records changes,
// and a row saying "estimate 3 became estimate 3" is noise a reader would have
// to learn to ignore.
func (r *taskHistoryRepo) recordContentChange(tx Tx, projectID, taskID, actor string, before, after taskLifecycleState) error {
	if !floatPtrEqual(before.estimate, after.estimate) {
		if err := r.Append(tx, &TaskHistoryEntry{
			Actor:       actor,
			ProjectID:   projectID,
			TaskID:      &taskID,
			Kind:        HistoryEstimate,
			OldEstimate: before.estimate,
			NewEstimate: after.estimate,
		}); err != nil {
			return err
		}
	}
	if !strPtrEqual(before.parent, after.parent) {
		if err := r.Append(tx, &TaskHistoryEntry{
			Actor:     actor,
			ProjectID: projectID,
			TaskID:    &taskID,
			Kind:      HistoryParent,
			OldParent: before.parent,
			NewParent: after.parent,
		}); err != nil {
			return err
		}
	}
	return nil
}

// recordArchive journals an archival or a restore. Archive is idempotent, so
// a call that does not change the flag records nothing.
func (r *taskHistoryRepo) recordArchive(tx Tx, projectID, taskID, actor string, was, now bool) error {
	if was == now {
		return nil
	}
	kind := HistoryRestored
	if now {
		kind = HistoryArchived
	}
	archived := now
	return r.Append(tx, &TaskHistoryEntry{
		Actor:     actor,
		ProjectID: projectID,
		TaskID:    &taskID,
		Kind:      kind,
		Archived:  &archived,
	})
}

// recordMove journals a column change, carrying the KIND of both columns so a
// replay knows which bucket the card left and which it entered without having
// to ask today's columns table what those columns mean now.
func (r *taskHistoryRepo) recordMove(tx Tx, projectID, taskID, actor string, before taskLifecycleState, toColumn string, toKind domain.Kind) error {
	if before.columnID == toColumn {
		return nil
	}
	fromColumn := before.columnID
	fromKind := before.kind
	return r.Append(tx, &TaskHistoryEntry{
		Actor:      actor,
		ProjectID:  projectID,
		TaskID:     &taskID,
		Kind:       HistoryMoved,
		FromColumn: &fromColumn,
		FromKind:   &fromKind,
		ToColumn:   &toColumn,
		ToKind:     &toKind,
	})
}

// recordColumnKind journals a COLUMN's kind change. No task row is touched by
// such an edit, yet every card sitting in the column changes bucket — turning
// an active column into a done one finishes all of them at once. Without this
// entry a replay would disagree with the live board and no test would notice.
func (r *taskHistoryRepo) recordColumnKind(tx Tx, projectID, columnID, actor string, from, to domain.Kind) error {
	if from == to {
		return nil
	}
	col := columnID
	fromKind := from
	toKind := to
	return r.Append(tx, &TaskHistoryEntry{
		Actor:      actor,
		ProjectID:  projectID,
		Kind:       HistoryColumnKind,
		FromColumn: &col,
		FromKind:   &fromKind,
		ToColumn:   &col,
		ToKind:     &toKind,
	})
}
