package service

import (
	"context"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// KANB-35 — readiness weighted by estimates.
// ---------------------------------------------------------------------------

func liveReadiness(t *testing.T, env *testEnv) EstimateReadiness {
	t.Helper()
	got, err := env.svc.ProjectProgress(context.Background(), env.actor,
		ProjectProgressInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ProjectProgress: %v", err)
	}
	return got.Readiness
}

func mustPercent(t *testing.T, r EstimateReadiness) int {
	t.Helper()
	if r.Percent == nil {
		t.Fatalf("readiness has no percent: %+v", r)
	}
	return *r.Percent
}

// Acceptance 1: only unarchived leaves count, and an umbrella parent is not
// added on top of its children.
func TestReadiness_CountsLeavesOnlyAndNeverTheUmbrellaTwice(t *testing.T) {
	env := openTestEnv(t)

	umbrella := makeBacklogTask(t, env, "umbrella")
	childA := makeBacklogTask(t, env, "child A")
	childB := makeBacklogTask(t, env, "child B")
	standalone := makeBacklogTask(t, env, "standalone")
	archived := makeBacklogTask(t, env, "archived")

	setParent(t, env, childA, umbrella)
	setParent(t, env, childB, umbrella)
	setEstimate(t, env, childA, 3)
	setEstimate(t, env, childB, 5)
	setEstimate(t, env, standalone, 2)
	setEstimate(t, env, archived, 1000)
	// A wildly wrong umbrella estimate: if it were counted, nothing below
	// would add up.
	setEstimate(t, env, umbrella, 999)
	archiveTask(t, env, archived, true)

	moveTask(t, env, childA, "Done")

	r := liveReadiness(t, env)
	if r.Basis != ReadinessEstimates {
		t.Fatalf("basis = %q, want estimates", r.Basis)
	}
	// Working set: childA, childB, standalone. The umbrella has children, the
	// archived card is out.
	if r.Leaves != 3 || r.LeavesEstimated != 3 {
		t.Fatalf("leaves = %d (%d estimated), want 3 of 3", r.Leaves, r.LeavesEstimated)
	}
	if r.EstimateTotal != 10 {
		t.Fatalf("estimate total = %v, want 10 (3+5+2, no umbrella, no archived)", r.EstimateTotal)
	}
	if r.EstimateDone != 3 {
		t.Fatalf("estimate done = %v, want 3", r.EstimateDone)
	}
	if got := mustPercent(t, r); got != 30 {
		t.Fatalf("percent = %d, want 30", got)
	}
	if r.Partial {
		t.Fatal("everything is estimated, yet the figure claims to be partial")
	}
	if r.Unit != env.proj.EstimateUnit {
		t.Fatalf("unit = %q, want the project unit %q", r.Unit, env.proj.EstimateUnit)
	}

	// An umbrella whose children are all archived becomes a leaf itself, and
	// its own estimate starts counting. That is the rule, not an accident.
	archiveTask(t, env, childA, true)
	archiveTask(t, env, childB, true)
	r = liveReadiness(t, env)
	if r.Leaves != 2 {
		t.Fatalf("after archiving both children, leaves = %d, want 2 (umbrella + standalone)", r.Leaves)
	}
	if r.EstimateTotal != 1001 {
		t.Fatalf("estimate total = %v, want 1001 (999 umbrella + 2 standalone)", r.EstimateTotal)
	}
}

// Acceptance 2: coverage is reported, and a partly-estimated board says so.
func TestReadiness_ReportsCoverageAndMarksItselfPartial(t *testing.T) {
	env := openTestEnv(t)

	estimated := makeBacklogTask(t, env, "sized")
	done := makeBacklogTask(t, env, "sized and done")
	makeBacklogTask(t, env, "unsized one")
	makeBacklogTask(t, env, "unsized two")

	setEstimate(t, env, estimated, 6)
	setEstimate(t, env, done, 2)
	moveTask(t, env, done, "Done")

	r := liveReadiness(t, env)
	if r.Basis != ReadinessEstimates {
		t.Fatalf("basis = %q, want estimates", r.Basis)
	}
	if !r.Partial {
		t.Fatal("two of four working tasks are unestimated, yet the figure is not marked partial")
	}
	if r.LeavesEstimated != 2 || r.Leaves != 4 {
		t.Fatalf("coverage = %d of %d, want 2 of 4", r.LeavesEstimated, r.Leaves)
	}
	if r.Coverage != "2 of 4 estimated" {
		t.Fatalf("coverage phrase = %q, want %q", r.Coverage, "2 of 4 estimated")
	}
	// The unestimated pair is NOT counted as zero effort: if it were, the
	// denominator would still be 8 but an unsized feature would be free, and
	// the coverage figure would be the only warning. It is left out entirely.
	if r.EstimateTotal != 8 || r.EstimateDone != 2 {
		t.Fatalf("fraction = %v/%v, want 2/8", r.EstimateDone, r.EstimateTotal)
	}
	if got := mustPercent(t, r); got != 25 {
		t.Fatalf("percent = %d, want 25", got)
	}
}

// Acceptance 3: a zero denominator is "no data", never 100%.
func TestReadiness_ZeroDenominatorIsNoData(t *testing.T) {
	t.Run("no working tasks at all", func(t *testing.T) {
		env := openTestEnv(t)
		r := liveReadiness(t, env)
		if r.Basis != ReadinessNone {
			t.Fatalf("basis = %q, want none", r.Basis)
		}
		if r.Percent != nil {
			t.Fatalf("percent = %d on an empty board, want no answer at all", *r.Percent)
		}
		if r.Coverage != "no working tasks" {
			t.Fatalf("coverage = %q, want %q", r.Coverage, "no working tasks")
		}
	})

	t.Run("estimates that sum to zero", func(t *testing.T) {
		env := openTestEnv(t)
		a := makeBacklogTask(t, env, "zero one")
		b := makeBacklogTask(t, env, "zero two")
		setEstimate(t, env, a, 0)
		setEstimate(t, env, b, 0)
		moveTask(t, env, a, "Done")

		r := liveReadiness(t, env)
		if r.Basis != ReadinessNone {
			t.Fatalf("basis = %q, want none — 0/0 is not an answer", r.Basis)
		}
		if r.Percent != nil {
			t.Fatalf("percent = %d, want no answer: half of nothing is not 50%% and all of nothing is not 100%%", *r.Percent)
		}
	})
}

// Acceptance 6: with no estimates anywhere, counting cards is allowed — and
// is labelled as counting cards, never as a calculation by estimates.
func TestReadiness_TaskCountFallbackIsLabelledSeparately(t *testing.T) {
	env := openTestEnv(t)
	a := makeBacklogTask(t, env, "one")
	makeBacklogTask(t, env, "two")
	makeBacklogTask(t, env, "three")
	makeBacklogTask(t, env, "four")
	moveTask(t, env, a, "Done")

	r := liveReadiness(t, env)
	if r.Basis != ReadinessTaskCount {
		t.Fatalf("basis = %q, want task_count", r.Basis)
	}
	if r.Label != "by task count" {
		t.Fatalf("label = %q, want %q", r.Label, "by task count")
	}
	if got := mustPercent(t, r); got != 25 {
		t.Fatalf("percent = %d, want 25 (1 of 4 cards)", got)
	}
	if r.EstimateTotal != 0 || r.LeavesEstimated != 0 {
		t.Fatalf("the fallback invented estimates: %v over %d estimated leaves", r.EstimateTotal, r.LeavesEstimated)
	}
	if strings.Contains(strings.ToLower(r.Label), "estimate") {
		t.Fatalf("label %q presents a card count as an estimate calculation", r.Label)
	}

	// One estimate anywhere is enough to switch back to the real thing.
	setEstimate(t, env, a, 4)
	r = liveReadiness(t, env)
	if r.Basis != ReadinessEstimates || r.Label != "by estimates" {
		t.Fatalf("basis/label = %q/%q, want estimates/by estimates", r.Basis, r.Label)
	}
	if !r.Partial {
		t.Fatal("one of four estimated, yet the figure is not partial")
	}
}

// Acceptance 4: the historical curve uses the estimates that were current at
// the time, not today's. This is the test the whole "do not recompute the
// past" rule stands on.
func TestReadiness_HistoricalCurveUsesEstimatesOfTheTime(t *testing.T) {
	env := openTestEnv(t)
	small := makeBacklogTask(t, env, "small")
	big := makeBacklogTask(t, env, "big")

	setEstimate(t, env, small, 1)
	setEstimate(t, env, big, 3)
	moveTask(t, env, small, "Done")

	entries := journalOf(t, env)
	cutoff := entries[len(entries)-1].ID
	// Back then: 1 of 4 done = 25%.
	then := readinessFrom(replayJournal(entries, cutoff), env.proj.EstimateUnit)
	if got := mustPercent(t, then); got != 25 {
		t.Fatalf("readiness at the time = %d%%, want 25%%", got)
	}

	// Today someone decides the unfinished card is far bigger than they
	// thought. Recomputing the past from today's numbers would say
	// 1/98 = 1%; the journal says the point stays at 25%.
	setEstimate(t, env, big, 97)

	entries = journalOf(t, env)
	stillThen := readinessFrom(replayJournal(entries, cutoff), env.proj.EstimateUnit)
	if got := mustPercent(t, stillThen); got != 25 {
		t.Fatalf("the past point moved to %d%% after an estimate was edited today; it must stay at 25%%", got)
	}
	if stillThen.EstimateTotal != 4 {
		t.Fatalf("the past point now divides by %v, want the 4 that was current then", stillThen.EstimateTotal)
	}

	now := readinessFrom(replayJournal(entries, 0), env.proj.EstimateUnit)
	if got := mustPercent(t, now); got != 1 {
		t.Fatalf("today's readiness = %d%%, want 1%% (1 of 98)", got)
	}
	// And today's replayed figure is the live figure: one definition.
	live := liveReadiness(t, env)
	if *live.Percent != *now.Percent || live.EstimateTotal != now.EstimateTotal {
		t.Fatalf("live %+v disagrees with the replay to now %+v", live, now)
	}
}

// Acceptance 5: the consequence is stated on the result itself, in one line,
// so a surface drawing the curve has it in hand. It is a constant, not prose
// each renderer reinvents.
func TestReadiness_CarriesTheHistoricalCaveat(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "one")
	setEstimate(t, env, task, 2)

	if HistoricalReadinessNote == "" {
		t.Fatal("the historical caveat is empty")
	}
	if r := liveReadiness(t, env); r.HistoricalNote != HistoricalReadinessNote {
		t.Fatalf("live readiness note = %q, want the shared constant", r.HistoricalNote)
	}

	var svc Service = env.svc
	got, err := svc.ProgressHistory(context.Background(), env.actor, ProgressHistoryInput{
		ProjectKey:    env.proj.Key,
		IncludeReplay: true,
	})
	if err != nil {
		t.Fatalf("ProgressHistory: %v", err)
	}
	if len(got.Replay) == 0 {
		t.Fatal("no replayed points")
	}
	for i, p := range got.Replay {
		if p.Readiness.HistoricalNote != HistoricalReadinessNote {
			t.Fatalf("replayed point %d carries note %q, want the shared constant", i, p.Readiness.HistoricalNote)
		}
	}
	// The note has to actually say the thing, not merely be non-empty.
	low := strings.ToLower(HistoricalReadinessNote)
	for _, want := range []string{"estimates", "at the time"} {
		if !strings.Contains(low, want) {
			t.Fatalf("the caveat never mentions %q: %q", want, HistoricalReadinessNote)
		}
	}
}

// The unit is carried through as the project's own unit of effort. Nothing in
// here converts it into time remaining, and the type gives no way to.
func TestReadiness_KeepsTheProjectUnit(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "one")
	setEstimate(t, env, task, 5)

	if err := env.Write(context.Background(), func(tx store.Tx) error {
		p, err := env.Projects().GetByID(tx, env.proj.ID)
		if err != nil {
			return err
		}
		p.EstimateUnit = "pt"
		return env.Projects().Update(tx, p, nil)
	}); err != nil {
		t.Fatalf("change the project unit: %v", err)
	}
	if got := liveReadiness(t, env).Unit; got != "pt" {
		t.Fatalf("unit = %q, want pt", got)
	}
}

// readinessFrom is the one place a percentage is decided, so the boundary
// cases are pinned directly on it rather than only through a database.
func TestReadinessFrom_BoundaryCases(t *testing.T) {
	cases := []struct {
		name    string
		counts  boardCounts
		basis   ReadinessBasis
		percent *int
	}{
		{"nothing at all", boardCounts{}, ReadinessNone, nil},
		{"leaves, no estimates", boardCounts{Leaves: 4, DoneLeaves: 3}, ReadinessTaskCount, intPtrLocal(75)},
		{"estimates summing to zero", boardCounts{Leaves: 2, LeavesEstimated: 2}, ReadinessNone, nil},
		{"everything finished", boardCounts{
			Leaves: 2, DoneLeaves: 2, LeavesEstimated: 2, EstimateTotal: 8, EstimateDone: 8,
		}, ReadinessEstimates, intPtrLocal(100)},
		{"nothing finished", boardCounts{
			Leaves: 2, DoneLeaves: 0, LeavesEstimated: 2, EstimateTotal: 8,
		}, ReadinessEstimates, intPtrLocal(0)},
		{"rounds half up", boardCounts{
			Leaves: 2, DoneLeaves: 1, LeavesEstimated: 2, EstimateTotal: 8, EstimateDone: 3,
		}, ReadinessEstimates, intPtrLocal(38)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := readinessFrom(tc.counts, "h")
			if r.Basis != tc.basis {
				t.Fatalf("basis = %q, want %q", r.Basis, tc.basis)
			}
			switch {
			case tc.percent == nil && r.Percent != nil:
				t.Fatalf("percent = %d, want none", *r.Percent)
			case tc.percent != nil && r.Percent == nil:
				t.Fatalf("percent = none, want %d", *tc.percent)
			case tc.percent != nil && *r.Percent != *tc.percent:
				t.Fatalf("percent = %d, want %d", *r.Percent, *tc.percent)
			}
		})
	}
}

// The readiness on the live result and the readiness at the last point of the
// curve are the same number, because both go through readinessFrom. A board
// showing two automatic figures that disagree is worse than one that shows
// neither.
func TestReadiness_LiveAgreesWithTheLastHistoricalPoint(t *testing.T) {
	env := openTestEnv(t)
	a := makeBacklogTask(t, env, "a")
	b := makeBacklogTask(t, env, "b")
	c := makeBacklogTask(t, env, "c")
	setEstimate(t, env, a, 2)
	setEstimate(t, env, b, 6)
	moveTask(t, env, a, "Done")
	archiveTask(t, env, c, true)

	live := liveReadiness(t, env)
	points := buildHistoryPoints(journalOf(t, env), env.proj.EstimateUnit)
	last := points[len(points)-1].Readiness

	if live.Basis != last.Basis || *live.Percent != *last.Percent ||
		live.Leaves != last.Leaves || live.LeavesEstimated != last.LeavesEstimated ||
		live.EstimateTotal != last.EstimateTotal || live.EstimateDone != last.EstimateDone {
		t.Fatalf("live readiness %+v disagrees with the last historical point %+v", live, last)
	}
	if live.Basis != ReadinessEstimates || *live.Percent != 25 {
		t.Fatalf("readiness = %q %v, want estimates 25%%", live.Basis, live.Percent)
	}
}
