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
