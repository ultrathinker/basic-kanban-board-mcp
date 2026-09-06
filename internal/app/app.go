// Package app is the composition root: it turns a validated config.Config
// into a runnable server by wiring the store, the auth manager, the events
// bus, the service and the internal/web handler tree together. It also owns
// the three process-level invariants the rest of the program assumes but
// cannot enforce itself: exactly one serving process per data directory
// (the ownership lock), one bounded admission boundary in front of the single
// writer, and one lifecycle owner for background maintenance.
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
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/demo"
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

// Option configures the wiring seams on New. Both factories are required: a
// server whose board service or MCP endpoints are deliberately absent is a
// configuration error, and New refuses it instead of serving 501s and 503s
// that only fail when a user shows up.
type Option func(*options)

// ServiceFactory builds a service.Service from the store view and the
// in-process events bus that New opened. This is the seam cmd/kanban uses to
// wire the real service.New(store, bus) without this package importing the
// concrete constructor (PLAN §4 — composition lives in cmd/kanban).
type ServiceFactory func(store.Store, service.EventPublisher) service.Service

// MCPFactory builds the two MCP HTTP handlers from the freshly-built service
// and the auth manager. Same shape as ServiceFactory: the composition root
// owns the choice of mcp.NewHTTPHandler vs anything else, and this package
// only declares the seam.
type MCPFactory func(svc service.Service, mgr *auth.Manager) (handler, readonly http.Handler)

type options struct {
	serviceFactory ServiceFactory
	mcpFactory     MCPFactory
}

// WithServiceFactory installs the closure that builds the service.Service
// from the store and the bus New created. The store the factory receives is
// the gated view: its writes pass the admission boundary, so every write in
// the process — service, auth, web — shares one bounded queue.
func WithServiceFactory(f ServiceFactory) Option {
	return func(o *options) {
		if f != nil {
			o.serviceFactory = f
		}
	}
}

// WithMCPFactory installs a closure that runs inside New, after the service
// and auth manager have been built, to mount /mcp and /mcp/readonly.
func WithMCPFactory(f MCPFactory) Option {
	return func(o *options) {
		if f != nil {
			o.mcpFactory = f
		}
	}
}

// App is a fully wired, runnable server. Ownership of the data directory is
// held from New until teardown, so a second server against the same
// directory is refused for the App's whole lifetime.
type App struct {
	cfg    config.Config
	store  store.Store // the raw store App itself closes
	gate   *gatedStore // the only store view handed to service/auth/maintenance
	auth   *auth.Manager
	bus    *events.Bus
	svc    service.Service
	web    *web.Web
	lock   *OwnerLock
	banner Banner

	httpServer *http.Server
	shutdown   chan struct{}
	shutOnce   sync.Once

	maintInterval time.Duration
	maintStop     chan struct{}
	maintStopOnce sync.Once
	maintWG       sync.WaitGroup
	maintMu       sync.Mutex
	maintRuns     int
	maintLastAt   time.Time
	maintLastErr  error

	teardownOnce sync.Once
	teardownErr  error
}

// New opens the store under exclusive ownership of the data directory — the
// ownership lock is taken BEFORE store.Open so migrations, which run inside
// Open, can never race a second process — bootstraps the admin token, builds
// the auth manager and events bus, runs the wiring factories, parses the
// embedded templates and builds the HTTP handler tree. Every failure after
// the lock is taken releases it, so a failed start never locks the next one
// out.
func New(ctx context.Context, cfg config.Config, opts ...Option) (*App, error) {
	o := options{}
	for _, fn := range opts {
		fn(&o)
	}
	if o.serviceFactory == nil {
		return nil, errors.New("app: no service factory supplied — pass app.WithServiceFactory so the server has a board service (see cmd/kanban/wire.go)")
	}
	if o.mcpFactory == nil {
		return nil, errors.New("app: no MCP handler factory supplied — pass app.WithMCPFactory so /mcp and /mcp/readonly are mounted (see cmd/kanban/wire.go)")
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("app: create data dir: %w", err)
	}
	lock, err := AcquireOwnerLock(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, store.Config{Path: databasePath(cfg.DataDir)})
	if err != nil {
		_ = lock.Release()
		return nil, fmt.Errorf("app: open store: %w", err)
	}
	gate := newGatedStore(st, writerQueueDepth)

	mgr := auth.NewManagerFromStore(gate, cfg.TrustedProxies, cfg.BaseURL, cfg.InsecureHTTP, time.Now)

	// Bootstrap only creates a row when the token table is empty. store.Open
	// already applied every migration under the ownership lock taken above,
	// so a second `kanban serve` cannot even reach this point against the
	// same data dir — the bootstrap double-mint window is closed by
	// ownership, not by luck.
	adminSecret, err := mgr.Bootstrap(ctx, cfg.AdminToken)
	if err != nil {
		_ = st.Close()
		_ = lock.Release()
		return nil, fmt.Errorf("app: bootstrap admin token: %w", err)
	}

	// One value, two consumers: the bus replays from it on reconnect, and the
	// web layer reads the activity snapshot from it directly. Before this was
	// shared, the activity page drained the bus instead and needed a 150ms
	// sleep to paper over the race (independent review #13).
	history := &storeHistory{st: st}
	bus := events.New(history, 64)

	svc := o.serviceFactory(gate, bus)

	// --demo runs before the listener opens, so the first request already sees
	// a populated board. It is idempotent, so leaving the flag in a
	// docker-compose file is harmless; it is also fatal on failure, because a
	// server that silently ignored the flag would send the user hunting for a
	// board that was never seeded.
	if cfg.Demo {
		if _, err := demo.Seed(ctx, svc); err != nil {
			_ = st.Close()
			_ = lock.Release()
			return nil, err
		}
	}

	tpl, err := templates.New()
	if err != nil {
		_ = st.Close()
		_ = lock.Release()
		return nil, fmt.Errorf("app: parse templates: %w", err)
	}

	publicURL := cfg.BaseURL
	if publicURL == "" {
		publicURL = "http://" + cfg.Addr
	}
	publicURL = strings.TrimRight(publicURL, "/")

	a := &App{
		cfg:           cfg,
		store:         st,
		gate:          gate,
		auth:          mgr,
		bus:           bus,
		svc:           svc,
		lock:          lock,
		shutdown:      make(chan struct{}),
		maintInterval: maintenanceInterval,
		maintStop:     make(chan struct{}),
		banner: Banner{
			BoardURL:      publicURL,
			AgentSetupURL: publicURL + "/agent-setup",
			AdminToken:    adminSecret,
		},
	}

	mcpHandler, mcpReadonlyHandler := o.mcpFactory(svc, mgr)

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

// WriterStats snapshots the writer admission boundary. See gatedStore.
func (a *App) WriterStats() WriterStats { return a.gate.stats() }

// Run starts listening and serves until ctx is canceled, then drains
// in-flight requests (including SSE clients, which select on the shutdown
// channel handed to internal/web), stops the maintenance loop, closes the
// store and releases ownership. It returns nil on a clean shutdown.
func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.httpServer.Addr)
	if err != nil {
		// The App never served; tear down so the ownership lock and store do
		// not outlive the failed run.
		a.beginShutdown()
		return errors.Join(fmt.Errorf("app: listen %s: %w", a.httpServer.Addr, err), a.teardown())
	}
	return a.serve(ctx, ln)
}

// serve is split out from Run so tests can pass a listener bound to an
// ephemeral port (":0") without racing the real configured address.
func (a *App) serve(ctx context.Context, ln net.Listener) error {
	// Maintenance lives exactly as long as serving does: started before the
	// first request can arrive, stopped before the store goes away.
	a.startMaintenance(ctx)
	serveErr := make(chan error, 1)
	go func() { serveErr <- a.httpServer.Serve(ln) }()

	select {
	case <-ctx.Done():
		a.beginShutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		shutErr := a.httpServer.Shutdown(shutdownCtx)
		a.stopMaintenance()
		closeErr := a.teardown()
		if shutErr != nil {
			return fmt.Errorf("app: shutdown: %w", shutErr)
		}
		return closeErr
	case err := <-serveErr:
		a.beginShutdown()
		a.stopMaintenance()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.Join(err, a.teardown())
		}
		return a.teardown()
	}
}

// beginShutdown closes the shutdown channel exactly once, signaling every
// SSE handler (which selects on it) to stop holding the connection open so
// http.Server.Shutdown does not have to wait for its idle timeout.
func (a *App) beginShutdown() {
	a.shutOnce.Do(func() { close(a.shutdown) })
}

// teardown closes the store and releases the ownership lock, exactly once,
// in that order: the store must stop writing before the claim that protects
// those writes is dropped. A second call is a no-op returning the first
// result.
func (a *App) teardown() error {
	a.teardownOnce.Do(func() {
		a.teardownErr = errors.Join(a.store.Close(), a.lock.Release())
	})
	return a.teardownErr
}

// Close releases the store and the ownership lock without going through
// Run/serve. Used by callers (and tests) that built an App only to inspect
// it. Idempotent.
func (a *App) Close() error {
	a.beginShutdown()
	a.stopMaintenance()
	return a.teardown()
}
