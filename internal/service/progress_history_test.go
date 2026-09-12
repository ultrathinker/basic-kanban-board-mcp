package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// ProgressHistory: KANB-13's fetch-on-click chart backend. A thin read over
// the same store.Progress().History that ProjectProgress already uses to
// build ManualTracks' counts — these tests pin the scoping (project vs task),
// the permission checks every other progress read already enforces, and
// that this method does not re-sort/filter/decimate (chart.go's job, not
// this one's).
// ---------------------------------------------------------------------------

// TestProgressHistory_ProjectScopeExcludesTaskMarks: TaskKey empty selects
// only the project-level track (task_id IS NULL) — task marks must never
// bleed in, the same rule ProjectProgress's Manual metric already follows.
func TestProgressHistory_ProjectScopeExcludesTaskMarks(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	task := makeBacklogTask(t, env, "has its own track")

	addProgressMark(t, env, "p1", nil, "alpha", 20, progressTestBase)
	addProgressMark(t, env, "p2", nil, "alpha", 55, progressTestBase.Add(time.Hour))
	addProgressMark(t, env, "t1", &task.ID, "alpha", 90, progressTestBase.Add(2*time.Hour))

	got, err := env.svc.ProgressHistory(ctx, env.actor, ProgressHistoryInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ProgressHistory: %v", err)
	}
	if got.ProjectKey != env.proj.Key {
		t.Fatalf("ProjectKey = %q, want %q", got.ProjectKey, env.proj.Key)
	}
	if got.TaskKey != "" {
		t.Fatalf("TaskKey = %q, want empty for the project scope", got.TaskKey)
	}
	if len(got.Marks) != 2 {
		t.Fatalf("Marks = %d, want 2 (project-level only, no task marks): %+v", len(got.Marks), got.Marks)
	}
	for _, m := range got.Marks {
		if m.TaskID != nil {
			t.Fatalf("project scope returned a task-scoped mark: %+v", m)
		}
	}
	// Chronological order (the store's own contract), oldest first.
	if !got.Marks[0].CreatedAt.Before(got.Marks[1].CreatedAt) {
		t.Fatalf("marks not chronological: %+v", got.Marks)
	}
	if got.Marks[0].Percent != 20 || got.Marks[1].Percent != 55 {
		t.Fatalf("unexpected percents in order: %+v", got.Marks)
	}
}

// TestProgressHistory_TaskScope: a TaskKey selects exactly that task's
// track, excluding both the project-level track and any other task's marks
// — the sawtooth chart for card A must never draw card B's history.
func TestProgressHistory_TaskScope(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	taskA := makeBacklogTask(t, env, "card A")
	taskB := makeBacklogTask(t, env, "card B")

	addProgressMark(t, env, "a1", &taskA.ID, "alpha", 91, progressTestBase)
	addProgressMark(t, env, "a2", &taskA.ID, "alpha", 72, progressTestBase.Add(time.Hour)) // the sawtooth
	addProgressMark(t, env, "b1", &taskB.ID, "alpha", 40, progressTestBase)
	addProgressMark(t, env, "proj1", nil, "alpha", 10, progressTestBase)

	got, err := env.svc.ProgressHistory(ctx, env.actor, ProgressHistoryInput{ProjectKey: env.proj.Key, TaskKey: taskA.Key})
	if err != nil {
		t.Fatalf("ProgressHistory: %v", err)
	}
	if got.TaskKey != taskA.Key {
		t.Fatalf("TaskKey = %q, want %q", got.TaskKey, taskA.Key)
	}
	if len(got.Marks) != 2 {
		t.Fatalf("Marks = %d, want 2 (task A's own track only): %+v", len(got.Marks), got.Marks)
	}
	if got.Marks[0].Percent != 91 || got.Marks[1].Percent != 72 {
		t.Fatalf("unexpected marks for task A: %+v", got.Marks)
	}
}

// TestProgressHistory_TaskFromAnotherProjectIsNotFound: a task key that
// belongs to a different project than ProjectKey must not leak that
// project's history through a mismatched key.
func TestProgressHistory_TaskFromAnotherProjectIsNotFound(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	if _, err := env.svc.ProjectUpsert(ctx, env.actor, ProjectUpsertInput{
		Mode: UpsertCreate, Key: "OTH", Name: "Other project",
	}); err != nil {
		t.Fatalf("create OTH: %v", err)
	}
	if _, err := env.svc.TaskCreate(ctx, env.actor, TaskCreateInput{
		Tasks: []NewTask{{ProjectKey: "OTH", Title: "outsider", Type: domain.TypeTask, Priority: domain.PriorityMedium}},
	}); err != nil {
		t.Fatalf("create OTH-1: %v", err)
	}

	got, err := env.svc.ProgressHistory(ctx, env.actor, ProgressHistoryInput{ProjectKey: env.proj.Key, TaskKey: "OTH-1"})
	if err == nil {
		t.Fatalf("expected not-found error, got result: %+v", got)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want a not-found style error", err)
	}
}

// TestProgressHistory_RequiresReadScope: an actor without read scope is
// refused before any store access, exactly like every other progress read.
func TestProgressHistory_RequiresReadScope(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	noRead := env.actorWith(t, func(a *Actor) { a.Scopes = domain.Scopes{} })
	if _, err := env.svc.ProgressHistory(ctx, noRead, ProgressHistoryInput{ProjectKey: env.proj.Key}); err == nil {
		t.Fatal("actor without read scope got a result, want forbidden")
	} else if !strings.Contains(err.Error(), "read") {
		t.Fatalf("error should name the missing read scope: %v", err)
	}
}

// TestProgressHistory_RestrictedToOtherProjectIsForbidden: a token scoped to
// a different project cannot read this project's history — the chart must
// not become a side channel around per-token project scoping.
func TestProgressHistory_RestrictedToOtherProjectIsForbidden(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	restricted := env.actorWith(t, restrictedTo("SOMEOTHER"))
	if _, err := env.svc.ProgressHistory(ctx, restricted, ProgressHistoryInput{ProjectKey: env.proj.Key}); err == nil {
		t.Fatal("actor restricted to another project got a result, want forbidden")
	}
}

// TestProgressHistory_EmptyHistoryIsNotAnError: a task nobody has assessed
// yet returns an empty Marks slice, not an error — the caller (the fetch
// handler) treats that as "nothing to chart", not a failure.
func TestProgressHistory_EmptyHistoryIsNotAnError(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	task := makeBacklogTask(t, env, "never assessed")

	got, err := env.svc.ProgressHistory(ctx, env.actor, ProgressHistoryInput{ProjectKey: env.proj.Key, TaskKey: task.Key})
	if err != nil {
		t.Fatalf("ProgressHistory: %v", err)
	}
	if len(got.Marks) != 0 {
		t.Fatalf("Marks = %+v, want empty", got.Marks)
	}
}
