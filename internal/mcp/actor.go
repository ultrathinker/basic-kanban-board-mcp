package mcp

import (
	"context"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// actorFromContext resolves the authenticated caller that internal/auth's
// RequireAuth middleware attached to the request context into a
// service.Actor. Every exported HTTP handler in this package requires that
// middleware to run first (see NewHTTPHandler / NewReadOnlyHTTPHandler); a
// missing actor here means a caller wired the transport wrong, not that the
// request is unauthenticated (that case is already rejected with 401/403
// before an MCP tool handler ever runs).
func actorFromContext(ctx context.Context) (service.Actor, *domain.Error) {
	res := auth.FromContext(ctx)
	if res == nil || res.Actor == nil {
		return service.Actor{}, domain.Forbidden(
			"no authenticated actor in request context",
			"This endpoint must be mounted behind internal/auth.Manager.RequireAuth; the MCP tool layer never accepts an unauthenticated caller.",
		)
	}
	return service.Actor{
		TokenID:     res.Actor.TokenID,
		Name:        res.Actor.Name,
		Scopes:      res.Actor.Scopes,
		ProjectKeys: res.Actor.ProjectKeys,
	}, nil
}
