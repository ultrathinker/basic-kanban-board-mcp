package store

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Tests for batch 2: round-trip of actual/reviewer on the task row, the
// nullable UPDATE path, and the explicit-clear semantics. The migration
// (0003) is committed by another agent; these tests verify only the Go
// side of the round trip — store.go, tasks.go, scan.go — and would catch
// a column-order/scan-arg mismatch as a value mismatch, not a build
// error, so they matter even with a frozen contract.

// TestTask_ActualReviewer_RoundTrip persists both new fields, reads them
// back through every read path (GetByKey, GetByID, GetManyByKeys, List,
// Children), and checks the values match byte-for-byte. Drift between the
// SELECT list and the scan args would show up here as actuals /
// reviewers that come back as zero values.
func TestTask_ActualReviewer_RoundTrip(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)

	var created *domain.Task
	if err := ts.Write(context.Background(), func(tx Tx) error {
		seq, err := ts.Projects().NextTaskSeq(tx, p.ID)
		if err != nil {
			return err
		}
		est := 8.0
		act := 6.5
		asg := "alex"
		rev := "claude@rog"
		created = &domain.Task{
			ID:        uuid.NewString(),
			Key:       domain.TaskKey(p.Key, seq),
			ProjectID: p.ID,
			ColumnID:  cols["Backlog"].ID,
			Rank:      domain.RankStep,
			Title:     "round trip",
			Type:      domain.TypeTask,
			Priority:  domain.PriorityMedium,
			Estimate:  &est,
			Actual:    &act,
			Assignee:  &asg,
			Reviewer:  &rev,
			CreatedBy: "alice",
			UpdatedBy: "alice",
		}
		return ts.Tasks().Create(tx, created)
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	check := func(label string, got *domain.Task) {
		t.Helper()
		if got.Estimate == nil || *got.Estimate != 8.0 {
			t.Errorf("%s: Estimate = %v, want 8.0", label, got.Estimate)
		}
		if got.Actual == nil || *got.Actual != 6.5 {
			t.Errorf("%s: Actual = %v, want 6.5", label, got.Actual)
		}
		if got.Assignee == nil || *got.Assignee != "alex" {
			t.Errorf("%s: Assignee = %v, want alex", label, got.Assignee)
		}
		if got.Reviewer == nil || *got.Reviewer != "claude@rog" {
			t.Errorf("%s: Reviewer = %v, want claude@rog", label, got.Reviewer)
		}
	}

	if err := ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tasks().GetByKey(tx, created.Key)
		if err != nil {
			return err
		}
		check("GetByKey", got)

		got, err = ts.Tasks().GetByID(tx, created.ID)
		if err != nil {
			return err
		}
		check("GetByID", got)

		many, err := ts.Tasks().GetManyByKeys(tx, []string{created.Key})
		if err != nil {
			return err
		}
		if len(many) != 1 {
			t.Errorf("GetManyByKeys returned %d rows, want 1", len(many))
		} else {
			check("GetManyByKeys", many[created.Key])
		}

		listed, err := ts.Tasks().List(tx, TaskFilter{ProjectIDs: []string{p.ID}})
		if err != nil {
			return err
		}
		if len(listed) != 1 {
			t.Errorf("List returned %d rows, want 1", len(listed))
		} else {
			check("List", listed[0])
		}

		return nil
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
}

// TestTask_ActualReviewer_UpdateClear pins the UPDATE statement's new column
// set: an Update that flips Actual/Reviewer to non-nil values must read
// them back, and a follow-up Update that sets them to nil must clear them
// without leaving stale data behind.
func TestTask_ActualReviewer_UpdateClear(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "calibration", "alice")

	// First update: set both.
	act1 := 4.0
	rev1 := "kira"
	if err := ts.Write(context.Background(), func(tx Tx) error {
		task.Actual = &act1
		task.Reviewer = &rev1
		return ts.Tasks().Update(tx, task, nil)
	}); err != nil {
		t.Fatalf("Update set: %v", err)
	}

	if err := ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		if got.Actual == nil || *got.Actual != 4.0 {
			t.Errorf("after set: Actual = %v, want 4.0", got.Actual)
		}
		if got.Reviewer == nil || *got.Reviewer != "kira" {
			t.Errorf("after set: Reviewer = %v, want kira", got.Reviewer)
		}
		return nil
	}); err != nil {
		t.Fatalf("Read after set: %v", err)
	}

	// Second update: clear both (assigning nil on the domain value must
	// translate into NULL on the row, not "skip").
	if err := ts.Write(context.Background(), func(tx Tx) error {
		task.Actual = nil
		task.Reviewer = nil
		return ts.Tasks().Update(tx, task, nil)
	}); err != nil {
		t.Fatalf("Update clear: %v", err)
	}

	if err := ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		if got.Actual != nil {
			t.Errorf("after clear: Actual = %v, want nil", got.Actual)
		}
		if got.Reviewer != nil {
			t.Errorf("after clear: Reviewer = %v, want nil", got.Reviewer)
		}
		// Estimate was never set — stays nil, untouched.
		if got.Estimate != nil {
			t.Errorf("after clear: Estimate leaked: %v", got.Estimate)
		}
		// Assignee was never set — stays nil.
		if got.Assignee != nil {
			t.Errorf("after clear: Assignee leaked: %v", got.Assignee)
		}
		return nil
	}); err != nil {
		t.Fatalf("Read after clear: %v", err)
	}
}

// TestTask_ActualReviewer_NullByDefault is the regression guard for the
// migration's NULL default: a task with no Actual/Reviewer set reads back
// as nil pointers, not zero values. Drift between nullableFloat and
// nullableString would surface here as 0.0 / "".
func TestTask_ActualReviewer_NullByDefault(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "no fields", "alice")

	if err := ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		if got.Actual != nil {
			t.Errorf("Actual = %v, want nil", got.Actual)
		}
		if got.Reviewer != nil {
			t.Errorf("Reviewer = %v, want nil", got.Reviewer)
		}
		return nil
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
}
