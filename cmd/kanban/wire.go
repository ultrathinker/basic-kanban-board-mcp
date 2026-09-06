package main

import (
	"context"
	"net/http"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/app"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/config"
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

// printBanner writes the post-boot lines: where the board is, where an agent
// gets its setup snippet, and — exactly once, only when New minted it — the
// bootstrap admin token.
func printBanner(b app.Banner) {
	if b.BoardURL != "" {
		println("  Board:       ", b.BoardURL)
	}
	if b.AgentSetupURL != "" {
		println("  Agent setup: ", b.AgentSetupURL)
	}
	if b.AdminToken != "" {
		println("")
		println("  Admin token (shown once, store it now):")
		println("   ", b.AdminToken)
		println("")
	}
}
