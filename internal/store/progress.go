package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

type progressRepo struct{ s *sqlStore }

// progressScope is the WHERE fragment shared by every scoped progress read
// and delete. A taskID selects that task's scope; a nil taskID selects the
// project-level scope (task_id IS NULL). The fragment always carries the
// project filter — marks are never read or removed across projects.
func progressScope(projectID string, taskID *string) (string, []any) {
	if taskID == nil {
		return "project_id = ? AND task_id IS NULL", []any{projectID}
	}
	return "project_id = ? AND task_id = ?", []any{projectID, *taskID}
}

func (r *progressRepo) Add(tx Tx, m *domain.ProgressMark) error {
	if m == nil {
		return errors.New("store: progress.Add: nil mark")
	}
	if m.ID == "" || m.ProjectID == "" || m.Assessor == "" {
		return domain.Invalid("progress_mark", "id, project_id and assessor are required",
			"Set those fields before calling progress Add.")
	}
	if m.CreatedAt.IsZero() {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		m.CreatedAt = now
	}
	tw := tx.(*txWrap)
	var taskID any
	if m.TaskID != nil {
		taskID = *m.TaskID
	}
	_, err := tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO progress_marks(id, project_id, task_id, assessor, percent, eta, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		m.ID, m.ProjectID, taskID, m.Assessor, m.Percent, nullableTime(m.ETA), formatTime(m.CreatedAt),
	)
	if err != nil {
		if IsForeignKeyViolation(err) {
			task := "none"
			if m.TaskID != nil {
				task = *m.TaskID
			}
			return missingRef(
				fmt.Sprintf("progress mark %s references a project (%s) or a task (%s) that does not exist",
					m.ID, m.ProjectID, task),
				"Check both ids with a board read, then retry.",
			)
		}
		if IsCheckViolation(err) {
			return domain.Invalid("percent",
				fmt.Sprintf("progress percent %d is outside 0..100", m.Percent),
				"Send a percent between 0 and 100.")
		}
		return fmt.Errorf("store: insert progress mark: %w", err)
	}
	return nil
}

// LatestByAssessor returns the newest mark of every assessor in the scope,
// one row per assessor, sorted by assessor so reads are deterministic. The
// id tie-break in the window order keeps the one-row-per-assessor promise
// even when two marks land on the same timestamp.
func (r *progressRepo) LatestByAssessor(tx Tx, projectID string, taskID *string) ([]domain.ProgressMark, error) {
	if projectID == "" {
		return nil, domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	scope, args := progressScope(projectID, taskID)
	tw := tx.(*txWrap)
	q := `SELECT id, project_id, task_id, assessor, percent, eta, created_at
	      FROM (
	          SELECT id, project_id, task_id, assessor, percent, eta, created_at,
	                 ROW_NUMBER() OVER (
	                     PARTITION BY assessor
	                     ORDER BY created_at DESC, id DESC
	                 ) AS rn
	          FROM progress_marks
	          WHERE ` + scope + `
	      )
	      WHERE rn = 1
	      ORDER BY assessor ASC`
	rows, err := tw.tx.QueryContext(tw.ctx(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: latest progress marks: %w", err)
	}
	defer rows.Close()
	return scanProgressMarks(rows)
}

// History returns every mark in the scope, oldest first. Nothing thins the
// table, so this grows forever by design.
func (r *progressRepo) History(tx Tx, projectID string, taskID *string) ([]domain.ProgressMark, error) {
	if projectID == "" {
		return nil, domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	scope, args := progressScope(projectID, taskID)
	tw := tx.(*txWrap)
	rows, err := tw.tx.QueryContext(tw.ctx(), `
		SELECT id, project_id, task_id, assessor, percent, eta, created_at
		FROM progress_marks
		WHERE `+scope+`
		ORDER BY created_at ASC, id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: progress history: %w", err)
	}
	defer rows.Close()
	return scanProgressMarks(rows)
}

// DeleteTrack removes every mark of one assessor within the scope — a real
// DELETE, the only one this table has. Other assessors' rows, and the
// project-level scope when a task scope is deleted, are untouched.
func (r *progressRepo) DeleteTrack(tx Tx, projectID string, taskID *string, assessor string) (int64, error) {
	if projectID == "" {
		return 0, domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	if assessor == "" {
		return 0, domain.Invalid("assessor", "assessor is empty", "Pass the assessor whose track to remove.")
	}
	scope, args := progressScope(projectID, taskID)
	tw := tx.(*txWrap)
	res, err := tw.tx.ExecContext(tw.ctx(),
		"DELETE FROM progress_marks WHERE "+scope+" AND assessor = ?",
		append(args, assessor)...,
	)
	if err != nil {
		return 0, fmt.Errorf("store: delete progress track: %w", err)
	}
	return res.RowsAffected()
}

func scanProgressMarks(rows *sql.Rows) ([]domain.ProgressMark, error) {
	var out []domain.ProgressMark
	for rows.Next() {
		var (
			m      domain.ProgressMark
			taskID sql.NullString
			eta    sql.NullString
			ts     string
		)
		if err := rows.Scan(&m.ID, &m.ProjectID, &taskID, &m.Assessor, &m.Percent, &eta, &ts); err != nil {
			return nil, fmt.Errorf("store: scan progress mark: %w", err)
		}
		if taskID.Valid {
			s := taskID.String
			m.TaskID = &s
		}
		etaPtr, err := parseTimePtr(eta)
		if err != nil {
			return nil, err
		}
		m.ETA = etaPtr
		t, err := parseTime(ts)
		if err != nil {
			return nil, err
		}
		m.CreatedAt = t
		out = append(out, m)
	}
	return out, rows.Err()
}
