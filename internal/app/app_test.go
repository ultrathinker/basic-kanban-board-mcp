package app

import (
	"context"
	"errors"
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

// wiringOptions is the production-shaped wiring every test uses: the real
// service built from the store New opened, and trivial MCP handlers that
// answer 200 (the handler content is not what these tests assert on).
func wiringOptions() []Option {
	return []Option{
		WithServiceFactory(func(st store.Store, pub service.EventPublisher) service.Service {
			return service.New(st, pub)
		}),
		WithMCPFactory(func(_ service.Service, _ *auth.Manager) (http.Handler, http.Handler) {
			ok := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				rw.WriteHeader(http.StatusOK)
			})
			return ok, ok
		}),
	}
}

// newTestApp builds a fully wired App against dir with a pre-seeded admin
// token, so tests hold a known secret and Bootstrap mints nothing.
func newTestApp(t *testing.T, dir string) *App {
	t.Helper()
	const secret = "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx"
	preSeedAdminToken(t, dir, secret)
	a, err := New(context.Background(), testConfig(t, dir), wiringOptions()...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// preSeedAdminToken inserts an admin token into the token table directly via
// the store layer, so a test knows the secret it can authenticate with.
//
// It originally existed as a workaround: Bootstrap could not run against a
// real SQLite store at all, because mintAndStore built a token with no ID and
// the store rejects that. That is fixed — Bootstrap now works on a fresh
// database and survives a restart with a supplied secret. The helper stays
// only because these tests want a *known* secret without reading one back out
// of a banner, which keeps them focused on what New() wires rather than on
// credential plumbing.
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

// TestNew_OpensStoreAndConfirmsShape confirms the composition root opens the
// store, runs migrations, builds the handler tree and produces a banner.
// It pre-seeds an admin row so the test holds a known secret; Bootstrap then
// sees a non-empty table and mints nothing.
func TestNew_OpensStoreAndConfirmsShape(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
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
	if a.Addr() != a.cfg.Addr {
		t.Fatalf("Addr = %q, want %q", a.Addr(), a.cfg.Addr)
	}

	// The pre-seeded secret still verifies through the new app's manager.
	tok, err := a.auth.VerifyToken(context.Background(), "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx")
	if err != nil || tok == nil {
		t.Fatalf("pre-seeded secret does not verify: (%v, %v)", tok, err)
	}
	if tok.Name != "admin" {
		t.Fatalf("name = %q, want admin", tok.Name)
	}
}

// TestNew_SequentialReopenStaysIdempotent confirms that after one App closes
// and releases ownership, a second New against the same data dir succeeds,
// observes the pre-seeded admin token, does not mint a new one, and continues
// to verify the seeded secret.
func TestNew_SequentialReopenStaysIdempotent(t *testing.T) {
	dir := t.TempDir()
	const secret = "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx"
	preSeedAdminToken(t, dir, secret)

	a1, err := New(context.Background(), testConfig(t, dir), wiringOptions()...)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if err := a1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	a2, err := New(context.Background(), testConfig(t, dir), wiringOptions()...)
	if err != nil {
		t.Fatalf("second New after Close: %v", err)
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

// TestNew_SecondServerRefused is the topology test: while one App is open on
// a data directory, a second New must refuse with the ownership error naming
// the lock file and the database — never silently share the file.
func TestNew_SecondServerRefused(t *testing.T) {
	dir := t.TempDir()
	a1 := newTestApp(t, dir)
	defer a1.Close()

	a2, err := New(context.Background(), testConfig(t, dir), wiringOptions()...)
	if err == nil {
		_ = a2.Close()
		t.Fatal("second New on an owned data dir must refuse")
	}
	if !errors.Is(err, ErrOwnerHeld) {
		t.Fatalf("err = %v, want ErrOwnerHeld", err)
	}
	msg := err.Error()
	for _, want := range []string{"refusing to start", "kanban.lock", "kanban.db", "kanban serve"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal message missing %q:\n%s", want, msg)
		}
	}

	// Releasing the first App must make the directory available again —
	// a closed server may never lock the next one out.
	if err := a1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	a3, err := New(context.Background(), testConfig(t, dir), wiringOptions()...)
	if err != nil {
		t.Fatalf("New after the owner closed: %v", err)
	}
	defer a3.Close()
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
	if _, err := New(context.Background(), cfg, wiringOptions()...); err == nil {
		t.Fatal("expected New to fail when data dir cannot be created")
	}
}

// TestNew_RequiresFactories: New must fail loud when a wiring factory is
// missing — a server whose board service or MCP endpoints are absent is a
// configuration error, not a server that 501s until someone notices.
func TestNew_RequiresFactories(t *testing.T) {
	dir := t.TempDir()
	preSeedAdminToken(t, dir, "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx")
	cfg := testConfig(t, dir)

	svcOnly := WithServiceFactory(func(st store.Store, pub service.EventPublisher) service.Service {
		return service.New(st, pub)
	})

	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("New without a service factory must refuse")
	}
	if _, err := New(context.Background(), cfg, svcOnly); err == nil {
		t.Fatal("New without an MCP factory must refuse")
	}
	// The refusals happen before anything is opened, so the directory must
	// not be left locked.
	lock, err := AcquireOwnerLock(dir)
	if err != nil {
		t.Fatalf("failed wiring left the data dir locked: %v", err)
	}
	_ = lock.Release()
}

// TestNew_FactoryReceivesGatedStoreAndBus proves the factory seam hands over
// exactly what production wiring needs: the admission-gated store view (the
// same one the auth middleware sees) and the App's own events bus, so service
// publications reach the SSE subscribers the web layer registered.
func TestNew_FactoryReceivesGatedStoreAndBus(t *testing.T) {
	dir := t.TempDir()
	preSeedAdminToken(t, dir, "kbn_preseeded00xx00xx00xx00xx00xx00xx00xx")

	var (
		gotStore store.Store
		gotPub   service.EventPublisher
	)
	a, err := New(context.Background(), testConfig(t, dir),
		WithServiceFactory(func(st store.Store, pub service.EventPublisher) service.Service {
			gotStore = st
			gotPub = pub
			return service.New(st, pub)
		}),
		WithMCPFactory(func(_ service.Service, _ *auth.Manager) (http.Handler, http.Handler) {
			ok := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusOK) })
			return ok, ok
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	if gotStore == nil {
		t.Fatal("factory did not receive a store")
	}
	if _, ok := gotStore.(*gatedStore); !ok {
		t.Fatalf("factory store type = %T, want *gatedStore (the admission-gated view)", gotStore)
	}
	if gotPub != a.bus {
		t.Fatalf("factory bus (%T) != App bus (%T)", gotPub, a.bus)
	}
}

// TestNew_MCPHandlersMounted proves the MCP factory output is reachable at
// /mcp and /mcp/readonly rather than a stub.
func TestNew_MCPHandlersMounted(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	defer a.Close()

	for _, path := range []string{"/mcp", "/mcp/readonly"} {
		rw := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		a.Handler().ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("%s: code %d, want 200 from the mounted test handler", path, rw.Code)
		}
	}
}

// TestServe_ShutdownClean confirms serve(ctx, ln) returns promptly when the
// context is cancelled, with maintenance stopped and the store closed. This
// exercises the same path the cmd/kanban serve runner drives.
func TestServe_ShutdownClean(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)

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
	// After serve returns, ownership is released: a new App may take it.
	a2, err := New(context.Background(), testConfig(t, dir), wiringOptions()...)
	if err != nil {
		t.Fatalf("New after serve shut down: %v (ownership not released?)", err)
	}
	defer a2.Close()
}

// TestHandler_RoutesHealthz confirms the wired handler tree routes requests
// — proves the Go 1.22 pattern-mux registrations did not panic at startup.
// Every probed path returns a real status (never 0) and never 5xx.
func TestHandler_RoutesHealthz(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
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

// TestClose_ReleasesStoreAndOwnership confirms Close tears everything down
// and a second Close does not panic — every teardown step is once-guarded.
func TestClose_ReleasesStoreAndOwnership(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second Close panicked: %v", r)
		}
	}()
	_ = a.Close()

	// Ownership must be free again: the lock file may still sit on disk
	// (that is fine — it is inert), but no one may hold it.
	lock, err := AcquireOwnerLock(dir)
	if err != nil {
		t.Fatalf("Close did not release ownership: %v", err)
	}
	_ = lock.Release()
}
