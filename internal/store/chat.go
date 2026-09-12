package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

type chatRepo struct{ s *sqlStore }

// Add appends one message to the project chat. Rows are never pruned.
// The body is stored verbatim without formatting or escaping; escaping
// is the responsibility of the template / presentation layer.
func (r *chatRepo) Add(tx Tx, m *domain.ChatMessage) error {
	if m == nil {
		return errors.New("store: chat.Add: nil message")
	}
	if m.ID == "" || m.ProjectID == "" || m.Author == "" {
		return domain.Invalid("chat_message", "id, project_id and author are required",
			"Set those fields before calling chat Add.")
	}
	if err := domain.ValidateChatMessageBody(m.Body); err != nil {
		return err
	}
	if m.CreatedAt.IsZero() {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		m.CreatedAt = now
	}
	tw := tx.(*txWrap)
	_, err := tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO chat_messages(id, project_id, author, body, created_at)
		VALUES (?,?,?,?,?)`,
		m.ID, m.ProjectID, m.Author, m.Body, formatTime(m.CreatedAt),
	)
	if err != nil {
		if IsForeignKeyViolation(err) {
			return domain.NotFound("project", m.ProjectID)
		}
		return fmt.Errorf("store: insert chat message: %w", err)
	}
	return nil
}

// List returns a page of chat messages, newest first. If f.ProjectID is nil or
// empty, messages across all projects are returned. When f.Before is given, it
// pages strictly backwards in time; ties on created_at are broken by id DESC
// so that pages do not skip or duplicate rows at page boundaries.
func (r *chatRepo) List(tx Tx, f ChatFilter) ([]domain.ChatMessage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	tw := tx.(*txWrap)

	var where []string
	var args []any

	if f.ProjectID != nil && *f.ProjectID != "" {
		where = append(where, "project_id = ?")
		args = append(args, *f.ProjectID)
	} else if len(f.ProjectIDs) > 0 {
		placeholders, pArgs := makeInClause(f.ProjectIDs)
		where = append(where, "project_id IN ("+placeholders+")")
		args = append(args, pArgs...)
	}

	if f.Before != nil {
		if !f.Before.CreatedAt.IsZero() && f.Before.ID != "" {
			where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
			formatted := formatTime(f.Before.CreatedAt)
			args = append(args, formatted, formatted, f.Before.ID)
		} else if !f.Before.CreatedAt.IsZero() {
			where = append(where, "created_at < ?")
			args = append(args, formatTime(f.Before.CreatedAt))
		} else if f.Before.ID != "" {
			where = append(where, "id < ?")
			args = append(args, f.Before.ID)
		}
	}

	q := `SELECT id, project_id, author, body, created_at
	      FROM chat_messages`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := tw.tx.QueryContext(tw.ctx(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list chat messages: %w", err)
	}
	defer rows.Close()

	return scanChatMessages(rows)
}

func scanChatMessages(rows *sql.Rows) ([]domain.ChatMessage, error) {
	out := make([]domain.ChatMessage, 0)
	for rows.Next() {
		var (
			m  domain.ChatMessage
			ts string
		)
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Author, &m.Body, &ts); err != nil {
			return nil, fmt.Errorf("store: scan chat message: %w", err)
		}
		t, err := parseTime(ts)
		if err != nil {
			return nil, err
		}
		m.CreatedAt = t
		out = append(out, m)
	}
	return out, rows.Err()
}
