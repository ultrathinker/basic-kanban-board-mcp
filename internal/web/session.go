package web

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// sessionFromRequest resolves the browser session cookie to its token,
// replicating the (session cookie -> Session -> Token) resolution
// auth.Manager.RequireAuth performs internally. That method is not reusable
// as-is for page routes: on failure it writes a 401/403 JSON response
// directly, whereas an HTML page must redirect to /login instead. Manager's
// Tokens/Sessions/Now fields are exported precisely so this seam can be
// built without duplicating the token hash/verify logic — only the
// session-lookup half is repeated here.
func (w *Web) sessionFromRequest(r *http.Request) (*domain.Token, error) {
	cookieName := auth.DefaultCookiePolicy(false).Name
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	sess, err := w.d.Auth.Sessions.GetSession(r.Context(), c.Value)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}
	now := w.d.Auth.Now()
	if now.After(sess.ExpiresAt) || now.Sub(sess.LastSeenAt) > domain.SessionIdleTTL {
		return nil, nil
	}
	toks, err := w.d.Auth.Tokens.List(r.Context())
	if err != nil {
		return nil, err
	}
	for _, t := range toks {
		if t.ID == sess.TokenID {
			if !t.Active() {
				return nil, nil
			}
			if err := w.d.Auth.Sessions.TouchSession(r.Context(), sess.ID, now); err != nil {
				log.Printf("web: touch session: %v", err)
			}
			return t, nil
		}
	}
	return nil, nil
}

// actorFor converts an authenticated token into the service.Actor the
// service layer expects. Actor identity always comes from the token, never
// from a parameter (AGENTS.md non-negotiable).
func actorFor(t *domain.Token) service.Actor {
	return service.Actor{
		TokenID:     t.ID,
		Name:        t.Name,
		Scopes:      t.Scopes,
		ProjectKeys: t.ProjectKeys,
	}
}

// requireSessionPage resolves the session and enforces minScope, redirecting
// to /login or rendering a 403 page on failure. It returns ok=false after it
// has already written a response.
func (w *Web) requireSessionPage(rw http.ResponseWriter, r *http.Request, minScope domain.Scope) (*domain.Token, bool) {
	tok, err := w.sessionFromRequest(r)
	if err != nil {
		w.pageError(rw, r, err)
		return nil, false
	}
	if tok == nil {
		redirect := "/login?next=" + url.QueryEscape(r.URL.RequestURI())
		http.Redirect(rw, r, redirect, http.StatusSeeOther)
		return nil, false
	}
	if !tok.Scopes.Has(minScope) {
		w.pageError(rw, r, domain.Forbidden(
			"this token does not have the "+string(minScope)+" scope",
			"Sign in with a token that has at least "+string(minScope)+" scope, or ask an admin to grant it.",
		))
		return nil, false
	}
	return tok, true
}

// requireAPIAuth resolves either a session or a bearer/API-key token via the
// shared auth middleware and enforces minScope, writing a JSON error on
// failure. Used by fragment/API-shaped endpoints where a non-2xx status
// (not a redirect) is the right failure mode.
func (w *Web) requireAPIAuth(rw http.ResponseWriter, r *http.Request, minScope domain.Scope) (*domain.Token, bool) {
	var (
		tok *domain.Token
		ok  bool
	)
	h := w.d.Auth.RequireAuth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		res := auth.FromContext(r.Context())
		if res == nil || res.Token == nil {
			return
		}
		tok = res.Token
		ok = true
	}))
	rec := &statusPeek{ResponseWriter: rw}
	h.ServeHTTP(rec, r)
	if !ok {
		if !rec.wrote {
			apiError(rw, domain.Forbidden("authentication failed", "Sign in or pass a valid bearer token."))
		}
		return nil, false
	}
	if !tok.Scopes.Has(minScope) {
		apiError(rw, domain.Forbidden(
			"this token does not have the "+string(minScope)+" scope",
			"Use a token with at least "+string(minScope)+" scope.",
		))
		return nil, false
	}
	return tok, true
}

// statusPeek lets requireAPIAuth tell whether the wrapped RequireAuth
// middleware already wrote a response (it writes 401/403/413/429 directly on
// failure) so we don't double-write.
type statusPeek struct {
	http.ResponseWriter
	wrote bool
}

func (s *statusPeek) WriteHeader(code int) {
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusPeek) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// verifyCSRF checks the double-submit cookie on state-changing requests,
// but only when the request was authenticated by a session cookie. Bearer
// tokens, API keys and one-time SSE tickets authenticate a request the
// caller wrote themselves — there is no browser in the loop, so the
// cross-origin attack CSRF defends against is structurally impossible.
//
// The auth layer's authenticateRequest gives bearer / API-key precedence
// over the session cookie (when both are present), so the cleanest signal
// here is whether the request carried a credential the caller composed
// themselves: an Authorization: Bearer … header, an X-API-Key header, or a
// one-time ?ticket=… query parameter. If any of those is present, the
// request is not a browser-driven cookie submission, and CSRF does not
// apply. Otherwise the request reached us via the session cookie path and
// we must verify the double-submit token.
//
// requireAPIAuth / requireSessionPage populate the auth-result context
// before verifyCSRF runs, so callers using one of those seams get exactly
// the right behaviour. Handlers that bypass those two helpers (none today,
// but a future `requireAPIAuth`-less POST would) must still get the right
// answer here because they would have to supply their own bearer header
// for auth.Manager to accept the call.
//
// Manager handles both the X-CSRF-Token header and the csrf_token form
// field — auth.CSRFFieldName and the templates now agree, so a single pass
// through Manager is sufficient. We translate its auth.ErrForbidden
// sentinel into the same domain.Error envelope every pageError expects.
func (w *Web) verifyCSRF(r *http.Request) error {
	if requestUsesNonSessionCredential(r) {
		// Bearer / API-key / ticket — no browser cookies to forge, no
		// CSRF defence to apply.
		return nil
	}
	if err := w.d.Auth.VerifyCSRF(r); err != nil {
		return domain.Forbidden(
			"CSRF token missing or invalid",
			"Reload the page and submit again — your CSRF cookie or form field did not match.",
		)
	}
	return nil
}

// requestUsesNonSessionCredential reports whether the request carries a
// credential that a third-party site could not have caused the user's
// browser to send.
//
// Only request HEADERS qualify. A cross-origin form or image can make a
// browser issue a request to any URL it likes, cookies attached, but it
// cannot set an arbitrary header on it — that is the whole asymmetry CSRF
// protection rests on.
//
// A query parameter is NOT such a credential. An earlier version of this
// function also returned true for a `?ticket=` parameter, on the reasoning
// that SSE tickets are only issued to a holder who already proved
// possession of a token. That reasoning was wrong in a way worth recording:
// this function never validated the ticket, it only checked that the
// parameter was present, and the query string is entirely attacker-chosen.
// Any page could POST to /admin/tokens?ticket=x with the victim's session
// cookie attached and the CSRF check would be skipped outright. Tickets are
// consumed on GET /events, which is not state-changing and never reaches
// verifyCSRF, so honouring them here bought nothing and cost the admin
// forms their protection.
//
// If a future caller needs an exemption, derive it from the resolved
// auth.AuthResult.Kind after authentication, never from the request surface.
func requestUsesNonSessionCredential(r *http.Request) bool {
	if r == nil {
		return false
	}
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		// Any non-empty Authorization is enough — Manager will reject a
		// malformed one with 403, and the CSRF defence would not have
		// helped that 403 anyway.
		return true
	}
	if strings.TrimSpace(r.Header.Get("X-API-Key")) != "" {
		return true
	}
	return false
}

// projectSwitcherMaxProjects caps how many projects the topbar renders
// inline (the no-JS form fallback, and the seeded prefetch when JS does
// land). Beyond this the header would render hundreds of <option>s or a
// hundreds-long list; the combobox's search endpoint picks up the rest.
const projectSwitcherMaxProjects = 20

// newPage builds the common Page envelope: CSRF token/cookie, current user,
// and the shared layout (project switcher). ctx is used to fetch the
// project list; a service error there is logged and degrades to an empty
// switcher rather than failing the whole page.
func (w *Web) newPage(ctx context.Context, rw http.ResponseWriter, r *http.Request, tok *domain.Token, nav string) view.Page {
	csrfTok, err := w.d.Auth.IssueCSRF()
	if err != nil {
		log.Printf("web: issue csrf token: %v", err)
	}
	w.d.Auth.SetCSRFCookie(rw, r, csrfTok)

	p := view.Page{
		Title:     "basic-kanban-board-mcp",
		Nav:       nav,
		CSRFToken: csrfTok,
		CSRF:      csrfTok,
		BaseURL:   w.d.BaseURL,
	}
	// The operating prompts are static and live in the shared layout (the
	// topbar "Prompts" dialog), so every page carries them.
	p.Layout.Prompts = view.StandardPrompts()
	if tok != nil {
		p.CurrentUser = tok.Name
		p.Layout.LoggedInActor = tok.Name
		// IsAdmin gates the "New project" chrome; it comes from the session
		// token's scopes, never from the request.
		p.Layout.IsAdmin = tok.Scopes.Has(domain.ScopeAdmin)
		projs := w.projectSummaries(ctx, tok)
		p.Layout.Projects = projs
		// CurrentName is what the topbar combobox prints when the user has
		// not typed yet. We resolve it from the projects we just fetched so
		// we never have to issue a second service call: the topbar already
		// loaded the summary list, and a missing CurrentKey (the "All
		// projects" mode) maps cleanly to an empty CurrentName, which the
		// template turns into the literal placeholder text.
		for _, pr := range projs {
			if pr.Key == p.Layout.CurrentKey {
				p.Layout.CurrentName = pr.Name
				break
			}
		}
	}
	return p
}

// projectSummaries lists the projects the token can see, for the topbar
// switcher. Errors (including the placeholder service's ErrServiceUnavailable
// during early integration) degrade to an empty list so page chrome still
// renders instead of taking down every page.
//
// The result is capped at projectSwitcherMaxProjects entries: the no-JS
// fallback cannot usefully render hundreds of <option>s, and shipping the
// full list to every page would let the client reconstruct the whole
// project keyspace (information only the search endpoint should expose,
// one query at a time). The combobox pulls the rest through
// /projects/search as the user types.
func (w *Web) projectSummaries(ctx context.Context, tok *domain.Token) []view.ProjectSummary {
	board, err := w.d.Service.BoardGet(ctx, actorFor(tok), service.BoardGetInput{View: service.ViewSummary})
	if err != nil {
		log.Printf("web: project switcher: board_get: %v", err)
		return nil
	}
	out := make([]view.ProjectSummary, 0, len(board.Projects))
	for _, p := range board.Projects {
		out = append(out, view.ProjectSummary{Key: p.Key, Name: p.Name, Focus: p.FocusKey})
		if len(out) >= projectSwitcherMaxProjects {
			break
		}
	}
	return out
}
