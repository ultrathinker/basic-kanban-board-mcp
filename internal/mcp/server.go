// Package mcp adapts internal/service.Service onto the Model Context
// Protocol: nine tools, mounted over streamable HTTP, plus a read-only
// variant and the initialize instructions. This package holds no business
// logic — every rule it seems to enforce (WIP, dependencies, versions,
// cycles) actually lives in internal/domain and internal/service; this
// layer only translates MCP requests into service calls and service
// results back into the {ok, op, data, meta} / {ok:false, op, error}
// envelope (PLAN §6).
package mcp

import (
	"net/http"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ServerName is the MCP Implementation.Name every client sees during
// initialize.
const ServerName = "basic-kanban-board-mcp"

// NewServer builds the full nine-tool MCP server. version is the build
// version reported in the MCP Implementation block (cmd/kanban's -X-linked
// version string).
func NewServer(svc service.Service, version string) *gomcp.Server {
	s := gomcp.NewServer(&gomcp.Implementation{Name: ServerName, Version: version}, &gomcp.ServerOptions{
		Instructions: instructionsText,
	})
	registerBoardGet(s, svc)
	registerTaskNext(s, svc, false)
	registerTaskGet(s, svc)
	registerTaskCreate(s, svc)
	registerTaskUpdate(s, svc)
	registerTaskLink(s, svc)
	registerTaskClaim(s, svc)
	registerTaskRemove(s, svc)
	registerProjectUpsert(s, svc)
	s.AddReceivingMiddleware(schemaErrorEnvelope)
	return s
}

// NewReadOnlyServer builds the three-tool /mcp/readonly server (PLAN §6
// rule 4): board_get, task_get, and task_next with claim/start disabled.
// It shares every line of translation logic with the full server via the
// same register* functions — only which tools are registered, and
// task_next's readOnly flag, differ.
func NewReadOnlyServer(svc service.Service, version string) *gomcp.Server {
	s := gomcp.NewServer(&gomcp.Implementation{Name: ServerName + "-readonly", Version: version}, &gomcp.ServerOptions{
		Instructions: instructionsText,
	})
	registerBoardGet(s, svc)
	registerTaskGet(s, svc)
	registerTaskNext(s, svc, true)
	s.AddReceivingMiddleware(schemaErrorEnvelope)
	return s
}

// streamableOptions is shared by both HTTP handlers. Stateless is
// deliberate: this product has no need for server-initiated requests
// (no sampling, no roots), and statelessness means every POST is its own
// MCP "session" whose context is exactly that request's context — the
// only way internal/auth's per-request RequireAuth middleware can hand a
// fresh, correctly-scoped actor to every tool call, including the Nth call
// in a long-lived client, not just the first ("initialize") request the
// SDK would otherwise pin a stateful session's context to.
var streamableOptions = &gomcp.StreamableHTTPOptions{Stateless: true}

// NewHTTPHandler returns the `/mcp` endpoint: every tool, mandatory bearer
// auth. mgr must be non-nil; the handler panics on the first request
// otherwise, because an MCP tool call with no authenticated actor is a
// wiring bug, not a runtime condition (see actorFromContext).
func NewHTTPHandler(svc service.Service, mgr *auth.Manager, version string) http.Handler {
	srv := NewServer(svc, version)
	h := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, streamableOptions)
	return mgr.RequireAuth(h)
}

// NewReadOnlyHTTPHandler returns the `/mcp/readonly` endpoint.
func NewReadOnlyHTTPHandler(svc service.Service, mgr *auth.Manager, version string) http.Handler {
	srv := NewReadOnlyServer(svc, version)
	h := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, streamableOptions)
	return mgr.RequireAuth(h)
}
