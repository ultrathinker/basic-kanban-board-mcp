package store

import (
	"context"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// CountsByTask: the batched per-(task, assessor) mark count the delete-track
// control's confirmation ("how many history points will be lost") needs for
// every card the board renders, in one query regardless of how many cards
// there are — the same batching contract LatestByTask already promises.
// ---------------------------------------------------------------------------

func TestProgressMarks_CountsByTask(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	taskA := seedTask(t, ts, p, cols["Backlog"], "A", "tester")
	taskB := seedTask(t, ts, p, cols["Backlog"], "B", "tester")
	taskC := seedTask(t, ts, p, cols["Backlog"], "C", "tester") // no marks on purpose

	addMark(t, ts, &domain.ProgressMark{ID: "pm-a1", ProjectID: p.ID, TaskID: &taskA.ID, Assessor: "alpha", Percent: 10, CreatedAt: progressBase})
	addMark(t, ts, &domain.ProgressMark{ID: "pm-a2", ProjectID: p.ID, TaskID: &taskA.ID, Assessor: "alpha", Percent: 50, CreatedAt: progressBase.Add(time.Second)})
	addMark(t, ts, &domain.ProgressMark{ID: "pm-a3", ProjectID: p.ID, TaskID: &taskA.ID, Assessor: "beta", Percent: 60, CreatedAt: progressBase.Add(2 * time.Second)})
	addMark(t, ts, &domain.ProgressMark{ID: "pm-b1", ProjectID: p.ID, TaskID: &taskB.ID, Assessor: "alpha", Percent: 80, CreatedAt: progressBase.Add(3 * time.Second)})
	// Project-level mark (task_id IS NULL) must never leak into a per-task count.
	addMark(t, ts, &domain.ProgressMark{ID: "pm-p1", ProjectID: p.ID, Assessor: "gamma", Percent: 15})
	// Another project's marks must not leak across the project boundary either.
	other := &domain.Project{
		ID: "other-project-counts", Key: "OTHERC", Name: "Other",
		Version: 1, NextTaskSeq: 1, EstimateUnit: "h",
		EnforceDependencies: true, StrictDone: false, ClaimTTLSeconds: 3600,
	}
	if err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Projects().Create(tx, other)
	}); err != nil {
		t.Fatalf("seed other project: %v", err)
	}
	addMark(t, ts, &domain.ProgressMark{ID: "pm-o1", ProjectID: other.ID, TaskID: &taskA.ID, Assessor: "ghost", Percent: 1})

	readCounts := func() map[string]map[string]int {
		t.Helper()
		var got map[string]map[string]int
		if err := ts.Write(context.Background(), func(tx Tx) error {
			var err error
			got, err = ts.Progress().CountsByTask(tx, p.ID)
			return err
		}); err != nil {
			t.Fatalf("CountsByTask: %v", err)
		}
		return got
	}

	got := readCounts()
	if len(got) != 2 {
		t.Fatalf("tasks with counts = %d, want 2 (task C has no marks)", len(got))
	}
	if got[taskA.ID]["alpha"] != 2 || got[taskA.ID]["beta"] != 1 {
		t.Fatalf("task A counts = %v, want alpha=2 beta=1", got[taskA.ID])
	}
	if len(got[taskB.ID]) != 1 || got[taskB.ID]["alpha"] != 1 {
		t.Fatalf("task B counts = %v, want alpha=1 only", got[taskB.ID])
	}
	if _, ok := got[taskC.ID]; ok {
		t.Fatalf("task C has no marks but appears in counts: %v", got[taskC.ID])
	}
	if _, ok := got[""]; ok {
		t.Fatal("project-level marks leaked into the per-task counts under a \"\" key")
	}

	// Deleting alpha's track on task A drops alpha from the next read and
	// leaves beta (same task) and alpha-on-task-B untouched.
	if removed := deleteTrack(t, ts, p.ID, &taskA.ID, "alpha"); removed != 2 {
		t.Fatalf("deleteTrack removed %d rows, want 2", removed)
	}
	got = readCounts()
	if _, ok := got[taskA.ID]["alpha"]; ok {
		t.Fatalf("alpha's count on task A survived the delete: %v", got[taskA.ID])
	}
	if got[taskA.ID]["beta"] != 1 {
		t.Fatalf("beta's count on task A changed after deleting alpha: %v", got[taskA.ID])
	}
	if got[taskB.ID]["alpha"] != 1 {
		t.Fatalf("task B's alpha count changed after deleting task A's alpha track: %v", got[taskB.ID])
	}
}

func TestProgressMarks_CountsByTask_EmptyProjectID(t *testing.T) {
	ts := openTestStore(t)
	if err := ts.Write(context.Background(), func(tx Tx) error {
		_, err := ts.Progress().CountsByTask(tx, "")
		return err
	}); err == nil {
		t.Fatal("CountsByTask(\"\"): want an error, got nil")
	}
}
