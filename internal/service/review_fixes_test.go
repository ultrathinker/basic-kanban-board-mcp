package service

import (
	"context"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Regression tests for the Codex low-effort review (2026-09-07).

// Bug 1: task_next(action:"start") must return the task's state AFTER the
// claim+move, not the pre-claim candidate snapshot — an agent chains its next
// if_version from data.tasks[], so a stale column/lease/version there breaks
// the one-round-trip loop the tool exists for.
func TestTaskNext_StartReturnsPostMutationView(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "start returns fresh state")
	before := freshView(t, env, task.Key)
	if before.ColumnName != "Backlog" {
		t.Fatalf("precondition: task is in %q, want Backlog", before.ColumnName)
	}

	res, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
		ProjectKey: env.proj.Key, Action: NextStart, Limit: 1,
	})
	if err != nil {
		t.Fatalf("task_next start: %v", err)
	}
	if res.StartedKey != task.Key {
		t.Fatalf("started %q, want %q", res.StartedKey, task.Key)
	}

	var got *domain.TaskView
	for i := range res.Tasks {
		if res.Tasks[i].Key == task.Key {
			got = &res.Tasks[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("started task %q is missing from data.tasks[]", task.Key)
	}

	// The returned view reflects the mutation: moved into the active column and
	// claimed by the actor — not the stale Backlog / unclaimed snapshot.
	if got.ColumnName != "Doing" {
		t.Errorf("returned column = %q, want Doing (was the stale snapshot returned?)", got.ColumnName)
	}
	if got.ColumnKind != domain.KindActive {
		t.Errorf("returned column kind = %q, want active", got.ColumnKind)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != env.actor.Name {
		t.Errorf("returned ClaimedBy = %v, want %q", got.ClaimedBy, env.actor.Name)
	}
	// And it agrees exactly with a fresh read of the same task.
	stored := freshView(t, env, task.Key)
	if got.Version != stored.Version || got.ColumnName != stored.ColumnName {
		t.Errorf("returned view (v%d %s) disagrees with stored (v%d %s)",
			got.Version, got.ColumnName, stored.Version, stored.ColumnName)
	}
}

// Bug 2: with strict_done on, a single atomic patch that ticks the final
// acceptance item AND moves to Done must pass — the gate has to judge the
// checklist the patch produces, not the stored one.
func TestTaskUpdate_StrictDone_CheckAndMoveInOnePatch(t *testing.T) {
	env := openTestEnv(t)

	// Enable strict_done on the seeded project.
	pv := env.proj.Version
	strict := true
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertUpdate, Key: env.proj.Key, IfVersion: &pv,
		Settings: &ProjectSettings{StrictDone: &strict},
	}); err != nil {
		t.Fatalf("enable strict_done: %v", err)
	}

	task := makeBacklogTask(t, env, "finish then close")
	// Give it one unchecked acceptance item.
	v := freshView(t, env, task.Key).Version
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key: task.Key, IfVersion: &v,
			Acceptance: []domain.AcceptanceItem{{Text: "the one thing", Done: false}},
		}},
	}); err != nil {
		t.Fatalf("seed acceptance: %v", err)
	}

	// One patch: tick the last criterion AND move to Done.
	v = freshView(t, env, task.Key).Version
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key: task.Key, IfVersion: &v,
			AcceptanceCheck: []int{0},
			Column:          "Done",
		}},
	})
	if err != nil {
		t.Fatalf("check+move batch: %v", err)
	}
	if !res.Items[0].OK {
		t.Fatalf("strict_done wrongly refused check+move in one patch: %+v", res.Items[0].Err)
	}
	got := freshView(t, env, task.Key)
	if got.ColumnName != "Done" {
		t.Errorf("column = %q, want Done", got.ColumnName)
	}
	if len(got.Acceptance) != 1 || !got.Acceptance[0].Done {
		t.Errorf("acceptance not checked: %+v", got.Acceptance)
	}
}

// Bug 3: project_upsert must reject a non-positive WIP limit and a WIP limit on
// a non-active column.
func TestProjectUpsert_RejectsInvalidWIP(t *testing.T) {
	env := openTestEnv(t)
	neg := -1
	three := 3
	zero := 0

	cases := []struct {
		name string
		cols []ColumnSpec
	}{
		{"negative WIP on active", []ColumnSpec{
			{Name: "B", Kind: domain.KindBacklog},
			{Name: "D", Kind: domain.KindActive, WIPLimit: &neg},
			{Name: "Done", Kind: domain.KindDone},
		}},
		{"zero WIP on active", []ColumnSpec{
			{Name: "B", Kind: domain.KindBacklog},
			{Name: "D", Kind: domain.KindActive, WIPLimit: &zero},
			{Name: "Done", Kind: domain.KindDone},
		}},
		{"WIP on backlog", []ColumnSpec{
			{Name: "B", Kind: domain.KindBacklog, WIPLimit: &three},
			{Name: "D", Kind: domain.KindActive},
			{Name: "Done", Kind: domain.KindDone},
		}},
	}
	for i, tc := range cases {
		_, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
			Mode: UpsertCreate, Key: "WP" + string(rune('A'+i)), Name: tc.name, Columns: tc.cols,
		})
		if err == nil {
			t.Errorf("%s: accepted, want validation error", tc.name)
			continue
		}
		if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation {
			t.Errorf("%s: error code = %v, want validation", tc.name, de)
		}
	}

	// A positive WIP on an active column is still fine.
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertCreate, Key: "WPOK", Name: "ok", Columns: []ColumnSpec{
			{Name: "B", Kind: domain.KindBacklog},
			{Name: "D", Kind: domain.KindActive, WIPLimit: &three},
			{Name: "Done", Kind: domain.KindDone},
		},
	}); err != nil {
		t.Errorf("positive WIP on active column was rejected: %v", err)
	}
}
