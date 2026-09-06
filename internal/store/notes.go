package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

type noteRepo struct{ s *sqlStore }

func (r *noteRepo) Add(tx Tx, n *domain.Note) error {
	if n == nil {
		return errors.New("store: note.Add: nil note")
	}
	if n.ID == "" || n.TaskID == "" || n.Author == "" {
		return domain.Invalid("note", "id, task_id and author are required",
			"Set those fields before calling note Add.")
	}
	if n.CreatedAt.IsZero() {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		n.CreatedAt = now
	}
	tw := tx.(*txWrap)
	_, err := tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO notes(id, task_id, author, body, created_at)
		VALUES (?,?,?,?,?)`,
		n.ID, n.TaskID, n.Author, n.Body, formatTime(n.CreatedAt),
	)
	if err != nil {
		if IsForeignKeyViolation(err) {
			return domain.NotFound("task", n.TaskID)
		}
		return fmt.Errorf("store: insert note: %w", err)
	}
	return nil
}

// ListByTask returns notes for a task, newest first, with optional before
// cursor for pagination. A zero/negative limit returns the cap.
func (r *noteRepo) ListByTask(tx Tx, taskID string, limit int, before *time.Time) ([]domain.Note, error) {
	if taskID == "" {
		return nil, domain.Invalid("task_id", "task id is empty", "Pass the task UUID.")
	}
	if limit <= 0 {
		limit = 50
	}
	tw := tx.(*txWrap)
	q := `SELECT id, task_id, author, body, created_at
	      FROM notes WHERE task_id = ?`
	args := []any{taskID}
	if before != nil {
		q += " AND created_at < ?"
		args = append(args, formatTime(*before))
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := tw.tx.QueryContext(tw.ctx(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list notes: %w", err)
	}
	defer rows.Close()
	var out []domain.Note
	for rows.Next() {
		var (
			n  domain.Note
			ts string
		)
		if err := rows.Scan(&n.ID, &n.TaskID, &n.Author, &n.Body, &ts); err != nil {
			return nil, fmt.Errorf("store: scan note: %w", err)
		}
		t, err := parseTime(ts)
		if err != nil {
			return nil, err
		}
		n.CreatedAt = t
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountByTasks returns the note count per task. Tasks with zero notes are
// omitted from the map; callers must treat absence as 0.
func (r *noteRepo) CountByTasks(tx Tx, taskIDs []string) (map[string]int, error) {
	if len(taskIDs) == 0 {
		return map[string]int{}, nil
	}
	tw := tx.(*txWrap)
	placeholders, args := makeInClause(taskIDs)
	q := `SELECT task_id, COUNT(*) FROM notes
	      WHERE task_id IN (` + placeholders + `)
	      GROUP BY task_id`
	rows, err := tw.tx.QueryContext(tw.ctx(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: count notes: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int, len(taskIDs))
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: scan note count: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}
