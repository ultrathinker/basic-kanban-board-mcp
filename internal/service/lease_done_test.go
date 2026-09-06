package service

import (
	"context"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// Tests for the F3 invariant: a live lease never survives crossing the done
// boundary, in either direction, on any path that moves a task between
// columns. The regression that motivated this: an agent finished its work,
// moved tasks to Done, and the board kept phantom claims for the full TTL —
// and anyone reopening the task inherited an invisible hour-long lock.

// startTask claims a backlog task into Doing via task_next(start), the way a
// real agent begins work.
func startTask(t *testing.T, env *testEnv, key string) {
	t.Helper()
	res, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
		ProjectKey: env.proj.Key,
		Action:     NextStart,
		Limit:      1,
	})
	if err != nil {
		t.Fatalf("start %s: %v", key, err)
	}
	if res.StartedKey != key {
		t.Fatalf("start took %q, want %q", res.StartedKey, key)
	}
}

// freshView re-reads one task through the service.
func freshView(t *testing.T, env *testEnv, key string) domain.TaskView {
	t.Helper()
	got, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{key}})
	if err != nil || len(got.Tasks) != 1 {
		t.Fatalf("task_get %s: %v / %d tasks", key, err, len(got.Tasks))
	}
	return got.Tasks[0]
}

// assertNoLease fails unless the view carries no trace of a lease.
func assertNoLease(t *testing.T, label string, tv domain.TaskView) {
	t.Helper()
	if tv.ClaimedBy != nil {
		t.Fatalf("%s: ClaimedBy = %q, want nil", label, *tv.ClaimedBy)
	}
	if tv.ClaimExpiresAt != nil {
		t.Fatalf("%s: ClaimExpiresAt = %v, want nil", label, tv.ClaimExpiresAt)
	}
	if tv.LeaseRemain != nil {
		t.Fatalf("%s: LeaseRemain = %v, want nil", label, tv.LeaseRemain)
	}
}

// TestTaskUpdate_MoveIntoDoneReleasesLease is the headline F3 case: a task
// claimed by one actor, closed by another, must land in Done with no lease —
// and the closer is still attributable through updated_by, not claimed_by.
func TestTaskUpdate_MoveIntoDoneReleasesLease(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "build the thing")
	startTask(t, env, task.Key)

	closer := env.actorWith(t, nonAdmin)
	closer.Name = "codex"
	res, err := env.svc.TaskUpdate(context.Background(), closer, TaskUpdateInput{
		Patches: []TaskPatch{{Key: task.Key, IfVersion: intPtrLocal(freshView(t, env, task.Key).Version), Column: "Done"}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.Items[0].OK {
		t.Fatalf("move to Done failed: %+v", res.Items[0].Err)
	}

	// The per-item view must be truthful too — agents plan their next
	// if_version from it.
	assertNoLease(t, "returned view", *res.Items[0].Task)

	fresh := freshView(t, env, task.Key)
	if fresh.ColumnName != "Done" {
		t.Fatalf("ColumnName = %q, want Done", fresh.ColumnName)
	}
	if fresh.DoneAt == nil {
		t.Fatalf("DoneAt is nil after moving into a done-kind column")
	}
	assertNoLease(t, "stored row", fresh)
	if fresh.UpdatedBy != closer.Name {
		t.Fatalf("UpdatedBy = %q, want %q (who closed it is recorded without a lease)",
			fresh.UpdatedBy, closer.Name)
	}

	// The returned view's version must match the stored row: a follow-up
	// edit using it as if_version must not conflict.
	followUp, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: task.Key, IfVersion: intPtrLocal(res.Items[0].Task.Version), Title: strPtrLocal("post-close note")}},
	})
	if err != nil {
		t.Fatalf("follow-up update: %v", err)
	}
	if !followUp.Items[0].OK {
		t.Fatalf("follow-up update with returned version failed: %+v", followUp.Items[0].Err)
	}
}

// TestTaskUpdate_MoveOutOfDoneArrivesUnclaimedForNext covers the reverse
// direction: a task reopened out of Done must be offered to a DIFFERENT
// actor by task_next straight away, not skipped as claimed_by_other until
// the stale TTL expires.
func TestTaskUpdate_MoveOutOfDoneArrivesUnclaimedForNext(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "reopen me")
	startTask(t, env, task.Key)
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: task.Key, IfVersion: intPtrLocal(freshView(t, env, task.Key).Version), Column: "Done"}},
	}); err != nil {
		t.Fatalf("move to Done: %v", err)
	}
	assertNoLease(t, "in Done", freshView(t, env, task.Key))

	out, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: task.Key, IfVersion: intPtrLocal(freshView(t, env, task.Key).Version), Column: "Backlog"}},
	})
	if err != nil {
		t.Fatalf("move out of Done: %v", err)
	}
	if !out.Items[0].OK {
		t.Fatalf("move out of Done failed: %+v", out.Items[0].Err)
	}
	assertNoLease(t, "back in Backlog", freshView(t, env, task.Key))

	other := env.actorWith(t, nonAdmin)
	other.Name = "codex"
	next, err := env.svc.TaskNext(context.Background(), other, TaskNextInput{
		ProjectKey: env.proj.Key,
		Action:     NextClaim,
		Limit:      3,
	})
	if err != nil {
		t.Fatalf("task_next claim: %v", err)
	}
	if next.ClaimedKey != task.Key {
		t.Fatalf("task_next claimed %q, want %q (reopened task must be immediately available)", next.ClaimedKey, task.Key)
	}
	if next.Reasons.ClaimedByOther != 0 {
		t.Fatalf("Reasons.ClaimedByOther = %d, want 0", next.Reasons.ClaimedByOther)
	}
}

// TestTaskUpdate_MoveWithinActiveColumnsKeepsLease guards against
// over-release: the lease means "I am working on this", and moving one's own
// claimed task between active columns (Doing -> Review) is still work.
func TestTaskUpdate_MoveWithinActiveColumnsKeepsLease(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "still working")
	startTask(t, env, task.Key)

	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: task.Key, IfVersion: intPtrLocal(freshView(t, env, task.Key).Version), Column: "Review"}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.Items[0].OK {
		t.Fatalf("move to Review failed: %+v", res.Items[0].Err)
	}
	fresh := freshView(t, env, task.Key)
	if fresh.ColumnName != "Review" {
		t.Fatalf("ColumnName = %q, want Review", fresh.ColumnName)
	}
	if fresh.ClaimedBy == nil || *fresh.ClaimedBy != env.actor.Name {
		t.Fatalf("ClaimedBy = %v, want %q (active-to-active moves keep the lease)", fresh.ClaimedBy, env.actor.Name)
	}
}

// TestTaskUpdate_CombinedMoveAndEditSucceeds pins the adjacent defect found
// while fixing F3: Move bumps the row's version itself, so re-checking the
// caller's if_version in Update turned every move+edit patch into a spurious
// conflict — reported as failed while the move had already been applied.
func TestTaskUpdate_CombinedMoveAndEditSucceeds(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "x")
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key: task.Key, IfVersion: intPtrLocal(task.Version),
			Column: "Done", Title: strPtrLocal("renamed on close"),
		}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.Items[0].OK {
		t.Fatalf("combined move+edit failed: %+v", res.Items[0].Err)
	}
	fresh := freshView(t, env, task.Key)
	if fresh.Title != "renamed on close" || fresh.ColumnName != "Done" {
		t.Fatalf("title=%q column=%q, want both applied", fresh.Title, fresh.ColumnName)
	}

	// Same shape under Atomic=true must not roll back either.
	task2 := makeBacklogTask(t, env, "y")
	atomicRes, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key: task2.Key, IfVersion: intPtrLocal(task2.Version),
			Column: "Done", Title: strPtrLocal("renamed too"),
		}},
		Atomic: true,
	})
	if err != nil {
		t.Fatalf("atomic update: %v", err)
	}
	if !atomicRes.Items[0].OK {
		t.Fatalf("atomic combined move+edit failed: %+v", atomicRes.Items[0].Err)
	}
}

// boardHasTask reports whether the task is visible anywhere on the board.
func boardHasTask(t *testing.T, env *testEnv, key string) bool {
	t.Helper()
	board, err := env.svc.BoardGet(context.Background(), env.actor, BoardGetInput{
		ProjectKey: env.proj.Key,
		View:       ViewTasks,
		DoneLimit:  50,
	})
	if err != nil {
		t.Fatalf("board_get: %v", err)
	}
	for _, bp := range board.Projects {
		for _, col := range bp.Columns {
			for _, tv := range col.Tasks {
				if tv.Key == key {
					return true
				}
			}
		}
	}
	return false
}

// TestTaskRemove_ArchiveStillBehavesAsBefore is the regression guard the F3
// brief asks for: the done-boundary rule must not have disturbed TaskRemove's
// archive path, whose force-release is the precedent the rule follows.
//
// The archive path carries a PRE-EXISTING store-layer defect outside this
// chunk (reported, not fixed here): Tasks().Archive binds a raw time.Time,
// so the post-archive re-read cannot parse archived_at, and AsError swallows
// the raw error — the item reports neither OK nor an error. Worse, restore
// fails at its first read, so it is a silent no-op. This test pins today's
// observable behaviour exactly: the archive's writes (force-release first,
// then archive) DO commit — the task leaves the board — while the response
// is the broken shape above. When the store bug is fixed, this test flips
// and should be tightened to assert the released lease directly.
func TestTaskRemove_ArchiveStillBehavesAsBefore(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "archive me")
	startTask(t, env, task.Key)

	arch, err := env.svc.TaskRemove(context.Background(), env.actor, TaskRemoveInput{
		Items: []RemoveItem{{Key: task.Key}},
	})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if arch.Items[0].OK || arch.Items[0].Err != nil {
		t.Fatalf("archive item shape changed by F3: OK=%v Err=%+v (pre-existing store defect pins OK=false, Err=nil)",
			arch.Items[0].OK, arch.Items[0].Err)
	}
	if boardHasTask(t, env, task.Key) {
		t.Fatalf("task still on the board after archive; the archive transaction did not commit")
	}

	// Restore is a pre-existing silent no-op for the same store reason: it
	// fails before writing anything, so the task must remain archived.
	rst, err := env.svc.TaskRemove(context.Background(), env.actor, TaskRemoveInput{
		Items:   []RemoveItem{{Key: task.Key}},
		Restore: true,
	})
	if err != nil {
		t.Fatalf("restore call: %v", err)
	}
	if rst.Items[0].OK || rst.Items[0].Err != nil {
		t.Fatalf("restore item shape changed by F3: OK=%v Err=%+v", rst.Items[0].OK, rst.Items[0].Err)
	}
	if boardHasTask(t, env, task.Key) {
		t.Fatalf("task back on the board after a restore that cannot have run")
	}
}

// projectVersion reads the seeded project's current version for an upsert.
func projectVersion(t *testing.T, env *testEnv) int {
	t.Helper()
	var v int
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		p, err := env.Projects().GetByKey(tx, env.proj.Key)
		if err != nil {
			return err
		}
		v = p.Version
		return nil
	}); err != nil {
		t.Fatalf("read project version: %v", err)
	}
	return v
}

// specsKeeping builds the full desired column list, minus the column being
// removed, preserving each column's kind and WIP limit.
func specsKeeping(removed string) []ColumnSpec {
	var out []ColumnSpec
	for _, def := range domain.DefaultColumns {
		if def.Name == removed {
			continue
		}
		out = append(out, ColumnSpec{Name: def.Name, Kind: def.Kind, WIPLimit: def.WIPLimit})
	}
	return out
}

// TestProjectUpsert_HerdIntoDoneReleasesLease: remove_columns herds every
// task (claimed ones included) into the destination column; landing in Done
// must end the lease just like an explicit move.
func TestProjectUpsert_HerdIntoDoneReleasesLease(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "herd me")
	if _, err := env.svc.TaskClaim(context.Background(), env.actor, TaskClaimInput{
		Key: task.Key, Action: ClaimTake,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	res, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode:          UpsertUpdate,
		Key:           env.proj.Key,
		IfVersion:     intPtrLocal(projectVersion(t, env)),
		Columns:       specsKeeping("Backlog"),
		RemoveColumns: []RemoveColumn{{Name: "Backlog", MoveTasksTo: "Done"}},
	})
	if err != nil {
		t.Fatalf("project_upsert: %v", err)
	}
	if res.Project.Key != env.proj.Key {
		t.Fatalf("project key = %q", res.Project.Key)
	}
	fresh := freshView(t, env, task.Key)
	if fresh.ColumnName != "Done" {
		t.Fatalf("ColumnName = %q, want Done after herding", fresh.ColumnName)
	}
	assertNoLease(t, "herded into Done", fresh)
}

// TestProjectUpsert_HerdOutOfDoneReleasesLease: removing a done column must
// not carry live leases into the active column either. A claimed task in a
// done column cannot be produced through the service any more, so the lease
// is planted directly — standing in for data written before this fix.
func TestProjectUpsert_HerdOutOfDoneReleasesLease(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "pre-fix leftover")
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: task.Key, IfVersion: intPtrLocal(task.Version), Column: "Done"}},
	}); err != nil {
		t.Fatalf("move to Done: %v", err)
	}
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		_, _, err := env.Tasks().Claim(tx, task.ID, "ghost-from-before-the-fix", time.Hour)
		return err
	}); err != nil {
		t.Fatalf("plant lease: %v", err)
	}

	if _, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode:          UpsertUpdate,
		Key:           env.proj.Key,
		IfVersion:     intPtrLocal(projectVersion(t, env)),
		Columns:       specsKeeping("Done"),
		RemoveColumns: []RemoveColumn{{Name: "Done", MoveTasksTo: "Review"}},
	}); err != nil {
		t.Fatalf("project_upsert: %v", err)
	}
	fresh := freshView(t, env, task.Key)
	if fresh.ColumnName != "Review" {
		t.Fatalf("ColumnName = %q, want Review after herding", fresh.ColumnName)
	}
	assertNoLease(t, "herded out of Done", fresh)
}
