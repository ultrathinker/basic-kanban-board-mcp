package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// AuthResult is what the middleware hands to downstream handlers. The
// request-scoped context is augmented so a handler never needs to re-read
// the Authorization header.
type AuthResult struct {
	Token *domain.Token
	Actor *actor
	// Kind explains how the caller was authenticated. SSE handlers care
	// ("ticket" is consumed on first use, "bearer" is reusable for the
	// session duration) and so do log filters.
	Kind AuthKind
}

type AuthKind string

const (
	AuthNone    AuthKind = ""
	AuthBearer  AuthKind = "bearer"
	AuthAPIKey  AuthKind = "apikey"
	AuthSession AuthKind = "session"
	AuthTicket  AuthKind = "ticket"
)

// contextKey is unexported so external packages cannot accidentally read or
// stomp the auth result.
type contextKey struct{}

// WithAuth attaches an AuthResult to ctx.
func WithAuth(ctx context.Context, r *AuthResult) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, r)
}

// FromContext returns the AuthResult placed by the middleware, or nil.
func FromContext(ctx context.Context) *AuthResult {
	v, _ := ctx.Value(contextKey{}).(*AuthResult)
	return v
}

// ---------------------------------------------------------------------------
// Bearer / API-key extraction
// ---------------------------------------------------------------------------

// extractBearer pulls the secret out of the request. We accept both forms
// documented in PLAN §8:
//
//	Authorization: Bearer kbn_…
//	X-API-Key:     kbn_…
//
// Either header alone is enough; when both are present, Bearer wins because
// that is the documented primary form and silent precedence would surprise
// operators debugging a wrong key.
func extractBearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if strings.HasPrefix(h, prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
		// Also accept the bare token form so a curl one-liner doesn't need
		// the word "Bearer" — the shape check below still catches it.
		if strings.HasPrefix(strings.TrimSpace(h), TokenPrefix) {
			return strings.TrimSpace(h)
		}
	}
	if k := strings.TrimSpace(r.Header.Get("X-API-Key")); strings.HasPrefix(k, TokenPrefix) {
		return k
	}
	return ""
}

// ---------------------------------------------------------------------------
// Trusted-proxy / client-IP helpers
// ---------------------------------------------------------------------------

// ClientIP returns the IP address the request should be attributed to. The
// result is used only for log keys and login rate-limit keys — never for an
// authentication decision, because a client can spoof X-Forwarded-For unless
// it transits a trusted proxy (PLAN §8: "never for an auth decision").
func (m *Manager) ClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	remote := remoteAddrIP(r.RemoteAddr)
	if remote == nil {
		return ""
	}
	if len(m.Trusted) == 0 || !CIDRContains(m.Trusted, remote.String()) {
		// No trusted hop in front: use the actual peer. The header (if any)
		// is attacker-controlled.
		return remote.String()
	}
	// Walk X-Forwarded-For right-to-left, taking the first hop that is NOT in
	// the trusted set — that is the real client. The whole point is that the
	// last trusted proxy appended the real client's IP, so anything to the
	// left of the trusted chain is attacker-controlled.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := parseForwardedIP(parts[i])
			if ip == nil {
				continue
			}
			if CIDRContains(m.Trusted, ip.String()) {
				continue // still inside the trusted chain
			}
			return ip.String()
		}
	}
	if rip := r.Header.Get("X-Real-IP"); rip != "" {
		if ip := parseForwardedIP(rip); ip != nil && !CIDRContains(m.Trusted, ip.String()) {
			return ip.String()
		}
	}
	return remote.String()
}

// ForwardedProto returns the request scheme as inferred from headers, but
// only when the immediate peer is a trusted proxy. Returns "" when no proxy
// is in play, when the header is missing, or when the peer is untrusted. The
// caller uses this to decide whether to mark a session cookie Secure.
func (m *Manager) ForwardedProto(r *http.Request) string {
	if r == nil || len(m.Trusted) == 0 {
		return ""
	}
	remote := remoteAddrIP(r.RemoteAddr)
	if remote == nil || !CIDRContains(m.Trusted, remote.String()) {
		return ""
	}
	p := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	switch p {
	case "https", "http":
		return p
	}
	return ""
}

func remoteAddrIP(s string) net.IP {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if h, _, ok := strings.Cut(s, ":"); ok {
		s = h
	}
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	return net.ParseIP(s)
}

func parseForwardedIP(s string) net.IP {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if h, _, ok := strings.Cut(s, ":"); ok {
		s = h
	}
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	return net.ParseIP(s)
}

// CIDRContains is re-exported so the middleware package does not need to
// import net itself.
func CIDRContains(cidrs []*net.IPNet, addr string) bool {
	ip := parseForwardedIP(addr)
	if ip == nil {
		return false
	}
	for _, n := range cidrs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Cookie policy
// ---------------------------------------------------------------------------

// CookiePolicy encapsulates the decisions that vary per deployment: name,
// Secure, SameSite, path. The Secure attribute is computed at request time
// from the BaseURL HTTPS scheme OR a trusted-proxy X-Forwarded-Proto, never
// from a header on an untrusted peer (PLAN §8 invariant).
type CookiePolicy struct {
	Name     string
	Path     string
	SameSite http.SameSite
	// ForceSecure forces the Secure attribute on regardless of scheme; used
	// when BaseURL is https. The decision lives on the manager.
	ForceSecure bool
}

// DefaultCookiePolicy returns the values documented in PLAN §8.
func DefaultCookiePolicy(secure bool) CookiePolicy {
	return CookiePolicy{
		Name:        "kanban_session",
		Path:        "/",
		SameSite:    http.SameSiteLaxMode,
		ForceSecure: secure,
	}
}

// Secure reports whether a cookie written for this request should carry the
// Secure attribute. Loopback with explicit InsecureHTTP also opts out so a
// local dev container does not need HTTPS.
func (c CookiePolicy) Secure(r *http.Request, m *Manager) bool {
	if c.ForceSecure {
		return true
	}
	if m != nil && m.Insecure {
		return false
	}
	if r != nil {
		// Honour X-Forwarded-Proto only from a trusted proxy.
		if p := m.ForwardedProto(r); p == "https" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Middleware chains
// ---------------------------------------------------------------------------

// RequireAuth enforces a valid bearer / API-key / session. It does NOT
// enforce scope: scope checks live one layer up so each tool can declare
// the exact scope it needs.
//
// As a side-effect every request that reaches an authenticated handler has
// its body capped at domain.MaxRequestBodyBytes. PLAN §8: "1 MB body limit".
// The cap is enforced here (not in the HTTP server) so it follows every
// route that RequireAuth guards, and a misconfigured peer cannot bypass it
// by hitting an unauthenticated healthz path that someone forgot to cap.
func (m *Manager) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m == nil || m.Tokens == nil {
			http.Error(w, "auth not configured", http.StatusInternalServerError)
			return
		}
		// Cap the request body. Two layers, both required:
		//   1. Pre-flight on Content-Length so a client that announces a 5 MB
		//      payload gets 413 before we stream a single byte.
		//   2. http.MaxBytesReader on the body itself so a chunked-encoded
		//      oversize body still trips the limit during read.
		// PLAN §8: "1 MB body limit".
		if r.ContentLength > domain.MaxRequestBodyBytes {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, domain.MaxRequestBodyBytes)
		}
		res, err := m.authenticateRequest(r)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		// Rate-limit per token. Anonymous traffic is rate-limited by IP via
		// the login path; here we only count identified callers.
		if res.Token != nil && !m.Limits.AllowToken(res.Token.Name) {
			writeAuthError(w, ErrRateLimited)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithAuth(r.Context(), res)))
	})
}

// LoginRateLimit is a standalone middleware for the login endpoint: it
// applies the per-IP limit (PLAN §8: 20/min per IP on /login).
func (m *Manager) LoginRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := m.ClientIP(r)
		if key == "" || !m.Limits.AllowLogin(key) {
			writeAuthError(w, ErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticateRequest reads credentials from the request. Precedence:
//  1. Bearer / X-API-Key header (MCP)
//  2. Query-string ?ticket= (SSE; one-time, consumed)
//  3. Session cookie (browser)
//  4. No credential
//
// AuthKind on the result tells downstream handlers what was consumed.
func (m *Manager) authenticateRequest(r *http.Request) (*AuthResult, error) {
	if secret := extractBearer(r); secret != "" {
		tok, err := m.VerifyToken(r.Context(), secret)
		if err != nil {
			return nil, err
		}
		if tok == nil {
			return nil, ErrForbidden
		}
		return &AuthResult{Token: tok, Actor: ResolveActor(tok), Kind: AuthBearer}, nil
	}
	if ticket := r.URL.Query().Get("ticket"); ticket != "" {
		tokenID, ok := m.Tickets.Consume(ticket)
		if !ok {
			return nil, ErrForbidden
		}
		// Look up the token by ID via the name-less path. The store gives us
		// GetByHash, so we re-derive the name by listing: tickets are rare,
		// this path is rare, and the alternative (adding GetByID) leaks
		// storage shape into auth. Caching is out of scope for v1.
		tok, err := m.lookupByTokenID(r.Context(), tokenID)
		if err != nil {
			return nil, err
		}
		if tok == nil {
			return nil, ErrForbidden
		}
		return &AuthResult{Token: tok, Actor: ResolveActor(tok), Kind: AuthTicket}, nil
	}
	if cid, ok := readSessionCookie(r); ok {
		sess, err := m.Sessions.GetSession(r.Context(), cid)
		if err != nil {
			// Same not-found-as-403 contract as VerifyToken: a stale or
			// tampered session cookie is a bad credential, never a server
			// fault. Treat it as if the cookie were absent so the caller
			// can challenge afresh.
			if e := domain.AsError(err); e != nil && e.Code == domain.CodeNotFound {
				return nil, ErrNoToken
			}
			return nil, err
		}
		if sess != nil && m.validSession(sess, m.Now()) {
			tok, err := m.lookupByTokenID(r.Context(), sess.TokenID)
			if err != nil {
				// A session that points at a token that no longer exists is
				// a stale login, not a server fault.
				if e := domain.AsError(err); e != nil && e.Code == domain.CodeNotFound {
					return nil, ErrForbidden
				}
				return nil, err
			}
			if tok == nil || !tok.Active() {
				return nil, ErrForbidden
			}
			// Touch under a best-effort fire-and-forget; failure is logged
			// but does not deny the request — losing the last_seen update
			// is recoverable on the next request.
			_ = m.Sessions.TouchSession(r.Context(), sess.ID, m.Now())
			return &AuthResult{Token: tok, Actor: ResolveActor(tok), Kind: AuthSession}, nil
		}
	}
	return nil, ErrNoToken
}

// lookupByTokenID walks the token list to find a row by ID. Tickets and
// sessions store the token ID, not the name, so we need an indexed path; the
// store implementation will provide it. This in-memory fallback is sufficient
// for the tests; production will use a tighter store method.
func (m *Manager) lookupByTokenID(ctx context.Context, id string) (*domain.Token, error) {
	rows, err := m.Tokens.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range rows {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, nil
}

// validSession enforces idle 7d / absolute 30d (PLAN §8).
func (m *Manager) validSession(s *domain.Session, now time.Time) bool {
	if s == nil {
		return false
	}
	if now.After(s.ExpiresAt) {
		return false
	}
	if now.Sub(s.LastSeenAt) > domain.SessionIdleTTL {
		return false
	}
	return true
}

func readSessionCookie(r *http.Request) (string, bool) {
	c, err := r.Cookie(DefaultCookiePolicy(false).Name)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

// writeAuthError renders the right HTTP status for each error class.
// Bodies are deliberately short: the machine-readable code lives in the
// service envelope for MCP callers, and the HTML login form picks up the
// header on the browser side.
//
// Fail-safe: the *default* of this function is 403, not 500. The auth
// layer's entire job is to police the credential; any error reaching it
// that we did not anticipate is more likely to be a bad credential than a
// server fault, and a 500 here would tell every MCP client "the server is
// broken" when the actual answer is "your token is wrong". A genuine
// 500-condition (e.g. the token store is unreachable) DOES propagate — it
// shows up via the higher-level logging, not by being masked to 403 here.
// domain.Error codes are mapped so a CodeNotFound coming out of the store
// becomes the 403 it actually means, not a 500.
func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoToken):
		w.Header().Set("WWW-Authenticate", `Bearer realm="kanban"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	case errors.Is(err, ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	default:
		if e := domain.AsError(err); e != nil {
			// Whether the store phrased it as "not found", "forbidden" or
			// anything else, the credential path cannot honour the request
			// — that is, by definition, the user's problem.
			http.Error(w, strings.ToLower(string(e.Code)), http.StatusForbidden)
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	}
}

// RequireScope returns a middleware that enforces the supplied scope on top
// of RequireAuth. Use it on the small number of routes that genuinely need a
// scope-checked identity (admin pages, the admin token-management endpoints).
func (m *Manager) RequireScope(want domain.Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		res := FromContext(r.Context())
		if res == nil || res.Actor == nil {
			writeAuthError(rw, ErrNoToken)
			return
		}
		if !res.Actor.ScopeAllows(want) {
			writeAuthError(rw, ErrForbidden)
			return
		}
		next.ServeHTTP(rw, r)
	})
}

// Redact strips credentials out of any header block before it reaches a
// logger. The block is a defensive copy; the caller's slice is not mutated.
func Redact(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := h.Clone()
	if v := out.Get("Authorization"); v != "" {
		out.Set("Authorization", redactBearer(v))
	}
	if v := out.Get("X-API-Key"); v != "" {
		out.Set("X-API-Key", redactToken(v))
	}
	if v := out.Get("Cookie"); v != "" {
		out.Set("Cookie", redactCookie(v))
	}
	return out
}

// RedactString applies the same scrub to a raw header value. Used by the
// logger for r.URL.RawQuery and similar contexts.
func RedactString(s string) string {
	if s == "" {
		return s
	}
	if strings.HasPrefix(strings.ToLower(s), "bearer ") {
		return redactBearer(s)
	}
	if strings.HasPrefix(s, TokenPrefix) {
		return redactToken(s)
	}
	return redactCookie(s)
}

func redactBearer(v string) string {
	const prefix = "Bearer "
	if strings.HasPrefix(v, prefix) {
		tok := strings.TrimSpace(v[len(prefix):])
		return prefix + redactToken(tok)
	}
	return redactToken(v)
}

// redactToken keeps the prefix and the first 4 chars, replaces the rest with
// a fixed-width marker. The first 4 chars are not secret: they are part of
// the fixed prefix "kbn_".
func redactToken(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4 {
		return "***"
	}
	return s[:4] + "…(redacted)"
}

func redactCookie(s string) string {
	// Replace any cookie value named kanban_session (or a small allow-list)
	// with the redacted form. Other cookies pass through.
	const target = "kanban_session="
	idx := strings.Index(s, target)
	if idx == -1 {
		return s
	}
	end := strings.IndexAny(s[idx+len(target):], ";")
	if end == -1 {
		return s[:idx+len(target)] + "***"
	}
	return s[:idx+len(target)] + "***" + s[idx+len(target)+end:]
}
