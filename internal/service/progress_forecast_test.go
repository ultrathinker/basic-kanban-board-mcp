package service

import (
	"context"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// addForecastMark appends one mark carrying an optional ETA, through the
// store, the same way addProgressMark (progress_test.go) does for a bare
// percent. eta may be nil (a percent-only update, restating nothing).
func addForecastMark(t *testing.T, env *testEnv, id string, taskID *string, assessor string, percent int, at time.Time, eta *time.Time) {
	t.Helper()
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Progress().Add(tx, &domain.ProgressMark{
			ID:        id,
			ProjectID: env.proj.ID,
			TaskID:    taskID,
			Assessor:  assessor,
			Percent:   percent,
			ETA:       eta,
			CreatedAt: at,
		})
	}); err != nil {
		t.Fatalf("add forecast mark %s: %v", id, err)
	}
}

// forecastETAString renders an optional forecast ETA for failure messages.
func forecastETAString(eta *time.Time) string {
	if eta == nil {
		return "nil"
	}
	return eta.Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// 1. Several assessors currently have a forecast: the LATEST (most
//    pessimistic) date wins, never an average, and the name attached must
//    be the assessor who actually gave that date.
// ---------------------------------------------------------------------------

func TestTaskProgress_ForecastPicksLatestNotAverage(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	task := makeBacklogTask(t, env, "several forecasters")

	etaAlpha := progressTestBase.Add(3 * 24 * time.Hour) // earliest
	etaBeta := progressTestBase.Add(10 * 24 * time.Hour) // latest: this must win
	etaGamma := progressTestBase.Add(6 * 24 * time.Hour) // middle
	// An average of the three would land around day 6.33 — nowhere near
	// beta's day 10, which is the whole point of this test.

	addForecastMark(t, env, "f1", &task.ID, "alpha", 40, progressTestBase, &etaAlpha)
	addForecastMark(t, env, "f2", &task.ID, "beta", 60, progressTestBase.Add(time.Minute), &etaBeta)
	addForecastMark(t, env, "f3", &task.ID, "gamma", 50, progressTestBase.Add(2*time.Minute), &etaGamma)

	res, err := env.svc.TaskProgress(ctx, env.actor, TaskProgressInput{ProjectKey: "BMB", Keys: []string{task.Key}})
	if err != nil {
		t.Fatalf("task_progress: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(res.Items))
	}
	item := res.Items[0]
	if item.ForecastETA == nil {
		t.Fatal("expected a forecast, got nil")
	}
	if !item.ForecastETA.Equal(etaBeta) {
		t.Fatalf("forecast = %s, want beta's latest date %s (not an average)", forecastETAString(item.ForecastETA), etaBeta.Format(time.RFC3339))
	}
	if item.ForecastBy != "beta" {
		t.Fatalf("forecast attributed to %q, want %q — the name must belong to the date shown", item.ForecastBy, "beta")
	}
}

// ---------------------------------------------------------------------------
// 2. An assessor's OLD forecast does not linger once their latest mark
//    restates percent without restating a date: the append-only model has
//    no honest way to say "my old promise still holds", so a stale forecast
//    must not win over a live one from someone else, and must not appear at
//    all if it was the only one.
// ---------------------------------------------------------------------------

func TestTaskProgress_ForecastDropsWhenLatestMarkOmitsETA(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	task := makeBacklogTask(t, env, "stale forecast")

	oldETA := progressTestBase.Add(20 * 24 * time.Hour)
	addForecastMark(t, env, "f1", &task.ID, "alpha", 40, progressTestBase, &oldETA)
	// alpha's newer mark reports fresh percent but no new date.
	addForecastMark(t, env, "f2", &task.ID, "alpha", 55, progressTestBase.Add(time.Hour), nil)

	res, err := env.svc.TaskProgress(ctx, env.actor, TaskProgressInput{ProjectKey: "BMB", Keys: []string{task.Key}})
	if err != nil {
		t.Fatalf("task_progress: %v", err)
	}
	item := res.Items[0]
	if item.Percent == nil || *item.Percent != 55 {
		t.Fatalf("percent should still update to the latest mark (55), got %s", percentString(item.Percent))
	}
	if item.ForecastETA != nil {
		t.Fatalf("stale forecast leaked through: got %s, want nil (alpha's latest mark did not restate a date)", forecastETAString(item.ForecastETA))
	}
	if item.ForecastBy != "" {
		t.Fatalf("ForecastBy = %q, want empty alongside a nil ForecastETA", item.ForecastBy)
	}
}

// ---------------------------------------------------------------------------
// 3. Nobody has ever given a forecast for this task: nil/"" — the same
//    "nothing to render" fact the view layer turns into "no badge at all".
// ---------------------------------------------------------------------------

func TestTaskProgress_NoForecastWhenNobodyGaveOne(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	task := makeBacklogTask(t, env, "percent only")

	addForecastMark(t, env, "f1", &task.ID, "alpha", 40, progressTestBase, nil)
	addForecastMark(t, env, "f2", &task.ID, "beta", 60, progressTestBase.Add(time.Minute), nil)

	res, err := env.svc.TaskProgress(ctx, env.actor, TaskProgressInput{ProjectKey: "BMB", Keys: []string{task.Key}})
	if err != nil {
		t.Fatalf("task_progress: %v", err)
	}
	item := res.Items[0]
	if item.Percent == nil || *item.Percent != 50 {
		t.Fatalf("percent = %s, want 50 (mean of 40 and 60)", percentString(item.Percent))
	}
	if item.ForecastETA != nil || item.ForecastBy != "" {
		t.Fatalf("expected no forecast, got ETA=%s by=%q", forecastETAString(item.ForecastETA), item.ForecastBy)
	}
}

// ---------------------------------------------------------------------------
// 4. A forecast exactly at the boundary of "now" and one already in the
//    past: the SERVICE layer reports the raw date either way (it is the
//    VIEW layer's job — view.NewAssessedProgress — to decide "overdue"
//    against wall-clock time). This pins that the service never mutates or
//    drops a past-due forecast; it is still the honest freshest promise.
// ---------------------------------------------------------------------------

func TestTaskProgress_PastForecastStillReported(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	task := makeBacklogTask(t, env, "already overdue")

	// Truncated to millisecond precision: the store's TEXT timestamp column
	// round-trips at millisecond resolution (internal/store/scan.go), so a
	// raw time.Now() would fail time.Equal after the read-back by a sub-
	// millisecond rounding difference that has nothing to do with what this
	// test is pinning.
	past := time.Now().UTC().Truncate(time.Millisecond).Add(-48 * time.Hour)
	addForecastMark(t, env, "f1", &task.ID, "alpha", 90, progressTestBase, &past)

	res, err := env.svc.TaskProgress(ctx, env.actor, TaskProgressInput{ProjectKey: "BMB", Keys: []string{task.Key}})
	if err != nil {
		t.Fatalf("task_progress: %v", err)
	}
	item := res.Items[0]
	if item.ForecastETA == nil || !item.ForecastETA.Equal(past) {
		t.Fatalf("forecast = %s, want the past date %s reported as-is", forecastETAString(item.ForecastETA), past.Format(time.RFC3339))
	}
	if item.ForecastBy != "alpha" {
		t.Fatalf("ForecastBy = %q, want %q", item.ForecastBy, "alpha")
	}
}

// ---------------------------------------------------------------------------
// 5. Tie: two assessors currently promise the EXACT same instant. The
//    result must be deterministic (alphabetically-first assessor), not
//    flaky map-iteration order.
// ---------------------------------------------------------------------------

func TestTaskProgress_ForecastTieBreaksDeterministically(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	task := makeBacklogTask(t, env, "tied forecast")

	same := progressTestBase.Add(5 * 24 * time.Hour)
	addForecastMark(t, env, "f1", &task.ID, "zeta", 40, progressTestBase, &same)
	addForecastMark(t, env, "f2", &task.ID, "alpha", 60, progressTestBase.Add(time.Minute), &same)

	res, err := env.svc.TaskProgress(ctx, env.actor, TaskProgressInput{ProjectKey: "BMB", Keys: []string{task.Key}})
	if err != nil {
		t.Fatalf("task_progress: %v", err)
	}
	item := res.Items[0]
	if item.ForecastETA == nil || !item.ForecastETA.Equal(same) {
		t.Fatalf("forecast = %s, want the tied date %s", forecastETAString(item.ForecastETA), same.Format(time.RFC3339))
	}
	if item.ForecastBy != "alpha" {
		t.Fatalf("tie-break: ForecastBy = %q, want the alphabetically-first assessor %q", item.ForecastBy, "alpha")
	}
}

// ---------------------------------------------------------------------------
// 6. Project-level ("Manual") forecast mirrors the same pick-latest rule
//    over the project-level track only — proving the wiring into
//    ProjectProgress, independent of the task-level wiring above.
// ---------------------------------------------------------------------------

func TestProjectProgress_ManualForecastPicksLatest(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	etaAlpha := progressTestBase.Add(4 * 24 * time.Hour)
	etaBeta := progressTestBase.Add(9 * 24 * time.Hour) // latest: must win

	addForecastMark(t, env, "p1", nil, "alpha", 30, progressTestBase, &etaAlpha)
	addForecastMark(t, env, "p2", nil, "beta", 45, progressTestBase.Add(time.Minute), &etaBeta)

	res, err := env.svc.ProjectProgress(ctx, env.actor, ProjectProgressInput{ProjectKey: "BMB"})
	if err != nil {
		t.Fatalf("project_progress: %v", err)
	}
	if res.ManualForecastETA == nil || !res.ManualForecastETA.Equal(etaBeta) {
		t.Fatalf("manual forecast = %s, want beta's latest date %s", forecastETAString(res.ManualForecastETA), etaBeta.Format(time.RFC3339))
	}
	if res.ManualForecastBy != "beta" {
		t.Fatalf("ManualForecastBy = %q, want %q", res.ManualForecastBy, "beta")
	}
}

// TestProjectProgress_NoManualForecastWhenNoneGiven pins the empty case at
// the project level too.
func TestProjectProgress_NoManualForecastWhenNoneGiven(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	res, err := env.svc.ProjectProgress(ctx, env.actor, ProjectProgressInput{ProjectKey: "BMB"})
	if err != nil {
		t.Fatalf("project_progress: %v", err)
	}
	if res.ManualForecastETA != nil || res.ManualForecastBy != "" {
		t.Fatalf("expected no manual forecast on an unassessed project, got ETA=%s by=%q", forecastETAString(res.ManualForecastETA), res.ManualForecastBy)
	}
}
