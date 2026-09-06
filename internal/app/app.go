// Package app is the composition root: it turns a validated config.Config
// into a runnable server by wiring the store, the auth manager, the events
// bus, the (placeholder, until internal/service lands) service, and the
// internal/web handler tree together, and owns the interface seams that let
// packages built independently fit together without changing any of them.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/config"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// Banner is the startup-banner data PLAN §10 wants printed once. app never
// prints it itself — cmd/kanban owns that presentation — New only computes
// the values.
type Banner struct {
	BoardURL      string
	AgentSetupURL string
	// AdminToken is non-empty only when Bootstrap minted a brand-new token
	// during this call to New. The caller must print it exactly once.
	AdminToken string
}

// Option configures an optional seam on the App. The zero value (no options)
// is a fully functional server: /mcp and /mcp/readonly answer 501 until
// WithMCP or WithMCPFactory is supplied, and board/task operations answer
// 503 until WithService or WithServiceFactory is supplied.
type Option func(*options)

// ServiceFactory builds a service.Service from the freshly-opened store and
// the in-process events bus. This is the seam cmd/kanban uses to wire the
// real service.New(store, bus) without this package having to know the
// concrete constructor (PLAN §4 — internal/app never imports internal/mcp
// or internal/service's constructor; composition lives in cmd/kanban).
type ServiceFactory func(store.Store, service.EventPublisher) service.Service

// MCPFactory builds the two MCP HTTP handlers from the freshly-built
// service and the auth manager. Same shape as ServiceFactory: the
// composition root owns the choice of mcp.NewHTTPHandler vs anything
// else, and this package only declares the seam.
type MCPFactory func(svc service.Service, mgr *auth.Manager) (handler, readonly http.Handler)

type options struct {
	mcp            http.Handler
	mcpReadonly    http.Handler
	service        service.Service
	serviceFactory ServiceFactory
	mcpFactory     MCPFactory
}

// WithMCP mounts handler at /mcp and readonly at /mcp/readonly. Either may be
// nil to leave that mount point on the built-in 501 stub. internal/mcp is a
// parallel work stream (docs/tasks/G-mcp-tools.md); this package never
// imports it — the caller (cmd/kanban's final wiring) does.
//
// WithMCP and WithMCPFactory are mutually exclusive: a caller using
// WithMCPFactory has no pre-built handler to supply, and a caller using
// WithMCP does not want this package to delay-mount it from the freshly
// wired service. If both are set, WithMCPFactory wins because it is the
// newer, more flexible seam (H3-brief).
func WithMCP(handler, readonly http.Handler) Option {
	return func(o *options) {
		if handler != nil {
			o.mcp = handler
		}
		if readonly != nil {
			o.mcpReadonly = readonly
		}
	}
}

// WithMCPFactory installs a closure that runs inside New, after the store,
// auth manager and service have been built, to mount /mcp and /mcp/readonly.
// Use this instead of WithMCP when the handler factory needs the freshly-built
// service (e.g. mcp.NewHTTPHandler(svc, mgr, version)).
func WithMCPFactory(f MCPFactory) Option {
	return func(o *options) {
		if f != nil {
			o.mcpFactory = f
		}
	}
}

// WithService overrides the placeholder service.Service. Pass the real
// implementation here once internal/service (docs/tasks/F-service.md) has
// one; until then New installs a stub that answers every call with
// web.ErrServiceUnavailable.
//
// WithService and WithServiceFactory are mutually exclusive in the same
// sense as WithMCP / WithMCPFactory: a factory takes precedence when set.
func WithService(svc service.Service) Option {
	return func(o *options) {
		if svc != nil {
			o.service = svc
		}
	}
}

// WithServiceFactory installs a closure that runs inside New after the
// store and events bus are wired, returning the real service.Service. Use
// this in production wiring — the caller does not have access to the store
// until New has built it, so a factory is the only way to construct the
// service from the actual infrastructure this package opened.
func WithServiceFactory(f ServiceFactory) Option {
	return func(o *options) {
		if f != nil {
			o.serviceFactory = f
		}
	}
}

// App is a fully wired, runnable server.
type App struct {
	cfg    config.Config
	store  store.Store
	auth   *auth.Manager
	bus    *events.Bus
	svc    service.Service
	web    *web.Web
	banner Banner

	httpServer *http.Server
	shutdown   chan struct{}
	shutOnce   sync.Once
}

// New opens the store (running migrations under the store's startup lock),
// bootstraps the admin token, builds the auth manager and events bus, wires
// the (placeholder or supplied) service, parses the embedded templates and
// builds the HTTP handler tree.
func New(ctx context.Context, cfg config.Config, opts ...Option) (*App, error) {
	o := options{
		mcp:         nil,
		mcpReadonly: nil,
	}
	for _, fn := range opts {
		fn(&o)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("app: create data dir: %w", err)
	}
	st, err := store.Open(ctx, store.Config{Path: filepath.Join(cfg.DataDir, "kanban.db")})
	if err != nil {
		return nil, fmt.Errorf("app: open store: %w", err)
	}

	mgr := auth.NewManagerFromStore(st, cfg.TrustedProxies, cfg.BaseURL, cfg.InsecureHTTP, time.Now)

	// Bootstrap only creates a row when the token table is empty. store.Open
	// already applied migrations before we get here, and the writer pool is
	// a single connection, so there is no window where two concurrent
	// `kanban serve` startups on the same data dir could both observe an
	// empty table and double-mint an admin token.
	adminSecret, err := mgr.Bootstrap(ctx, cfg.AdminToken)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("app: bootstrap admin token: %w", err)
	}

	// One value, two consumers: the bus replays from it on reconnect, and the
	// web layer reads the activity snapshot from it directly. Before this was
	// shared, the activity page drained the bus instead and needed a 150ms
	// sleep to paper over the race (independent review #13).
	history := &storeHistory{st: st}
	bus := events.New(history, 64)

	svc := o.service
	if o.serviceFactory != nil {
		// Factory wins over a pre-built service when both are supplied.
		// events.Bus satisfies service.EventPublisher by structural
		// typing (both expose Publish(domain.Event)), so the cast is
		// implicit at the call site — no adapter needed.
		svc = o.serviceFactory(st, bus)
	}
	if svc == nil {
		svc = newUnwiredService()
	}

	tpl, err := templates.New()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("app: parse templates: %w", err)
	}

	publicURL := cfg.BaseURL
	if publicURL == "" {
		publicURL = "http://" + cfg.Addr
	}
	publicURL = strings.TrimRight(publicURL, "/")

	a := &App{
		cfg:      cfg,
		store:    st,
		auth:     mgr,
		bus:      bus,
		svc:      svc,
		shutdown: make(chan struct{}),
		banner: Banner{
			BoardURL:      publicURL,
			AgentSetupURL: publicURL + "/agent-setup",
			AdminToken:    adminSecret,
		},
	}

	// MCP mount resolution: factory wins over a pre-built handler. If
	// neither is set, web.New will substitute the 501 stub, which is
	// exactly the behaviour every other stage expects.
	var mcpHandler, mcpReadonlyHandler http.Handler
	switch {
	case o.mcpFactory != nil:
		mcpHandler, mcpReadonlyHandler = o.mcpFactory(svc, mgr)
	case o.mcp != nil || o.mcpReadonly != nil:
		mcpHandler = o.mcp
		mcpReadonlyHandler = o.mcpReadonly
	}

	a.web = web.New(web.Deps{
		Service:            svc,
		Auth:               mgr,
		Bus:                bus,
		History:            history,
		Templates:          tpl,
		BaseURL:            publicURL,
		ClaimTTLDefault:    cfg.ClaimTTL,
		ProjectLookup:      projectLookup{st: st},
		Backup:             st.Backup,
		BackupDir:          func() string { return filepath.Join(cfg.DataDir, "backups") },
		MCPHandler:         mcpHandler,
		MCPReadonlyHandler: mcpReadonlyHandler,
		Shutdown:           a.shutdown,
	})

	a.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           a.web.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return a, nil
}

// Banner returns the startup-banner data; cmd/kanban prints it.
func (a *App) Banner() Banner { return a.banner }

// Handler exposes the built http.Handler tree directly, for httptest-based
// tests that do not want a real listener.
func (a *App) Handler() http.Handler { return a.httpServer.Handler }

// Addr returns the configured listen address.
func (a *App) Addr() string { return a.httpServer.Addr }

// Run starts listening and serves until ctx is canceled, then drains
// in-flight requests (including SSE clients, which select on the shutdown
// channel handed to internal/web) and closes the store. It returns nil on a
// clean shutdown.
func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("app: listen %s: %w", a.httpServer.Addr, err)
	}
	return a.serve(ctx, ln)
}

// serve is split out from Run so tests can pass a listener bound to an
// ephemeral port (":0") without racing the real configured address.
func (a *App) serve(ctx context.Context, ln net.Listener) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- a.httpServer.Serve(ln) }()

	select {
	case <-ctx.Done():
		a.beginShutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		shutErr := a.httpServer.Shutdown(shutdownCtx)
		closeErr := a.store.Close()
		if shutErr != nil {
			return fmt.Errorf("app: shutdown: %w", shutErr)
		}
		return closeErr
	case err := <-serveErr:
		a.beginShutdown()
		closeErr := a.store.Close()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return closeErr
	}
}

// beginShutdown closes the shutdown channel exactly once, signaling every
// SSE handler (which selects on it) to stop holding the connection open so
// http.Server.Shutdown does not have to wait for its idle timeout.
func (a *App) beginShutdown() {
	a.shutOnce.Do(func() { close(a.shutdown) })
}

// Close releases the store without going through Run/Shutdown. Used by
// callers (and tests) that built an App only to inspect it.
func (a *App) Close() error {
	a.beginShutdown()
	return a.store.Close()
}
