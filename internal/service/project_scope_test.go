package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// TestProjectUpsert_RequiresAdmin pins the scope contract documented in
// docs/AGENT-SETUP.md: project_upsert is an admin operation, for both create and
// update. A write token — what standard coding agents run with — must not be
// able to create, reconfigure, or archive a project, so a prompt-injected or
// buggy agent cannot silently retire or reshape the board.
func TestProjectUpsert_RequiresAdmin(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	writer := env.actorWith(t, nonAdmin) // write + read, no admin

	// create is refused for a write token...
	_, err := env.svc.ProjectUpsert(ctx, writer, ProjectUpsertInput{
		Mode: UpsertCreate, Key: "NEWP", Name: "New board",
	})
	assertForbidden(t, err, "write-scope create")

	// ...and so is an update that would archive the existing project.
	v := env.proj.Version
	arch := true
	_, err = env.svc.ProjectUpsert(ctx, writer, ProjectUpsertInput{
		Mode: UpsertUpdate, Key: env.proj.Key, IfVersion: &v, Archived: &arch,
	})
	assertForbidden(t, err, "write-scope archive")

	// An admin token still can create.
	if _, err := env.svc.ProjectUpsert(ctx, env.actor, ProjectUpsertInput{
		Mode: UpsertCreate, Key: "NEWP", Name: "New board",
	}); err != nil {
		t.Fatalf("admin create should be allowed: %v", err)
	}
}

func assertForbidden(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a forbidden error, got nil", what)
	}
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeForbidden {
		t.Fatalf("%s: error = %v, want code %q", what, err, domain.CodeForbidden)
	}
}
