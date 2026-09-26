package service

import (
	"context"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Tests for KANB-59 items 4-7: the requests the IAMT retrospective found
// coming back silently empty or refused without a way forward.

func addProject(t *testing.T, env *testEnv, key, name string) {
	t.Helper()
	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertCreate, Key: key, Name: name,
	}); err != nil {
		t.Fatalf("create %s: %v", key, err)
	}
}

func boardKey(t *testing.T, env *testEnv, a Actor, ref string) (string, *domain.Error) {
	t.Helper()
	b, err := env.svc.BoardGet(context.Background(), a, BoardGetInput{ProjectKey: ref, View: ViewSummary})
	if err != nil {
		return "", domain.AsError(err)
	}
	if len(b.Projects) != 1 {
		t.Fatalf("%q: %d projects, want 1", ref, len(b.Projects))
	}
	return b.Projects[0].Key, nil
}

func TestResolveProject_ByName(t *testing.T) {
	env := openTestEnv(t) // BMB "Test"
	addProject(t, env, "BEE", "BeeMemoryBank")
	addProject(t, env, "BEX", "Bee Extras")
	addProject(t, env, "KAN", "Kanban")
	addProject(t, env, "KANB", "Kanban Board")

	for ref, want := range map[string]string{
		"bmb":           "BMB", // the key still wins
		"BeeMemoryBank": "BEE", // exact name, any case
		"beememorybank": "BEE",
		"extras":        "BEX", // a unique part of a name
		"kanban":        "KAN", // an exact name beats a longer one containing it
	} {
		got, derr := boardKey(t, env, env.actor, ref)
		if derr != nil || got != want {
			t.Errorf("%q -> %q, %v; want %s", ref, got, derr, want)
		}
	}

	// Two names contain "be" (and no key is BE): refuse, and name both.
	_, derr := boardKey(t, env, env.actor, "be")
	if derr == nil || derr.Code != domain.CodeValidation ||
		!strings.Contains(derr.Message, "BEE (BeeMemoryBank)") || !strings.Contains(derr.Message, "BEX (Bee Extras)") {
		t.Errorf("ambiguous name: %+v, want validation naming both projects", derr)
	}

	// A miss lists what does exist.
	_, derr = boardKey(t, env, env.actor, "nothing like it")
	if derr == nil || derr.Code != domain.CodeNotFound || !strings.Contains(derr.Remediation, "BEE (BeeMemoryBank)") {
		t.Errorf("miss: %+v, want not_found listing the projects", derr)
	}

	// A token scoped to BMB finds BMB by name, and cannot learn anything
	// about a project outside its scope by guessing names.
	scoped := Actor{Name: "scoped", Scopes: domain.Scopes{domain.ScopeRead}, ProjectKeys: []string{"BMB"}}
	if got, derr := boardKey(t, env, scoped, "test"); derr != nil || got != "BMB" {
		t.Errorf("scoped token by its own project's name: %q, %v", got, derr)
	}
	_, derr = boardKey(t, env, scoped, "BeeMemoryBank")
	if derr == nil || derr.Code != domain.CodeForbidden {
		t.Errorf("scoped token by a foreign name: %+v, want forbidden", derr)
	}
	if derr != nil && strings.Contains(derr.Remediation+derr.Message, "BEE") {
		t.Errorf("the refusal leaks a project outside the token's scope: %+v", derr)
	}
}

func TestBoardGet_UnknownColumnIsAnErrorListingTheRealOnes(t *testing.T) {
	env := openTestEnv(t)
	_, err := env.svc.BoardGet(context.Background(), env.actor, BoardGetInput{
		ProjectKey: "BMB", Filter: BoardFilter{Columns: []string{"Doing", "In progress"}},
	})
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation {
		t.Fatalf("err = %v, want validation", err)
	}
	if !strings.Contains(de.Message, `"In progress"`) || strings.Contains(de.Message, `"Doing"`) {
		t.Errorf("message %q must name exactly the unknown column", de.Message)
	}
	for _, c := range []string{"Backlog", "Doing", "Done"} {
		if !strings.Contains(de.Remediation, c) {
			t.Errorf("remediation %q does not list column %s", de.Remediation, c)
		}
	}
	// Names are matched case-insensitively, as the filter always has.
	if _, err := env.svc.BoardGet(context.Background(), env.actor, BoardGetInput{
		ProjectKey: "BMB", Filter: BoardFilter{Columns: []string{"doing"}},
	}); err != nil {
		t.Errorf("a real column in another case was refused: %v", err)
	}
}

func TestBoardGet_ColumnKindFilter(t *testing.T) {
	env := openTestEnv(t)
	makeBacklogTask(t, env, "waits")
	started := makeBacklogTask(t, env, "runs")
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{env.moveTo(t, started.Key, "Doing")},
	}); err != nil {
		t.Fatal(err)
	}

	b, err := env.svc.BoardGet(context.Background(), env.actor, BoardGetInput{
		ProjectKey: "BMB", Filter: BoardFilter{ColumnKinds: []domain.Kind{domain.KindActive}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range b.Projects[0].Columns {
		wantTasks := c.Kind == domain.KindActive
		if (len(c.Tasks) > 0) != wantTasks {
			t.Errorf("column %s (%s) has %d tasks with filter active", c.Name, c.Kind, len(c.Tasks))
		}
	}

	_, err = env.svc.BoardGet(context.Background(), env.actor, BoardGetInput{
		ProjectKey: "BMB", Filter: BoardFilter{ColumnKinds: []domain.Kind{"in-progress"}},
	})
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation {
		t.Errorf("unknown kind: %v, want validation", err)
	}
}

func TestTaskCreate_OutcomeConclusionBlocks(t *testing.T) {
	env := openTestEnv(t)
	existing := makeBacklogTask(t, env, "downstream")
	before := existing.Version

	holds := domain.OutcomeHolds
	res, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{Tasks: []NewTask{
		{
			ProjectKey: "BMB", Title: "found and fixed on the way", Type: domain.TypeBug, Ref: "a",
			Column: "Done", Outcome: &holds, Conclusion: "the race was in the checkpoint",
			Blocks: []string{existing.Key, "@b"},
		},
		// The same edge stated from the other end must not be added twice.
		{ProjectKey: "BMB", Title: "follow-up", Type: domain.TypeTask, Ref: "b", BlockedBy: []string{"@a"}},
	}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a, b := res.Tasks[0], res.Tasks[1]
	if a.Outcome != domain.OutcomeHolds || a.Conclusion != "the race was in the checkpoint" {
		t.Errorf("outcome/conclusion = %q/%q", a.Outcome, a.Conclusion)
	}
	if b.Outcome != domain.OutcomeOpen {
		t.Errorf("default outcome = %q, want open", b.Outcome)
	}

	got, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{a.Key, existing.Key, b.Key}})
	if err != nil {
		t.Fatal(err)
	}
	av, xv, bv := got.Tasks[0], got.Tasks[1], got.Tasks[2]
	if strings.Join(av.Blocks, ",") != strings.Join(sortedKeys(existing.Key, b.Key), ",") {
		t.Errorf("a.Blocks = %v, want %s and %s", av.Blocks, existing.Key, b.Key)
	}
	// a is done, so it no longer holds anything up: BlockedBy lists OPEN
	// blockers only. The edge itself is what must exist.
	if len(xv.BlockedBy) != 0 || len(bv.BlockedBy) != 0 {
		t.Errorf("a done blocker still reported open: x=%v b=%v", xv.BlockedBy, bv.BlockedBy)
	}
	if xv.Version == before {
		t.Errorf("the existing task's version did not move when it gained a blocker (task_link moves it)")
	}

	bad := domain.Outcome("maybe")
	_, err = env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{Tasks: []NewTask{
		{ProjectKey: "BMB", Title: "x", Type: domain.TypeTask, Outcome: &bad},
	}})
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation || de.Field != "outcome" {
		t.Errorf("invalid outcome: %v, want validation on outcome", err)
	}
	_, err = env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{Tasks: []NewTask{
		{ProjectKey: "BMB", Title: "x", Type: domain.TypeTask, Ref: "self", Blocks: []string{"@self"}},
	}})
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation {
		t.Errorf("self-block: %v, want validation", err)
	}
}

func sortedKeys(a, b string) []string {
	if a < b {
		return []string{a, b}
	}
	return []string{b, a}
}
