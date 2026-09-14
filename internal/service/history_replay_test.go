package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// KANB-31 — replaying the journal must agree with the live board.
// ---------------------------------------------------------------------------

// diffCounts names every field on which two answers disagree. It is a named
// function rather than a pile of inline comparisons for one reason: a
// reconciliation test is only worth having if it can actually fail, and a
// comparator that is itself under test cannot quietly degrade into "assert
// true". TestDiffCountsReportsEveryDisagreement pins that down.
func diffCounts(live, replayed boardCounts) []string {
	var out []string
	cmp := func(name string, a, b any) {
		if fmt.Sprint(a) != fmt.Sprint(b) {
			out = append(out, fmt.Sprintf("%s: live=%v replay=%v", name, a, b))
		}
	}
	cmp("total", live.TotalTasks, replayed.TotalTasks)
	cmp("open", live.OpenTasks, replayed.OpenTasks)
	cmp("done", live.DoneTasks, replayed.DoneTasks)
	cmp("leaves", live.Leaves, replayed.Leaves)
	cmp("done_leaves", live.DoneLeaves, replayed.DoneLeaves)
	cmp("leaves_estimated", live.LeavesEstimated, replayed.LeavesEstimated)
	cmp("estimate_total", live.EstimateTotal, replayed.EstimateTotal)
	cmp("estimate_done", live.EstimateDone, replayed.EstimateDone)
	return out
}

func journalOf(t *testing.T, env *testEnv) []store.TaskHistoryEntry {
	t.Helper()
	var out []store.TaskHistoryEntry
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		var err error
		out, err = env.TaskHistory().ListByProject(tx, env.proj.ID)
		return err
	}); err != nil {
		t.Fatalf("read journal: %v", err)
	}
	return out
}

func liveCounts(t *testing.T, env *testEnv) boardCounts {
	t.Helper()
	var out boardCounts
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		var err error
		out, err = liveBoardCounts(env.svc.(*svc), tx, env.proj.ID)
		return err
	}); err != nil {
		t.Fatalf("live counts: %v", err)
	}
	return out
}

// moveTask drives a column change through the store the way every product
// path does, so the journal sees exactly what production writes.
func moveTask(t *testing.T, env *testEnv, task *domain.Task, column string) {
	t.Helper()
	col := env.cols[column]
	if col == nil {
		t.Fatalf("no column %q", column)
	}
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		before, after, err := env.Tasks().NeighbourRanks(tx, col.ID, store.RankBottom)
		if err != nil {
			return err
		}
		return env.Tasks().Move(tx, task.ID, col.ID, (before+after)/2+1, env.actor.Name)
	}); err != nil {
		t.Fatalf("move %s to %s: %v", task.Key, column, err)
	}
}

func archiveTask(t *testing.T, env *testEnv, task *domain.Task, archived bool) {
	t.Helper()
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Tasks().Archive(tx, task.ID, archived, env.actor.Name)
	}); err != nil {
		t.Fatalf("archive %s = %v: %v", task.Key, archived, err)
	}
}

func setEstimate(t *testing.T, env *testEnv, task *domain.Task, est float64) {
	t.Helper()
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		cur, err := env.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		v := est
		cur.Estimate = &v
		cur.UpdatedBy = env.actor.Name
		return env.Tasks().Update(tx, cur, nil)
	}); err != nil {
		t.Fatalf("set estimate on %s: %v", task.Key, err)
	}
}

func setParent(t *testing.T, env *testEnv, task *domain.Task, parent *domain.Task) {
	t.Helper()
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		cur, err := env.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		if parent == nil {
			cur.ParentID = nil
		} else {
			cur.ParentID = &parent.ID
		}
		cur.UpdatedBy = env.actor.Name
		return env.Tasks().Update(tx, cur, nil)
	}); err != nil {
		t.Fatalf("reparent %s: %v", task.Key, err)
	}
}

// ---------------------------------------------------------------------------
// Acceptance 1 and 2: replay to "now" equals the live query.
// ---------------------------------------------------------------------------

// TestReplayMatchesTheLiveBoard drives the board through every kind of change
// the journal records — create, move, finish, reopen, reparent, re-estimate,
// archive both an open and a finished card, restore one, and change a
// column's kind under the cards sitting in it — and then asserts that
// replaying the journal to now reproduces the live counters field for field.
//
// This is the test the whole journal exists to keep honest: the live path is
// a fast SELECT and the historical path is a replay, and nothing except this
// comparison stops them from drifting apart in a direction nobody is looking.
func TestReplayMatchesTheLiveBoard(t *testing.T) {
	env := openTestEnv(t)

	umbrella := makeBacklogTask(t, env, "umbrella")
	childA := makeBacklogTask(t, env, "child A")
	childB := makeBacklogTask(t, env, "child B")
	loner := makeBacklogTask(t, env, "loner")
	doomed := makeBacklogTask(t, env, "doomed")
	finished := makeBacklogTask(t, env, "finished then archived")
	restored := makeBacklogTask(t, env, "archived then restored")

	setParent(t, env, childA, umbrella)
	setParent(t, env, childB, umbrella)
	setEstimate(t, env, childA, 3)
	setEstimate(t, env, childB, 5)
	setEstimate(t, env, loner, 2)
	setEstimate(t, env, umbrella, 100) // an umbrella must not be counted at all
	setEstimate(t, env, finished, 7)

	moveTask(t, env, childA, "Doing")
	moveTask(t, env, childA, "Done")
	moveTask(t, env, childA, "Doing") // reopened
	moveTask(t, env, childA, "Done")  // and closed again
	moveTask(t, env, finished, "Done")
	moveTask(t, env, loner, "Doing")

	archiveTask(t, env, doomed, true)    // open card archived
	archiveTask(t, env, finished, true)  // finished card archived
	archiveTask(t, env, restored, true)  // archived...
	archiveTask(t, env, restored, false) // ...and brought back

	// Re-estimate after the fact: today's edit must not move yesterday.
	setEstimate(t, env, childB, 9)

	// A column changing kind finishes every card in it without touching a
	// single task row. If the journal did not record that, this is where the
	// two answers would part company.
	if err := env.Write(store.WithActor(context.Background(), env.actor.Name), func(tx store.Tx) error {
		c, err := env.Columns().GetByID(tx, env.cols["Doing"].ID)
		if err != nil {
			return err
		}
		c.Kind = domain.KindDone
		return env.Columns().Update(tx, c)
	}); err != nil {
		t.Fatalf("turn Doing into a done column: %v", err)
	}

	live := liveCounts(t, env)
	replayed := replayJournal(journalOf(t, env), 0)
	if diffs := diffCounts(live, replayed); len(diffs) > 0 {
		t.Fatalf("replay disagrees with the live board:\n  %v\nlive=%+v\nreplay=%+v",
			diffs, live, replayed)
	}
	// Sanity: the board is not empty, so the agreement above is not the
	// agreement of two zeroes.
	if live.TotalTasks == 0 || live.DoneTasks == 0 || live.EstimateTotal == 0 {
		t.Fatalf("the fixture degenerated to an empty board: %+v", live)
	}

	// And the fixture really does depend on the column-kind entry: strip it
	// from the journal and the two answers part company. Without this check
	// the agreement above could be the agreement of a path never taken.
	var withoutColumnKind []store.TaskHistoryEntry
	for _, e := range journalOf(t, env) {
		if e.Kind == store.HistoryColumnKind {
			continue
		}
		withoutColumnKind = append(withoutColumnKind, e)
	}
	if d := diffCounts(live, replayJournal(withoutColumnKind, 0)); len(d) == 0 {
		t.Fatal("dropping the column-kind entry changed nothing: the fixture never exercised that path")
	}
}

// The reconciliation above is only load-bearing if it can fail. Two checks
// keep it from rotting into a tautology, and neither needs a defect planted
// in the product:
//
//   - diffCounts itself reports a disagreement on every field it compares.
//   - Real, legitimately different inputs — a replay stopped one entry short
//     of now, and a journal missing an entry the way a forgotten mutation
//     path would leave it — produce a non-empty diff against the live board.
func TestDiffCountsReportsEveryDisagreement(t *testing.T) {
	base := boardCounts{
		TotalTasks: 5, OpenTasks: 3, DoneTasks: 2,
		Leaves: 4, DoneLeaves: 2, LeavesEstimated: 3,
		EstimateTotal: 10, EstimateDone: 4,
	}
	if d := diffCounts(base, base); len(d) != 0 {
		t.Fatalf("identical counts reported as different: %v", d)
	}
	mutators := map[string]func(*boardCounts){
		"total":            func(c *boardCounts) { c.TotalTasks++ },
		"open":             func(c *boardCounts) { c.OpenTasks++ },
		"done":             func(c *boardCounts) { c.DoneTasks++ },
		"leaves":           func(c *boardCounts) { c.Leaves++ },
		"done_leaves":      func(c *boardCounts) { c.DoneLeaves++ },
		"leaves_estimated": func(c *boardCounts) { c.LeavesEstimated++ },
		"estimate_total":   func(c *boardCounts) { c.EstimateTotal += 0.5 },
		"estimate_done":    func(c *boardCounts) { c.EstimateDone += 0.5 },
	}
	for name, mutate := range mutators {
		other := base
		mutate(&other)
		d := diffCounts(base, other)
		if len(d) != 1 {
			t.Fatalf("changing %s produced %d diffs, want exactly 1: %v", name, len(d), d)
		}
	}
}

func TestReconciliationCatchesARealDivergence(t *testing.T) {
	env := openTestEnv(t)
	a := makeBacklogTask(t, env, "a")
	b := makeBacklogTask(t, env, "b")
	setEstimate(t, env, a, 4)
	moveTask(t, env, a, "Done")
	archiveTask(t, env, b, true)

	entries := journalOf(t, env)
	live := liveCounts(t, env)
	if d := diffCounts(live, replayJournal(entries, 0)); len(d) > 0 {
		t.Fatalf("the fixture itself does not reconcile: %v", d)
	}

	// A replay stopped one entry short of now is a genuinely different state,
	// and the comparison must say so.
	shortOfNow := replayJournal(entries, entries[len(entries)-1].ID-1)
	if d := diffCounts(live, shortOfNow); len(d) == 0 {
		t.Fatal("replaying one entry short of now still compared equal: the reconciliation cannot fail")
	}

	// And the failure mode the journal actually guards against: a mutation
	// path that forgot to record. Dropping the archival from the journal is
	// exactly what that would leave behind.
	var withoutArchival []store.TaskHistoryEntry
	for _, e := range entries {
		if e.Kind == store.HistoryArchived {
			continue
		}
		withoutArchival = append(withoutArchival, e)
	}
	if len(withoutArchival) == len(entries) {
		t.Fatal("no archival entry in the journal; the test is not exercising what it claims")
	}
	if d := diffCounts(live, replayJournal(withoutArchival, 0)); len(d) == 0 {
		t.Fatal("a journal missing the archival still compared equal to the live board")
	}
}

// ---------------------------------------------------------------------------
// Acceptance 3: closed, reopened, closed again leaves TWO closings.
// ---------------------------------------------------------------------------

func TestReplay_ReopeningProducesTwoClosings(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "reopened twice")

	moveTask(t, env, task, "Done")
	moveTask(t, env, task, "Doing")
	moveTask(t, env, task, "Done")

	entries := journalOf(t, env)
	closings := 0
	for _, e := range entries {
		if e.Kind == store.HistoryMoved && e.ToKind != nil && *e.ToKind == domain.KindDone {
			closings++
		}
	}
	if closings != 2 {
		t.Fatalf("history holds %d closings, want 2 — a reopened card must not lose its first completion", closings)
	}

	// Replayed entry by entry, the done count shows the dip and the recovery,
	// which is the point: a single done_at column can only ever remember the
	// last completion. (The comparison walks entry ids rather than
	// timestamps: the database clock has millisecond resolution and a test
	// closes and reopens a card faster than that, so the CURVE legitimately
	// collapses all three moves into one instant. The journal does not.)
	var doneSeries []int
	for _, e := range entries {
		doneSeries = append(doneSeries, replayJournal(entries, e.ID).DoneTasks)
	}
	sawOne, sawZeroAfterOne := false, false
	for _, d := range doneSeries {
		if d == 1 {
			sawOne = true
		}
		if sawOne && d == 0 {
			sawZeroAfterOne = true
		}
	}
	if !sawOne || !sawZeroAfterOne {
		t.Fatalf("done series %v never shows the close/reopen dip", doneSeries)
	}
	if last := doneSeries[len(doneSeries)-1]; last != 1 {
		t.Fatalf("done series ends at %d, want 1 (the card is closed again)", last)
	}
}

// ---------------------------------------------------------------------------
// Acceptance 4, 5 and 6: what archival does to the three counters.
// ---------------------------------------------------------------------------

// Archiving ALWAYS removes one from total, and removes one from the bucket
// the card was actually in: open for an unfinished card, done for a finished
// one. It never adds to done. The identity open = total - done holds in both
// cases — which is the arithmetic that showed the original phrasing of this
// rule ("archival lowers total AND open") to be self-contradictory for a card
// that was sitting in Done.
func TestReplay_ArchivalMovesTheRightCounter(t *testing.T) {
	cases := []struct {
		name     string
		finished bool
	}{
		{"an unfinished card leaves the open bucket", false},
		{"a finished card leaves the done bucket", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := openTestEnv(t)
			// A second card so the board is never empty on either side of the
			// archival and the counters have something to hold steady against.
			makeBacklogTask(t, env, "bystander")
			task := makeBacklogTask(t, env, "subject")
			if tc.finished {
				moveTask(t, env, task, "Done")
			}

			entries := journalOf(t, env)
			before := replayJournal(entries, 0)

			archiveTask(t, env, task, true)
			after := replayJournal(journalOf(t, env), 0)

			if after.TotalTasks != before.TotalTasks-1 {
				t.Fatalf("total %d -> %d, want a drop of exactly 1", before.TotalTasks, after.TotalTasks)
			}
			if tc.finished {
				if after.DoneTasks != before.DoneTasks-1 {
					t.Fatalf("done %d -> %d, want a drop of 1", before.DoneTasks, after.DoneTasks)
				}
				if after.OpenTasks != before.OpenTasks {
					t.Fatalf("open %d -> %d, want it unchanged", before.OpenTasks, after.OpenTasks)
				}
			} else {
				if after.OpenTasks != before.OpenTasks-1 {
					t.Fatalf("open %d -> %d, want a drop of 1", before.OpenTasks, after.OpenTasks)
				}
				if after.DoneTasks != before.DoneTasks {
					t.Fatalf("done %d -> %d, want it unchanged", before.DoneTasks, after.DoneTasks)
				}
			}
			if after.DoneTasks > before.DoneTasks {
				t.Fatal("archiving added to done, which it must never do")
			}
			for _, c := range []struct {
				when string
				c    boardCounts
			}{{"before", before}, {"after", after}} {
				if c.c.OpenTasks != c.c.TotalTasks-c.c.DoneTasks {
					t.Fatalf("%s archival: open=%d but total-done=%d", c.when,
						c.c.OpenTasks, c.c.TotalTasks-c.c.DoneTasks)
				}
			}

			// And the live board agrees with the replay on the other side.
			if d := diffCounts(liveCounts(t, env), after); len(d) > 0 {
				t.Fatalf("after archival the replay diverges: %v", d)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Acceptance 7: today's estimate edits do not move yesterday's points.
// ---------------------------------------------------------------------------

func TestReplay_PastPointsSurviveTodaysEstimateEdits(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "re-estimated")
	other := makeBacklogTask(t, env, "steady")
	setEstimate(t, env, task, 3)
	setEstimate(t, env, other, 1)
	moveTask(t, env, task, "Done")

	entries := journalOf(t, env)
	cutoff := entries[len(entries)-1].ID
	was := replayJournal(entries, cutoff)
	if was.EstimateTotal != 4 || was.EstimateDone != 3 {
		t.Fatalf("baseline point = total %v / done %v, want 4 / 3", was.EstimateTotal, was.EstimateDone)
	}

	// Someone re-sizes the card today, twice over.
	setEstimate(t, env, task, 30)
	setEstimate(t, env, other, 10)

	entries = journalOf(t, env)
	still := replayJournal(entries, cutoff)
	if still != was {
		t.Fatalf("the past point moved: was %+v, now %+v", was, still)
	}
	now := replayJournal(entries, 0)
	if now.EstimateTotal != 40 || now.EstimateDone != 30 {
		t.Fatalf("today's point = total %v / done %v, want 40 / 30", now.EstimateTotal, now.EstimateDone)
	}
	if d := diffCounts(liveCounts(t, env), now); len(d) > 0 {
		t.Fatalf("today's replay diverges from the live board: %v", d)
	}
}

// The replayed curve never reaches back before the journal's origin. Without
// this the temptation is to fill the gap from tasks.created_at, which knows
// when cards appeared and nothing about when they moved, finished or were
// archived — half a truth, on a chart, with no way to see which half.
func TestReplay_DrawsNothingBeforeTheOrigin(t *testing.T) {
	env := openTestEnv(t)
	makeBacklogTask(t, env, "one")
	makeBacklogTask(t, env, "two")

	origin := readOrigin(t, env)
	for _, p := range buildHistoryPoints(journalOf(t, env), "h") {
		if p.At.Before(origin) {
			t.Fatalf("history point at %s predates the journal origin %s", p.At, origin)
		}
	}
}

// The replay is reachable from outside the package through service.Service,
// with no type assertion to a concrete type.
func TestProgressHistory_ExposesTheReplay(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "one")
	setEstimate(t, env, task, 2)
	moveTask(t, env, task, "Done")

	var svc Service = env.svc
	got, err := svc.ProgressHistory(context.Background(), env.actor, ProgressHistoryInput{
		ProjectKey:    env.proj.Key,
		IncludeReplay: true,
	})
	if err != nil {
		t.Fatalf("ProgressHistory: %v", err)
	}
	if len(got.Replay) == 0 {
		t.Fatal("IncludeReplay returned no points")
	}
	if got.HistoryStartsAt.IsZero() {
		t.Fatal("HistoryStartsAt is zero")
	}
	last := got.Replay[len(got.Replay)-1]
	if last.TotalTasks != 1 || last.DoneTasks != 1 || last.EstimateDone != 2 {
		t.Fatalf("last replayed point = %+v, want 1 task, done, 2h of estimate", last)
	}
}
