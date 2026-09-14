package service

import (
	"context"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// KANB-37 — the counters' invariant, as a test rather than a convention.
//
// The board once showed 212 total, 203 done, 12 open — done + open = 215 at
// total 212. The card lists three candidate causes: parents counted by one
// counter and not by another; archived cards still sitting in done while
// gone from total; numbers refreshed at different moments. This fixture is
// built so each cause would break the identity in its own way, on the LIVE
// path (the same liveBoardCounts the board renders from), after every step
// the board can go through.
//
// The current arithmetic cannot produce the screenshot's split by
// construction: countBoard reduces every card to one cardState and all three
// counters come out of the same loop over the same list, with open DEFINED
// as total - done. What a test can still catch is the counting drifting back
// to separate queries — for instance a "faster" open count over non-done
// columns, which would part company with total the moment a column changes
// kind or a card skips the expected path.
// ---------------------------------------------------------------------------

func TestCounts_TotalPartitionsIntoDoneAndOpen(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	umbrella := makeBacklogTask(t, env, "umbrella")
	childA := makeBacklogTask(t, env, "child A")
	childB := makeBacklogTask(t, env, "child B")
	makeBacklogTask(t, env, "loner")
	doomedOpen := makeBacklogTask(t, env, "archived open")
	doomedDone := makeBacklogTask(t, env, "archived done")
	setParent(t, env, childA, umbrella)
	setParent(t, env, childB, umbrella)

	check := func(step string) boardCounts {
		t.Helper()
		live := liveCounts(t, env)
		if live.OpenTasks != live.TotalTasks-live.DoneTasks {
			t.Fatalf("%s: open=%d but total=%d, done=%d — the counters stopped partitioning the board",
				step, live.OpenTasks, live.TotalTasks, live.DoneTasks)
		}
		return live
	}

	// After creation: six live cards, none finished. The parent is one of
	// them — the "parents counted by one counter only" cause would already
	// show here as total != done + open.
	check("creation")

	// A card born STRAIGHT INTO done: the newest card is a finished one, and
	// the identity must hold with it from the first journal entry.
	res, err := env.svc.TaskCreate(ctx, env.actor, TaskCreateInput{Tasks: []NewTask{{
		ProjectKey: env.proj.Key,
		Title:      "born done",
		Type:       domain.TypeTask,
		Column:     "Done",
	}}})
	if err != nil {
		t.Fatalf("create into Done: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("created %d tasks, want 1", len(res.Tasks))
	}
	live := check("a card born into done")
	if live.DoneTasks != 1 {
		t.Fatalf("after the born-done card done=%d, want 1", live.DoneTasks)
	}

	// Finishing and reopening a child: two moves, one card, identity holding
	// at both ends.
	moveTask(t, env, childA, "Done")
	check("a child finished")
	moveTask(t, env, childA, "Doing")
	check("the child reopened")

	// The parent itself finishes: a done umbrella must still sit in exactly
	// one bucket of total, and never in the leaves metric — it has children,
	// so leaves stay at the born-done card alone.
	moveTask(t, env, umbrella, "Done")
	live = check("an umbrella finished")
	if live.DoneLeaves != 1 {
		t.Fatalf("done leaves = %d, want exactly the born-done card (a done umbrella is not a leaf): %+v",
			live.DoneLeaves, live)
	}

	// Archiving, both ways. An archived card leaves EVERY counter: an open
	// card out of total (open falls with it), and — the screenshot's
	// suspicion — a done card out of done too, never lingering there while
	// gone from total.
	moveTask(t, env, doomedDone, "Done")
	archiveTask(t, env, doomedOpen, true)
	live = check("an open card archived")
	beforeDone := live.DoneTasks
	archiveTask(t, env, doomedDone, true)
	live = check("a done card archived")
	if live.DoneTasks != beforeDone-1 {
		t.Fatalf("archiving a done card moved done %d -> %d, want a drop of exactly 1 — the card left the project, it did not stop being finished",
			beforeDone, live.DoneTasks)
	}

	// The end state, spelled out: live cards are umbrella (done), childA
	// (open), childB (open), loner (open), bornDone (done) — total 5, done 2,
	// open 3. Both archived cards are in none of the numbers.
	if live.TotalTasks != 5 || live.DoneTasks != 2 || live.OpenTasks != 3 {
		t.Fatalf("end state = total %d, done %d, open %d, want 5 / 2 / 3", live.TotalTasks, live.DoneTasks, live.OpenTasks)
	}
	if live.Leaves != 4 || live.DoneLeaves != 1 {
		t.Fatalf("leaves = %d/%d, want 4 leaves of which 1 done (the umbrella counts in total, never in leaves)",
			live.DoneLeaves, live.Leaves)
	}
}
