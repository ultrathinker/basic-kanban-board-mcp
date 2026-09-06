package mcp

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// fakeService is a scriptable implementation of service.Service. Every
// method is driven by a recorded "next" handler the test sets before the
// call: the first invocation consumes the handler and, if none is queued,
// returns a typed zero value (success) or whatever Default*Err the test
// configured. It is safe for concurrent use.
//
// The fake records the last call to each method on Last* fields so a test
// can assert which inputs the tool handler forwarded to the service layer
// without a single mocking framework.
type fakeService struct {
	mu sync.Mutex

	// BoardGet.
	NextBoardGet      func(ctx context.Context, a service.Actor, in service.BoardGetInput) (*service.Board, error)
	DefaultBoardGet   *service.Board
	DefaultBoardGetEr error
	LastBoardGet      service.BoardGetInput
	LastBoardGetActor service.Actor

	// TaskNext.
	NextTaskNext      func(ctx context.Context, a service.Actor, in service.TaskNextInput) (*service.NextResult, error)
	DefaultTaskNext   *service.NextResult
	DefaultTaskNextEr error
	LastTaskNext      service.TaskNextInput
	LastTaskNextActor service.Actor

	// TaskGet.
	NextTaskGet      func(ctx context.Context, a service.Actor, in service.TaskGetInput) (*service.TaskGetResult, error)
	DefaultTaskGet   *service.TaskGetResult
	DefaultTaskGetEr error
	LastTaskGet      service.TaskGetInput
	LastTaskGetActor service.Actor

	// TaskCreate.
	NextTaskCreate      func(ctx context.Context, a service.Actor, in service.TaskCreateInput) (*service.TaskCreateResult, error)
	DefaultTaskCreate   *service.TaskCreateResult
	DefaultTaskCreateEr error
	LastTaskCreate      service.TaskCreateInput
	LastTaskCreateActor service.Actor

	// TaskUpdate.
	NextTaskUpdate      func(ctx context.Context, a service.Actor, in service.TaskUpdateInput) (*service.TaskUpdateResult, error)
	DefaultTaskUpdate   *service.TaskUpdateResult
	DefaultTaskUpdateEr error
	LastTaskUpdate      service.TaskUpdateInput
	LastTaskUpdateActor service.Actor

	// TaskLink.
	NextTaskLink      func(ctx context.Context, a service.Actor, in service.TaskLinkInput) (*service.TaskLinkResult, error)
	DefaultTaskLink   *service.TaskLinkResult
	DefaultTaskLinkEr error
	LastTaskLink      service.TaskLinkInput
	LastTaskLinkActor service.Actor

	// TaskClaim.
	NextTaskClaim      func(ctx context.Context, a service.Actor, in service.TaskClaimInput) (*service.TaskClaimResult, error)
	DefaultTaskClaim   *service.TaskClaimResult
	DefaultTaskClaimEr error
	LastTaskClaim      service.TaskClaimInput
	LastTaskClaimActor service.Actor

	// TaskRemove.
	NextTaskRemove      func(ctx context.Context, a service.Actor, in service.TaskRemoveInput) (*service.TaskRemoveResult, error)
	DefaultTaskRemove   *service.TaskRemoveResult
	DefaultTaskRemoveEr error
	LastTaskRemove      service.TaskRemoveInput
	LastTaskRemoveActor service.Actor

	// ProjectUpsert.
	NextProjectUpsert      func(ctx context.Context, a service.Actor, in service.ProjectUpsertInput) (*service.ProjectUpsertResult, error)
	DefaultProjectUpsert   *service.ProjectUpsertResult
	DefaultProjectUpsertEr error
	LastProjectUpsert      service.ProjectUpsertInput
	LastProjectUpsertActor service.Actor
}

func (f *fakeService) BoardGet(ctx context.Context, a service.Actor, in service.BoardGetInput) (*service.Board, error) {
	f.mu.Lock()
	h := f.NextBoardGet
	if h != nil {
		f.NextBoardGet = nil
	}
	f.LastBoardGet = in
	f.LastBoardGetActor = a
	f.mu.Unlock()
	if h != nil {
		return h(ctx, a, in)
	}
	return f.DefaultBoardGet, f.DefaultBoardGetEr
}

func (f *fakeService) TaskNext(ctx context.Context, a service.Actor, in service.TaskNextInput) (*service.NextResult, error) {
	f.mu.Lock()
	h := f.NextTaskNext
	if h != nil {
		f.NextTaskNext = nil
	}
	f.LastTaskNext = in
	f.LastTaskNextActor = a
	f.mu.Unlock()
	if h != nil {
		return h(ctx, a, in)
	}
	return f.DefaultTaskNext, f.DefaultTaskNextEr
}

func (f *fakeService) TaskGet(ctx context.Context, a service.Actor, in service.TaskGetInput) (*service.TaskGetResult, error) {
	f.mu.Lock()
	h := f.NextTaskGet
	if h != nil {
		f.NextTaskGet = nil
	}
	f.LastTaskGet = in
	f.LastTaskGetActor = a
	f.mu.Unlock()
	if h != nil {
		return h(ctx, a, in)
	}
	return f.DefaultTaskGet, f.DefaultTaskGetEr
}

func (f *fakeService) TaskCreate(ctx context.Context, a service.Actor, in service.TaskCreateInput) (*service.TaskCreateResult, error) {
	f.mu.Lock()
	h := f.NextTaskCreate
	if h != nil {
		f.NextTaskCreate = nil
	}
	f.LastTaskCreate = in
	f.LastTaskCreateActor = a
	f.mu.Unlock()
	if h != nil {
		return h(ctx, a, in)
	}
	return f.DefaultTaskCreate, f.DefaultTaskCreateEr
}

func (f *fakeService) TaskUpdate(ctx context.Context, a service.Actor, in service.TaskUpdateInput) (*service.TaskUpdateResult, error) {
	f.mu.Lock()
	h := f.NextTaskUpdate
	if h != nil {
		f.NextTaskUpdate = nil
	}
	f.LastTaskUpdate = in
	f.LastTaskUpdateActor = a
	f.mu.Unlock()
	if h != nil {
		return h(ctx, a, in)
	}
	return f.DefaultTaskUpdate, f.DefaultTaskUpdateEr
}

func (f *fakeService) TaskLink(ctx context.Context, a service.Actor, in service.TaskLinkInput) (*service.TaskLinkResult, error) {
	f.mu.Lock()
	h := f.NextTaskLink
	if h != nil {
		f.NextTaskLink = nil
	}
	f.LastTaskLink = in
	f.LastTaskLinkActor = a
	f.mu.Unlock()
	if h != nil {
		return h(ctx, a, in)
	}
	return f.DefaultTaskLink, f.DefaultTaskLinkEr
}

func (f *fakeService) TaskClaim(ctx context.Context, a service.Actor, in service.TaskClaimInput) (*service.TaskClaimResult, error) {
	f.mu.Lock()
	h := f.NextTaskClaim
	if h != nil {
		f.NextTaskClaim = nil
	}
	f.LastTaskClaim = in
	f.LastTaskClaimActor = a
	f.mu.Unlock()
	if h != nil {
		return h(ctx, a, in)
	}
	return f.DefaultTaskClaim, f.DefaultTaskClaimEr
}

func (f *fakeService) TaskRemove(ctx context.Context, a service.Actor, in service.TaskRemoveInput) (*service.TaskRemoveResult, error) {
	f.mu.Lock()
	h := f.NextTaskRemove
	if h != nil {
		f.NextTaskRemove = nil
	}
	f.LastTaskRemove = in
	f.LastTaskRemoveActor = a
	f.mu.Unlock()
	if h != nil {
		return h(ctx, a, in)
	}
	return f.DefaultTaskRemove, f.DefaultTaskRemoveEr
}

func (f *fakeService) ProjectUpsert(ctx context.Context, a service.Actor, in service.ProjectUpsertInput) (*service.ProjectUpsertResult, error) {
	f.mu.Lock()
	h := f.NextProjectUpsert
	if h != nil {
		f.NextProjectUpsert = nil
	}
	f.LastProjectUpsert = in
	f.LastProjectUpsertActor = a
	f.mu.Unlock()
	if h != nil {
		return h(ctx, a, in)
	}
	return f.DefaultProjectUpsert, f.DefaultProjectUpsertEr
}

// fakeActor returns a non-zero Actor for tests that need an identity
// without standing up a token. The tokenID is intentionally a placeholder —
// the auth-result payload (the part the MCP package sees) is just the actor.
func fakeActor() service.Actor {
	return service.Actor{
		TokenID: "tok-test",
		Name:    "tester",
		Scopes:  domain.Scopes{domain.ScopeRead, domain.ScopeWrite, domain.ScopeAdmin},
	}
}

// authContextFor returns a context with a populated AuthResult so
// actorFromContext can find it. The Token is built just well enough for the
// result to satisfy any downstream isActive check; tests that exercise
// per-token behaviour stand up a real Manager instead.
func authContextFor(ctx context.Context) context.Context {
	tok := &domain.Token{
		ID:        "tok-test",
		Name:      "tester",
		Scopes:    domain.Scopes{domain.ScopeRead, domain.ScopeWrite, domain.ScopeAdmin},
		CreatedAt: time.Now(),
	}
	return auth.WithAuth(ctx, &auth.AuthResult{Token: tok, Actor: auth.ResolveActor(tok)})
}

// errService is the canonical "service blew up" sentinel — a non-nil error
// that is NOT a *domain.Error, used by asDomainError's "should never happen"
// branch. The MCP layer wraps it as a validation error per AGENTS.md "fail
// loud".
var errService = errors.New("service kaboom")
