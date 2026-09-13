package service

import (
	"context"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// Tests for KANB-27: project_upsert's `description_append`, built to match
// task_update's body_append exactly — same mutual exclusion, same join rule,
// same empty-original behaviour, same length bound, same race protection.

// freshProject reads the project's current stored state straight from the
// store, the project-level analogue of freshView.
func freshProject(t *testing.T, env *testEnv, key string) *domain.Project {
	t.Helper()
	var p *domain.Project
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		var err error
		p, err = env.Projects().GetByKey(tx, key)
		return err
	}); err != nil {
		t.Fatalf("read project %s: %v", key, err)
	}
	return p
}

// TestProjectUpsert_DescriptionAppend_EmptyAndNonEmpty mirrors
// TestTaskUpdate_BodyAppend_EmptyAndNonEmpty: appending onto an empty
// description just becomes the description; appending again joins with
// exactly "\n\n".
func TestProjectUpsert_DescriptionAppend_EmptyAndNonEmpty(t *testing.T) {
	env := openTestEnv(t)

	append1 := "first block"
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertCreate, Key: "DAP", Name: "Description Append",
		DescriptionAppend: &append1,
	}); err != nil {
		t.Fatalf("create with description_append: %v", err)
	}
	p := freshProject(t, env, "DAP")
	if p.Description != "first block" {
		t.Fatalf("description after create-time append = %q, want %q", p.Description, "first block")
	}

	append2 := "second block"
	v := p.Version
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertUpdate, Key: "DAP", IfVersion: &v,
		DescriptionAppend: &append2,
	}); err != nil {
		t.Fatalf("update append: %v", err)
	}
	p = freshProject(t, env, "DAP")
	want := "first block\n\nsecond block"
	if p.Description != want {
		t.Errorf("description after second append = %q, want %q", p.Description, want)
	}
}

// TestProjectUpsert_DescriptionAndDescriptionAppend_MutuallyExclusive
// mirrors TestTaskUpdate_BodyAndBodyAppend_MutuallyExclusive: same code,
// same "mutually exclusive" wording, same remediation requirement, and
// nothing is written.
func TestProjectUpsert_DescriptionAndDescriptionAppend_MutuallyExclusive(t *testing.T) {
	env := openTestEnv(t)
	desc := "replacement"
	appendText := "extra"

	_, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertCreate, Key: "DAX", Name: "Mutual Exclusion",
		Description:       &desc,
		DescriptionAppend: &appendText,
	})
	if err == nil {
		t.Fatalf("description + description_append was accepted")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation {
		t.Fatalf("error code = %v, want validation", de)
	}
	if !strings.Contains(de.Message, "mutually exclusive") {
		t.Errorf("message %q does not explain the refusal", de.Message)
	}
	if de.Remediation == "" {
		t.Errorf("refusal carries no remediation")
	}

	// Nothing was created at all — the project must not exist.
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		_, getErr := env.Projects().GetByKey(tx, "DAX")
		return getErr
	}); domain.AsError(err) == nil || domain.AsError(err).Code != domain.CodeNotFound {
		t.Errorf("project DAX should not have been created")
	}
}

// TestProjectUpsert_DescriptionAppend_RequiresIfVersion verifies the race
// protection the brief calls out explicitly: project_upsert's mode:"update"
// already requires if_version unconditionally (unlike task_update, where
// only replacement-style fields do), so description_append inherits that
// guard for free — a call without if_version must still be refused.
func TestProjectUpsert_DescriptionAppend_RequiresIfVersion(t *testing.T) {
	env := openTestEnv(t)
	seed := "seed"
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertCreate, Key: "DAV", Name: "Version Guard", Description: &seed,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	appendText := "unguarded"
	_, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertUpdate, Key: "DAV",
		DescriptionAppend: &appendText,
	})
	if err == nil {
		t.Fatalf("update with description_append and no if_version was accepted")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation || de.Field != "if_version" {
		t.Fatalf("error = %+v, want a validation refusal on if_version", de)
	}
	if p := freshProject(t, env, "DAV"); p.Description != "seed" {
		t.Errorf("description changed despite the missing if_version: %q", p.Description)
	}
}

// TestProjectUpsert_DescriptionAppend_ConcurrentAppendsDoNotLoseALine is the
// concrete race the brief warns about: two agents appending "at once" must
// not silently drop one line. The second call's stale if_version is
// refused as a conflict instead of overwriting blind, forcing a re-read —
// exactly the protection task_update's body_append gets from if_version
// being required on any replacement-style field.
func TestProjectUpsert_DescriptionAppend_ConcurrentAppendsDoNotLoseALine(t *testing.T) {
	env := openTestEnv(t)
	seed := "seed"
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertCreate, Key: "DAC", Name: "Concurrent Append", Description: &seed,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	v := freshProject(t, env, "DAC").Version

	lineA := "agent A's line"
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertUpdate, Key: "DAC", IfVersion: &v,
		DescriptionAppend: &lineA,
	}); err != nil {
		t.Fatalf("agent A append: %v", err)
	}

	// Agent B started from the same stale version A did (the "concurrent"
	// part) and tries to append with it now that A has already landed.
	lineB := "agent B's line"
	_, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertUpdate, Key: "DAC", IfVersion: &v,
		DescriptionAppend: &lineB,
	})
	if err == nil {
		t.Fatalf("agent B's stale append succeeded; A's line can be silently lost")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeConflict {
		t.Fatalf("error code = %v, want conflict", de)
	}

	got := freshProject(t, env, "DAC")
	want := "seed\n\nagent A's line"
	if got.Description != want {
		t.Fatalf("description = %q, want %q — A's append must survive intact", got.Description, want)
	}
}

// TestProjectUpsert_DescriptionAppendCannotOverflow mirrors
// TestTaskUpdate_BodyAppendCannotOverflow: each append is individually
// small, but the joined result is still bounded.
func TestProjectUpsert_DescriptionAppendCannotOverflow(t *testing.T) {
	env := openTestEnv(t)
	nearFull := strings.Repeat("y", domain.MaxBodyBytes-10)
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertCreate, Key: "DAO", Name: "Overflow Guard", Description: &nearFull,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	v := freshProject(t, env, "DAO").Version
	tail := strings.Repeat("z", 100)
	_, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertUpdate, Key: "DAO", IfVersion: &v,
		DescriptionAppend: &tail,
	})
	if err == nil {
		t.Fatalf("an overflowing append was accepted")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation {
		t.Fatalf("error code = %v, want validation", de)
	}
	if got := freshProject(t, env, "DAO"); got.Description != nearFull {
		t.Errorf("description changed despite a refused overflowing append (len now %d)", len(got.Description))
	}
}
