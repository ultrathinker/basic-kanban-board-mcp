package service

import (
	"context"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Tests for architecture review #18: hierarchy and dependency edges are
// project-local, and every external task reference goes through the
// actor-aware resolver.
//
// Two refusals with different jobs:
//   - a token that cannot read the other project is refused BEFORE the row is
//     read, and learns nothing about it (forbidden);
//   - a token that can read both is still refused, because v1 has no
//     cross-project graph (validation).

// makeOtherProject creates a second project with the default columns and one
// task in it, and returns that task's key.
func makeOtherProject(t *testing.T, env *testEnv, key string) domain.TaskView {
	t.Helper()
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertCreate,
		Key:  key,
		Name: "Other project",
	}); err != nil {
		t.Fatalf("create project %s: %v", key, err)
	}
	res, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{
		Tasks: []NewTask{{
			ProjectKey: key,
			Title:      "outsider",
			Type:       domain.TypeTask,
			Priority:   domain.PriorityMedium,
		}},
	})
	if err != nil {
		t.Fatalf("create task in %s: %v", key, err)
	}
	return res.Tasks[0]
}

func TestTaskLink_CrossProjectEdgeRefused(t *testing.T) {
	env := openTestEnv(t)
	outsider := makeOtherProject(t, env, "OTH")
	local := makeBacklogTask(t, env, "local work")

	for _, tc := range []struct {
		name    string
		blocker string
		blocked string
	}{
		{"foreign blocker", outsider.Key, local.Key},
		{"foreign blocked", local.Key, outsider.Key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.svc.TaskLink(context.Background(), env.actor, TaskLinkInput{
				Add: []LinkPair{{Blocker: tc.blocker, Blocked: tc.blocked}},
			})
			if err == nil {
				t.Fatalf("cross-project link was accepted")
			}
			de := domain.AsError(err)
			if de == nil || de.Code != domain.CodeValidation {
				t.Fatalf("error = %v, want a validation refusal", err)
			}
			if !strings.Contains(de.Message, "different projects") {
				t.Fatalf("message %q does not say why it was refused", de.Message)
			}
			if de.Remediation == "" {
				t.Fatalf("refusal carries no remediation")
			}
		})
	}

	// Nothing was written by either attempt.
	if bb := freshView(t, env, local.Key).BlockedBy; len(bb) != 0 {
		t.Fatalf("local task ended up blocked by %v", bb)
	}
	if bl := freshView(t, env, local.Key).Blocks; len(bl) != 0 {
		t.Fatalf("local task ended up blocking %v", bl)
	}
}

// TestTaskLink_RestrictedActorLearnsNothing is the leak half of #18: the
// refusal a project-restricted token gets must be the same whether the other
// key exists or not, and must not repeat the key back.
func TestTaskLink_RestrictedActorLearnsNothing(t *testing.T) {
	env := openTestEnv(t)
	outsider := makeOtherProject(t, env, "OTH")
	local := makeBacklogTask(t, env, "local work")
	restricted := env.actorWith(t, restrictedTo(env.proj.Key))

	link := func(blocked string) *domain.Error {
		t.Helper()
		_, err := env.svc.TaskLink(context.Background(), restricted, TaskLinkInput{
			Add: []LinkPair{{Blocker: local.Key, Blocked: blocked}},
		})
		if err == nil {
			t.Fatalf("restricted token linked to %s", blocked)
		}
		de := domain.AsError(err)
		if de == nil {
			t.Fatalf("error %v is not a domain error", err)
		}
		return de
	}

	real := link(outsider.Key)
	if real.Code != domain.CodeForbidden {
		t.Fatalf("code = %s, want forbidden", real.Code)
	}
	if strings.Contains(real.Message, outsider.Key) {
		t.Fatalf("refusal %q repeats the inaccessible task key", real.Message)
	}

	// A key that does not exist in that project must be refused identically,
	// or the difference alone tells the caller which tasks are real.
	ghost := link("OTH-9999")
	if ghost.Code != real.Code || ghost.Message != real.Message {
		t.Fatalf("existing and non-existent keys are distinguishable:\n  %s / %q\n  %s / %q",
			real.Code, real.Message, ghost.Code, ghost.Message)
	}

	if bl := freshView(t, env, local.Key).Blocks; len(bl) != 0 {
		t.Fatalf("a refused link was written anyway: %v", bl)
	}
}

func TestTaskCreate_CrossProjectParentRefused(t *testing.T) {
	env := openTestEnv(t)
	outsider := makeOtherProject(t, env, "OTH")

	_, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{
		Tasks: []NewTask{{
			ProjectKey: env.proj.Key,
			Title:      "adopted by a stranger",
			Type:       domain.TypeTask,
			Priority:   domain.PriorityMedium,
			Parent:     outsider.Key,
		}},
	})
	if err == nil {
		t.Fatalf("cross-project parent was accepted")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation || de.Field != "parent" {
		t.Fatalf("error = %+v, want a validation refusal on parent", de)
	}
}

func TestTaskCreate_CrossProjectBlockerRefused(t *testing.T) {
	env := openTestEnv(t)
	outsider := makeOtherProject(t, env, "OTH")

	_, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{
		Tasks: []NewTask{{
			ProjectKey: env.proj.Key,
			Title:      "blocked from outside",
			Type:       domain.TypeTask,
			Priority:   domain.PriorityMedium,
			BlockedBy:  []string{outsider.Key},
		}},
	})
	if err == nil {
		t.Fatalf("cross-project blocker was accepted")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation || de.Field != "blocked_by" {
		t.Fatalf("error = %+v, want a validation refusal on blocked_by", de)
	}
}

// TestTaskCreate_CrossProjectRefInBatchRefused covers the in-batch symbolic
// ref, which never touches the repository and so needs its own check.
func TestTaskCreate_CrossProjectRefInBatchRefused(t *testing.T) {
	env := openTestEnv(t)
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertCreate, Key: "OTH", Name: "Other project",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	_, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{
		Tasks: []NewTask{
			{ProjectKey: env.proj.Key, Title: "here", Type: domain.TypeTask,
				Priority: domain.PriorityMedium, Ref: "here"},
			{ProjectKey: "OTH", Title: "there", Type: domain.TypeTask,
				Priority: domain.PriorityMedium, Parent: "@here"},
		},
	})
	if err == nil {
		t.Fatalf("cross-project @ref parent was accepted")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation || de.Field != "parent" {
		t.Fatalf("error = %+v, want a validation refusal on parent", de)
	}
}

// TestTaskCreate_RestrictedActorCannotAttachToInvisibleProject is the create
// half of the leak: guessing a task key must not attach a relationship to a
// project the token cannot read.
func TestTaskCreate_RestrictedActorCannotAttachToInvisibleProject(t *testing.T) {
	env := openTestEnv(t)
	outsider := makeOtherProject(t, env, "OTH")
	restricted := env.actorWith(t, restrictedTo(env.proj.Key))

	_, err := env.svc.TaskCreate(context.Background(), restricted, TaskCreateInput{
		Tasks: []NewTask{{
			ProjectKey: env.proj.Key,
			Title:      "guessing",
			Type:       domain.TypeTask,
			Priority:   domain.PriorityMedium,
			BlockedBy:  []string{outsider.Key},
		}},
	})
	if err == nil {
		t.Fatalf("restricted token attached a blocker in an invisible project")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeForbidden {
		t.Fatalf("error = %+v, want forbidden", de)
	}
	if strings.Contains(de.Message, outsider.Key) {
		t.Fatalf("refusal %q repeats the inaccessible task key", de.Message)
	}
}

func TestTaskUpdate_CrossProjectReparentRefused(t *testing.T) {
	env := openTestEnv(t)
	outsider := makeOtherProject(t, env, "OTH")
	local := makeBacklogTask(t, env, "local work")

	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:       local.Key,
			IfVersion: intPtrLocal(local.Version),
			Parent:    FieldString{Set: true, Value: outsider.Key},
		}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.Items[0].OK {
		t.Fatalf("cross-project reparent was accepted")
	}
	if res.Items[0].Err == nil || res.Items[0].Err.Code != domain.CodeValidation {
		t.Fatalf("error = %+v, want a validation refusal", res.Items[0].Err)
	}

	after := freshView(t, env, local.Key)
	if after.ParentID != nil {
		t.Fatalf("task acquired a cross-project parent")
	}
	if after.Version != local.Version {
		t.Fatalf("version = %d, want %d unchanged", after.Version, local.Version)
	}
}

// TestTaskUpdate_RestrictedActorReparentLearnsNothing is the reparent path's
// version of the leak test: the old code read the parent straight out of the
// repository, so a restricted token could adopt a parent it cannot see.
func TestTaskUpdate_RestrictedActorReparentLearnsNothing(t *testing.T) {
	env := openTestEnv(t)
	outsider := makeOtherProject(t, env, "OTH")
	local := makeBacklogTask(t, env, "local work")
	restricted := env.actorWith(t, restrictedTo(env.proj.Key))

	reparent := func(parent string) *domain.Error {
		t.Helper()
		res, err := env.svc.TaskUpdate(context.Background(), restricted, TaskUpdateInput{
			Patches: []TaskPatch{{
				Key:       local.Key,
				IfVersion: intPtrLocal(local.Version),
				Parent:    FieldString{Set: true, Value: parent},
			}},
		})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if res.Items[0].OK {
			t.Fatalf("restricted token reparented under %s", parent)
		}
		if res.Items[0].Err == nil {
			t.Fatalf("failed item carries no error")
		}
		return res.Items[0].Err
	}

	real := reparent(outsider.Key)
	if real.Code != domain.CodeForbidden {
		t.Fatalf("code = %s, want forbidden", real.Code)
	}
	if strings.Contains(real.Message, outsider.Key) {
		t.Fatalf("refusal %q repeats the inaccessible task key", real.Message)
	}
	ghost := reparent("OTH-9999")
	if ghost.Code != real.Code || ghost.Message != real.Message {
		t.Fatalf("existing and non-existent keys are distinguishable:\n  %s / %q\n  %s / %q",
			real.Code, real.Message, ghost.Code, ghost.Message)
	}

	if freshView(t, env, local.Key).ParentID != nil {
		t.Fatalf("a refused reparent was written anyway")
	}
}
