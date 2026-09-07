// Package web is the HTTP surface: routing, page/fragment handlers, SSE and
// the session/CSRF plumbing that sits in front of internal/service.
//
// Routing uses the stdlib net/http 1.22+ pattern ServeMux (method + wildcard
// patterns) — no framework. UI routes authenticate with the session cookie;
// API-shaped routes (fragments, SSE tickets) accept either the session or a
// bearer token via internal/auth.Manager.RequireAuth.
package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// ProjectResolver resolves a project's human key to its internal storage id.
// It exists because domain.Event.ProjectID (what events.Bus filters on) is
// the internal id, but the web layer only ever sees the human key from the
// URL, and the frozen service.Service interface has no method that returns
// it. The composition root (internal/app) supplies the implementation; see
// the deviation note in the task report for why this seam exists at all.
type ProjectResolver interface {
	ProjectIDForKey(ctx context.Context, key string) (string, bool, error)
}

// EventHistory reads committed events for server-rendered snapshots (the
// activity page's initial list). It exists for the same reason as
// ProjectResolver: the frozen service.Service interface has no events read,
// and events.Bus — the other in-process source — can only hand history over
// as an asynchronous replay with no completion signal. The method set
// intentionally matches events.HistoryLoader's Since so the composition root
// can wire the very same store-backed adapter the bus already uses.
type EventHistory interface {
	// Since returns events with ID greater than afterID, oldest first. The
	// limit semantics are events.HistoryLoader's: afterID zero reads from
	// the start of the table, limit zero returns everything available.
	Since(projectID string, afterID int64, limit int) ([]domain.Event, error)
	// Latest returns the newest `limit` events for the project, oldest first,
	// bounding the read in the query. The activity snapshot uses this instead
	// of reading the whole table and truncating in memory, so a long-lived
	// append-only event log does not turn the page load into a latency cliff.
	Latest(projectID string, limit int) ([]domain.Event, error)
}

// Deps are the dependencies the composition root wires together. Every field
// is required except MCPHandler/MCPReadonlyHandler (default to a 501 stub)
// and Now (defaults to time.Now).
type Deps struct {
	Service   service.Service
	Auth      *auth.Manager
	Bus       *events.Bus
	Templates *templates.Engine

	// BaseURL is the externally visible origin, used for absolute links in
	// agent-setup snippets. Empty means "derive from the request".
	BaseURL string

	// ClaimTTLDefault seeds the claim fragment when the caller does not
	// specify a TTL.
	ClaimTTLDefault time.Duration

	ProjectLookup ProjectResolver

	// History reads committed events for the activity page's initial
	// server-rendered snapshot. Optional: when nil, snapshotEvents falls
	// back to draining a throwaway bus subscription (an idle-timed guess at
	// replay completion — the slow path review #13 is about), so a build
	// whose composition root has not wired this yet keeps working. Wire it
	// to the same loader events.Bus was constructed with.
	History EventHistory

	// Backup performs a VACUUM INTO copy for "GET /admin/backup"; BackupDir
	// names the destination directory. The composition root wires Backup
	// directly to store.Store.Backup so this package never imports
	// internal/store. Both default to a "not configured" stand-in.
	Backup    func(ctx context.Context, dest string) error
	BackupDir func() string

	// MCPHandler / MCPReadonlyHandler are mounted at /mcp and /mcp/readonly.
	// internal/mcp is a parallel work stream (docs/tasks/G-mcp-tools.md); web
	// never imports it. Nil means "not wired yet" and answers 501.
	MCPHandler         http.Handler
	MCPReadonlyHandler http.Handler

	// Shutdown, when non-nil, is closed by the composition root when a
	// graceful shutdown begins. Long-lived handlers (SSE) select on it so
	// they return promptly instead of holding the listener open.
	Shutdown <-chan struct{}

	// Now is the clock; tests override it. Defaults to time.Now.
	Now func() time.Time
}

// Web holds the wired dependencies and builds the http.Handler tree.
type Web struct {
	d Deps
}

// New constructs a Web from Deps, filling in defaults for optional fields.
func New(d Deps) *Web {
	if d.MCPHandler == nil {
		d.MCPHandler = stubHandler("mcp")
	}
	if d.MCPReadonlyHandler == nil {
		d.MCPReadonlyHandler = stubHandler("mcp/readonly")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Shutdown == nil {
		d.Shutdown = make(chan struct{}) // never closes; fine as a default
	}
	if d.Backup == nil {
		d.Backup = func(context.Context, string) error {
			return errors.New("web: backup is not configured on this server")
		}
	}
	if d.BackupDir == nil {
		d.BackupDir = func() string { return "." }
	}
	return &Web{d: d}
}

// Handler returns the full http.Handler tree: routes wrapped in the global
// security-header middleware.
func (w *Web) Handler() http.Handler {
	mux := http.NewServeMux()
	w.routes(mux)
	return securityHeaders(mux)
}

func (w *Web) routes(mux *http.ServeMux) {
	// Ops.
	mux.HandleFunc("GET /healthz", w.handleHealthz)
	mux.HandleFunc("GET /readyz", w.handleReadyz)

	// Pages.
	mux.HandleFunc("GET /{$}", w.handleOverview)
	mux.HandleFunc("GET /p/{key}", w.handleBoard)
	mux.HandleFunc("GET /p/{key}/activity", w.handleActivity)
	mux.HandleFunc("GET /projects/search", w.handleProjectsSearch)
	// Admin-only project creation (the "New project" form). POST because it
	// changes state and carries a CSRF token; the handler enforces admin scope.
	mux.HandleFunc("POST /projects", w.handleProjectCreate)
	// POST, not GET: an anchor cannot carry a CSRF token, and these three
	// either write to disk (backup) or stream the whole board out (exports).
	// The templates submit them as forms with a csrf_token field.
	mux.HandleFunc("POST /p/{key}/export", w.handleProjectExport)
	mux.HandleFunc("GET /t/{key}", w.handleDrawer)

	mux.HandleFunc("GET /login", w.handleLoginForm)
	mux.Handle("POST /login", w.d.Auth.LoginRateLimit(http.HandlerFunc(w.handleLoginSubmit)))
	mux.HandleFunc("POST /logout", w.handleLogout)

	mux.HandleFunc("GET /agent-setup", w.handleAgentSetup)

	mux.HandleFunc("GET /admin", w.handleAdmin)
	mux.HandleFunc("POST /admin/tokens", w.handleAdminTokenCreate)
	mux.HandleFunc("POST /admin/tokens/{name}/rotate", w.handleAdminTokenRotate)
	mux.HandleFunc("POST /admin/tokens/{name}/revoke", w.handleAdminTokenRevoke)
	mux.HandleFunc("POST /admin/export", w.handleAdminExport)
	mux.HandleFunc("POST /admin/backup", w.handleAdminBackup)

	// htmx/fetch fragment mutations.
	mux.HandleFunc("POST /fragments/move", w.handleFragmentMove)
	mux.HandleFunc("POST /fragments/acceptance", w.handleFragmentAcceptance)
	mux.HandleFunc("POST /fragments/notes", w.handleFragmentNote)
	mux.HandleFunc("POST /fragments/claim", w.handleFragmentClaim)

	// SSE.
	mux.HandleFunc("GET /events", w.handleEvents)
	mux.HandleFunc("GET /events/p/{key}", w.handleEvents)
	mux.HandleFunc("POST /events/ticket", w.handleEventsTicket)

	// Static assets.
	mux.Handle("GET /static/", w.staticHandler())

	// MCP mount seam (narrow: an http.Handler field on Deps). internal/web
	// never imports internal/mcp.
	mux.Handle("/mcp", w.d.MCPHandler)
	mux.Handle("/mcp/readonly", w.d.MCPReadonlyHandler)
}

// stubHandler answers 501 with a small JSON envelope until the composition
// root has a real handler to mount at name.
func stubHandler(name string) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(http.StatusNotImplemented)
		_, _ = rw.Write([]byte(`{"ok":false,"error":{"code":"not_implemented","message":"` + name + ` is not mounted in this build yet"}}`))
	})
}

// securityHeaders sets the headers PLAN §8 requires on every response: no
// inline scripts (external app.js is the only script source), no framing,
// no sniffing, no referrer leakage. style-src allows 'unsafe-inline' because
// the templates use inline style="" attributes for one-off layout tweaks;
// the risk that CSP's no-inline-script rule defends against (arbitrary JS
// execution) does not apply to inline styles.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"base-uri 'none'; " +
		"form-action 'self'; " +
		"frame-ancestors 'none'"
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h := rw.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(rw, r)
	})
}
