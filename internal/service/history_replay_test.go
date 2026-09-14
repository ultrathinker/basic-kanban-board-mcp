package service

import (
	"context"
	"fmt"
	"testing"
	"time"

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

// KANB-34: the curve must CARRY which instants were archival. A chart cannot
// tell an archival from a completion by looking at the counts — both make a
// bucket fall — so each replayed point names how many cards left the project
// AT that instant, and the renderer turns that into the axis tick that stops
// a falling total from being read as lost data or as finished work.
//
// Both archival cases must appear: an open card leaving drops total AND open,
// a finished card leaving drops total while open holds — and a plain move to
// done, which is work FINISHING, must carry no tick at all.
func TestHistoryPoints_FlagTheArchivalInstants(t *testing.T) {
	env := openTestEnv(t)
	openCard := makeBacklogTask(t, env, "archived while open")
	finished := makeBacklogTask(t, env, "archived when done")
	mover := makeBacklogTask(t, env, "moved to done, never archived")

	moveTask(t, env, finished, "Done")
	waitForClockTick(t, env)
	archiveTask(t, env, openCard, true)
	waitForClockTick(t, env)
	archiveTask(t, env, finished, true)
	waitForClockTick(t, env)
	moveTask(t, env, mover, "Done")

	pts := buildHistoryPoints(journalOf(t, env), env.proj.EstimateUnit)
	if len(pts) < 4 {
		t.Fatalf("the curve has %d points, want at least 4 — the fixture collapsed into fewer instants than it exercised", len(pts))
	}

	flagged := 0
	for i := 1; i < len(pts); i++ {
		p, prev := pts[i], pts[i-1]
		switch {
		case p.Archived == 0:
			// A non-archival instant may not lose work. The one other thing
			// that lowers a bucket is a completion, and that leaves total
			// alone.
			if p.TotalTasks < prev.TotalTasks {
				t.Fatalf("point %d: total fell %d -> %d with no archival flag — the drop would be drawn in silence",
					i, prev.TotalTasks, p.TotalTasks)
			}
		default:
			flagged++
			if p.Archived != 1 {
				t.Fatalf("point %d flags %d archived, want 1", i, p.Archived)
			}
			if p.TotalTasks != prev.TotalTasks-1 {
				t.Fatalf("archival point %d: total %d -> %d, want a drop of exactly 1",
					i, prev.TotalTasks, p.TotalTasks)
			}
			if p.DoneTasks > prev.DoneTasks {
				t.Fatalf("archival point %d raised done, which archival must never do", i)
			}
			if flagged == 1 {
				// The open card left: open falls with total.
				if p.OpenTasks != prev.OpenTasks-1 {
					t.Fatalf("archiving the OPEN card: open %d -> %d, want a drop of 1",
						prev.OpenTasks, p.OpenTasks)
				}
			} else {
				// The finished card left: open must hold.
				if p.OpenTasks != prev.OpenTasks {
					t.Fatalf("archiving the DONE card: open %d -> %d, want it unchanged",
						prev.OpenTasks, p.OpenTasks)
				}
			}
		}
	}
	if flagged != 2 {
		t.Fatalf("the curve flags %d archival instants, want exactly 2 (one open card, one finished card)", flagged)
	}
	// The last point is the plain completion, not an archival: the flag
	// belongs to departures, never to work finishing.
	if last := pts[len(pts)-1]; last.Archived != 0 {
		t.Fatalf("the final point (a plain move to done) carries an archival flag")
	}
	if last := pts[len(pts)-1]; last.OpenTasks != pts[len(pts)-2].OpenTasks-1 || last.TotalTasks != pts[len(pts)-2].TotalTasks {
		t.Fatalf("the final point is not the completion: total %d -> %d, open %d -> %d",
			pts[len(pts)-2].TotalTasks, last.TotalTasks, pts[len(pts)-2].OpenTasks, last.OpenTasks)
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

// Acceptance 1: state at a POINT IN TIME, not only at a point in the journal.
// replayAt is the by-time face of the same walk — it picks the last entry at
// or before the instant asked for, answers "empty board" for an instant that
// predates the whole journal, and agrees with the by-id replay everywhere
// they overlap.
func TestReplayAt_AnswersForAnInstant(t *testing.T) {
	env := openTestEnv(t)
	a := makeBacklogTask(t, env, "a")
	setEstimate(t, env, a, 4)
	moveTask(t, env, a, "Done")
	makeBacklogTask(t, env, "b")

	entries := journalOf(t, env)
	origin := readOrigin(t, env)

	if got := replayAt(entries, origin.Add(-time.Hour)); got != (boardCounts{}) {
		t.Fatalf("replay before the journal began = %+v, want an empty board", got)
	}
	last := entries[len(entries)-1]
	if got, want := replayAt(entries, last.TS), replayJournal(entries, 0); got != want {
		t.Fatalf("replay at the last entry's instant = %+v, want the same as replaying everything %+v", got, want)
	}
	if got, want := replayAt(entries, time.Time{}), replayJournal(entries, 0); got != want {
		t.Fatalf("replay at the zero time = %+v, want now %+v", got, want)
	}
	if d := diffCounts(liveCounts(t, env), replayAt(entries, last.TS)); len(d) > 0 {
		t.Fatalf("replay at now diverges from the live board: %v", d)
	}
}

// waitForClockTick blocks until the database clock reads a different instant
// from the one it reads now.
//
// buildHistoryPoints collapses everything recorded inside a single instant
// into one point, which is correct — a chart cannot draw two states at the
// same moment. But it means a test about a PAST point has to make the past a
// genuinely different instant from today, and the journal is stamped from the
// database clock at millisecond resolution. Polling that same clock is the
// honest way to know it has moved; a fixed sleep would be a guess about timer
// granularity on whatever machine happens to run this.
func waitForClockTick(t *testing.T, env *testEnv) {
	t.Helper()
	read := func() time.Time {
		var now time.Time
		if err := env.Read(context.Background(), func(tx store.Tx) error {
			var err error
			now, err = tx.Now()
			return err
		}); err != nil {
			t.Fatalf("read database clock: %v", err)
		}
		return now
	}
	start := read()
	deadline := time.Now().Add(5 * time.Second)
	for !read().After(start) {
		if time.Now().After(deadline) {
			t.Fatal("the database clock did not advance within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

// samePoint compares two history points field by field. It exists because
// HistoryPoint carries a *int percent, so == would compare pointer identity
// and quietly pass for two points that disagree about everything.
func samePoint(a, b HistoryPoint) bool {
	if !a.At.Equal(b.At) ||
		a.TotalTasks != b.TotalTasks || a.OpenTasks != b.OpenTasks || a.DoneTasks != b.DoneTasks ||
		a.Archived != b.Archived ||
		a.Leaves != b.Leaves || a.DoneLeaves != b.DoneLeaves || a.LeavesEstimated != b.LeavesEstimated ||
		a.EstimateTotal != b.EstimateTotal || a.EstimateDone != b.EstimateDone {
		return false
	}
	if a.Readiness.Basis != b.Readiness.Basis || a.Readiness.Partial != b.Readiness.Partial ||
		a.Readiness.Coverage != b.Readiness.Coverage {
		return false
	}
	switch {
	case a.Readiness.Percent == nil || b.Readiness.Percent == nil:
		return a.Readiness.Percent == nil && b.Readiness.Percent == nil
	default:
		return *a.Readiness.Percent == *b.Readiness.Percent
	}
}

// KANB-31 acceptance 7 and KANB-35 acceptance 4, proved ON THE PATH THAT
// BUILDS THE USER'S CURVE.
//
// There are two ways into the replay: replayJournal, cut off by entry id, and
// buildHistoryPoints, which walks by time and is what ProgressHistory returns
// as Result.Replay. The rule "today's estimate edits do not rewrite
// yesterday's points" was originally only asserted against the first, so the
// function that actually implements the rule for the product had no test of
// its own standing on it. This is that test.
func TestHistoryPoints_PastPointsSurviveTodaysEstimateEdits(t *testing.T) {
	env := openTestEnv(t)
	unit := env.proj.EstimateUnit
	small := makeBacklogTask(t, env, "small")
	big := makeBacklogTask(t, env, "big")

	setEstimate(t, env, small, 1)
	setEstimate(t, env, big, 3)
	moveTask(t, env, small, "Done")

	before := buildHistoryPoints(journalOf(t, env), unit)
	if len(before) == 0 {
		t.Fatal("no history points at all")
	}
	past := before[len(before)-1]
	if past.EstimateTotal != 4 || past.EstimateDone != 1 {
		t.Fatalf("the past point divides %v/%v, want 1/4", past.EstimateDone, past.EstimateTotal)
	}
	if past.Readiness.Percent == nil || *past.Readiness.Percent != 25 {
		t.Fatalf("readiness at the past point = %v, want 25%%", past.Readiness.Percent)
	}

	// Today is a different instant, or the question does not arise.
	waitForClockTick(t, env)

	// Today someone decides the unfinished card is far bigger, and the
	// finished one a little bigger too. Recomputing the past from today's
	// numbers would put that old point at 3/100 = 3%.
	setEstimate(t, env, big, 97)
	setEstimate(t, env, small, 3)

	after := buildHistoryPoints(journalOf(t, env), unit)
	var found *HistoryPoint
	for i := range after {
		if after[i].At.Equal(past.At) {
			found = &after[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the past instant %s vanished from the curve; the test cannot prove anything", past.At)
	}
	if !samePoint(past, *found) {
		t.Fatalf("the past point was rewritten by today's estimate edits:\n  was %+v\n  now %+v", past, *found)
	}

	// The edits must have landed on a LATER instant, otherwise "past" and
	// "today" were the same point and the check above was vacuous.
	last := after[len(after)-1]
	if !last.At.After(past.At) {
		t.Fatalf("today's edits landed at %s, not after the past point at %s", last.At, past.At)
	}
	// And today's point did move, so the curve is not simply frozen.
	if last.EstimateTotal != 100 || last.EstimateDone != 3 {
		t.Fatalf("today's point divides %v/%v, want 3/100", last.EstimateDone, last.EstimateTotal)
	}
	if last.Readiness.Percent == nil || *last.Readiness.Percent != 3 {
		t.Fatalf("readiness today = %v, want 3%%", last.Readiness.Percent)
	}

	// The same curve, reached the way a caller reaches it.
	var svc Service = env.svc
	got, err := svc.ProgressHistory(context.Background(), env.actor, ProgressHistoryInput{
		ProjectKey:    env.proj.Key,
		IncludeReplay: true,
	})
	if err != nil {
		t.Fatalf("ProgressHistory: %v", err)
	}
	var served *HistoryPoint
	for i := range got.Replay {
		if got.Replay[i].At.Equal(past.At) {
			served = &got.Replay[i]
			break
		}
	}
	if served == nil {
		t.Fatalf("the past instant %s is missing from the served curve", past.At)
	}
	if !samePoint(past, *served) {
		t.Fatalf("the curve served to a caller rewrote the past point:\n  was %+v\n  now %+v", past, *served)
	}
}

// A card can be created STRAIGHT INTO a done column — task_create takes a
// column name and the create path does not restrict which kind it may be — so
// the journal's very first entry about that card has to carry the real kind of
// the column it was born in. It is the only place the journal ever learns the
// bucket of a newborn card, and getting it wrong is not a blip: the replay
// would count the card as open forever while the live board counts it as done.
//
// Every other fixture in this suite creates cards in Backlog, which is exactly
// why this needs saying out loud.
func TestReplay_CardCreatedStraightIntoADoneColumn(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	res, err := env.svc.TaskCreate(ctx, env.actor, TaskCreateInput{Tasks: []NewTask{{
		ProjectKey: env.proj.Key,
		Title:      "born finished",
		Type:       domain.TypeTask,
		Column:     "Done",
	}}})
	if err != nil {
		t.Fatalf("TaskCreate into Done: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("created %d tasks, want 1", len(res.Tasks))
	}
	created := res.Tasks[0]
	if created.ColumnID != env.cols["Done"].ID {
		t.Fatalf("the card landed in column %s, want Done (%s)", created.ColumnID, env.cols["Done"].ID)
	}

	// The live board counts it as finished from the moment it exists.
	live := liveCounts(t, env)
	if live.TotalTasks != 1 || live.DoneTasks != 1 || live.OpenTasks != 0 {
		t.Fatalf("live board = %+v, want 1 task, done", live)
	}

	// The journal must have READ the column's kind, not assumed one.
	entries := journalOf(t, env)
	var birth *store.TaskHistoryEntry
	for i := range entries {
		if entries[i].Kind == store.HistoryCreated && entries[i].TaskID != nil && *entries[i].TaskID == created.ID {
			birth = &entries[i]
			break
		}
	}
	if birth == nil {
		t.Fatal("no creation entry in the journal")
	}
	if birth.ToKind == nil {
		t.Fatal("the creation entry records no column kind at all")
	}
	if *birth.ToKind != domain.KindDone {
		t.Fatalf("the creation entry records column kind %q, want %q — the journal guessed instead of reading it",
			*birth.ToKind, domain.KindDone)
	}

	// And the two answers agree, which is the thing that would drift forever.
	if d := diffCounts(live, replayJournal(entries, 0)); len(d) > 0 {
		t.Fatalf("a card born in a done column makes the replay diverge: %v\nlive=%+v", d, live)
	}
}

// The lifecycle journal is kept forever, and the ONE deletion allowed to
// touch it is the ON DELETE CASCADE on task_id. That is not a retention rule,
// it is the tie that keeps the two answers together: a hard delete (admin CLI
// only) removes the card from the live counters instantly, so the journal has
// to stop counting it in the same breath. Without the cascade the replay goes
// on counting a card that no longer exists, and the gap never closes.
//
// Nothing else covers this — TaskRepo.Delete writes no journal entry at all,
// by design, because the cascade is the whole mechanism.
func TestReplay_HardDeleteTakesTheJournalWithIt(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	keeper := makeBacklogTask(t, env, "keeper")
	doomed := makeBacklogTask(t, env, "doomed")
	setEstimate(t, env, keeper, 2)
	setEstimate(t, env, doomed, 5)
	moveTask(t, env, doomed, "Done")

	// The card has a real history behind it, so the cascade has something to
	// take: creation, an estimate and a move.
	var beforeEntries int
	for _, e := range journalOf(t, env) {
		if e.TaskID != nil && *e.TaskID == doomed.ID {
			beforeEntries++
		}
	}
	if beforeEntries < 3 {
		t.Fatalf("the doomed card left %d journal entries, want at least 3", beforeEntries)
	}

	if err := env.Write(ctx, func(tx store.Tx) error {
		return env.Tasks().Delete(tx, doomed.ID)
	}); err != nil {
		t.Fatalf("hard delete: %v", err)
	}

	// Its entries went with it.
	entries := journalOf(t, env)
	for _, e := range entries {
		if e.TaskID != nil && *e.TaskID == doomed.ID {
			t.Fatalf("journal still holds a %q entry for a hard-deleted card", e.Kind)
		}
	}

	// The live board and the replay tell the same story: two cards became
	// one, and the finished one is gone rather than finished.
	live := liveCounts(t, env)
	if live.TotalTasks != 1 || live.DoneTasks != 0 || live.EstimateTotal != 2 {
		t.Fatalf("live board = %+v, want 1 open card worth 2", live)
	}
	if d := diffCounts(live, replayJournal(entries, 0)); len(d) > 0 {
		t.Fatalf("a hard-deleted card still haunts the replay: %v\nlive=%+v", d, live)
	}

	// The surviving card kept its own history: the cascade is scoped to the
	// row that was deleted, not a sweep.
	var kept int
	for _, e := range entries {
		if e.TaskID != nil && *e.TaskID == keeper.ID {
			kept++
		}
	}
	if kept < 2 {
		t.Fatalf("the surviving card has %d journal entries left, want its creation and estimate at least", kept)
	}
}
