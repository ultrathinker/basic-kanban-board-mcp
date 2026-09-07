package service

import (
	"context"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Review follow-up: task_update must enforce the same length bounds as
// task_create. Before this, an update could set an over-long title/body, and
// repeated body_append could grow a row past MaxBodyBytes because each piece
// was individually under the per-field schema cap.

func updateExpectValidation(t *testing.T, env *testEnv, patch TaskPatch) {
	t.Helper()
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{Patches: []TaskPatch{patch}})
	if err != nil {
		t.Fatalf("batch error: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Err == nil {
		t.Fatalf("patch was accepted, want validation refusal: %+v", res.Items[0])
	}
	if de := domain.AsError(res.Items[0].Err); de == nil || de.Code != domain.CodeValidation {
		t.Fatalf("error code = %v, want validation", de)
	}
}

func TestTaskUpdate_RejectsOverlongTitleBodyNames(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "bounds")
	v := freshView(t, env, task.Key).Version

	longTitle := strings.Repeat("x", domain.MaxTitleLen+1)
	updateExpectValidation(t, env, TaskPatch{Key: task.Key, IfVersion: &v, Title: &longTitle})

	longBody := strings.Repeat("x", domain.MaxBodyBytes+1)
	updateExpectValidation(t, env, TaskPatch{Key: task.Key, IfVersion: &v, Body: &longBody})

	longName := strings.Repeat("x", domain.MaxAssigneeLen+1)
	updateExpectValidation(t, env, TaskPatch{Key: task.Key, IfVersion: &v, Assignee: FieldString{Set: true, Value: longName}})
	updateExpectValidation(t, env, TaskPatch{Key: task.Key, IfVersion: &v, Reviewer: FieldString{Set: true, Value: longName}})

	// Nothing was written by any refused patch.
	got := freshView(t, env, task.Key)
	if got.Title != "bounds" || got.Body != "" || got.Assignee != nil || got.Reviewer != nil {
		t.Errorf("a refused patch leaked through: %+v", got.Task)
	}
}

// TestTaskUpdate_BodyAppendCannotOverflow: each append is small, but their sum
// must still be bounded — the concatenated body is validated, not the piece.
func TestTaskUpdate_BodyAppendCannotOverflow(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "append bound")

	// Fill the body to just under the limit with one legal write.
	nearFull := strings.Repeat("y", domain.MaxBodyBytes-10)
	v := freshView(t, env, task.Key).Version
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: task.Key, IfVersion: &v, Body: &nearFull}},
	}); err != nil {
		t.Fatalf("seed body: %v", err)
	}

	// An append that would push the total past the limit is refused, and the
	// body is left at its pre-append value.
	v = freshView(t, env, task.Key).Version
	tail := strings.Repeat("z", 100)
	updateExpectValidation(t, env, TaskPatch{Key: task.Key, IfVersion: &v, BodyAppend: &tail})

	if got := freshView(t, env, task.Key); got.Body != nearFull {
		t.Errorf("body changed despite a refused overflowing append (len now %d)", len(got.Body))
	}
}
