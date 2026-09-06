package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

type projectRepo struct{ s *sqlStore }

// Create inserts a new project. The caller must have already assigned p.ID
// (UUID) and validated p.Key; we set timestamps and the initial version.
// The unique-key constraint surfaces as ErrConflict on a duplicate.
func (r *projectRepo) Create(tx Tx, p *domain.Project) error {
	if p == nil {
		return errors.New("store: project.Create: nil project")
	}
	if p.ID == "" || p.Key == "" {
		return domain.Invalid("project", "id and key are required",
			"Set ID and Key before calling project Create.")
	}
	tw := tx.(*txWrap)
	now, err := tx.Now()
	if err != nil {
		return err
	}
	p.CreatedAt = now
	p.UpdatedAt = now
	if p.Version == 0 {
		p.Version = 1
	}
	if p.NextTaskSeq == 0 {
		p.NextTaskSeq = 1
	}
	if p.ClaimTTLSeconds == 0 {
		p.ClaimTTLSeconds = int(domain.ClaimTTLDefault.Seconds())
	}
	if p.EstimateUnit == "" {
		p.EstimateUnit = "h"
	}
	_, err = tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO projects(
			id, key, name, description, version, next_task_seq,
			focus_task_id, estimate_unit, enforce_dependencies, strict_done,
			claim_ttl_seconds, archived_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Key, p.Name, p.Description, p.Version, p.NextTaskSeq,
		nullableIDPtr(p.FocusTaskID), p.EstimateUnit, boolToInt(p.EnforceDependencies), boolToInt(p.StrictDone),
		p.ClaimTTLSeconds, nullableTime(p.ArchivedAt), formatTime(p.CreatedAt), formatTime(p.UpdatedAt),
	)
	if err != nil {
		if IsUniqueViolation(err) {
			return wrapf(domain.Conflict(nil, 0, 0), "project key %q already exists", p.Key)
		}
		return fmt.Errorf("store: insert project: %w", err)
	}
	return nil
}

// Update writes configuration changes (not a content task write, but the
// project's own version is bumped because the brief calls project config
// changes "structural" at the project level). When ifVersion is non-nil
// and does not match, returns domain.Conflict with the current row.
// changes "structural" at the project level). When ifVersion is non-nil
// and does not match, returns domain.Conflict with the current row.
func (r *projectRepo) Update(tx Tx, p *domain.Project, ifVersion *int) error {
	if p == nil || p.ID == "" {
		return errors.New("store: project.Update: nil or missing id")
	}
	tw := tx.(*txWrap)
	now, err := tx.Now()
	if err != nil {
		return err
	}
	p.UpdatedAt = now
	if ifVersion != nil {
		var current int
		err := tw.tx.QueryRowContext(tw.ctx(), "SELECT version FROM projects WHERE id = ?", p.ID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotFound("project", p.ID)
		}
		if err != nil {
			return fmt.Errorf("store: read project version: %w", err)
		}
		if current != *ifVersion {
			cur, _ := r.GetByID(tx, p.ID)
			return domain.Conflict(cur, *ifVersion, current)
		}
	}
	res, err := tw.tx.ExecContext(tw.ctx(), `
		UPDATE projects SET
			name=?, description=?, focus_task_id=?, estimate_unit=?,
			enforce_dependencies=?, strict_done=?, claim_ttl_seconds=?,
			archived_at=?, version=version+1, updated_at=?
		WHERE id=?`,
		p.Name, p.Description, nullableIDPtr(p.FocusTaskID), p.EstimateUnit,
		boolToInt(p.EnforceDependencies), boolToInt(p.StrictDone), p.ClaimTTLSeconds,
		nullableTime(p.ArchivedAt), formatTime(p.UpdatedAt), p.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: project update rows: %w", err)
	}
	if n == 0 {
		return domain.NotFound("project", p.ID)
	}
	// Reflect the new version locally so callers can re-read it.
	p.Version++
	return nil
}

func (r *projectRepo) GetByKey(tx Tx, key string) (*domain.Project, error) {
	if key == "" {
		return nil, domain.Invalid("key", "project key is empty", "Pass the project key (e.g. BMB).")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(), `
		SELECT id, key, name, description, version, next_task_seq,
		       focus_task_id, estimate_unit, enforce_dependencies, strict_done,
		       claim_ttl_seconds, archived_at, created_at, updated_at
		FROM projects WHERE key = ? COLLATE NOCASE`, key)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errProject(key)
	}
	return p, err
}

func (r *projectRepo) GetByID(tx Tx, id string) (*domain.Project, error) {
	if id == "" {
		return nil, domain.Invalid("id", "project id is empty", "Pass the project UUID.")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(), `
		SELECT id, key, name, description, version, next_task_seq,
		       focus_task_id, estimate_unit, enforce_dependencies, strict_done,
		       claim_ttl_seconds, archived_at, created_at, updated_at
		FROM projects WHERE id = ?`, id)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errProject(id)
	}
	return p, err
}

func (r *projectRepo) List(tx Tx, includeArchived bool) ([]*domain.Project, error) {
	tw := tx.(*txWrap)
	q := `SELECT id, key, name, description, version, next_task_seq,
		       focus_task_id, estimate_unit, enforce_dependencies, strict_done,
		       claim_ttl_seconds, archived_at, created_at, updated_at
		FROM projects`
	if !includeArchived {
		q += " WHERE archived_at IS NULL"
	}
	q += " ORDER BY key ASC"
	rows, err := tw.tx.QueryContext(tw.ctx(), q)
	if err != nil {
		return nil, fmt.Errorf("store: list projects: %w", err)
	}
	defer rows.Close()
	var out []*domain.Project
	for rows.Next() {
		p, err := scanProjectRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// NextTaskSeq allocates the next sequence number atomically. The UPDATE…
// RETURNING pattern is supported by modernc (SQLite 3.35+). We subtract 1
// to return the value the caller just consumed; updating to a new value
// inside the same row is what makes the allocation serializable.
func (r *projectRepo) NextTaskSeq(tx Tx, projectID string) (int, error) {
	if projectID == "" {
		return 0, domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	tw := tx.(*txWrap)
	var seq int
	err := tw.tx.QueryRowContext(tw.ctx(), `
		UPDATE projects SET next_task_seq = next_task_seq + 1
		WHERE id = ?
		RETURNING next_task_seq - 1`, projectID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, domain.NotFound("project", projectID)
	}
	if err != nil {
		return 0, fmt.Errorf("store: next_task_seq: %w", err)
	}
	return seq, nil
}

// SetFocus updates the project's focus pointer and bumps the project
// version. The focus event is logged at the service layer.
func (r *projectRepo) SetFocus(tx Tx, projectID string, taskID *string) error {
	if projectID == "" {
		return domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	tw := tx.(*txWrap)
	ts, err := tx.Now()
	if err != nil {
		return err
	}
	now := formatTime(ts)
	res, err := tw.tx.ExecContext(tw.ctx(), `
		UPDATE projects SET focus_task_id = ?, version = version + 1, updated_at = ?
		WHERE id = ?`, nullableIDPtr(taskID), now, projectID)
	if err != nil {
		return fmt.Errorf("store: set focus: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.NotFound("project", projectID)
	}
	return nil
}

func (r *projectRepo) Archive(tx Tx, projectID string, archived bool) error {
	if projectID == "" {
		return domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	tw := tx.(*txWrap)
	ts, err := tx.Now()
	if err != nil {
		return err
	}
	now := formatTime(ts)
	var arg any
	if archived {
		arg = now
	} else {
		arg = nil
	}
	res, err := tw.tx.ExecContext(tw.ctx(), `
		UPDATE projects SET archived_at = ?, version = version + 1, updated_at = ?
		WHERE id = ?`, arg, now, projectID)
	if err != nil {
		return fmt.Errorf("store: archive project: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.NotFound("project", projectID)
	}
	return nil
}

// Delete hard-removes a project. Only the admin CLI calls it (the store
// surface never exposes it). Cascade order matters: tasks reference
// columns with ON DELETE RESTRICT, so we drop tasks first. Notes and
// links cascade naturally.
func (r *projectRepo) Delete(tx Tx, projectID string) error {
	if projectID == "" {
		return domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	tw := tx.(*txWrap)
	if _, err := tw.tx.ExecContext(tw.ctx(), "DELETE FROM tasks WHERE project_id = ?", projectID); err != nil {
		if IsForeignKeyViolation(err) {
			return domain.Forbidden(
				"project still has dependent data the cascade could not remove",
				"Run kanban doctor and try again; if this persists, restore from backup.",
			)
		}
		return fmt.Errorf("store: delete tasks: %w", err)
	}
	if _, err := tw.tx.ExecContext(tw.ctx(), "DELETE FROM columns WHERE project_id = ?", projectID); err != nil {
		return fmt.Errorf("store: delete columns: %w", err)
	}
	res, err := tw.tx.ExecContext(tw.ctx(), "DELETE FROM projects WHERE id = ?", projectID)
	if err != nil {
		return fmt.Errorf("store: delete project: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.NotFound("project", projectID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// errProject is the project-scoped NotFound builder. Only a caller that
// still holds the identifier it looked the row up by may call it — see the
// note on scanProject.
var errProject = func(key string) *domain.Error { return domain.NotFound("project", key) }

// internalErrorf wraps an *Error with an extra message context for
// unexpected situations, e.g. "this should never happen" branches.
// Implemented in links.go as a package-level helper (we cannot define
// methods on a non-local type).

// scanProject / scanProjectRows materialize a Project from a query.
//
// scanProject deliberately passes sql.ErrNoRows straight back instead of
// turning it into a domain error: it is handed a *sql.Row and has no idea
// which key or id the caller queried with. It used to answer with
// domain.NotFound("project", "?"), and that literal "?" travelled all the
// way out to the MCP client as `project "?" not found` — an error naming
// nothing an agent could act on. Only GetByKey/GetByID, which still hold the
// identifier, may build the NotFound.
func scanProject(row *sql.Row) (*domain.Project, error) {
	var (
		p             domain.Project
		focus         sql.NullString
		archived      sql.NullString
		created, upd  string
		enforceSD, sd int
	)
	err := row.Scan(
		&p.ID, &p.Key, &p.Name, &p.Description, &p.Version, &p.NextTaskSeq,
		&focus, &p.EstimateUnit, &enforceSD, &sd,
		&p.ClaimTTLSeconds, &archived, &created, &upd,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan project: %w", err)
	}
	if focus.Valid {
		s := focus.String
		p.FocusTaskID = &s
	}
	if archived.Valid {
		t, err := parseTime(archived.String)
		if err != nil {
			return nil, err
		}
		p.ArchivedAt = &t
	}
	if t, err := parseTime(created); err == nil {
		p.CreatedAt = t
	}
	if t, err := parseTime(upd); err == nil {
		p.UpdatedAt = t
	}
	p.EnforceDependencies = enforceSD != 0
	p.StrictDone = sd != 0
	return &p, nil
}

// scanProjectRows is the *sql.Rows variant for List. Two helpers because
// sql.Row.Scan and sql.Rows.Scan differ in what they return on no-rows.
func scanProjectRows(rows *sql.Rows) (*domain.Project, error) {
	var (
		p             domain.Project
		focus         sql.NullString
		archived      sql.NullString
		created, upd  string
		enforceSD, sd int
	)
	if err := rows.Scan(
		&p.ID, &p.Key, &p.Name, &p.Description, &p.Version, &p.NextTaskSeq,
		&focus, &p.EstimateUnit, &enforceSD, &sd,
		&p.ClaimTTLSeconds, &archived, &created, &upd,
	); err != nil {
		return nil, fmt.Errorf("store: scan project row: %w", err)
	}
	if focus.Valid {
		s := focus.String
		p.FocusTaskID = &s
	}
	if archived.Valid {
		t, err := parseTime(archived.String)
		if err != nil {
			return nil, err
		}
		p.ArchivedAt = &t
	}
	if t, err := parseTime(created); err == nil {
		p.CreatedAt = t
	}
	if t, err := parseTime(upd); err == nil {
		p.UpdatedAt = t
	}
	p.EnforceDependencies = enforceSD != 0
	p.StrictDone = sd != 0
	return &p, nil
}

// boolToInt turns a Go bool into the 0/1 INTEGER the schema stores.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullableIDPtr stores *string as either a real value or SQL NULL.
func nullableIDPtr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// _ keeps the strings import used somewhere in this file even when the
// project scanning is the only thing that pulls in strings.
var _ = strings.TrimSpace

// _ keeps the context import for any future context-aware helpers.
var _ = context.Background
