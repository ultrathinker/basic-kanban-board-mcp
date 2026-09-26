package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// KANB-18: CSRF hardening for the day the board leaves 127.0.0.1.

func postForm(w *Web, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rw := httptest.NewRecorder()
	w.Handler().ServeHTTP(rw, req)
	return rw
}

func responseCookie(rw *httptest.ResponseRecorder, name string) *http.Cookie {
	var last *http.Cookie
	for _, c := range rw.Result().Cookies() {
		if c.Name == name {
			last = c
		}
	}
	return last
}

// The login page goes through the same newPage as the board, so its <meta>
// and its cookie must agree too (item 3 of the card).
func TestCSRF_LoginPageIsSelfConsistent(t *testing.T) {
	w := newCSRFReissueWeb(t)
	rec := do(w, "GET", "/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login = %d", rec.Code)
	}
	meta := metaCSRFToken(t, rec.Body.String())
	cookie, ok := setCSRFCookie(rec.Result())
	if !ok || meta == "" || meta != cookie {
		t.Fatalf("login page: meta=%q cookie=%q (set=%v), want equal and non-empty", meta, cookie, ok)
	}
}

// The token rotates on both authentication transitions (item 1): the
// anonymous token of the login form does not survive signing in, and the
// session's token does not survive signing out.
func TestCSRF_RotatesOnLoginAndLogout(t *testing.T) {
	w := newCSRFReissueWeb(t)
	mgr := w.d.Auth

	page := do(w, "GET", "/login", nil)
	anon := metaCSRFToken(t, page.Body.String())
	login := postForm(w, "/login",
		url.Values{"token": {testAdminSecret}, "csrf_token": {anon}, "next": {"/"}},
		&http.Cookie{Name: auth.CSRFCookieName, Value: anon})
	if login.Code != http.StatusSeeOther {
		t.Fatalf("POST /login = %d\n%s", login.Code, login.Body.String())
	}
	sessCookie := responseCookie(login, "kanban_session")
	csrfIn := responseCookie(login, auth.CSRFCookieName)
	if sessCookie == nil || csrfIn == nil {
		t.Fatalf("login set session=%v csrf=%v, want both", sessCookie, csrfIn)
	}
	if csrfIn.Value == anon {
		t.Errorf("the anonymous CSRF token survived signing in")
	}
	if want := mgr.CSRFTokenForSession(sessCookie.Value); csrfIn.Value != want {
		t.Errorf("CSRF cookie after login is not the session's token")
	}

	logout := postForm(w, "/logout", url.Values{"csrf_token": {csrfIn.Value}},
		&http.Cookie{Name: "kanban_session", Value: sessCookie.Value},
		&http.Cookie{Name: auth.CSRFCookieName, Value: csrfIn.Value})
	if logout.Code != http.StatusSeeOther {
		t.Fatalf("POST /logout = %d\n%s", logout.Code, logout.Body.String())
	}
	csrfOut := responseCookie(logout, auth.CSRFCookieName)
	if csrfOut == nil || csrfOut.Value == "" || csrfOut.Value == csrfIn.Value {
		t.Errorf("the session's CSRF token survived signing out: %+v", csrfOut)
	}
}

// Over HTTPS both cookies carry the __Host- prefix, and a plain-named session
// cookie — what a sibling host could plant — is not honoured.
func TestCSRF_HostPrefixOverHTTPS(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }
	mgr := auth.NewManager(&authMemTokenStore{}, &authMemSessionStore{}, nil, "https://kanban.example", false, now)
	if _, err := mgr.Bootstrap(context.Background(), testAdminSecret); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	boards := map[string]*service.Board{"BMB": doneToggleBoard("BMB", "Card")}
	w, _ := newDoneToggleWeb(t, mgr, boards)

	page := do(w, "GET", "/login", nil)
	anon := responseCookie(page, auth.HostCookiePrefix+auth.CSRFCookieName)
	if anon == nil || !anon.Secure || anon.Path != "/" || anon.Domain != "" {
		t.Fatalf("login page CSRF cookie = %+v, want __Host- name, Secure, Path=/, no Domain", anon)
	}
	if responseCookie(page, auth.CSRFCookieName) != nil {
		t.Errorf("a plain-named CSRF cookie was still written over HTTPS")
	}

	login := postForm(w, "/login",
		url.Values{"token": {testAdminSecret}, "csrf_token": {anon.Value}, "next": {"/"}},
		&http.Cookie{Name: auth.HostCookiePrefix + auth.CSRFCookieName, Value: anon.Value})
	sess := responseCookie(login, auth.HostCookiePrefix+"kanban_session")
	if login.Code != http.StatusSeeOther || sess == nil || !sess.Secure || !sess.HttpOnly {
		t.Fatalf("POST /login = %d, session cookie %+v; want a Secure HttpOnly __Host- session cookie", login.Code, sess)
	}

	// The prefixed cookie signs the caller in...
	if rec := do(w, "GET", "/p/BMB", nil, sess); rec.Code != http.StatusOK {
		t.Errorf("GET /p/BMB with the __Host- session = %d, want 200", rec.Code)
	}
	// ...the same session id under the plain name does not.
	planted := &http.Cookie{Name: "kanban_session", Value: sess.Value}
	if rec := do(w, "GET", "/p/BMB", nil, planted); rec.Code == http.StatusOK {
		t.Errorf("a plain-named session cookie was honoured over HTTPS")
	}
}
