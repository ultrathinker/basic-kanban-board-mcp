package web

import (
	"net/http"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// handleLoginForm is "GET /login". An already-signed-in caller is bounced
// straight to their destination rather than shown the form again.
func (w *Web) handleLoginForm(rw http.ResponseWriter, r *http.Request) {
	next := safeRedirect(r.URL.Query().Get("next"))
	if tok, err := w.sessionFromRequest(r); err == nil && tok != nil {
		http.Redirect(rw, r, next, http.StatusSeeOther)
		return
	}
	page := w.newPage(r.Context(), rw, r, nil, "login")
	page.Model = view.LoginModel{Redirect: next}
	w.render(rw, r, http.StatusOK, "page-login", page)
}

// handleLoginSubmit is "POST /login". It is registered behind
// auth.Manager.LoginRateLimit (PLAN §8: 20/min per IP).
func (w *Web) handleLoginSubmit(rw http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		w.renderLoginError(rw, r, http.StatusForbidden, "Your session expired — reload the page and try again.", "")
		return
	}
	secret := strings.TrimSpace(r.PostFormValue("token"))
	next := safeRedirect(r.PostFormValue("next"))

	sess, err := w.d.Auth.CreateSession(r.Context(), secret)
	if err != nil {
		w.renderLoginError(rw, r, http.StatusUnauthorized, "That token was not recognized or has been revoked.", next)
		return
	}
	w.d.Auth.SetSessionCookie(rw, r, sess)
	http.Redirect(rw, r, next, http.StatusSeeOther)
}

func (w *Web) renderLoginError(rw http.ResponseWriter, r *http.Request, status int, msg, next string) {
	page := w.newPage(r.Context(), rw, r, nil, "login")
	page.Model = view.LoginModel{Error: msg, Redirect: next}
	w.render(rw, r, status, "page-login", page)
}

// handleLogout is "POST /logout". Not currently linked from any template
// (the topbar has no sign-out control yet), but the endpoint is safe,
// tested infrastructure for whenever the UI grows one.
func (w *Web) handleLogout(rw http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		apiError(rw, err)
		return
	}
	if c, err := r.Cookie(auth.DefaultCookiePolicy(false).Name); err == nil && c.Value != "" {
		_ = w.d.Auth.Sessions.DeleteSession(r.Context(), c.Value)
	}
	w.d.Auth.ClearSessionCookie(rw, r)
	http.Redirect(rw, r, "/login", http.StatusSeeOther)
}

// safeRedirect only allows same-site, absolute-path redirects: an open
// redirect via "next" is a classic phishing vector.
func safeRedirect(next string) string {
	next = strings.TrimSpace(next)
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}
