package service

import (
	"context"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Tests for batch 2: apply-time wiring of Actual / Reviewer / BodyAppend,
// the mutual-exclusion rule with Body, and the version bump each causes.

func TestTaskUpdate_ActualSetAndClear(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "calibrate")

	// Set both Actual and Reviewer in one patch.
	act := 5.5
	rev := "claude"
	v := freshView(t, env, task.Key).Version
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:       task.Key,
			IfVersion: &v,
			Actual:    FieldFloat{Set: true, Value: act},
			Reviewer:  FieldString{Set: true, Value: rev},
		}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.Items[0].OK {
		t.Fatalf("update failed: %+v", res.Items[0].Err)
	}
	if res.Items[0].Task.Actual == nil || *res.Items[0].Task.Actual != 5.5 {
		t.Errorf("returned Actual = %v, want 5.5", res.Items[0].Task.Actual)
	}
	if res.Items[0].Task.Reviewer == nil || *res.Items[0].Task.Reviewer != "claude" {
		t.Errorf("returned Reviewer = %v, want claude", res.Items[0].Task.Reviewer)
	}

	// Stored row agrees with the returned view.
	stored := freshView(t, env, task.Key)
	if stored.Actual == nil || *stored.Actual != 5.5 {
		t.Errorf("stored Actual = %v, want 5.5", stored.Actual)
	}
	if stored.Reviewer == nil || *stored.Reviewer != "claude" {
		t.Errorf("stored Reviewer = %v, want claude", stored.Reviewer)
	}

	// Both fields must require if_version (they're replacement-style).
	act2 := 6.0
	if res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:    task.Key,
			Actual: FieldFloat{Set: true, Value: act2},
		}},
	}); err != nil {
		t.Fatalf("batch error: %v", err)
	} else if len(res.Items) != 1 || res.Items[0].Err == nil {
		t.Errorf("actual update without if_version was accepted (err=%v)", res.Items[0].Err)
	} else if de := domain.AsError(res.Items[0].Err); de == nil || de.Code != domain.CodeValidation {
		t.Errorf("error code = %v, want validation", de.Code)
	}

	// Clear both via the three-state Clear path.
	v = stored.Version
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:       task.Key,
			IfVersion: &v,
			Actual:    FieldFloat{Clear: true},
			Reviewer:  FieldString{Clear: true},
		}},
	}); err != nil {
		t.Fatalf("update clear: %v", err)
	}
	cleared := freshView(t, env, task.Key)
	if cleared.Actual != nil {
		t.Errorf("cleared Actual = %v, want nil", cleared.Actual)
	}
	if cleared.Reviewer != nil {
		t.Errorf("cleared Reviewer = %v, want nil", cleared.Reviewer)
	}
}

// TestTaskUpdate_ActualReviewer_BumpVersion is the contract piece: any
// replacement-style field must bump the version, so a follow-up edit that
// re-uses the pre-patch if_version must conflict.
func TestTaskUpdate_ActualReviewer_BumpVersion(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "version bump")
	pre := freshView(t, env, task.Key).Version

	act := 3.0
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:       task.Key,
			IfVersion: &pre,
			Actual:    FieldFloat{Set: true, Value: act},
		}},
	}); err != nil {
		t.Fatalf("actual update: %v", err)
	}
	post := freshView(t, env, task.Key).Version
	if post <= pre {
		t.Fatalf("version did not bump ( pre=%d post=%d )", pre, post)
	}

	// Re-using the stale if_version on a replacement field is a conflict.
	rev := "kira"
	if res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:       task.Key,
			IfVersion: &pre, // stale
			Reviewer:  FieldString{Set: true, Value: rev},
		}},
	}); err != nil {
		t.Fatalf("batch error: %v", err)
	} else if len(res.Items) != 1 || res.Items[0].Err == nil {
		t.Fatalf("stale if_version was accepted (err=%v)", res.Items[0].Err)
	} else if de := domain.AsError(res.Items[0].Err); de == nil || de.Code != domain.CodeConflict {
		t.Errorf("error code = %v, want conflict", de.Code)
	}
}

// TestTaskUpdate_BodyAppend_EmptyAndNonEmpty: with no existing body the
// appended text becomes the whole body; with existing content the
// appended text is separated by a blank line.
func TestTaskUpdate_BodyAppend_EmptyAndNonEmpty(t *testing.T) {
	env := openTestEnv(t)

	// First, on an empty body: the appended text becomes the whole body.
	a := makeBacklogTask(t, env, "append to empty")
	v := freshView(t, env, a.Key).Version
	append1 := "first block"
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:        a.Key,
			IfVersion:  &v,
			BodyAppend: &append1,
		}},
	}); err != nil {
		t.Fatalf("append to empty: %v", err)
	}
	got := freshView(t, env, a.Key)
	if got.Body != "first block" {
		t.Errorf("body after first append = %q, want %q", got.Body, "first block")
	}

	// Then a second append onto non-empty: separator is exactly "\n\n".
	v = got.Version
	append2 := "second block"
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:        a.Key,
			IfVersion:  &v,
			BodyAppend: &append2,
		}},
	}); err != nil {
		t.Fatalf("append to non-empty: %v", err)
	}
	got = freshView(t, env, a.Key)
	want := "first block\n\nsecond block"
	if got.Body != want {
		t.Errorf("body after second append = %q, want %q", got.Body, want)
	}
}

// TestTaskUpdate_BodyAndBodyAppend_MutuallyExclusive refuses a single
// patch that sets both Body and BodyAppend. The error must use the
// CodeValidation code, name the offending field, and carry a remediation
// (AGENTS.md: "Fail loud").
func TestTaskUpdate_BodyAndBodyAppend_MutuallyExclusive(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "mutual exclusion")
	v := freshView(t, env, task.Key).Version

	body := "new body"
	append := "extra"
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:        task.Key,
			IfVersion:  &v,
			Body:       &body,
			BodyAppend: &append,
		}},
	})
	if err != nil {
		t.Fatalf("batch error: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Err == nil {
		t.Fatalf("body + body_append was accepted (err=%v)", res.Items[0].Err)
	}
	de := domain.AsError(res.Items[0].Err)
	if de == nil || de.Code != domain.CodeValidation {
		t.Fatalf("error code = %v, want validation", de.Code)
	}
	if !strings.Contains(de.Message, "mutually exclusive") {
		t.Errorf("message %q does not explain the refusal", de.Message)
	}
	if de.Remediation == "" {
		t.Errorf("refusal carries no remediation")
	}

	// And nothing was written: the stored body is still empty.
	if got := freshView(t, env, task.Key); got.Body != "" {
		t.Errorf("body changed despite refused patch: %q", got.Body)
	}
}

// TestTaskUpdate_BodyAppendBumpsVersion: BodyAppend is a replacement-style
// field by isReplacementPatch's rule — it must require if_version and
// bump the version on commit.
func TestTaskUpdate_BodyAppendBumpsVersion(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "body append version")
	pre := freshView(t, env, task.Key).Version

	append := "piece"
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:        task.Key,
			IfVersion:  &pre,
			BodyAppend: &append,
		}},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	post := freshView(t, env, task.Key).Version
	if post <= pre {
		t.Fatalf("BodyAppend did not bump version ( pre=%d post=%d )", pre, post)
	}
}

// TestTaskGet_ReturnsEstimateUnit pins EstimateUnit onto every read
// surface that hydrates through the central TaskView builder (task_get;
// board_get and task_next share the same path and are covered
// transitively).
func TestTaskGet_ReturnsEstimateUnit(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "unit")

	// The seeded project uses "h"; flipping it to "d" must come back
	// through task_get — otherwise a caller cannot render "3.5d" without
	// a second read.
	v := env.proj.Version
	d := "d"
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode:      UpsertUpdate,
		Key:       env.proj.Key,
		IfVersion: &v,
		Settings:  &ProjectSettings{EstimateUnit: &d},
	}); err != nil {
		t.Fatalf("project_upsert: %v", err)
	}

	res, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{task.Key}})
	if err != nil {
		t.Fatalf("task_get: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("task_get returned %d tasks, want 1", len(res.Tasks))
	}
	if res.Tasks[0].EstimateUnit != "d" {
		t.Errorf("EstimateUnit = %q, want d", res.Tasks[0].EstimateUnit)
	}
}

// TestTaskCreate_ActualReviewerRoundTrip is the create path: a NewTask
// carrying Actual/Reviewer must persist them, and the returned view must
// echo them back.
func TestTaskCreate_ActualReviewerRoundTrip(t *testing.T) {
	env := openTestEnv(t)
	act := 2.0
	rev := "kira"
	res, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{
		Tasks: []NewTask{{
			ProjectKey: env.proj.Key,
			Title:      "create with actual/reviewer",
			Type:       domain.TypeTask,
			Priority:   domain.PriorityMedium,
			Actual:     &act,
			Reviewer:   &rev,
		}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("returned %d tasks, want 1", len(res.Tasks))
	}
	if res.Tasks[0].Actual == nil || *res.Tasks[0].Actual != 2.0 {
		t.Errorf("create returned Actual = %v, want 2.0", res.Tasks[0].Actual)
	}
	if res.Tasks[0].Reviewer == nil || *res.Tasks[0].Reviewer != "kira" {
		t.Errorf("create returned Reviewer = %v, want kira", res.Tasks[0].Reviewer)
	}
	// Reading back through task_get confirms persistence, not just the
	// in-flight construction.
	get, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{
		Keys: []string{res.Tasks[0].Key},
	})
	if err != nil {
		t.Fatalf("task_get: %v", err)
	}
	if get.Tasks[0].Actual == nil || *get.Tasks[0].Actual != 2.0 {
		t.Errorf("stored Actual = %v, want 2.0", get.Tasks[0].Actual)
	}
	if get.Tasks[0].Reviewer == nil || *get.Tasks[0].Reviewer != "kira" {
		t.Errorf("stored Reviewer = %v, want kira", get.Tasks[0].Reviewer)
	}
}
