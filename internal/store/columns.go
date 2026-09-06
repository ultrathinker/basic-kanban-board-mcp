package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

type columnRepo struct{ s *sqlStore }

func (r *columnRepo) ListByProject(tx Tx, projectID string) ([]*domain.Column, error) {
	if projectID == "" {
		return nil, domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	tw := tx.(*txWrap)
	rows, err := tw.tx.QueryContext(tw.ctx(), `
		SELECT id, project_id, name, position, kind, wip_limit
		FROM columns WHERE project_id = ?
		ORDER BY position ASC, name ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list columns: %w", err)
	}
	defer rows.Close()
	var out []*domain.Column
	for rows.Next() {
		c, err := scanColumn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *columnRepo) GetByName(tx Tx, projectID, name string) (*domain.Column, error) {
	if projectID == "" || name == "" {
		return nil, domain.Invalid("column", "project id and name are required", "Pass both the project UUID and the column name.")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(), `
		SELECT id, project_id, name, position, kind, wip_limit
		FROM columns WHERE project_id = ? AND name = ? COLLATE NOCASE`, projectID, name)
	c, err := scanColumnRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("column", name)
	}
	return c, err
}

func (r *columnRepo) GetByID(tx Tx, id string) (*domain.Column, error) {
	if id == "" {
		return nil, domain.Invalid("id", "column id is empty", "Pass the column UUID.")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(), `
		SELECT id, project_id, name, position, kind, wip_limit
		FROM columns WHERE id = ?`, id)
	c, err := scanColumnRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("column", id)
	}
	return c, err
}

func (r *columnRepo) Create(tx Tx, c *domain.Column) error {
	if c == nil || c.ID == "" || c.ProjectID == "" || c.Name == "" {
		return domain.Invalid("column", "id, project_id and name are required", "Set those before calling column Create.")
	}
	if !c.Kind.Valid() {
		return domain.Invalid("kind", fmt.Sprintf("column kind %q is invalid", c.Kind),
			"Use one of: backlog, active, done.")
	}
	tw := tx.(*txWrap)
	_, err := tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO columns(id, project_id, name, position, kind, wip_limit)
		VALUES (?,?,?,?,?,?)`,
		c.ID, c.ProjectID, c.Name, c.Position, string(c.Kind), nullableIntPtr(c.WIPLimit),
	)
	if err != nil {
		if IsUniqueViolation(err) {
			return wrapf(domain.Conflict(nil, 0, 0), "column name %q already exists in project", c.Name)
		}
		return fmt.Errorf("store: insert column: %w", err)
	}
	return nil
}

func (r *columnRepo) Update(tx Tx, c *domain.Column) error {
	if c == nil || c.ID == "" {
		return domain.Invalid("column", "id is required", "Set the column id before Update.")
	}
	if !c.Kind.Valid() {
		return domain.Invalid("kind", fmt.Sprintf("column kind %q is invalid", c.Kind),
			"Use one of: backlog, active, done.")
	}
	tw := tx.(*txWrap)
	res, err := tw.tx.ExecContext(tw.ctx(), `
		UPDATE columns SET name=?, position=?, kind=?, wip_limit=?
		WHERE id=?`,
		c.Name, c.Position, string(c.Kind), nullableIntPtr(c.WIPLimit), c.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update column: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.NotFound("column", c.ID)
	}
	return nil
}

// Delete refuses if any non-archived tasks still reference the column. The
// task removal path always uses the service layer; this guard is the
// last line of defence for direct admin calls.
func (r *columnRepo) Delete(tx Tx, id string) error {
	if id == "" {
		return domain.Invalid("id", "column id is empty", "Pass the column UUID.")
	}
	tw := tx.(*txWrap)
	var n int
	if err := tw.tx.QueryRowContext(tw.ctx(),
		"SELECT COUNT(*) FROM tasks WHERE column_id = ? AND archived_at IS NULL", id,
	).Scan(&n); err != nil {
		return fmt.Errorf("store: count column tasks: %w", err)
	}
	if n > 0 {
		return domain.Forbidden(
			fmt.Sprintf("column still holds %d unarchived task(s)", n),
			"Move or archive the tasks first; column delete never silently loses data.",
		)
	}
	res, err := tw.tx.ExecContext(tw.ctx(), "DELETE FROM columns WHERE id = ?", id)
	if err != nil {
		if IsForeignKeyViolation(err) {
			return domain.Forbidden(
				"column is still referenced by tasks",
				"Move or archive the tasks first.",
			)
		}
		return fmt.Errorf("store: delete column: %w", err)
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		return domain.NotFound("column", id)
	}
	return nil
}

// CountTasks returns unarchived tasks in the column, optionally excluding
// one task. Used by CheckMove inside the same transaction as the move.
func (r *columnRepo) CountTasks(tx Tx, columnID string, excludeTaskID string) (int, error) {
	if columnID == "" {
		return 0, domain.Invalid("column_id", "column id is empty", "Pass the column UUID.")
	}
	tw := tx.(*txWrap)
	var n int
	q := "SELECT COUNT(*) FROM tasks WHERE column_id = ? AND archived_at IS NULL"
	args := []any{columnID}
	if excludeTaskID != "" {
		q += " AND id <> ?"
		args = append(args, excludeTaskID)
	}
	if err := tw.tx.QueryRowContext(tw.ctx(), q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count column tasks: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// scan helpers
// ---------------------------------------------------------------------------

// scanColumnRow handles *sql.Row.Scan; ErrNoRows becomes a domain.NotFound.
func scanColumnRow(row *sql.Row) (*domain.Column, error) {
	var c domain.Column
	var wip sql.NullInt64
	if err := row.Scan(&c.ID, &c.ProjectID, &c.Name, &c.Position, &c.Kind, &wip); err != nil {
		return nil, err
	}
	if wip.Valid {
		v := int(wip.Int64)
		c.WIPLimit = &v
	}
	return &c, nil
}

// scanColumn is the *sql.Rows variant.
func scanColumn(rows *sql.Rows) (*domain.Column, error) {
	var c domain.Column
	var wip sql.NullInt64
	if err := rows.Scan(&c.ID, &c.ProjectID, &c.Name, &c.Position, &c.Kind, &wip); err != nil {
		return nil, fmt.Errorf("store: scan column: %w", err)
	}
	if wip.Valid {
		v := int(wip.Int64)
		c.WIPLimit = &v
	}
	return &c, nil
}

func nullableIntPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
