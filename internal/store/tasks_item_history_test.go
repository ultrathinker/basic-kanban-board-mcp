package store

import (
	"context"
	"testing"
	"time"
)

// ItemHistory feeds the board's item-count chart: how many tasks the project
// held over time, and how many were still open. Two exclusions are
// load-bearing and neither is visible in the returned numbers, so they need
// their own assertions — a leak would simply inflate both curves and nobody
// would know which reading was wrong.

func readItemHistory(t *testing.T, ts *testStore, projectID string) []ItemLifespan {
	t.Helper()
	var got []ItemLifespan
	if err := ts.Read(context.Background(), func(tx Tx) error {
		var err error
		got, err = ts.Tasks().ItemHistory(tx, projectID)
		return err
	}); err != nil {
		t.Fatalf("ItemHistory: %v", err)
	}
	return got
}

func TestTasks_ItemHistory_ExcludesArchivedAndOtherProjects(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	live1 := seedTask(t, ts, p, cols["Backlog"], "live one", "tester")
	live2 := seedTask(t, ts, p, cols["Backlog"], "live two", "tester")
	archived := seedTask(t, ts, p, cols["Backlog"], "archived", "tester")

	// Archive one task directly: the chart must count the project the way
	// the board's own "tasks done" metric does, and that metric drops
	// archived tasks from BOTH sides of its fraction. If they leaked in
	// here, the total curve would count work the board itself does not.
	if err := ts.Write(context.Background(), func(tx Tx) error {
		tw := tx.(*txWrap)
		_, err := tw.tx.ExecContext(tw.ctx(),
			"UPDATE tasks SET archived_at = ? WHERE id = ?", formatTime(time.Now().UTC()), archived.ID)
		return err
	}); err != nil {
		t.Fatalf("archive task: %v", err)
	}

	// A second project's tasks must not cross the boundary either.
	other, otherCols := seedProjectKey(t, ts, "OTHERI")
	seedTask(t, ts, other, otherCols["Backlog"], "not ours", "tester")

	got := readItemHistory(t, ts, p.ID)
	if len(got) != 2 {
		t.Fatalf("ItemHistory returned %d tasks, want 2 (archived and other-project tasks must not count): %+v", len(got), got)
	}
	_ = live1
	_ = live2
	for _, sp := range got {
		if sp.DoneAt != nil {
			t.Errorf("a backlog task reported a completion time: %+v", sp)
		}
	}
}

// TestTasks_ItemHistory_ReportsCompletionAndOrdersOldestFirst: done_at is the
// only signal the open curve has, and the caller replays the rows in order.
func TestTasks_ItemHistory_ReportsCompletionAndOrdersOldestFirst(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	first := seedTask(t, ts, p, cols["Backlog"], "first", "tester")
	second := seedTask(t, ts, p, cols["Backlog"], "second", "tester")

	// Move one into the done column through the real Move, which is what
	// stamps done_at — not a hand-written UPDATE, so this also pins that
	// the chart's data source and the board's own rule are the same one.
	if err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Tasks().Move(tx, second.ID, cols["Done"].ID, 1000, "tester")
	}); err != nil {
		t.Fatalf("move to done: %v", err)
	}

	got := readItemHistory(t, ts, p.ID)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].CreatedAt.After(got[1].CreatedAt) {
		t.Errorf("rows are not oldest-first: %v then %v", got[0].CreatedAt, got[1].CreatedAt)
	}

	var done, open int
	for _, sp := range got {
		if sp.DoneAt != nil {
			done++
			if sp.DoneAt.Before(sp.CreatedAt) {
				t.Errorf("a task reported being finished before it existed: %+v", sp)
			}
		} else {
			open++
		}
	}
	if done != 1 || open != 1 {
		t.Errorf("done=%d open=%d, want 1 and 1", done, open)
	}

	// Moving it back out of the done column must clear the completion: a
	// reopened task is open again, and a curve that never recovers would
	// report work as finished that is not.
	if err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Tasks().Move(tx, second.ID, cols["Backlog"].ID, 500, "tester")
	}); err != nil {
		t.Fatalf("move back: %v", err)
	}
	got = readItemHistory(t, ts, p.ID)
	for _, sp := range got {
		if sp.DoneAt != nil {
			t.Errorf("a task moved out of the done column still reports a completion: %+v", sp)
		}
	}
	_ = first
}

func TestTasks_ItemHistory_EmptyProjectAndBadInput(t *testing.T) {
	ts := openTestStore(t)
	p, _ := seedProject(t, ts)

	if got := readItemHistory(t, ts, p.ID); len(got) != 0 {
		t.Errorf("a project with no tasks returned %d rows", len(got))
	}
	if err := ts.Read(context.Background(), func(tx Tx) error {
		_, err := ts.Tasks().ItemHistory(tx, "")
		return err
	}); err == nil {
		t.Error("an empty project id was accepted")
	}
}
