package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// tokens
// ---------------------------------------------------------------------------

type tokenRepo struct{ s *sqlStore }

func (r *tokenRepo) Create(tx Tx, t *domain.Token) error {
	if t == nil {
		return errors.New("store: token.Create: nil token")
	}
	if t.ID == "" || t.Name == "" || len(t.Hash) == 0 {
		return domain.Invalid("token", "id, name and hash are required",
			"Set those before calling token Create.")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = tx.Now().UTC()
	}
	scopes, err := encodeJSON(t.Scopes)
	if err != nil {
		return err
	}
	projectKeys, err := encodeJSON(t.ProjectKeys)
	if err != nil {
		return err
	}
	tw := tx.(*txWrap)
	_, err = tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO tokens(id, name, hash, scopes, project_keys, created_at, last_used_at, revoked_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.Hash, scopes, projectKeys,
		formatTime(t.CreatedAt), nullableTime(t.LastUsedAt), nullableTime(t.RevokedAt),
	)
	if err != nil {
		if IsUniqueViolation(err) {
			return wrapf(domain.Conflict(nil, 0, 0), "token name %q already exists", t.Name)
		}
		return fmt.Errorf("store: insert token: %w", err)
	}
	return nil
}

func (r *tokenRepo) GetByName(tx Tx, name string) (*domain.Token, error) {
	if name == "" {
		return nil, domain.Invalid("name", "token name is empty", "Pass the token name.")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(), `
		SELECT id, name, hash, scopes, project_keys, created_at, last_used_at, revoked_at
		FROM tokens WHERE name = ? COLLATE NOCASE`, name)
	return scanToken(row)
}

// GetByHash is the auth hot path; the caller does the constant-time
// compare before calling this lookup. The unique index on `hash` keeps it
// to a single row fetch.
func (r *tokenRepo) GetByHash(tx Tx, hash []byte) (*domain.Token, error) {
	if len(hash) == 0 {
		return nil, domain.Invalid("hash", "token hash is empty", "Hash the secret with SHA-256 first.")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(), `
		SELECT id, name, hash, scopes, project_keys, created_at, last_used_at, revoked_at
		FROM tokens WHERE hash = ?`, hash)
	return scanToken(row)
}

func (r *tokenRepo) List(tx Tx) ([]*domain.Token, error) {
	tw := tx.(*txWrap)
	rows, err := tw.tx.QueryContext(tw.ctx(), `
		SELECT id, name, hash, scopes, project_keys, created_at, last_used_at, revoked_at
		FROM tokens ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list tokens: %w", err)
	}
	defer rows.Close()
	var out []*domain.Token
	for rows.Next() {
		tk, err := scanTokenRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tk)
	}
	return out, rows.Err()
}

func (r *tokenRepo) Revoke(tx Tx, name string) error {
	if name == "" {
		return domain.Invalid("name", "token name is empty", "Pass the token name.")
	}
	tw := tx.(*txWrap)
	now := formatTime(tx.Now().UTC())
	res, err := tw.tx.ExecContext(tw.ctx(), "UPDATE tokens SET revoked_at = ? WHERE name = ? COLLATE NOCASE", now, name)
	if err != nil {
		return fmt.Errorf("store: revoke token: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.NotFound("token", name)
	}
	return nil
}

func (r *tokenRepo) TouchLastUsed(tx Tx, id string) error {
	if id == "" {
		return domain.Invalid("id", "token id is empty", "Pass the token UUID.")
	}
	tw := tx.(*txWrap)
	now := formatTime(tx.Now().UTC())
	_, err := tw.tx.ExecContext(tw.ctx(), "UPDATE tokens SET last_used_at = ? WHERE id = ?", now, id)
	if err != nil {
		return fmt.Errorf("store: touch token: %w", err)
	}
	return nil
}

func (r *tokenRepo) Count(tx Tx) (int, error) {
	tw := tx.(*txWrap)
	var n int
	if err := tw.tx.QueryRowContext(tw.ctx(), "SELECT COUNT(*) FROM tokens").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count tokens: %w", err)
	}
	return n, nil
}

// scanToken / scanTokenRows materialize a Token from a query.
func scanToken(row *sql.Row) (*domain.Token, error) {
	var (
		t         domain.Token
		scopes    string
		keys      string
		created   string
		lastUsed  sql.NullString
		revoked   sql.NullString
	)
	if err := row.Scan(&t.ID, &t.Name, &t.Hash, &scopes, &keys, &created, &lastUsed, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.NotFound("token", "?")
		}
		return nil, fmt.Errorf("store: scan token: %w", err)
	}
	if err := fillToken(&t, scopes, keys, created, lastUsed, revoked); err != nil {
		return nil, err
	}
	return &t, nil
}

func scanTokenRows(rows *sql.Rows) (*domain.Token, error) {
	var (
		t        domain.Token
		scopes   string
		keys     string
		created  string
		lastUsed sql.NullString
		revoked  sql.NullString
	)
	if err := rows.Scan(&t.ID, &t.Name, &t.Hash, &scopes, &keys, &created, &lastUsed, &revoked); err != nil {
		return nil, fmt.Errorf("store: scan token row: %w", err)
	}
	if err := fillToken(&t, scopes, keys, created, lastUsed, revoked); err != nil {
		return nil, err
	}
	return &t, nil
}

func fillToken(t *domain.Token, scopes, keys, created string, lastUsed, revoked sql.NullString) error {
	sc, err := decodeScopes(scopes)
	if err != nil {
		return err
	}
	t.Scopes = sc
	ks, err := decodeStrings(keys)
	if err != nil {
		return err
	}
	t.ProjectKeys = ks
	ct, err := parseTime(created)
	if err != nil {
		return err
	}
	t.CreatedAt = ct
	if lastUsed.Valid {
		lt, err := parseTime(lastUsed.String)
		if err != nil {
			return err
		}
		t.LastUsedAt = &lt
	}
	if revoked.Valid {
		rt, err := parseTime(revoked.String)
		if err != nil {
			return err
		}
		t.RevokedAt = &rt
	}
	return nil
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

type sessionRepo struct{ s *sqlStore }

func (r *sessionRepo) Create(tx Tx, s *domain.Session) error {
	if s == nil {
		return errors.New("store: session.Create: nil session")
	}
	if s.ID == "" || s.TokenID == "" {
		return domain.Invalid("session", "id and token_id are required",
			"Set those before calling session Create.")
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = tx.Now().UTC()
	}
	if s.LastSeenAt.IsZero() {
		s.LastSeenAt = s.CreatedAt
	}
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt = s.CreatedAt.Add(domain.SessionIdleTTL)
	}
	tw := tx.(*txWrap)
	_, err := tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO sessions(id, token_id, created_at, last_seen_at, expires_at)
		VALUES (?,?,?,?,?)`,
		s.ID, s.TokenID, formatTime(s.CreatedAt), formatTime(s.LastSeenAt), formatTime(s.ExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("store: insert session: %w", err)
	}
	return nil
}

func (r *sessionRepo) Get(tx Tx, id string) (*domain.Session, error) {
	if id == "" {
		return nil, domain.Invalid("id", "session id is empty", "Pass the session id.")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(), `
		SELECT id, token_id, created_at, last_seen_at, expires_at
		FROM sessions WHERE id = ?`, id)
	var s domain.Session
	var created, lastSeen, expires string
	if err := row.Scan(&s.ID, &s.TokenID, &created, &lastSeen, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.NotFound("session", id)
		}
		return nil, fmt.Errorf("store: scan session: %w", err)
	}
	ct, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	s.CreatedAt = ct
	lt, err := parseTime(lastSeen)
	if err != nil {
		return nil, err
	}
	s.LastSeenAt = lt
	et, err := parseTime(expires)
	if err != nil {
		return nil, err
	}
	s.ExpiresAt = et
	return &s, nil
}

// Touch updates the last-seen timestamp. The caller may pass an explicit
// `now` to align with the calling transaction's clock.
func (r *sessionRepo) Touch(tx Tx, id string, now time.Time) error {
	if id == "" {
		return domain.Invalid("id", "session id is empty", "Pass the session id.")
	}
	if now.IsZero() {
		now = tx.Now().UTC()
	}
	tw := tx.(*txWrap)
	res, err := tw.tx.ExecContext(tw.ctx(),
		"UPDATE sessions SET last_seen_at = ? WHERE id = ?", formatTime(now), id)
	if err != nil {
		return fmt.Errorf("store: touch session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.NotFound("session", id)
	}
	return nil
}

func (r *sessionRepo) Delete(tx Tx, id string) error {
	if id == "" {
		return domain.Invalid("id", "session id is empty", "Pass the session id.")
	}
	tw := tx.(*txWrap)
	if _, err := tw.tx.ExecContext(tw.ctx(), "DELETE FROM sessions WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

func (r *sessionRepo) DeleteExpired(tx Tx, now time.Time) (int, error) {
	if now.IsZero() {
		now = tx.Now().UTC()
	}
	tw := tx.(*txWrap)
	res, err := tw.tx.ExecContext(tw.ctx(), "DELETE FROM sessions WHERE expires_at < ?", formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// idempotency
// ---------------------------------------------------------------------------

type idempotencyRepo struct{ s *sqlStore }

func (r *idempotencyRepo) Get(tx Tx, tokenID, key string) (*domain.IdempotencyRecord, error) {
	if tokenID == "" || key == "" {
		return nil, domain.Invalid("idempotency", "token_id and key are required",
			"Both fields are part of the record key.")
	}
	tw := tx.(*txWrap)
	now := formatTime(tx.Now().UTC())
	row := tw.tx.QueryRowContext(tw.ctx(), `
		SELECT token_id, key, request_hash, response, expires_at
		FROM idempotency
		WHERE token_id = ? AND key = ? AND expires_at > ?`, tokenID, key, now)
	var rec domain.IdempotencyRecord
	var expires string
	if err := row.Scan(&rec.TokenID, &rec.Key, &rec.RequestHash, &rec.Response, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.NotFound("idempotency", key)
		}
		return nil, fmt.Errorf("store: scan idempotency: %w", err)
	}
	et, err := parseTime(expires)
	if err != nil {
		return nil, err
	}
	rec.ExpiresAt = et
	return &rec, nil
}

// Put writes a record. A duplicate (tokenID, key) is a hard error: the
// service layer is responsible for checking Get first and rejecting
// mismatched content. We never silently overwrite, because that would
// erase an audit trail.
func (r *idempotencyRepo) Put(tx Tx, rec *domain.IdempotencyRecord) error {
	if rec == nil || rec.TokenID == "" || rec.Key == "" {
		return domain.Invalid("idempotency", "token_id and key are required",
			"Both fields are part of the record key.")
	}
	if rec.ExpiresAt.IsZero() {
		rec.ExpiresAt = tx.Now().UTC().Add(domain.IdempotencyTTL)
	}
	tw := tx.(*txWrap)
	_, err := tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO idempotency(token_id, key, request_hash, response, expires_at)
		VALUES (?,?,?,?,?)`,
		rec.TokenID, rec.Key, rec.RequestHash, rec.Response, formatTime(rec.ExpiresAt),
	)
	if err != nil {
		if IsUniqueViolation(err) {
			return idempotencyMismatch("idempotency key %q already used", rec.Key)
		}
		return fmt.Errorf("store: insert idempotency: %w", err)
	}
	return nil
}

func (r *idempotencyRepo) DeleteExpired(tx Tx, now time.Time) (int, error) {
	if now.IsZero() {
		now = tx.Now().UTC()
	}
	tw := tx.(*txWrap)
	res, err := tw.tx.ExecContext(tw.ctx(), "DELETE FROM idempotency WHERE expires_at < ?", formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("store: delete expired idempotency: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
