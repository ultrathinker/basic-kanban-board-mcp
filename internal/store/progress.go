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

// LatestByTask returns the most recent mark of every assessor for every task
// track of the project in ONE read — the board renders dozens of cards and
// must not issue a query per card. Keyed by task id; a task with no marks is
// absent from the map; project-level marks (task_id IS NULL) are not
// included — they belong to LatestByAssessor's project scope.
func (r *progressRepo) LatestByTask(tx Tx, projectID string) (map[string][]domain.ProgressMark, error) {
	if projectID == "" {
		return nil, domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	tw := tx.(*txWrap)
	q := `SELECT id, project_id, task_id, assessor, percent, eta, created_at
	      FROM (
	          SELECT id, project_id, task_id, assessor, percent, eta, created_at,
	                 ROW_NUMBER() OVER (
	                     PARTITION BY task_id, assessor
	                     ORDER BY created_at DESC, id DESC
	                 ) AS rn
	          FROM progress_marks
	          WHERE project_id = ? AND task_id IS NOT NULL
	      )
	      WHERE rn = 1
	      ORDER BY task_id ASC, assessor ASC`
	rows, err := tw.tx.QueryContext(tw.ctx(), q, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: latest progress marks by task: %w", err)
	}
	defer rows.Close()
	marks, err := scanProgressMarks(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]domain.ProgressMark, len(marks))
	for _, m := range marks {
		out[*m.TaskID] = append(out[*m.TaskID], m)
	}
	return out, nil
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

// HistoryTail returns the NEWEST limit marks of the scope, still handed back
// oldest-first (the one chronological order this table is ever read in), plus
// the total number of marks in the scope.
//
// It exists so a bounded read stays bounded in SQL. Reading the whole history
// and slicing the tail in Go produces the same answer and does none of the
// work the limit was asked for: on a scope with half a million marks — which
// this append-only table is built to reach, since nothing ever thins it — a
// request for the last 200 would materialise all 500,000 rows first.
//
// A limit of zero or less is a programming error rather than "no limit": the
// caller that wants everything calls History, which says so in its name.
func (r *progressRepo) HistoryTail(tx Tx, projectID string, taskID *string, limit int) ([]domain.ProgressMark, int, error) {
	if projectID == "" {
		return nil, 0, domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	if limit <= 0 {
		return nil, 0, domain.Invalid("limit", "limit must be positive", "Call History when the whole scope is wanted.")
	}
	scope, args := progressScope(projectID, taskID)
	tw := tx.(*txWrap)

	var total int
	if err := tw.tx.QueryRowContext(tw.ctx(),
		"SELECT COUNT(*) FROM progress_marks WHERE "+scope, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count progress history: %w", err)
	}

	rows, err := tw.tx.QueryContext(tw.ctx(), `
		SELECT id, project_id, task_id, assessor, percent, eta, created_at FROM (
			SELECT id, project_id, task_id, assessor, percent, eta, created_at
			FROM progress_marks
			WHERE `+scope+`
			ORDER BY created_at DESC, id DESC
			LIMIT ?
		) ORDER BY created_at ASC, id ASC`, append(args, limit)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: progress history tail: %w", err)
	}
	defer rows.Close()
	marks, err := scanProgressMarks(rows)
	if err != nil {
		return nil, 0, err
	}
	return marks, total, nil
}

// CountsByAssessor returns how many marks each assessor has logged in the
// PROJECT-level scope (task_id IS NULL) — the batched counterpart of
// LatestByAssessor, and the project-scope twin of CountsByTask.
//
// The board used to get this by reading the whole project history and
// tallying it in Go. That was defended as cheap because the project scope is
// read once per page render, but neither half of that holds: the table is
// append-only and nothing thins it, and a render is not rare — every SSE
// signal makes the page re-read itself, so one agent writing thirty updates
// cost thirty full-history scans of a table that only grows.
func (r *progressRepo) CountsByAssessor(tx Tx, projectID string) (map[string]int, error) {
	if projectID == "" {
		return nil, domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	tw := tx.(*txWrap)
	rows, err := tw.tx.QueryContext(tw.ctx(), `
		SELECT assessor, COUNT(*)
		FROM progress_marks
		WHERE project_id = ? AND task_id IS NULL
		GROUP BY assessor`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: counts progress marks by assessor: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var assessor string
		var n int
		if err := rows.Scan(&assessor, &n); err != nil {
			return nil, fmt.Errorf("store: scan assessor count: %w", err)
		}
		out[assessor] = n
	}
	return out, rows.Err()
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

// CountsByTask returns, for every task track of the project, how many marks
// each assessor has logged — keyed by task id, then assessor. It is the
// batched counterpart of LatestByTask: the delete-track control needs "how
// many history points will be lost" for every card the board renders, and a
// count-per-card would repeat LatestByTask's own per-card-query mistake.
// Project-level marks (task_id IS NULL) are not included, matching
// LatestByTask's own scope.
func (r *progressRepo) CountsByTask(tx Tx, projectID string) (map[string]map[string]int, error) {
	if projectID == "" {
		return nil, domain.Invalid("project_id", "project id is empty", "Pass the project UUID.")
	}
	tw := tx.(*txWrap)
	rows, err := tw.tx.QueryContext(tw.ctx(), `
		SELECT task_id, assessor, COUNT(*)
		FROM progress_marks
		WHERE project_id = ? AND task_id IS NOT NULL
		GROUP BY task_id, assessor`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: counts progress marks by task: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var taskID, assessor string
		var n int
		if err := rows.Scan(&taskID, &assessor, &n); err != nil {
			return nil, fmt.Errorf("store: scan task assessor count: %w", err)
		}
		if out[taskID] == nil {
			out[taskID] = map[string]int{}
		}
		out[taskID][assessor] = n
	}
	return out, rows.Err()
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
