package auth

import (
	"crypto/subtle"
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

// VerifyCSRF checks that the request carried a CSRF token equal to the cookie
// value, using constant-time comparison. It accepts both the hidden field
// and the X-CSRF-Token header. Either alone is enough: cookie + form/header
// is the documented double-submit pattern.
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
	c, err := r.Cookie(CSRFCookieName)
	if err != nil || c.Value == "" {
		return ErrForbidden
	}
	got := readCSRFToken(r)
	if got == "" {
		return ErrForbidden
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(c.Value)) != 1 {
		return ErrForbidden
	}
	return nil
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
