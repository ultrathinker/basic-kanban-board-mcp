package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// The two bounded reads of progress_marks.
//
// This table is append-only and nothing ever thins it — the migration says so
// and the pruner cannot reach it. So every read of it that is on a hot path,
// or that an agent can issue in a loop, has to be bounded IN SQL. Reading the
// whole scope and keeping a slice of it in Go returns the same answer while
// doing exactly the work the bound was supposed to avoid, and no assertion on
// the returned value can tell the two apart. These tests therefore pin the
// observable consequences a read-then-slice implementation could not fake:
// the total is reported separately from the page, and the page is the NEWEST
// marks rather than the first ones the table happens to yield.
// ---------------------------------------------------------------------------

func TestProgressMarks_HistoryTail_KeepsTheNewestAndReportsTheTotal(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "A", "tester")

	// Ten marks, one per hour, percent == index so the identity of each row
	// is readable straight off the assertion.
	const n = 10
	for i := 0; i < n; i++ {
		addMark(t, ts, &domain.ProgressMark{
			ID: fmt.Sprintf("pm-tail-%02d", i), ProjectID: p.ID, TaskID: &task.ID,
			Assessor: "alpha", Percent: i,
			CreatedAt: progressBase.Add(time.Duration(i) * time.Hour),
		})
	}

	tail := func(limit int) ([]domain.ProgressMark, int) {
		t.Helper()
		var marks []domain.ProgressMark
		var total int
		if err := ts.Read(context.Background(), func(tx Tx) error {
			var err error
			marks, total, err = ts.Progress().HistoryTail(tx, p.ID, &task.ID, limit)
			return err
		}); err != nil {
			t.Fatalf("HistoryTail(%d): %v", limit, err)
		}
		return marks, total
	}

	marks, total := tail(3)
	if total != n {
		t.Errorf("total = %d, want %d — the total is the SCOPE size, not the page size", total, n)
	}
	if len(marks) != 3 {
		t.Fatalf("len(marks) = %d, want 3", len(marks))
	}
	// The newest three (7, 8, 9), and among themselves still oldest-first:
	// the one chronological order this table is ever read in.
	for i, want := range []int{7, 8, 9} {
		if marks[i].Percent != want {
			t.Errorf("marks[%d].Percent = %d, want %d (the newest three, oldest-first)", i, marks[i].Percent, want)
		}
	}

	// A limit at or above the scope size returns everything, and total still
	// agrees with it.
	marks, total = tail(n * 5)
	if len(marks) != n || total != n {
		t.Fatalf("oversized limit: len=%d total=%d, want %d and %d", len(marks), total, n, n)
	}
	if marks[0].Percent != 0 || marks[n-1].Percent != n-1 {
		t.Errorf("oversized limit changed the order: first=%d last=%d", marks[0].Percent, marks[n-1].Percent)
	}

	// An empty scope is not an error: no marks, and a total of zero.
	var emptyTotal int
	var emptyMarks []domain.ProgressMark
	if err := ts.Read(context.Background(), func(tx Tx) error {
		var err error
		emptyMarks, emptyTotal, err = ts.Progress().HistoryTail(tx, p.ID, nil, 5)
		return err
	}); err != nil {
		t.Fatalf("HistoryTail on an empty scope: %v", err)
	}
	if len(emptyMarks) != 0 || emptyTotal != 0 {
		t.Errorf("empty project scope: len=%d total=%d, want 0 and 0", len(emptyMarks), emptyTotal)
	}

	// A non-positive limit is a programming error, not "give me everything":
	// the caller that wants everything calls History, which says so.
	if err := ts.Read(context.Background(), func(tx Tx) error {
		_, _, err := ts.Progress().HistoryTail(tx, p.ID, &task.ID, 0)
		return err
	}); err == nil {
		t.Error("HistoryTail(0) returned no error; a zero limit must be refused, not read as unbounded")
	}
}

// TestProgressMarks_CountsByAssessor is CountsByTask's project-scope twin: it
// must count ONLY project-level marks (task_id IS NULL), so the board's
// "N assessments" figure never silently folds in per-card opinions.
func TestProgressMarks_CountsByAssessor(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "A", "tester")

	addMark(t, ts, &domain.ProgressMark{ID: "pm-pa1", ProjectID: p.ID, Assessor: "alpha", Percent: 10, CreatedAt: progressBase})
	addMark(t, ts, &domain.ProgressMark{ID: "pm-pa2", ProjectID: p.ID, Assessor: "alpha", Percent: 40, CreatedAt: progressBase.Add(time.Second)})
	addMark(t, ts, &domain.ProgressMark{ID: "pm-pb1", ProjectID: p.ID, Assessor: "beta", Percent: 60, CreatedAt: progressBase.Add(2 * time.Second)})
	// Task-level marks must NOT be counted here.
	addMark(t, ts, &domain.ProgressMark{ID: "pm-t1", ProjectID: p.ID, TaskID: &task.ID, Assessor: "alpha", Percent: 99, CreatedAt: progressBase.Add(3 * time.Second)})
	addMark(t, ts, &domain.ProgressMark{ID: "pm-t2", ProjectID: p.ID, TaskID: &task.ID, Assessor: "gamma", Percent: 99, CreatedAt: progressBase.Add(4 * time.Second)})

	// Nor must another project's.
	other := &domain.Project{
		ID: "other-project-assessor", Key: "OTHERA", Name: "Other",
		Version: 1, NextTaskSeq: 1, EstimateUnit: "h",
		EnforceDependencies: true, StrictDone: false, ClaimTTLSeconds: 3600,
	}
	if err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Projects().Create(tx, other)
	}); err != nil {
		t.Fatalf("seed other project: %v", err)
	}
	addMark(t, ts, &domain.ProgressMark{ID: "pm-o1", ProjectID: other.ID, Assessor: "ghost", Percent: 1})

	var got map[string]int
	if err := ts.Read(context.Background(), func(tx Tx) error {
		var err error
		got, err = ts.Progress().CountsByAssessor(tx, p.ID)
		return err
	}); err != nil {
		t.Fatalf("CountsByAssessor: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("assessors counted = %d (%v), want 2 — gamma only marked a TASK, and ghost is another project", len(got), got)
	}
	if got["alpha"] != 2 {
		t.Errorf("alpha = %d, want 2 (its task-level mark must not be folded in)", got["alpha"])
	}
	if got["beta"] != 1 {
		t.Errorf("beta = %d, want 1", got["beta"])
	}
	if _, ok := got["gamma"]; ok {
		t.Error("gamma appears in the project-level counts, but only ever marked a task")
	}
	if _, ok := got["ghost"]; ok {
		t.Error("another project's assessor leaked across the project boundary")
	}
}
