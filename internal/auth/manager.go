// Package auth owns everything between the request and the service layer:
// tokens (hash, verify, mint), browser sessions, CSRF, rate limits, trusted
// proxy resolution, SSE one-time tickets and log redaction.
//
// The package is deliberately decoupled from the database: it depends on
// TokenRepo and SessionRepo *interfaces* so the tests can run without SQLite
// and so the store agent can change implementation without rewriting auth.
//
// The security invariants enforced here are the product (PLAN §8). They are
// written as code so the next person to "simplify" them has to read the rules
// they would break.
package auth

import (
	"context"
	"crypto/subtle"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// TokenPrefix marks every wire secret so an operator can recognise a leaked
// value in logs and rotate it without guessing what system it belongs to.
const TokenPrefix = "kbn_"

// SecretBytes is the entropy floor for a token secret. 256 random bits is
// well above brute-force range; combined with the SHA-256 hash in storage,
// unsalted is acceptable because there are no rainbow tables to mount against
// 2^256 space — see SECURITY.md invariant.
const SecretBytes = 32

// HashBytes is the size of the SHA-256 digest; the column width is fixed so
// the token table does not need a variable-length column.
const HashBytes = sha256.Size

// TokenLookup is the auth-facing view of the token store. The store agent
// implements it; auth never sees *sql.Tx.
type TokenLookup interface {
	GetByHash(ctx context.Context, hash []byte) (*domain.Token, error)
	GetByName(ctx context.Context, name string) (*domain.Token, error)
	List(ctx context.Context) ([]*domain.Token, error)
	Count(ctx context.Context) (int, error)
	Create(ctx context.Context, t *domain.Token) error
	// UpdateHash replaces the stored secret hash of an existing token; see
	// Manager.Rotate.
	UpdateHash(ctx context.Context, id string, hash []byte) error
	Revoke(ctx context.Context, name string) error
}

// SessionLookup is the auth-facing view of the session store. The methods
// are deliberately named CreateSession / GetSession etc. so an adapter that
// satisfies both TokenLookup and SessionLookup can disambiguate the two
// Create methods (which have different value types).
type SessionLookup interface {
	CreateSession(ctx context.Context, s *domain.Session) error
	GetSession(ctx context.Context, id string) (*domain.Session, error)
	TouchSession(ctx context.Context, id string, now time.Time) error
	DeleteSession(ctx context.Context, id string) error
}

// ClockFunc returns the current time. Tests inject a fixed clock.
type ClockFunc func() time.Time

// Manager is the top-level handle. It is safe for concurrent use.
type Manager struct {
	Tokens   TokenLookup
	Sessions SessionLookup
	Tickets  *TicketStore
	Limits   *RateLimiter
	Trusted  []*net.IPNet
	BaseURL  string
	Insecure bool // true -> cookie Secure off (loopback)
	Now      ClockFunc
}

// NewManager wires the dependencies. baseURL is the public origin (used for
// cookie scoping); insecure mirrors config.InsecureHTTP; trusted is the
// pre-parsed CIDR list; now is the clock (tests inject).
func NewManager(toks TokenLookup, sess SessionLookup, trusted []*net.IPNet, baseURL string, insecure bool, now ClockFunc) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{
		Tokens:   toks,
		Sessions: sess,
		Tickets:  NewTicketStore(domain.TicketTTL, now),
		Limits:   NewRateLimiter(now),
		Trusted:  trusted,
		BaseURL:  baseURL,
		Insecure: insecure,
		Now:      now,
	}
}

// ---------------------------------------------------------------------------
// Token rendering, hashing, comparison.
// ---------------------------------------------------------------------------

// NewSecret returns a freshly-generated, base32-encoded 32-byte secret with
// the kbn_ prefix. The randomness comes from crypto/rand so a second process
// cannot reproduce a value it didn't observe.
func NewSecret() (string, error) {
	var raw [SecretBytes]byte
	if err := readRandom(raw[:]); err != nil {
		return "", err
	}
	// Lower-case base32 (no padding) keeps the wire form unambiguous inside
	// shell quotes and JSON. Hex would be 50% longer for the same entropy.
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	return TokenPrefix + strings.ToLower(enc), nil
}

// HashToken returns the storage digest. SHA-256, unsalted, is acceptable here
// because the input is 256 random bits: an attacker who steals the database
// cannot precompute a rainbow table of 2^256 candidates. Adding a salt would
// only buy an extra configuration knob that someone will eventually mis-set.
func HashToken(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	out := make([]byte, HashBytes)
	copy(out, sum[:])
	return out
}

// HashHex is the canonical hex form for log lines and exported diagnostics.
// The plain bytes are the storage form; hex avoids base64 padding confusion
// when a human reads the output.
func HashHex(hash []byte) string { return hex.EncodeToString(hash) }

// VerifyToken resolves a candidate secret to a Token, or returns nil if the
// secret is unknown. The hash lookup is constant-time at the call site by
// virtue of the store's exact-match on hash equality; the constant-time
// compare below is the last line of defence against a future store that
// returns a fuzzy match (there isn't one today, but the rule outlives the
// implementation).
func (m *Manager) VerifyToken(ctx context.Context, secret string) (*domain.Token, error) {
	if secret == "" {
		return nil, nil
	}
	if !strings.HasPrefix(secret, TokenPrefix) {
		return nil, nil // unknown shape -> unknown token, no leak
	}
	hash := HashToken(secret)
	tok, err := m.Tokens.GetByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, nil
	}
	// Defence-in-depth: even though GetByHash returns the row whose stored
	// hash equals the candidate, re-run a constant-time compare so a future
	// store change (e.g. fuzzy matching) cannot regress this property.
	if subtle.ConstantTimeCompare(tok.Hash, hash) != 1 {
		return nil, nil
	}
	if !tok.Active() {
		return nil, nil
	}
	return tok, nil
}

// ResolveActor turns a verified Token into a service.Actor, applying scope
// implications and per-token project restriction.
func ResolveActor(t *domain.Token) *actor {
	if t == nil {
		return nil
	}
	return &actor{
		TokenID:     t.ID,
		Name:        t.Name,
		Scopes:      t.Scopes,
		ProjectKeys: t.ProjectKeys,
	}
}

// actor is the in-package mirror of service.Actor; we don't import service
// to keep this layer independent. The web/mcp layers will convert.
type actor struct {
	TokenID     string
	Name        string
	Scopes      domain.Scopes
	ProjectKeys []string
}

// ScopeAllows reports whether the actor carries the named scope. It mirrors
// domain.Scopes.Has but reads cleanly at call sites that already have an
// actor.
func (a *actor) ScopeAllows(want domain.Scope) bool {
	if a == nil {
		return false
	}
	return a.Scopes.Has(want)
}

// MayAccessProject mirrors domain.Token.MayAccessProject on the actor.
func (a *actor) MayAccessProject(key string) bool {
	if a == nil {
		return false
	}
	if len(a.ProjectKeys) == 0 {
		return true
	}
	for _, k := range a.ProjectKeys {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Bootstrap
// ---------------------------------------------------------------------------

// BootstrapError is returned by Bootstrap when no admin token was supplied
// AND one cannot be created externally. BootstrapToken is the secret value
// the operator must record — it is returned only once.
type BootstrapError struct {
	Reason string
}

func (e *BootstrapError) Error() string { return "auth: bootstrap: " + e.Reason }

// Bootstrap ensures the token table has at least one admin-scope token. When
// the table is empty it:
//   1. Uses supplied if non-empty.
//   2. Generates a new secret and stores the hash, returning the secret.
// The caller is responsible for printing the returned secret exactly once and
// for warning the operator to rotate it (PLAN §8: "this token is now in your
// logs — rotate it").
func (m *Manager) Bootstrap(ctx context.Context, supplied string) (token string, err error) {
	if supplied != "" {
		if err := m.mintAndStore(ctx, "admin", domain.Scopes{domain.ScopeAdmin}, supplied); err != nil {
			return "", err
		}
		return supplied, nil
	}
	count, err := m.Tokens.Count(ctx)
	if err != nil {
		return "", err
	}
	if count > 0 {
		return "", nil // caller did not need to bootstrap
	}
	secret, err := NewSecret()
	if err != nil {
		return "", err
	}
	if err := m.mintAndStore(ctx, "admin", domain.Scopes{domain.ScopeAdmin}, secret); err != nil {
		return "", err
	}
	return secret, nil
}

// mintAndStore creates a single token row. The hash, scopes and timestamp
// come from this layer so callers never see the storage shape.
func (m *Manager) mintAndStore(ctx context.Context, name string, scopes domain.Scopes, secret string) error {
	if !strings.HasPrefix(secret, TokenPrefix) {
		// A supplied secret that does not match our shape is rejected so
		// the operator cannot paste a 32-char hex that we would later be
		// unable to recognise as a kanban token.
		return &BootstrapError{Reason: fmt.Sprintf("supplied token for %q does not start with %q", name, TokenPrefix)}
	}
	t := &domain.Token{
		Name:      name,
		Hash:      HashToken(secret),
		Scopes:    scopes,
		CreatedAt: m.Now(),
	}
	return m.Tokens.Create(ctx, t)
}

// MintAndStore is the public-facing helper the CLI uses. The caller fills in
// ID, Name, Scopes, ProjectKeys and CreatedAt; this layer generates the
// secret, computes the hash and persists the row. The secret is returned
// exactly once — that is the whole point of "shown only this once".
func (m *Manager) MintAndStore(ctx context.Context, t *domain.Token) (string, error) {
	if t == nil {
		return "", &BootstrapError{Reason: "token is nil"}
	}
	if t.Name == "" {
		return "", &BootstrapError{Reason: "token name is empty"}
	}
	secret, err := NewSecret()
	if err != nil {
		return "", err
	}
	t.Hash = HashToken(secret)
	if t.CreatedAt.IsZero() {
		t.CreatedAt = m.Now().UTC()
	}
	if err := m.Tokens.Create(ctx, t); err != nil {
		return "", err
	}
	return secret, nil
}

// Rotate issues a fresh secret for an existing token and updates the stored
// hash. The previous secret is invalidated immediately. We do not preserve
// any per-caller lease state because leases live on tasks, not tokens.
func (m *Manager) Rotate(ctx context.Context, name string) (string, error) {
	tok, err := m.Tokens.GetByName(ctx, name)
	if err != nil {
		return "", err
	}
	if tok == nil {
		return "", &BootstrapError{Reason: fmt.Sprintf("token %q not found", name)}
	}
	if !tok.Active() {
		return "", &BootstrapError{Reason: fmt.Sprintf("token %q is revoked", name)}
	}
	secret, err := NewSecret()
	if err != nil {
		return "", err
	}
	tok.Hash = HashToken(secret)
	// Rotation replaces the hash on the existing row. It cannot go through
	// Create: name and hash both carry UNIQUE indexes, so re-creating the token
	// is a constraint violation, not an upsert.
	if err := m.Tokens.UpdateHash(ctx, tok.ID, tok.Hash); err != nil {
		return "", err
	}
	return secret, nil
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ErrNoToken signals that the request had no recognised credential. It is
// distinct from ErrForbidden so middleware can choose to challenge (401) vs
// deny (403).
var ErrNoToken = errors.New("auth: no token")

// ErrForbidden signals that a credential was present but lacked the needed
// scope or project access.
var ErrForbidden = errors.New("auth: forbidden")

// ErrRateLimited signals that the rate limiter rejected the request. The
// caller should respond 429 with a Retry-After header.
var ErrRateLimited = errors.New("auth: rate limited")

// IsLoopbackHost is re-exported so middleware can use the same definition
// the config package validated. We wrap net.IP.IsLoopback with the textual
// shortcuts.
func IsLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	switch strings.ToLower(host) {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
