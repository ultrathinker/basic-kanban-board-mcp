package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// CSRFCookieName is the cookie set alongside the session cookie. It is NOT
// HttpOnly: the browser-side form filler reads it to populate the hidden
// form field. The session cookie stays HttpOnly. This is the classic
// double-submit-cookie CSRF defence documented in OWASP CSRFGuard.
const CSRFCookieName = "kanban_csrf"

// CSRFFieldName is the hidden form field the browser-side template emits.
// The web layer renders `<input type="hidden" name="csrf_token" value="…">`
// inside every state-changing form. The value chosen here ("csrf_token")
// matches the templates the web agent shipped, so there is exactly one
// field name on both sides of the boundary.
const CSRFFieldName = "csrf_token"

// CSRFHeaderName is the alternative header (htmx and fetch callers prefer
// headers over hidden inputs; both must work).
const CSRFHeaderName = "X-CSRF-Token"

// IssueCSRF returns a fresh opaque CSRF token. The web layer calls this on
// page render, embeds the value in a hidden field, and also writes it as a
// non-HttpOnly cookie via SetCSRFCookie.
func (m *Manager) IssueCSRF() (string, error) { return m.CSRFToken() }

// SetCSRFCookie writes the CSRF token cookie. SameSite=Lax mirrors the
// session cookie; Secure is derived from the same policy so a misconfigured
// https-only deployment does not silently drop CSRF coverage. The expiry is
// the session idle TTL so an idle browser drops both cookies together.
func (m *Manager) SetCSRFCookie(w http.ResponseWriter, r *http.Request, token string) {
	pol := DefaultCookiePolicy(m.cookieSecureBase())
	c := &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     pol.Path,
		Expires:  m.Now().Add(domain.SessionIdleTTL),
		HttpOnly: false, // browser-side form filler must read this
		SameSite: pol.SameSite,
		Secure:   pol.Secure(r, m),
	}
	http.SetCookie(w, c)
}

// CSRFTokenForSession returns the CSRF token that belongs to one session:
// HMAC-SHA256 over the session id, truncated to the same csrfTokenByteLen a
// minted token carries, so it is indistinguishable in shape from one
// IssueCSRF produced.
//
// Why a derived token rather than a random one. The page re-reads itself over
// GET after every SSE signal, so the token a render hands out must be STABLE
// across renders or the already-rendered <meta name="csrf-token"> stops
// matching the cookie and every later POST is a 403. Reusing whatever token
// the request's own cookie carried achieved that stability, but it also meant
// the value the server embedded in the page was one the CLIENT supplied:
// ValidCSRFToken is a shape check, so anything 24-bytes-base64url was
// accepted and echoed back. An attacker who can write a cookie for this site
// (a sibling host under the same registrable domain is enough — SameSite=Lax
// is site-scoped and does not stop a cross-origin POST from one) could pin a
// token they knew and then satisfy a pure cookie-vs-form comparison.
//
// Deriving from the session id gives the same stability with none of that:
// the value is a deterministic function of a secret the browser never
// exposes to script (the session cookie is HttpOnly) and of a per-process
// key. An attacker who can set cookies still cannot produce the token,
// because they cannot read the session id.
func (m *Manager) CSRFTokenForSession(sessionID string) string {
	mac := hmac.New(sha256.New, m.csrfHMACKey())
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:csrfTokenByteLen])
}

// CSRFTokenForRequest returns the token a page rendered for r should embed.
// A request with a session gets that session's derived token — the same value
// on every render, which is what lets the page re-read itself over SSE
// without invalidating its own <meta>. A request without one (the login page)
// gets a freshly minted random token, as it always did.
func (m *Manager) CSRFTokenForRequest(r *http.Request) (string, error) {
	if r != nil {
		if sid := sessionIDForCSRF(r); sid != "" {
			return m.CSRFTokenForSession(sid), nil
		}
	}
	return m.CSRFToken()
}

// csrfHMACKey lazily mints the per-process key backing CSRFTokenForSession.
//
// If the CSPRNG fails the key stays zero rather than panicking mid-request:
// the defence rests on the session id being unguessable, not on this key
// being secret, so a zero key degrades to "still unforgeable by anyone who
// cannot read the session cookie" instead of to "open".
func (m *Manager) csrfHMACKey() []byte {
	m.csrfKeyOnce.Do(func() {
		_ = readRandom(m.csrfKey[:])
	})
	return m.csrfKey[:]
}

// sessionIDForCSRF returns the session cookie's value, or "" when the request
// carries none.
func sessionIDForCSRF(r *http.Request) string {
	c, err := r.Cookie(DefaultCookiePolicy(false).Name)
	if err != nil {
		return ""
	}
	return c.Value
}

// VerifyCSRF checks the CSRF token on a state-changing browser request, using
// constant-time comparison. It accepts both the hidden field and the
// X-CSRF-Token header; either alone is enough.
//
// There are two regimes, and the difference matters:
//
//   - The request carries a session cookie. The token is then verified
//     against the value derived from that session (CSRFTokenForSession), NOT
//     against the CSRF cookie. This is an authenticity check: only a caller
//     who could read the page rendered for this session can produce the
//     token, so planting a cookie proves nothing. Everything an agent or the
//     owner can actually change on the board goes through this branch.
//   - The request is anonymous (the login form). There is no session to bind
//     to yet, so the stateless double-submit comparison is all there is, and
//     it stays exactly as it was.
//
// Callers MUST invoke this on every state-changing UI route (POST, PUT,
// PATCH, DELETE). It is not the middleware's job because the middleware
// runs before the route is known and would have to inspect the method to
// know whether to gate — at which point we have just re-implemented the
// route check.
func (m *Manager) VerifyCSRF(r *http.Request) error {
	if r == nil {
		return ErrForbidden
	}
	got := readCSRFToken(r)
	if got == "" {
		return ErrForbidden
	}
	if sid := sessionIDForCSRF(r); sid != "" {
		want := m.CSRFTokenForSession(sid)
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			return ErrForbidden
		}
		return nil
	}
	c, err := r.Cookie(CSRFCookieName)
	if err != nil || c.Value == "" {
		return ErrForbidden
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(c.Value)) != 1 {
		return ErrForbidden
	}
	return nil
}

// csrfTokenByteLen is the raw entropy length CSRFToken encodes. It backs
// ValidCSRFToken below, which lets a caller sanity-check a token pulled out
// of an incoming request before deciding to reuse it (see web.newPage /
// KANB-16) instead of minting a fresh one on every render.
const csrfTokenByteLen = 24

// ValidCSRFToken reports whether token has the shape CSRFToken produces:
// base64 RawURLEncoding of csrfTokenByteLen cryptographically random bytes.
//
// This is a format check, not an authenticity check — CSRF tokens are
// deliberately not stored server-side (that is what makes the double-submit
// pattern in VerifyCSRF stateless), so there is nothing to look up here. Any
// opaque value of the right shape satisfies the double-submit contract
// exactly as well as one minted by CSRFToken; this only keeps a caller that
// wants to reuse a cookie value from perpetuating the empty string or some
// unrelated garbage that ended up in that cookie slot.
func ValidCSRFToken(token string) bool {
	if token == "" {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return false
	}
	return len(raw) == csrfTokenByteLen
}

// readCSRFToken picks the candidate from either the form value or the
// header — the spec allows either surface and the web layer picks per route.
func readCSRFToken(r *http.Request) string {
	if v := r.Header.Get(CSRFHeaderName); v != "" {
		return v
	}
	if r.Method == http.MethodPost {
		// ParseForm consumes the body; the caller can still read it
		// afterwards because the standard library restores r.Body for us
		// in Go 1.19+. We only fall back to the form path for POST so a
		// GET does not accidentally trigger a body parse.
		if err := r.ParseForm(); err == nil {
			if v := r.PostForm.Get(CSRFFieldName); v != "" {
				return v
			}
		}
	}
	return ""
}
