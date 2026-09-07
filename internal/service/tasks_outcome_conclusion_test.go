package service

import (
	"context"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Tests for batch 3: the research-outcome fields. Outcome is a validated enum
// that defaults to "open"; Conclusion is post-hoc free text. Both are
// replacement-style, so both require if_version and bump the version.

func TestTaskCreate_DefaultsOutcomeOpen(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "fresh")

	// A newly created task is "open" with no conclusion, both on the returned
	// view and when read back from storage.
	got := freshView(t, env, task.Key)
	if got.Outcome != domain.OutcomeOpen {
		t.Errorf("new task Outcome = %q, want open", got.Outcome)
	}
	if got.Conclusion != "" {
		t.Errorf("new task Conclusion = %q, want empty", got.Conclusion)
	}
}

func TestTaskUpdate_OutcomeAndConclusionRoundTrip(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "2d feigenbaum")

	v := freshView(t, env, task.Key).Version
	outcome := domain.OutcomeRefuted
	conclusion := "The generalization does not hold; runs 3–7 diverge."
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:        task.Key,
			IfVersion:  &v,
			Outcome:    &outcome,
			Conclusion: &conclusion,
		}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.Items[0].OK {
		t.Fatalf("update failed: %+v", res.Items[0].Err)
	}
	if res.Items[0].Task.Outcome != domain.OutcomeRefuted {
		t.Errorf("returned Outcome = %q, want refuted", res.Items[0].Task.Outcome)
	}
	if res.Items[0].Task.Conclusion != conclusion {
		t.Errorf("returned Conclusion = %q, want %q", res.Items[0].Task.Conclusion, conclusion)
	}

	// Stored row agrees.
	stored := freshView(t, env, task.Key)
	if stored.Outcome != domain.OutcomeRefuted {
		t.Errorf("stored Outcome = %q, want refuted", stored.Outcome)
	}
	if stored.Conclusion != conclusion {
		t.Errorf("stored Conclusion = %q, want %q", stored.Conclusion, conclusion)
	}

	// Reset outcome back to open and clear the conclusion with an empty string.
	v = stored.Version
	open := domain.OutcomeOpen
	empty := ""
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:        task.Key,
			IfVersion:  &v,
			Outcome:    &open,
			Conclusion: &empty,
		}},
	}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	reset := freshView(t, env, task.Key)
	if reset.Outcome != domain.OutcomeOpen {
		t.Errorf("reset Outcome = %q, want open", reset.Outcome)
	}
	if reset.Conclusion != "" {
		t.Errorf("reset Conclusion = %q, want empty", reset.Conclusion)
	}
}

func TestTaskUpdate_InvalidOutcomeRejected(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "bad outcome")
	v := freshView(t, env, task.Key).Version

	bad := domain.Outcome("wrong")
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:       task.Key,
			IfVersion: &v,
			Outcome:   &bad,
		}},
	})
	if err != nil {
		t.Fatalf("batch error: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Err == nil {
		t.Fatalf("invalid outcome was accepted (err=%v)", res.Items[0].Err)
	}
	de := domain.AsError(res.Items[0].Err)
	if de == nil || de.Code != domain.CodeValidation {
		t.Fatalf("error code = %v, want validation", de.Code)
	}
	if de.Remediation == "" {
		t.Errorf("refusal carries no remediation")
	}
	// Nothing was written: the task is still open.
	if got := freshView(t, env, task.Key); got.Outcome != domain.OutcomeOpen {
		t.Errorf("outcome changed despite refused patch: %q", got.Outcome)
	}
}

func TestTaskUpdate_ConclusionTooLongRejected(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "long conclusion")
	v := freshView(t, env, task.Key).Version

	huge := strings.Repeat("x", domain.MaxConclusionBytes+1)
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:        task.Key,
			IfVersion:  &v,
			Conclusion: &huge,
		}},
	})
	if err != nil {
		t.Fatalf("batch error: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Err == nil {
		t.Fatalf("oversized conclusion was accepted")
	}
	if de := domain.AsError(res.Items[0].Err); de == nil || de.Code != domain.CodeValidation {
		t.Errorf("error code = %v, want validation", de)
	}
}

func TestTaskUpdate_OutcomeRequiresIfVersionAndBumps(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "outcome version")

	// Without if_version it is refused (replacement-style field).
	holds := domain.OutcomeHolds
	if res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: task.Key, Outcome: &holds}},
	}); err != nil {
		t.Fatalf("batch error: %v", err)
	} else if len(res.Items) != 1 || res.Items[0].Err == nil {
		t.Fatalf("outcome without if_version was accepted")
	} else if de := domain.AsError(res.Items[0].Err); de == nil || de.Code != domain.CodeValidation {
		t.Errorf("error code = %v, want validation", de)
	}

	// With if_version it lands and bumps the version.
	pre := freshView(t, env, task.Key).Version
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: task.Key, IfVersion: &pre, Outcome: &holds}},
	}); err != nil {
		t.Fatalf("outcome update: %v", err)
	}
	if post := freshView(t, env, task.Key).Version; post <= pre {
		t.Fatalf("outcome did not bump version ( pre=%d post=%d )", pre, post)
	}
}
