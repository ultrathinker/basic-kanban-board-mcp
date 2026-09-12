package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

func TestService_ImplementsService(t *testing.T) {
	var _ Service = (*svc)(nil)
}

func TestProgressSet_TaskAndProjectInStore(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	task := makeBacklogTask(t, env, "sample task for progress")
	eta := progressTestBase.Add(24 * time.Hour)

	// 1. Task progress mark
	taskRes, err := env.svc.ProgressSet(ctx, env.actor, ProgressSetInput{
		Assessor: "agent-1",
		Percent:  35,
		TaskKey:  task.Key,
		ETA:      &eta,
	})
	if err != nil {
		t.Fatalf("ProgressSet task: %v", err)
	}
	if taskRes.TaskKey != task.Key {
		t.Errorf("taskKey = %q, want %q", taskRes.TaskKey, task.Key)
	}
	if taskRes.ProjectKey != env.proj.Key {
		t.Errorf("projectKey = %q, want %q", taskRes.ProjectKey, env.proj.Key)
	}
	if taskRes.SummaryPercent == nil || *taskRes.SummaryPercent != 35 {
		t.Errorf("summary percent = %v, want 35", taskRes.SummaryPercent)
	}
	if taskRes.Assessors != 1 {
		t.Errorf("assessors = %d, want 1", taskRes.Assessors)
	}
	if taskRes.Mark.TaskID == nil || *taskRes.Mark.TaskID != task.ID {
		t.Errorf("mark TaskID = %v, want %s", taskRes.Mark.TaskID, task.ID)
	}

	// 2. Project progress mark (task_id is nil in store)
	projRes, err := env.svc.ProgressSet(ctx, env.actor, ProgressSetInput{
		Assessor:   "project-evaluator",
		Percent:    60,
		ProjectKey: env.proj.Key,
	})
	if err != nil {
		t.Fatalf("ProgressSet project: %v", err)
	}
	if projRes.TaskKey != "" {
		t.Errorf("taskKey = %q, want empty for project-level", projRes.TaskKey)
	}
	if projRes.ProjectKey != env.proj.Key {
		t.Errorf("projectKey = %q, want %q", projRes.ProjectKey, env.proj.Key)
	}
	if projRes.SummaryPercent == nil || *projRes.SummaryPercent != 60 {
		t.Errorf("summary percent = %v, want 60", projRes.SummaryPercent)
	}
	if projRes.Assessors != 1 {
		t.Errorf("assessors = %d, want 1", projRes.Assessors)
	}
	if projRes.Mark.TaskID != nil {
		t.Errorf("mark TaskID = %v, want nil for project-level", projRes.Mark.TaskID)
	}
}

func TestProgressSet_TwoCallsSameAssessor_SummaryTakesSecond(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	task := makeBacklogTask(t, env, "task with multiple revisions")

	// Call 1: alpha sets 30
	res1, err := env.svc.ProgressSet(ctx, env.actor, ProgressSetInput{
		Assessor: "alpha",
		Percent:  30,
		TaskKey:  task.Key,
	})
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if *res1.SummaryPercent != 30 || res1.Assessors != 1 {
		t.Errorf("call 1 summary = %v, assessors = %d", res1.SummaryPercent, res1.Assessors)
	}

	time.Sleep(15 * time.Millisecond)

	// Call 2: same assessor alpha updates to 80
	res2, err := env.svc.ProgressSet(ctx, env.actor, ProgressSetInput{
		Assessor: "alpha",
		Percent:  80,
		TaskKey:  task.Key,
	})
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	// Summary must be 80, NOT the mean of 30 and 80 (55)
	if *res2.SummaryPercent != 80 || res2.Assessors != 1 {
		t.Errorf("call 2 summary = %v (want 80), assessors = %d (want 1)", res2.SummaryPercent, res2.Assessors)
	}

	// Verify history in store has both entries
	if err := env.Read(ctx, func(tx store.Tx) error {
		hist, err := env.Progress().History(tx, env.proj.ID, &task.ID)
		if err != nil {
			return err
		}
		if len(hist) != 2 {
			t.Fatalf("history len = %d, want 2", len(hist))
		}
		if hist[0].Percent != 30 || hist[1].Percent != 80 {
			t.Fatalf("history percents = [%d, %d], want [30, 80]", hist[0].Percent, hist[1].Percent)
		}
		return nil
	}); err != nil {
		t.Fatalf("read history: %v", err)
	}

	// Call 3: another assessor beta sets 40 -> summary is mean(80, 40) = 60
	res3, err := env.svc.ProgressSet(ctx, env.actor, ProgressSetInput{
		Assessor: "beta",
		Percent:  40,
		TaskKey:  task.Key,
	})
	if err != nil {
		t.Fatalf("call 3: %v", err)
	}
	if *res3.SummaryPercent != 60 || res3.Assessors != 2 {
		t.Errorf("call 3 summary = %v (want 60), assessors = %d (want 2)", res3.SummaryPercent, res3.Assessors)
	}
}

func TestProgressSet_RollbackDown_SavedAndReadFromHistory(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	task := makeBacklogTask(t, env, "task with downward revision")

	// First estimate: 70%
	_, err := env.svc.ProgressSet(ctx, env.actor, ProgressSetInput{
		Assessor: "alpha",
		Percent:  70,
		TaskKey:  task.Key,
	})
	if err != nil {
		t.Fatalf("set 70: %v", err)
	}

	time.Sleep(15 * time.Millisecond)

	// Rollback estimate: 45% (downward revision)
	res2, err := env.svc.ProgressSet(ctx, env.actor, ProgressSetInput{
		Assessor: "alpha",
		Percent:  45,
		TaskKey:  task.Key,
	})
	if err != nil {
		t.Fatalf("set 45: %v", err)
	}
	if *res2.SummaryPercent != 45 {
		t.Errorf("summary after downgrade = %v, want 45", res2.SummaryPercent)
	}

	// Read history directly from store to prove rollback is preserved
	if err := env.Read(ctx, func(tx store.Tx) error {
		hist, err := env.Progress().History(tx, env.proj.ID, &task.ID)
		if err != nil {
			return err
		}
		if len(hist) != 2 {
			t.Fatalf("history length = %d, want 2", len(hist))
		}
		if hist[0].Percent != 70 {
			t.Errorf("hist[0].Percent = %d, want 70", hist[0].Percent)
		}
		if hist[1].Percent != 45 {
			t.Errorf("hist[1].Percent = %d, want 45", hist[1].Percent)
		}
		return nil
	}); err != nil {
		t.Fatalf("read history: %v", err)
	}
}

func TestProgressSet_ReadOnlyScopeRejected(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	task := makeBacklogTask(t, env, "read only check")

	reader := env.actor
	reader.Scopes = domain.Scopes{domain.ScopeRead}

	_, err := env.svc.ProgressSet(ctx, reader, ProgressSetInput{
		Assessor: "alpha",
		Percent:  50,
		TaskKey:  task.Key,
	})
	if err == nil {
		t.Fatal("read-only actor set progress; want forbidden error")
	}
	if !strings.Contains(err.Error(), "write") {
		t.Errorf("error %q should mention missing write scope", err.Error())
	}
}

// writeSpyStore wraps a store.Store and counts calls to Write. Embedding
// store.Store forwards every other method (Read, Projects(), Progress(),
// ...) unchanged, so this only ever intercepts Write.
//
// It exists to tell a rejection the SERVICE made, before it ever opened a
// write transaction, apart from a rejection that only surfaced once a write
// transaction was already open and the STORE's own constraint fired inside
// it. That distinction matters here specifically: internal/store/progress.go
// maps a percent-column CHECK-constraint failure to the exact same
// domain.Invalid("percent", "progress percent %d is outside 0..100", ...)
// the service's own range check produces (byte-for-byte identical message
// and remediation), so neither the error code, the field name, nor the
// message text can tell the two apart. Whether Write was ever called can:
// the service's own checks all run before it calls store.Write, so a real
// service-layer rejection must leave writeCalls at 0.
type writeSpyStore struct {
	store.Store
	writeCalls int
}

func (s *writeSpyStore) Write(ctx context.Context, fn func(store.Tx) error) error {
	s.writeCalls++
	return s.Store.Write(ctx, fn)
}

// expectProgressSetValidation asserts that ProgressSet itself refuses in,
// with a CodeValidation error naming wantField, that it does so WITHOUT ever
// opening a write transaction (see writeSpyStore above — this is what proves
// the service's own boundary fired, not some other layer producing a
// same-shaped error), and that nothing was written for the given scope (task
// history when taskID != nil, project history otherwise). The service under
// test here is built fresh on a writeSpyStore wrapping env's real store, not
// env.svc directly, purely so writeCalls can be observed; it is the same
// store and the same data. Calling the real service (not the MCP tool
// wrapper) is the point: it is the only way to know the service's own rule
// fired, rather than a duplicate check living somewhere upstream of it (the
// MCP schema) or downstream of it (a store-level constraint).
func expectProgressSetValidation(t *testing.T, env *testEnv, in ProgressSetInput, wantField string, taskID *string) {
	t.Helper()
	ctx := context.Background()

	before := progressHistoryLen(t, env, taskID)

	spy := &writeSpyStore{Store: env.Store}
	svcUnderTest := New(spy, nil)

	_, err := svcUnderTest.ProgressSet(ctx, env.actor, in)
	if err == nil {
		t.Fatal("ProgressSet accepted invalid input; want a validation error")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation {
		t.Fatalf("error = %v (%T), want a domain.CodeValidation error", err, err)
	}
	if de.Field != wantField {
		t.Errorf("error field = %q, want %q (message: %s)", de.Field, wantField, de.Message)
	}
	if spy.writeCalls != 0 {
		t.Errorf("ProgressSet opened %d write transaction(s) before refusing; a genuine "+
			"service-layer rejection must reject BEFORE ever calling store.Write — a "+
			"transaction being opened here means some other layer (e.g. a store-level "+
			"CHECK constraint) is doing the actual rejecting, and the service's own check "+
			"may be gone or dead code", spy.writeCalls)
	}

	after := progressHistoryLen(t, env, taskID)
	if after != before {
		t.Errorf("history length changed from %d to %d; a refused ProgressSet must write nothing", before, after)
	}
}

func progressHistoryLen(t *testing.T, env *testEnv, taskID *string) int {
	t.Helper()
	var n int
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		hist, err := env.Progress().History(tx, env.proj.ID, taskID)
		if err != nil {
			return err
		}
		n = len(hist)
		return nil
	}); err != nil {
		t.Fatalf("read history: %v", err)
	}
	return n
}

// TestProgressSet_AssessorTooLongRejectedByService is the canary target: an
// independent review found that domain.ValidateActorName("assessor", ...) is
// called in the service (progress_set.go), but the only test guarding an
// over-long assessor name lives in internal/mcp and is actually satisfied by
// the tool's JSON-schema maxLength — the MCP test uses a fake service and
// asserts the fake was never called, so it would stay green even if this
// service-level check were deleted entirely. This test calls the real
// service directly, bypassing any schema, so IT is the one guarding the
// service boundary.
func TestProgressSet_AssessorTooLongRejectedByService(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "assessor bound check")

	longName := strings.Repeat("x", domain.MaxAssigneeLen+1)
	expectProgressSetValidation(t, env, ProgressSetInput{
		Assessor: longName,
		Percent:  50,
		TaskKey:  task.Key,
	}, "assessor", &task.ID)
}

// TestProgressSet_EmptyAssessorRejectedByService closes the same class of gap
// for the "assessor required" branch: the MCP-level test for this
// (TestProgressSet_EmptyAssessorRejected) also only exercises the tool
// wrapper's own duplicate check against a fake service.
func TestProgressSet_EmptyAssessorRejectedByService(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "assessor required check")

	expectProgressSetValidation(t, env, ProgressSetInput{
		Assessor: "   ",
		Percent:  50,
		TaskKey:  task.Key,
	}, "assessor", &task.ID)
}

// TestProgressSet_PercentOutOfRangeRejectedByService covers both directions
// of the 0..100 bound directly against the service. As with the assessor
// checks above, the MCP-level equivalent (TestProgressSet_PercentAndEtaValidation)
// only proves the tool wrapper's own duplicate range check fires, against a
// fake service that records whether it was called.
func TestProgressSet_PercentOutOfRangeRejectedByService(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "percent bound check")

	cases := []struct {
		name    string
		percent int
	}{
		{"negative", -5},
		{"over_100", 105},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectProgressSetValidation(t, env, ProgressSetInput{
				Assessor: "alpha",
				Percent:  tc.percent,
				TaskKey:  task.Key,
			}, "percent", &task.ID)
		})
	}
}

// TestProgressSet_BothTaskAndProjectRejectedByService and
// TestProgressSet_NeitherTaskNorProjectRejectedByService cover the mutual-
// exclusion rule on the service itself. The MCP-level equivalent
// (TestProgressSet_MutuallyExclusiveTargetValidation) only proves the tool
// wrapper's own duplicate check fires before ever reaching a fake service.
func TestProgressSet_BothTaskAndProjectRejectedByService(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "both target check")

	expectProgressSetValidation(t, env, ProgressSetInput{
		Assessor:   "alpha",
		Percent:    50,
		TaskKey:    task.Key,
		ProjectKey: env.proj.Key,
	}, "task", &task.ID)
}

func TestProgressSet_NeitherTaskNorProjectRejectedByService(t *testing.T) {
	env := openTestEnv(t)

	expectProgressSetValidation(t, env, ProgressSetInput{
		Assessor: "alpha",
		Percent:  50,
	}, "task", nil)
}
