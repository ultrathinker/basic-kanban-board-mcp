package service

import (
	"context"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// addWaitingColumn adds a KindWaiting column to env's project through the
// real ProjectUpsert path (Mode update), carrying the three default columns
// forward unchanged. Exercising the actual service call — not a direct
// store write — is the point: it proves validateColumnSpecs accepts the new
// kind end to end, and that the pre-existing Backlog/Doing/Done columns are
// not touched by the call (KANB-52's "existing projects unaffected").
func addWaitingColumn(t *testing.T, env *testEnv) *domain.Column {
	t.Helper()
	wipLimit := 3
	res, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode:      UpsertUpdate,
		Key:       env.proj.Key,
		IfVersion: &env.proj.Version,
		Columns: []ColumnSpec{
			{Name: "Backlog", Kind: domain.KindBacklog},
			{Name: "Doing", Kind: domain.KindActive, WIPLimit: &wipLimit},
			{Name: "Done", Kind: domain.KindDone},
			{Name: "Waiting", Kind: domain.KindWaiting},
		},
	})
	if err != nil {
		t.Fatalf("add Waiting column: %v", err)
	}
	env.proj.Version = res.Project.Version

	var backlog, doing, done, waiting *domain.Column
	for i := range res.Columns {
		c := res.Columns[i]
		switch c.Name {
		case "Backlog":
			backlog = &c
		case "Doing":
			doing = &c
		case "Done":
			done = &c
		case "Waiting":
			waiting = &c
		}
	}
	if backlog == nil || backlog.Kind != domain.KindBacklog {
		t.Fatalf("Backlog column missing or re-kinded: %+v", backlog)
	}
	if doing == nil || doing.Kind != domain.KindActive {
		t.Fatalf("Doing column missing or re-kinded: %+v", doing)
	}
	if done == nil || done.Kind != domain.KindDone {
		t.Fatalf("Done column missing or re-kinded: %+v", done)
	}
	if waiting == nil || waiting.Kind != domain.KindWaiting {
		t.Fatalf("Waiting column missing or wrong kind: %+v", waiting)
	}
	env.cols["Waiting"] = waiting
	env.cols["Doing"] = doing
	return waiting
}

// TestTaskUpdate_MoveToWaitingAllowsOpenDependencyButDoingRefuses is
// KANB-52's mandatory paired test at the full service level, mirroring
// domain's TestCheckMove_WaitingAllowsOpenDependencyButActiveRefuses but
// exercised through the real write path (TaskUpdate -> CheckMove), the way
// an MCP caller would actually trigger it. Same task, same open blocker:
// -> Waiting must succeed, -> Doing must still refuse.
func TestTaskUpdate_MoveToWaitingAllowsOpenDependencyButDoingRefuses(t *testing.T) {
	env := openTestEnv(t)
	addWaitingColumn(t, env)

	blocker := makeBacklogTask(t, env, "blocker")
	blocked := makeBacklogTask(t, env, "blocked")
	if _, err := env.svc.TaskLink(context.Background(), env.actor, TaskLinkInput{
		Add: []LinkPair{{Blocker: blocker.Key, Blocked: blocked.Key}},
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	refreshed, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{blocked.Key}})
	if err != nil || len(refreshed.Tasks) != 1 {
		t.Fatalf("refresh: %v / %d", err, len(refreshed.Tasks))
	}
	if len(refreshed.Tasks[0].BlockedBy) == 0 {
		t.Fatalf("fixture is broken: %s has no open blocker to test against", blocked.Key)
	}
	v := refreshed.Tasks[0].Version

	// Half 1: the destination the column exists for must be allowed.
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: blocked.Key, IfVersion: &v, Column: "Waiting"}},
	})
	if err != nil {
		t.Fatalf("move to Waiting: %v", err)
	}
	if !res.Items[0].OK {
		t.Fatalf("move to Waiting with an open dependency refused: %v, want allowed", res.Items[0].Err)
	}
	v = res.Items[0].Task.Version

	// Half 2: an ordinary active column must still refuse, on the SAME
	// task and the SAME still-open blocker.
	res, err = env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: blocked.Key, IfVersion: &v, Column: "Doing"}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.Items[0].OK {
		t.Fatalf("move to Doing with an open dependency succeeded; want blocked")
	}
	if res.Items[0].Err == nil || res.Items[0].Err.Code != domain.CodeBlocked {
		t.Fatalf("code = %v, want blocked", res.Items[0].Err)
	}
}

// TestWaitingColumn_TaskNextIgnoresIt is KANB-52 criterion 2: task_next must
// never offer a task sitting in a waiting column. It only ever gathers
// candidates from backlog-kind columns (see nextForProject), so a task
// parked in Waiting is structurally excluded — this pins that behaviour
// with a real peek call rather than trusting the source reading.
func TestWaitingColumn_TaskNextIgnoresIt(t *testing.T) {
	env := openTestEnv(t)
	addWaitingColumn(t, env)

	parked := makeBacklogTask(t, env, "parked")
	ready := makeBacklogTask(t, env, "ready")

	refreshed, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{parked.Key}})
	if err != nil || len(refreshed.Tasks) != 1 {
		t.Fatalf("refresh: %v / %d", err, len(refreshed.Tasks))
	}
	v := refreshed.Tasks[0].Version
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: parked.Key, IfVersion: &v, Column: "Waiting"}},
	}); err != nil {
		t.Fatalf("move to Waiting: %v", err)
	}

	out, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
		ProjectKey: env.proj.Key,
		Action:     NextPeek,
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("task_next: %v", err)
	}
	for _, tv := range out.Tasks {
		if tv.Key == parked.Key {
			t.Fatalf("task_next offered %s, which is parked in Waiting", parked.Key)
		}
	}
	sawReady := false
	for _, tv := range out.Tasks {
		if tv.Key == ready.Key {
			sawReady = true
		}
	}
	if !sawReady {
		t.Fatalf("task_next did not offer %s, the one genuinely ready backlog task", ready.Key)
	}
}

// TestWaitingColumn_DoesNotUnblockDependents is KANB-52 criterion 3: moving
// a BLOCKER into Waiting must not free whatever it blocks — only reaching a
// done-kind column does that. Waiting is deliberately not done-kind, so the
// existing "blocker is open unless its column is done" rule already covers
// this; the test pins it so a future change to that rule cannot silently
// make Waiting count as a finish.
func TestWaitingColumn_DoesNotUnblockDependents(t *testing.T) {
	env := openTestEnv(t)
	addWaitingColumn(t, env)

	blocker := makeBacklogTask(t, env, "blocker")
	blocked := makeBacklogTask(t, env, "blocked")
	if _, err := env.svc.TaskLink(context.Background(), env.actor, TaskLinkInput{
		Add: []LinkPair{{Blocker: blocker.Key, Blocked: blocked.Key}},
	}); err != nil {
		t.Fatalf("link: %v", err)
	}

	refreshedBlocker, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{blocker.Key}})
	if err != nil || len(refreshedBlocker.Tasks) != 1 {
		t.Fatalf("refresh blocker: %v / %d", err, len(refreshedBlocker.Tasks))
	}
	bv := refreshedBlocker.Tasks[0].Version
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: blocker.Key, IfVersion: &bv, Column: "Waiting"}},
	}); err != nil {
		t.Fatalf("move blocker to Waiting: %v", err)
	}

	refreshedBlocked, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{blocked.Key}})
	if err != nil || len(refreshedBlocked.Tasks) != 1 {
		t.Fatalf("refresh blocked: %v / %d", err, len(refreshedBlocked.Tasks))
	}
	if len(refreshedBlocked.Tasks[0].BlockedBy) == 0 {
		t.Fatalf("%s shows no open blocker after %s parked in Waiting; Waiting must not act like Done", blocked.Key, blocker.Key)
	}

	bv2 := refreshedBlocked.Tasks[0].Version
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: blocked.Key, IfVersion: &bv2, Column: "Doing"}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.Items[0].OK {
		t.Fatalf("%s moved to Doing while its blocker only sits in Waiting; want still blocked", blocked.Key)
	}
	if res.Items[0].Err == nil || res.Items[0].Err.Code != domain.CodeBlocked {
		t.Fatalf("code = %v, want blocked", res.Items[0].Err)
	}
}

// TestWaitingColumn_DoesNotOccupyActiveWIP is KANB-52 criterion 1's WIP
// half: a task parked in Waiting must not count against Doing's WIP limit.
// Doing's limit here is 3 (see addWaitingColumn); three tasks fill it, and a
// fourth parked in Waiting alongside them must not stop a later task from
// entering Doing once room frees up — this test only needs to show Doing's
// own occupant count is unaffected by what sits in Waiting.
func TestWaitingColumn_DoesNotOccupyActiveWIP(t *testing.T) {
	env := openTestEnv(t)
	addWaitingColumn(t, env)

	parked := makeBacklogTask(t, env, "parked")
	refreshed, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{parked.Key}})
	if err != nil || len(refreshed.Tasks) != 1 {
		t.Fatalf("refresh: %v / %d", err, len(refreshed.Tasks))
	}
	v := refreshed.Tasks[0].Version
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: parked.Key, IfVersion: &v, Column: "Waiting"}},
	}); err != nil {
		t.Fatalf("move to Waiting: %v", err)
	}

	var count int
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		var err error
		count, err = env.Columns().CountTasks(tx, env.cols["Doing"].ID, "")
		return err
	}); err != nil {
		t.Fatalf("count Doing: %v", err)
	}
	if count != 0 {
		t.Fatalf("Doing occupant count = %d after parking a task in Waiting, want 0 (Waiting must not occupy active WIP)", count)
	}
}
