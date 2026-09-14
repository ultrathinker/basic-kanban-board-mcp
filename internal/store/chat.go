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
	res, err := tw.tx.ExecContext(tw.ctx(), `
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
	// Seq is the rowid assigned by this very INSERT — the feed's monotonic
	// ordering key (see ChatMessage.Seq). Read back here so the in-memory
	// message and the row agree from birth; every later read re-derives it
	// from the row.
	seq, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: insert chat message: last insert id: %w", err)
	}
	m.Seq = seq
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

// GetBySeq resolves the message sitting at one feed position. Chat rows are
// never pruned, so a missing position means the cursor points outside this
// history (another database, or rows deleted underneath it); the caller
// pairs this with the id check to refuse such cursors loudly.
func (r *chatRepo) GetBySeq(tx Tx, seq int64) (*domain.ChatMessage, error) {
	if seq <= 0 {
		return nil, domain.Invalid("seq", "feed position must be a positive number",
			"Pass back a next_cursor the server issued, verbatim.")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(), chatSelectSQL+` WHERE rowid = ?`, seq)
	m, err := scanChatMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("message", fmt.Sprintf("position %d", seq))
	}
	return m, err
}

// GetBySenderKey resolves the message a (sender, idempotency_key) pair
// already produced, if any. Chat is never pruned, so the pair is stable for
// the message's whole lifetime — this is the durable retry guard, not the
// 24h idempotency table task_create uses.
func (r *chatRepo) GetBySenderKey(tx Tx, tokenID, key string) (*domain.ChatMessage, error) {
	if tokenID == "" || key == "" {
		return nil, domain.Invalid("idempotency", "author token id and key are required",
			"Both fields are part of the message's deduplication identity.")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(),
		chatSelectSQL+` WHERE author_token_id = ? AND idempotency_key = ?`, tokenID, key)
	m, err := scanChatMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("message", key)
	}
	return m, err
}

// chatSelectSQL is the shared projection for every chat read: the five
// original columns plus the communication-protocol columns (KANB-45..47),
// and the rowid as seq — the feed's ordering key (ChatMessage.Seq).
const chatSelectSQL = `SELECT id, project_id, author, body, created_at,
	kind, author_token_id, recipient, resolved_executor, reply_to_id, idempotency_key,
	rowid AS seq
	FROM chat_messages`

// List returns a page of chat messages. The direction comes from
// f.Ascending: false (default) reads newest first with f.Before paging
// strictly backwards in time (the panel's shape); true reads INSERTION
// order — the feed's shape, which starts at the beginning of history even
// before any cursor exists, so a consumer never silently misses a command
// written before it started. The feed pages by rowid, a monotonic insertion
// key: created_at has millisecond precision and arrivals share one
// millisecond, so timestamp+id ordering could place a newer message before
// an issued cursor and lose it to every later page. Backward pagination is
// not exposed to that: newcomers land at the TOP of a newest-first read,
// away from where the reader is walking, so it keeps its timestamp+id
// ordering.
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
		where = append(where, "rowid > ?")
		args = append(args, cur.Seq)
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

	q := chatSelectSQL
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if f.Ascending {
		q += " ORDER BY rowid ASC LIMIT ?"
	} else {
		q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	}
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
		&kind, &authorTk, &recip, &executor, &replyTo, &idemKey, &m.Seq); err != nil {
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
