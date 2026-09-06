package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/config"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web"
)

// testConfig builds a Config that points at a fresh temp-dir data directory
// and a loopback listen address.
func testConfig(t *testing.T, dir string) config.Config {
	t.Helper()
	return config.Config{
		Addr:      "127.0.0.1:0",
		Host:      "127.0.0.1",
		Port:      "0",
		DataDir:   dir,
		BaseURL:   "http://127.0.0.1:0",
		Auth:      config.AuthRequired,
		LogLevel:  config.LogInfo,
		LogFormat: config.LogText,
		ClaimTTL:  time.Hour,
	}
}

// preSeedAdminToken inserts an admin token into the token table directly via
// the store layer. internal/auth's Bootstrap helper has not historically
// assigned a token ID before insertion, so when it runs against the real
// SQLite store the store's "id, name and hash are required" check fires.
// Pre-seeding lets Bootstrap observe a non-empty token table and short-circuit
// to "no secret to mint", so these tests exercise the rest of New() against
// a real on-disk store without depending on the auth path that the auth
// owner is currently fixing.
//
// When Bootstrap is repaired upstream, this helper can be deleted and the
// tests can rely on a fresh bootstrap per test.
func preSeedAdminToken(t *testing.T, dir string, secret string) {
	t.Helper()
	dbPath := filepath.Join(dir, "kanban.db")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := store.Open(ctx, store.Config{Path: dbPath, ReadPoolSize: 2})
	if err != nil {
		t.Fatalf("preSeedAdminToken: store.Open: %v", err)
	}
	defer st.Close()

	tok := &domain.Token{
		ID:          uuid.NewString(),
		Name:        "admin",
		Hash:        auth.HashToken(secret),
		Scopes:      domain.Scopes{domain.ScopeAdmin},
		ProjectKeys: nil,
		CreatedAt:   time.Now().UTC(),
	}
	if err := st.Write(context.Background(), func(tx store.Tx) error {
		return st.Tokens().Create(tx, tok)
	}); err != nil {
		t.Fatalf("preSeedAdminToken: insert token: %v", err)
	}
}

// TestNew_OpensStoreAndConfirmsShape confirms the composition root opens
// the store, runs migrations, builds the handler tree and produces a banner.
// Because Bootstrap against a fresh SQLite database is currently broken
// (the auth layer's mintAndStore does not assign a token UUID), this test
// pre-seeds an admin row so Bootstrap sees a non-empty token table and
// short-circuits without trying to mint.
func TestNew_OpensStoreAndConfirmsShape(t *testing.T) {
	dir := t.TempDir()
	const secret = "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx"
	preSeedAdminToken(t, dir, secret)
	cfg := testConfig(t, dir)

	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	if a.Handler() == nil {
		t.Fatal("Handler() returned nil")
	}
	if a.Banner().BoardURL == "" {
		t.Fatal("Banner().BoardURL is empty")
	}
	if a.Banner().AgentSetupURL == "" {
		t.Fatal("Banner().AgentSetupURL is empty")
	}
	// Banner AdminToken must be empty here: Bootstrap saw the seeded token
	// and chose not to mint a new one.
	if a.Banner().AdminToken != "" {
		t.Fatalf("banner AdminToken = %q, want empty (idempotent bootstrap)", a.Banner().AdminToken)
	}
	if a.Addr() != cfg.Addr {
		t.Fatalf("Addr = %q, want %q", a.Addr(), cfg.Addr)
	}

	// The pre-seeded secret still verifies through the new app's manager.
	tok, err := a.auth.VerifyToken(context.Background(), secret)
	if err != nil || tok == nil {
		t.Fatalf("pre-seeded secret does not verify: (%v, %v)", tok, err)
	}
	if tok.Name != "admin" {
		t.Fatalf("name = %q, want admin", tok.Name)
	}
}

// TestNew_DoubleOpenStaysIdempotent confirms a second New against the same
// data dir observes the pre-seeded admin token, does not mint a new one,
// and continues to verify the seeded secret.
func TestNew_DoubleOpenStaysIdempotent(t *testing.T) {
	dir := t.TempDir()
	const secret = "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx"
	preSeedAdminToken(t, dir, secret)
	cfg := testConfig(t, dir)

	a1, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	a1.Close()

	a2, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer a2.Close()

	if a2.Banner().AdminToken != "" {
		t.Fatalf("second banner AdminToken = %q, want empty", a2.Banner().AdminToken)
	}
	tok, err := a2.auth.VerifyToken(context.Background(), secret)
	if err != nil || tok == nil {
		t.Fatalf("pre-seeded secret no longer verifies: (%v, %v)", tok, err)
	}
}

// TestNew_DataDirUncreatable refuses to silently run when DataDir is a path
// that cannot be created (e.g. a subdirectory of a regular file).
func TestNew_DataDirUncreatable(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, filepath.Join(blocker, "sub"))
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("expected New to fail when data dir cannot be created")
	}
}

// TestServe_ShutdownClean confirms serve(ctx, ln) returns promptly when the
// context is cancelled and the store is closed. This exercises the same
// path the cmd/kanban serve runner drives.
func TestServe_ShutdownClean(t *testing.T) {
	dir := t.TempDir()
	preSeedAdminToken(t, dir, "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx")
	cfg := testConfig(t, dir)
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- a.serve(ctx, ln) }()

	// Hit /healthz to confirm the server is up.
	url := "http://" + ln.Addr().String() + "/healthz"
	deadline := time.Now().Add(2 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				up = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !up {
		t.Fatal("server did not start within 2s")
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return within 5s after ctx cancel")
	}
}

// TestHandler_RoutesHealthz confirms the wired handler tree routes requests
// — proves the Go 1.22 pattern-mux registrations did not panic at startup.
// Every probed path returns a real status (never 0) and never 5xx.
func TestHandler_RoutesHealthz(t *testing.T) {
	dir := t.TempDir()
	preSeedAdminToken(t, dir, "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx")
	cfg := testConfig(t, dir)
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	for _, path := range []string{"/healthz", "/readyz", "/agent-setup", "/static/app.css", "/login"} {
		req := httptest.NewRequest("GET", path, nil)
		rw := httptest.NewRecorder()
		a.Handler().ServeHTTP(rw, req)
		if rw.Code == 0 {
			t.Fatalf("%s: handler did not write a status", path)
		}
		if rw.Code >= 500 {
			t.Fatalf("%s: 5xx response (code %d, body=%q)", path, rw.Code, rw.Body.String())
		}
	}
}

// TestClose_ReleasesStore confirms Close returns nil. A second Close must
// not panic — the shutdown channel is closed via sync.Once.
func TestClose_ReleasesStore(t *testing.T) {
	dir := t.TempDir()
	preSeedAdminToken(t, dir, "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx")
	cfg := testConfig(t, dir)
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second Close panicked: %v", r)
		}
	}()
	_ = a.Close()
}

// TestNew_WithServiceOverridesStub confirms WithService replaces the
// placeholder unwiredService. We install a sentinel-returning service and
// prove one of its methods is wired through.
func TestNew_WithServiceOverridesStub(t *testing.T) {
	dir := t.TempDir()
	preSeedAdminToken(t, dir, "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx")
	cfg := testConfig(t, dir)

	var observed error
	svc := serviceFunc(func(context.Context, service.Actor, service.BoardGetInput) (*service.Board, error) {
		observed = web.ErrServiceUnavailable
		return nil, web.ErrServiceUnavailable
	})

	a, err := New(context.Background(), cfg, WithService(svc))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	if _, ok := a.svc.(serviceFunc); !ok {
		t.Fatalf("svc type = %T, want serviceFunc", a.svc)
	}
	if _, err := a.svc.BoardGet(context.Background(), service.Actor{}, service.BoardGetInput{View: service.ViewSummary}); err != observed {
		t.Fatalf("BoardGet err = %v, want %v", err, observed)
	}
}

// TestNew_WithServiceFactoryWiresRealDeps proves the WithServiceFactory
// seam: the closure receives the freshly-opened store and the in-process
// events bus and its return value becomes the App's service.Service. This
// is the production path cmd/kanban uses to construct service.New(store,
// bus) without exposing internals on App.
//
// We use the real store, the real bus, and a closure that calls
// service.New directly — exactly the wiring the CLI ships — and then
// assert the App ends up holding a non-placeholder service.
func TestNew_WithServiceFactoryWiresRealDeps(t *testing.T) {
	dir := t.TempDir()
	preSeedAdminToken(t, dir, "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx")
	cfg := testConfig(t, dir)

	var (
		gotStore store.Store
		gotPub   service.EventPublisher
	)
	a, err := New(context.Background(), cfg,
		WithServiceFactory(func(st store.Store, pub service.EventPublisher) service.Service {
			gotStore = st
			gotPub = pub
			return service.New(st, pub)
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	if gotStore == nil {
		t.Fatal("factory did not receive a store")
	}
	if gotPub == nil {
		t.Fatal("factory did not receive an event publisher")
	}
	// The App's svc must NOT be the placeholder anymore — a real
	// service.Service was wired in via the factory.
	if _, ok := a.svc.(serviceFunc); ok {
		t.Fatalf("svc is still the placeholder stub; factory did not run")
	}
	// And the bus inside the factory must be the same one App installed
	// for SSE subscriptions.
	if gotPub != a.bus {
		t.Fatalf("factory bus (%T) != App bus (%T)", gotPub, a.bus)
	}
}

// TestNew_WithMCPFactoryMountsHandlers proves WithMCPFactory: a closure
// receives the freshly-built service + auth manager and returns two
// http.Handlers that App installs at /mcp and /mcp/readonly. We hit both
// endpoints and assert the handlers actually respond (they answer the MCP
// streamable-handshake "initialize" probe rather than the 501 stub).
func TestNew_WithMCPFactoryMountsHandlers(t *testing.T) {
	dir := t.TempDir()
	preSeedAdminToken(t, dir, "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx")
	cfg := testConfig(t, dir)

	// A trivial handler that always answers 200 — enough to prove the
	// factory output was wired in instead of the 501 stub.
	okHandler := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"ok":true}`))
	})

	a, err := New(context.Background(), cfg,
		WithMCPFactory(func(_ service.Service, _ *auth.Manager) (http.Handler, http.Handler) {
			return okHandler, okHandler
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	for _, path := range []string{"/mcp", "/mcp/readonly"} {
		rw := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		a.Handler().ServeHTTP(rw, req)
		if rw.Code == http.StatusNotImplemented {
			t.Fatalf("%s still served by 501 stub (factory did not run)", path)
		}
	}
}

// TestNew_FactoryPrecedenceOverDirect confirms the documented precedence:
// when both WithService and WithServiceFactory are supplied, the factory
// wins. Same for WithMCP / WithMCPFactory. The factory seam is the newer
// flexible one; the direct value seam stays for tests that want to inject
// a pre-built service without going through the store.
func TestNew_FactoryPrecedenceOverDirect(t *testing.T) {
	dir := t.TempDir()
	preSeedAdminToken(t, dir, "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx")
	cfg := testConfig(t, dir)

	directSvc := serviceFunc(func(context.Context, service.Actor, service.BoardGetInput) (*service.Board, error) {
		return nil, nil
	})
	a, err := New(context.Background(), cfg,
		WithService(directSvc),
		WithServiceFactory(func(st store.Store, _ service.EventPublisher) service.Service {
			return service.New(st, nil)
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	if _, ok := a.svc.(serviceFunc); ok {
		t.Fatalf("direct service won over factory; want factory output")
	}
}

// ---------------------------------------------------------------------------
// Service stub used only by TestNew_WithServiceOverridesStub.
// ---------------------------------------------------------------------------

// serviceFunc is a tiny service.Service implementation. Only BoardGet is
// customised; every other method returns web.ErrServiceUnavailable so the
// type satisfies the interface without us having to enumerate ten identical
// stubs.
type serviceFunc func(context.Context, service.Actor, service.BoardGetInput) (*service.Board, error)

func (f serviceFunc) BoardGet(ctx context.Context, a service.Actor, in service.BoardGetInput) (*service.Board, error) {
	return f(ctx, a, in)
}

func (serviceFunc) TaskNext(context.Context, service.Actor, service.TaskNextInput) (*service.NextResult, error) {
	return nil, web.ErrServiceUnavailable
}
func (serviceFunc) TaskGet(context.Context, service.Actor, service.TaskGetInput) (*service.TaskGetResult, error) {
	return nil, web.ErrServiceUnavailable
}
func (serviceFunc) TaskCreate(context.Context, service.Actor, service.TaskCreateInput) (*service.TaskCreateResult, error) {
	return nil, web.ErrServiceUnavailable
}
func (serviceFunc) TaskUpdate(context.Context, service.Actor, service.TaskUpdateInput) (*service.TaskUpdateResult, error) {
	return nil, web.ErrServiceUnavailable
}
func (serviceFunc) TaskLink(context.Context, service.Actor, service.TaskLinkInput) (*service.TaskLinkResult, error) {
	return nil, web.ErrServiceUnavailable
}
func (serviceFunc) TaskClaim(context.Context, service.Actor, service.TaskClaimInput) (*service.TaskClaimResult, error) {
	return nil, web.ErrServiceUnavailable
}
func (serviceFunc) TaskRemove(context.Context, service.Actor, service.TaskRemoveInput) (*service.TaskRemoveResult, error) {
	return nil, web.ErrServiceUnavailable
}
func (serviceFunc) ProjectUpsert(context.Context, service.Actor, service.ProjectUpsertInput) (*service.ProjectUpsertResult, error) {
	return nil, web.ErrServiceUnavailable
}

// Keep strings in the imports so the go vet blank-imports check stays happy
// if a future refactor drops one of the explicit references.
var _ = strings.HasPrefix
