package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/app"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/config"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/mcp"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// This file fills the wireServeRunner seam declared in main.go. It is the one
// place where the CLI knows that the server is internal/app.
//
// app.New requires both factories: the service is built from the store and
// bus New opened (so the service's store is exactly the gated view the auth
// middleware sees), and the MCP handlers are built from that service plus the
// auth manager. PLAN §4 keeps composition here — internal/app never imports
// internal/mcp or internal/service's constructor.
func init() {
	wireServeRunner = func(cfg config.Config) (ServiceRunner, error) {
		a, err := app.New(context.Background(), cfg,
			app.WithServiceFactory(buildService),
			app.WithMCPFactory(buildMCPHandlers(version)),
		)
		if err != nil {
			return nil, err
		}
		printBanner(a.Banner())
		return a, nil
	}
	runStdioBridge = wireStdioBridge
	runStdioStandalone = wireStdioStandalone
}

type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+b.token)
	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

func wireStdioBridge(url, token string) error {
	endpoint := url
	if !strings.HasSuffix(endpoint, "/mcp") && !strings.HasSuffix(endpoint, "/mcp/readonly") {
		endpoint = strings.TrimRight(endpoint, "/") + "/mcp"
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdioTransport := &gomcp.StdioTransport{}
	stdioConn, err := stdioTransport.Connect(ctx)
	if err != nil {
		return fmt.Errorf("connect stdio: %w", err)
	}
	defer stdioConn.Close()

	httpClient := &http.Client{
		Transport: &bearerRoundTripper{
			token: token,
			base:  http.DefaultTransport,
		},
	}
	remoteTransport := &gomcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}
	remoteConn, err := remoteTransport.Connect(ctx)
	if err != nil {
		return fmt.Errorf("connect remote %s: %w", endpoint, err)
	}
	defer remoteConn.Close()

	errCh := make(chan error, 2)
	go func() {
		for {
			msg, err := stdioConn.Read(ctx)
			if err != nil {
				errCh <- err
				return
			}
			if err := remoteConn.Write(ctx, msg); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		for {
			msg, err := remoteConn.Read(ctx)
			if err != nil {
				errCh <- err
				return
			}
			if err := stdioConn.Write(ctx, msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	err = <-errCh
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func wireStdioStandalone(dataDir string) error {
	ctx := context.Background()
	st, err := openStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	svc := service.New(st, nil)
	srv := mcp.NewServer(svc, version)

	tok := &domain.Token{
		ID:     "standalone-local",
		Name:   "standalone",
		Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
	}
	authResult := &auth.AuthResult{
		Token: tok,
		Actor: auth.ResolveActor(tok),
		Kind:  auth.AuthBearer,
	}
	srv.AddReceivingMiddleware(func(next gomcp.MethodHandler) gomcp.MethodHandler {
		return func(ctx context.Context, method string, req gomcp.Request) (gomcp.Result, error) {
			return next(auth.WithAuth(ctx, authResult), method, req)
		}
	})

	return srv.Run(ctx, &gomcp.StdioTransport{})
}

// buildService wires the concrete service.Service. The events.Bus satisfies
// service.EventPublisher by structural typing (both expose
// Publish(domain.Event)), so no adapter is needed.
func buildService(st store.Store, pub service.EventPublisher) service.Service {
	return service.New(st, pub)
}

// buildMCPHandlers returns a closure that mounts the full and read-only MCP
// HTTP handlers at /mcp and /mcp/readonly respectively. Both handlers
// require bearer auth (Manager.RequireAuth is wired inside mcp.NewHTTPHandler
// itself), and both call back into the freshly-built service. The closure
// captures the build version so MCP clients see the same version string
// the binary reported in `kanban version`.
func buildMCPHandlers(version string) app.MCPFactory {
	return func(svc service.Service, mgr *auth.Manager) (http.Handler, http.Handler) {
		full := mcp.NewHTTPHandler(svc, mgr, version)
		ro := mcp.NewReadOnlyHTTPHandler(svc, mgr, version)
		return full, ro
	}
}

// printBanner writes the one post-boot line only New can produce: the
// bootstrap admin token, shown exactly once, on the run that minted it.
//
// The Board / Agent-setup / Data / Bind lines are printed by runServe in
// main.go before the server is wired, so they are deliberately NOT repeated
// here — printing them in both places gave every startup two identical banners
// (Codex launch-readiness review).
func printBanner(b app.Banner) {
	if b.AdminToken != "" {
		println("")
		println("  Admin token (shown once, store it now):")
		println("   ", b.AdminToken)
		println("")
	}
}
