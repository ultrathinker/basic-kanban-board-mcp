package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ErrInvalidCredential is returned by CreateSession when the supplied secret
// does not resolve to a token. We deliberately do NOT distinguish "no such
// token" from "revoked" so a probing attacker cannot enumerate valid names.
var ErrInvalidCredential = errors.New("auth: invalid credential")

// CreateSession validates a pasted secret and creates a browser session for
// the resulting token. The session ID is rendered into the cookie returned by
// the caller; we deliberately never log it.
func (m *Manager) CreateSession(ctx context.Context, secret string) (*domain.Session, error) {
	if secret == "" {
		return nil, ErrInvalidCredential
	}
	tok, err := m.VerifyToken(ctx, secret)
	if err != nil {
		return nil, err
	}
	if tok == nil || !tok.Active() {
		return nil, ErrInvalidCredential
	}
	now := m.Now()
	sid, err := newSessionID()
	if err != nil {
		return nil, err
	}
	sess := &domain.Session{
		ID:         sid,
		TokenID:    tok.ID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(domain.SessionAbsoluteTTL),
	}
	if err := m.Sessions.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// newSessionID returns a 256-bit opaque session identifier encoded as URL-safe
// base64. 256 bits is well above the brute-force floor; we use a CSPRNG so two
// sessions over the lifetime of the server never collide.
func newSessionID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// SetSessionCookie writes the session cookie for the browser. The Secure
// attribute is decided by CookiePolicy.Secure — see that method for the
// trusted-proxy rules.
func (m *Manager) SetSessionCookie(w http.ResponseWriter, r *http.Request, sess *domain.Session) {
	pol := DefaultCookiePolicy(m.cookieSecureBase())
	c := &http.Cookie{
		Name:     pol.Name,
		Value:    sess.ID,
		Path:     pol.Path,
		Expires:  sess.ExpiresAt,
		HttpOnly: true,
		SameSite: pol.SameSite,
		Secure:   pol.Secure(r, m),
	}
	http.SetCookie(w, c)
}

// ClearSessionCookie instructs the browser to drop the cookie. It is used by
// the logout endpoint and by any code path that wants to ensure the session
// does not outlive the request.
func (m *Manager) ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	pol := DefaultCookiePolicy(m.cookieSecureBase())
	c := &http.Cookie{
		Name:     pol.Name,
		Value:    "",
		Path:     pol.Path,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: pol.SameSite,
		Secure:   pol.Secure(r, m),
	}
	http.SetCookie(w, c)
}

// cookieSecureBase reports whether Secure should be on for cookies written
// in this deployment, regardless of request headers. ForceSecure is set when
// BaseURL is https; Insecure is set when the operator opted in to
// --insecure-http. Loopback is treated specially by CookiePolicy.Secure.
func (m *Manager) cookieSecureBase() bool {
	if m.Insecure {
		return false
	}
	return stringsHasPrefix(m.BaseURL, "https://")
}

// stringsHasPrefix is a tiny shim that lets the test file substitute a
// deterministic prefix without touching strings.HasPrefix.
var stringsHasPrefix = func(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// CSRFToken is a thin convenience: it returns a fresh opaque token suitable
// for embedding in a hidden form field. The actual verification lives in the
// web package because the cookie-or-header comparison is UI-specific.
func (m *Manager) CSRFToken() (string, error) {
	var raw [24]byte
	if err := readRandom(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
