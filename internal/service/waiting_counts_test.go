package service

import (
	"testing"
)

// ---------------------------------------------------------------------------
// KANB-52 meets KANB-31/KANB-37 — the fourth column kind against the counters.
//
// Two tracks arrived here without knowing about each other. One added the
// waiting kind ("visible and counted as open"). The other pinned
// total = done + open on the live path and pinned the journal replay against
// that same live answer. Neither fixture ever contained a waiting column, so
// "a parked card counts as open" held only by construction — done is defined
// as "sits in a done-kind column" and open as total - done — and by
// construction is exactly the kind of claim a merge is entitled to break
// without any compiler noticing.
//
// So this exercises it instead of reasoning about it: a real waiting column,
// added through the real ProjectUpsert path, cards parked in it, and after
// every step BOTH answers (the live SELECT and the replay of the journal)
// asserted to agree with each other and with the arithmetic.
// ---------------------------------------------------------------------------

func TestWaitingColumn_CountsAsOpenOnTheLiveBoardAndInTheReplay(t *testing.T) {
	env := openTestEnv(t)
	addWaitingColumn(t, env)

	parkedOpen := makeBacklogTask(t, env, "parked while open")
	parkedDone := makeBacklogTask(t, env, "finished, then parked")
	makeBacklogTask(t, env, "untouched")

	// agree is the whole point of the test: after every step the live board
	// must partition into done and open, and replaying the journal to now
	// must reproduce the live answer field for field. Either half alone
	// would let the pair drift.
	agree := func(step string) boardCounts {
		t.Helper()
		live := liveCounts(t, env)
		if live.OpenTasks != live.TotalTasks-live.DoneTasks {
			t.Fatalf("%s: open=%d but total=%d, done=%d - the counters stopped partitioning the board",
				step, live.OpenTasks, live.TotalTasks, live.DoneTasks)
		}
		replayed := replayJournal(journalOf(t, env), 0)
		if diffs := diffCounts(live, replayed); len(diffs) > 0 {
			t.Fatalf("%s: the replay disagrees with the live board:\n  %v\nlive=%+v\nreplay=%+v",
				step, diffs, live, replayed)
		}
		return live
	}

	start := agree("three cards in Backlog")
	if start.TotalTasks != 3 || start.DoneTasks != 0 || start.OpenTasks != 3 {
		t.Fatalf("start = total %d, done %d, open %d, want 3 / 0 / 3",
			start.TotalTasks, start.DoneTasks, start.OpenTasks)
	}

	// Parking an OPEN card must move nothing at all. Waiting is not done, and
	// a card that merely changed column has not left the project either.
	moveTask(t, env, parkedOpen, "Waiting")
	afterPark := agree("an open card parked in Waiting")
	if afterPark.TotalTasks != 3 || afterPark.DoneTasks != 0 || afterPark.OpenTasks != 3 {
		t.Fatalf("parking an open card gave total %d, done %d, open %d, want 3 / 0 / 3 - a parked card is still open work",
			afterPark.TotalTasks, afterPark.DoneTasks, afterPark.OpenTasks)
	}

	// Finishing a card, then parking it. The second move must take the card
	// back OUT of done: waiting is a state work passes through, not a place
	// completions are remembered.
	moveTask(t, env, parkedDone, "Done")
	finished := agree("a card finished")
	if finished.DoneTasks != 1 || finished.OpenTasks != 2 {
		t.Fatalf("after finishing one card done=%d open=%d, want 1 / 2", finished.DoneTasks, finished.OpenTasks)
	}
	moveTask(t, env, parkedDone, "Waiting")
	reopened := agree("the finished card parked in Waiting")
	if reopened.TotalTasks != 3 || reopened.DoneTasks != 0 || reopened.OpenTasks != 3 {
		t.Fatalf("parking a finished card gave total %d, done %d, open %d, want 3 / 0 / 3 - Waiting must not keep counting as Done",
			reopened.TotalTasks, reopened.DoneTasks, reopened.OpenTasks)
	}

	// The estimate rollup's working set sees a parked card the same way: a
	// live leaf that is not finished.
	if reopened.Leaves != 3 || reopened.DoneLeaves != 0 {
		t.Fatalf("leaves = %d of which %d done, want 3 / 0 - a parked card is a live, unfinished leaf",
			reopened.Leaves, reopened.DoneLeaves)
	}
}

// TestWaitingColumn_TheCurveDoesNotReadParkingAsFinishing checks the other
// surface the journal feeds: the item-count curve (KANB-34). A move into a
// waiting column is a journal entry like any other, and the two ways it could
// be misread are both checked here — as a completion (done rising, open
// falling) and as an archival (the axis tick that says work LEFT the
// project). It is neither: the totals hold still and no tick is raised.
func TestWaitingColumn_TheCurveDoesNotReadParkingAsFinishing(t *testing.T) {
	env := openTestEnv(t)
	addWaitingColumn(t, env)

	card := makeBacklogTask(t, env, "parked")
	makeBacklogTask(t, env, "untouched")
	moveTask(t, env, card, "Waiting")

	points := buildHistoryPoints(journalOf(t, env), env.proj.EstimateUnit)
	if len(points) == 0 {
		t.Fatal("the journal produced no points at all")
	}
	for i, p := range points {
		if p.Archived != 0 {
			t.Fatalf("point %d flags %d archivals; parking a card must not raise the archival tick", i, p.Archived)
		}
		if p.DoneTasks != 0 {
			t.Fatalf("point %d reports %d done; nothing was ever moved to a done column", i, p.DoneTasks)
		}
		if p.OpenTasks != p.TotalTasks {
			t.Fatalf("point %d has open=%d total=%d; every card on this board is open work",
				i, p.OpenTasks, p.TotalTasks)
		}
	}
	last := points[len(points)-1]
	if last.TotalTasks != 2 {
		t.Fatalf("the last point holds %d cards, want 2", last.TotalTasks)
	}
}
