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
//
// An empty Kind is normalized to update so every row carries a valid kind —
// legacy writers that predate the communication protocol keep working and
// the column's own default stays consistent with what a reader sees.
func (r *chatRepo) Add(tx Tx, m *domain.ChatMessage) error {
	if m == nil {
		return errors.New("store: chat.Add: nil message")
	}
	if m.ID == "" || m.ProjectID == "" || m.Author == "" {
		return domain.Invalid("chat_message", "id, project_id and author are required",
			"Set those fields before calling chat Add.")
	}
	if m.Kind == "" {
		m.Kind = domain.MessageUpdate
	}
	if !m.Kind.Valid() {
		return domain.Invalid("kind", fmt.Sprintf("message kind %q is invalid", m.Kind),
			"Use one of: update, scope_change, question, command.")
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
		INSERT INTO chat_messages(
			id, project_id, author, body, created_at,
			kind, author_token_id, recipient, resolved_executor, reply_to_id, idempotency_key)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.ProjectID, m.Author, m.Body, formatTime(m.CreatedAt),
		string(m.Kind), nullableString(m.AuthorTokenID), nullableString(m.Recipient),
		nullableString(m.ResolvedExecutor), nullableString(m.ReplyToID), nullableString(m.IdempotencyKey),
	)
	if err != nil {
		if IsForeignKeyViolation(err) {
			return domain.NotFound("project", m.ProjectID)
		}
		if IsUniqueViolation(err) {
			return wrapf(domain.Conflict(nil, 0, 0), "idempotency key %q was already used by this sender", m.IdempotencyKey)
		}
		return fmt.Errorf("store: insert chat message: %w", err)
	}
	return nil
}

// Get resolves one message by id. The caller holds the identifier (it is
// replying to it, or paginating from it), so a missing row is a NotFound that
// names it.
func (r *chatRepo) Get(tx Tx, id string) (*domain.ChatMessage, error) {
	if id == "" {
		return nil, domain.Invalid("id", "message id is empty", "Pass the message id.")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(), chatSelectSQL+` WHERE id = ?`, id)
	m, err := scanChatMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("message", id)
	}
	return m, err
}

// chatSelectSQL is the shared projection for every chat read: the five
// original columns plus the communication-protocol columns (KANB-45..47).
const chatSelectSQL = `SELECT id, project_id, author, body, created_at,
	kind, author_token_id, recipient, resolved_executor, reply_to_id, idempotency_key
	FROM chat_messages`

// List returns a page of chat messages. The direction comes from
// f.Ascending: false (default) reads newest first with f.Before paging
// strictly backwards in time (the panel's shape); true reads oldest first
// with f.After paging strictly forward — the feed's shape, which starts at
// the beginning of history even before any cursor exists, so a consumer
// never silently misses a command written before it started. Ties on
// created_at are broken by id in the read's own direction, so pages never
// skip or duplicate rows at boundaries.
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

	if cur := f.After; cur != nil {
		if !cur.CreatedAt.IsZero() && cur.ID != "" {
			where = append(where, "(created_at > ? OR (created_at = ? AND id > ?))")
			formatted := formatTime(cur.CreatedAt)
			args = append(args, formatted, formatted, cur.ID)
		} else if !cur.CreatedAt.IsZero() {
			where = append(where, "created_at > ?")
			args = append(args, formatTime(cur.CreatedAt))
		} else if cur.ID != "" {
			where = append(where, "id > ?")
			args = append(args, cur.ID)
		}
	}
	if cur := f.Before; cur != nil {
		if !cur.CreatedAt.IsZero() && cur.ID != "" {
			where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
			formatted := formatTime(cur.CreatedAt)
			args = append(args, formatted, formatted, cur.ID)
		} else if !cur.CreatedAt.IsZero() {
			where = append(where, "created_at < ?")
			args = append(args, formatTime(cur.CreatedAt))
		} else if cur.ID != "" {
			where = append(where, "id < ?")
			args = append(args, cur.ID)
		}
	}

	order := " DESC"
	if f.Ascending {
		order = " ASC"
	}

	q := chatSelectSQL
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at" + order + ", id" + order + " LIMIT ?"
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
		m, err := scanChatMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// scanner covers both *sql.Row and *sql.Rows for the shared projection.
type scanner interface{ Scan(dest ...any) error }

func scanChatMessage(row scanner) (*domain.ChatMessage, error) {
	var (
		m        domain.ChatMessage
		kind     string
		ts       string
		authorTk sql.NullString
		recip    sql.NullString
		executor sql.NullString
		replyTo  sql.NullString
		idemKey  sql.NullString
	)
	if err := row.Scan(&m.ID, &m.ProjectID, &m.Author, &m.Body, &ts,
		&kind, &authorTk, &recip, &executor, &replyTo, &idemKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: scan chat message: %w", err)
	}
	t, err := parseTime(ts)
	if err != nil {
		return nil, err
	}
	m.CreatedAt = t
	m.Kind = domain.MessageKind(kind)
	m.AuthorTokenID = authorTk.String
	m.Recipient = recip.String
	m.ResolvedExecutor = executor.String
	m.ReplyToID = replyTo.String
	m.IdempotencyKey = idemKey.String
	return &m, nil
}
