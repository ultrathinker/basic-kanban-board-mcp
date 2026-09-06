package app

import (
	"context"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web"
)

// unwiredService is what New installs when no real service.Service is
// supplied via WithService. internal/service (docs/tasks/F-service.md) is a
// parallel work stream with no concrete implementation as of this package's
// first cut; every method here fails loud with web.ErrServiceUnavailable
// (defined in internal/web, not here, so this package's dependents can
// recognize and render it without an import cycle) instead of leaving a nil
// interface that would panic the first time anything calls it.
//
// The rest of the server — health checks, static assets, login, agent-setup,
// token administration — does not depend on service.Service at all, so it
// runs correctly even while this stub is in place.
type unwiredService struct{}

func newUnwiredService() service.Service { return unwiredService{} }

func (unwiredService) BoardGet(context.Context, service.Actor, service.BoardGetInput) (*service.Board, error) {
	return nil, web.ErrServiceUnavailable
}

func (unwiredService) TaskNext(context.Context, service.Actor, service.TaskNextInput) (*service.NextResult, error) {
	return nil, web.ErrServiceUnavailable
}

func (unwiredService) TaskGet(context.Context, service.Actor, service.TaskGetInput) (*service.TaskGetResult, error) {
	return nil, web.ErrServiceUnavailable
}

func (unwiredService) TaskCreate(context.Context, service.Actor, service.TaskCreateInput) (*service.TaskCreateResult, error) {
	return nil, web.ErrServiceUnavailable
}

func (unwiredService) TaskUpdate(context.Context, service.Actor, service.TaskUpdateInput) (*service.TaskUpdateResult, error) {
	return nil, web.ErrServiceUnavailable
}

func (unwiredService) TaskLink(context.Context, service.Actor, service.TaskLinkInput) (*service.TaskLinkResult, error) {
	return nil, web.ErrServiceUnavailable
}

func (unwiredService) TaskClaim(context.Context, service.Actor, service.TaskClaimInput) (*service.TaskClaimResult, error) {
	return nil, web.ErrServiceUnavailable
}

func (unwiredService) TaskRemove(context.Context, service.Actor, service.TaskRemoveInput) (*service.TaskRemoveResult, error) {
	return nil, web.ErrServiceUnavailable
}

func (unwiredService) ProjectUpsert(context.Context, service.Actor, service.ProjectUpsertInput) (*service.ProjectUpsertResult, error) {
	return nil, web.ErrServiceUnavailable
}
