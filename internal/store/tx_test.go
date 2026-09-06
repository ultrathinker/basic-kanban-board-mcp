package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// Nested — savepoints
// ---------------------------------------------------------------------------

// TestNested_FailedItemLeavesNothingBehind is the test that proves the
// promise the nine tools make: a batch reports per-item results, and the
// item reported as failed must have written nothing at all.
//
// Each item here does several writes before it can fail — it renames the
// task, adds a note, and only then hits a version conflict — which is
// exactly the shape "validate everything first, then write" cannot cover
// once an item's work grows past a single statement.
func TestNested_FailedItemLeavesNothingBehind(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	tasks := []*domain.Task{
		seedTask(t, ts, p, cols["Backlog"], "one", "alice"),
		seedTask(t, ts, p, cols["Backlog"], "two", "alice"),
		seedTask(t, ts, p, cols["Backlog"], "three", "alice"),
	}
	const failing = 1

	ctx := context.Background()
	results := make([]error, len(tasks))
	err := ts.Write(ctx, func(tx Tx) error {
		for i, task := range tasks {
			results[i] = tx.Nested(func(itx Tx) error {
				cur, err := ts.Tasks().GetByID(itx, task.ID)
				if err != nil {
					return err
				}
				cur.Title = "renamed " + cur.Title
				if err := ts.Tasks().Update(itx, cur, nil); err != nil {
					return err
				}
				if err := ts.Notes().Add(itx, &domain.Note{
					ID:     uuid.NewString(),
					TaskID: task.ID,
					Author: "alice",
					Body:   "touched",
				}); err != nil {
					return err
				}
				if i == failing {
					// Fails only now, after the row and the note are
					// already written inside this unit.
					stale := 0
					return ts.Tasks().Update(itx, cur, &stale)
				}
				return nil
			})
		}
		// The batch itself succeeds; only one item did not.
		return nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	for i, res := range results {
		if i == failing {
			if res == nil {
				t.Fatalf("item %d: want an error, got nil", i)
			}
			if got := domain.AsError(res); got == nil || got.Code != domain.CodeConflict {
				t.Fatalf("item %d: want a conflict, got %v", i, res)
			}
			continue
		}
		if res != nil {
			t.Fatalf("item %d: unexpected error %v", i, res)
		}
	}

	err = ts.Read(ctx, func(tx Tx) error {
		for i, task := range tasks {
			cur, err := ts.Tasks().GetByID(tx, task.ID)
			if err != nil {
				return err
			}
			notes, err := ts.Notes().CountByTasks(tx, []string{task.ID})
			if err != nil {
				return err
			}
			if i == failing {
				if cur.Title != task.Title {
					t.Errorf("failed item: title = %q, want the original %q", cur.Title, task.Title)
				}
				if cur.Version != task.Version {
					t.Errorf("failed item: version = %d, want the original %d", cur.Version, task.Version)
				}
				if notes[task.ID] != 0 {
					t.Errorf("failed item: %d notes survived, want 0", notes[task.ID])
				}
				continue
			}
			if want := "renamed " + task.Title; cur.Title != want {
				t.Errorf("item %d: title = %q, want %q", i, cur.Title, want)
			}
			if cur.Version != task.Version+1 {
				t.Errorf("item %d: version = %d, want %d", i, cur.Version, task.Version+1)
			}
			if notes[task.ID] != 1 {
				t.Errorf("item %d: %d notes, want 1", i, notes[task.ID])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
}

// TestNested_NestsMoreThanOneLevel walks three levels down and fails the
// innermost one. Its parent must still be able to write afterwards, and
// everything above it must commit.
func TestNested_NestsMoreThanOneLevel(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()

	var keys []string
	mk := func(tx Tx, title string) error {
		task, err := newTaskIn(tx, ts, p, cols["Backlog"], title)
		if err != nil {
			return err
		}
		keys = append(keys, task.Key)
		return nil
	}

	err := ts.Write(ctx, func(tx Tx) error {
		return tx.Nested(func(l1 Tx) error {
			if err := mk(l1, "level-1"); err != nil {
				return err
			}
			return l1.Nested(func(l2 Tx) error {
				if err := mk(l2, "level-2"); err != nil {
					return err
				}
				inner := l2.Nested(func(l3 Tx) error {
					if err := mk(l3, "level-3-doomed"); err != nil {
						return err
					}
					return errors.New("boom")
				})
				if inner == nil {
					return errors.New("level 3 should have failed")
				}
				// The enclosing unit is still usable after its child rolled back.
				return mk(l2, "level-2-after")
			})
		})
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertTitles(t, ts, map[string]bool{
		"level-1":        true,
		"level-2":        true,
		"level-2-after":  true,
		"level-3-doomed": false,
	})
}

// TestNested_PanicDoesNotCommitTheUnitsWrites asserts that a panic inside
// fn is not a way to smuggle half an item into the transaction: the
// savepoint is rolled back on the way out, and the enclosing transaction
// still commits everything else.
func TestNested_PanicDoesNotCommitTheUnitsWrites(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)

	err := ts.Write(context.Background(), func(tx Tx) error {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("the panic did not reach the caller")
				}
			}()
			_ = tx.Nested(func(itx Tx) error {
				if _, err := newTaskIn(itx, ts, p, cols["Backlog"], "panicked"); err != nil {
					return err
				}
				panic("boom")
			})
		}()
		_, err := newTaskIn(tx, ts, p, cols["Backlog"], "after-panic")
		return err
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertTitles(t, ts, map[string]bool{
		"panicked":    false,
		"after-panic": true,
	})
}

// TestNested_RolledBackSavepointIsReleased is white-box on purpose:
// ROLLBACK TO rewinds a savepoint but leaves it on the stack, so a batch
// that rejects fifty items would accumulate fifty live frames. The only
// way to observe the RELEASE from outside is to try releasing the name
// again and require a failure.
func TestNested_RolledBackSavepointIsReleased(t *testing.T) {
	ts := openTestStore(t)
	ctx := context.Background()
	err := ts.Write(ctx, func(tx Tx) error {
		if e := tx.Nested(func(Tx) error { return errors.New("boom") }); e == nil {
			return errors.New("nested unit should have failed")
		}
		// The name is deterministic: the counter lives on the transaction
		// and this is its first savepoint.
		tw := tx.(*txWrap)
		if _, e := tw.tx.ExecContext(ctx, "RELEASE kanban_sp_1"); e == nil {
			return errors.New("kanban_sp_1 still exists; the rolled-back savepoint was not released")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// TestNested_SiblingUnitsGetDistinctSavepointNames guards the counter:
// two live savepoints sharing a name would make ROLLBACK TO unwind to the
// wrong one, silently discarding a sibling's committed work.
func TestNested_SiblingUnitsGetDistinctSavepointNames(t *testing.T) {
	st := &txState{}
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		name := st.nextSavepoint()
		if seen[name] {
			t.Fatalf("savepoint name %q handed out twice", name)
		}
		seen[name] = true
	}
}

// TestNested_SharesTheTransactionClock covers the trap the contract calls
// out: a nested unit is a scope inside one transaction, not a new point in
// time, so it must not re-read the clock.
func TestNested_SharesTheTransactionClock(t *testing.T) {
	ts := openTestStore(t)
	ctx := context.Background()

	t.Run("outer first", func(t *testing.T) {
		var outer, inner time.Time
		err := ts.Write(ctx, func(tx Tx) error {
			var err error
			if outer, err = tx.Now(); err != nil {
				return err
			}
			return tx.Nested(func(itx Tx) error {
				inner, err = itx.Now()
				return err
			})
		})
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if !outer.Equal(inner) {
			t.Fatalf("nested clock = %v, outer = %v", inner, outer)
		}
	})

	t.Run("nested first", func(t *testing.T) {
		var outer, inner time.Time
		err := ts.Write(ctx, func(tx Tx) error {
			err := tx.Nested(func(itx Tx) error {
				var e error
				inner, e = itx.Now()
				return e
			})
			if err != nil {
				return err
			}
			outer, err = tx.Now()
			return err
		})
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if !outer.Equal(inner) {
			t.Fatalf("outer clock = %v, nested = %v", outer, inner)
		}
	})
}

// TestNested_RetainedChildIsRefused covers the documented "do not retain
// the Tx passed to fn" rule with an error rather than a rollback that
// silently unwinds a scope the holder no longer owns.
func TestNested_RetainedChildIsRefused(t *testing.T) {
	ts := openTestStore(t)
	err := ts.Write(context.Background(), func(tx Tx) error {
		var child Tx
		if err := tx.Nested(func(itx Tx) error {
			child = itx
			return nil
		}); err != nil {
			return err
		}
		if err := child.Nested(func(Tx) error { return nil }); err == nil {
			return errors.New("a retained nested Tx was accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestNested_NilFunction(t *testing.T) {
	ts := openTestStore(t)
	err := ts.Write(context.Background(), func(tx Tx) error {
		if e := tx.Nested(nil); e == nil {
			return errors.New("nil fn was accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Now — the database clock, or an error
// ---------------------------------------------------------------------------

// TestNow_ReturnsErrorInsteadOfTheProcessClock is the whole point of the
// signature change. The clock is the ordering authority for lease expiry,
// done_at and the event log; a transaction that cannot read it must abort,
// not carry on with a timestamp from a different clock.
func TestNow_ReturnsErrorInsteadOfTheProcessClock(t *testing.T) {
	ts := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())

	var got time.Time
	var gotErr error
	var second error
	_ = ts.Write(ctx, func(tx Tx) error {
		cancel()
		got, gotErr = tx.Now()
		_, second = tx.Now()
		return errors.New("abort")
	})
	if gotErr == nil {
		t.Fatalf("Now() succeeded on a cancelled transaction, want an error")
	}
	if !got.IsZero() {
		t.Fatalf("Now() returned %v alongside an error; the process clock must never substitute", got)
	}
	// Sticky: a transaction may not get "no clock" once and a timestamp
	// the next time, or half its decisions would be stamped and half not.
	if second == nil || second.Error() != gotErr.Error() {
		t.Fatalf("second Now() = %v, want the same error as the first (%v)", second, gotErr)
	}
}

// TestNow_FailurePropagatesOutOfRepositoryWrites asserts the ripple:
// a repository that needs a timestamp reports the clock failure instead of
// writing a row stamped from somewhere else.
func TestNow_FailurePropagatesOutOfRepositoryWrites(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "clock", "alice")

	ctx, cancel := context.WithCancel(context.Background())
	var addErr error
	_ = ts.Write(ctx, func(tx Tx) error {
		cancel()
		addErr = ts.Notes().Add(tx, &domain.Note{
			ID:     uuid.NewString(),
			TaskID: task.ID,
			Author: "alice",
			Body:   "note",
		})
		return addErr
	})
	if addErr == nil {
		t.Fatalf("Notes().Add succeeded without a clock")
	}
	if !strings.Contains(addErr.Error(), "database clock") {
		t.Fatalf("Notes().Add error = %v, want the clock failure propagated verbatim", addErr)
	}
}

// TestNow_IsStableWithinOneTransaction keeps the property the old
// sync.Once provided: one transaction, one timestamp.
func TestNow_IsStableWithinOneTransaction(t *testing.T) {
	ts := openTestStore(t)
	err := ts.Write(context.Background(), func(tx Tx) error {
		first, err := tx.Now()
		if err != nil {
			return err
		}
		if first.IsZero() {
			return errors.New("Now() returned the zero time with no error")
		}
		if first.Location() != time.UTC {
			return errors.New("Now() is not UTC")
		}
		time.Sleep(2 * time.Millisecond)
		second, err := tx.Now()
		if err != nil {
			return err
		}
		if !first.Equal(second) {
			return errors.New("Now() moved inside one transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// TestArchive_TimestampRoundTrips guards a stamp that used to be handed to
// the driver as a time.Time: modernc formats those with time.Time.String(),
// which parseTime cannot read back, so an archived task became unreadable.
func TestArchive_TimestampRoundTrips(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "to-archive", "alice")

	ctx := context.Background()
	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Tasks().Archive(tx, task.ID, true, "alice")
	}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	err := ts.Read(ctx, func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		if cur.ArchivedAt == nil {
			return errors.New("ArchivedAt is nil after Archive(true)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read back archived task: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newTaskIn creates a task inside an existing transaction, which the
// package-level seedTask cannot do — it opens its own Write.
func newTaskIn(tx Tx, s Store, p *domain.Project, col *domain.Column, title string) (*domain.Task, error) {
	seq, err := s.Projects().NextTaskSeq(tx, p.ID)
	if err != nil {
		return nil, err
	}
	task := &domain.Task{
		ID:        uuid.NewString(),
		Key:       domain.TaskKey(p.Key, seq),
		ProjectID: p.ID,
		ColumnID:  col.ID,
		Rank:      domain.RankStep * int64(seq),
		Title:     title,
		Type:      domain.TypeTask,
		Priority:  domain.PriorityMedium,
		CreatedBy: "alice",
		UpdatedBy: "alice",
	}
	if err := s.Tasks().Create(tx, task); err != nil {
		return nil, err
	}
	return task, nil
}

// assertTitles checks which of the named titles exist after the
// transaction committed.
func assertTitles(t *testing.T, s Store, want map[string]bool) {
	t.Helper()
	found := map[string]bool{}
	err := s.Read(context.Background(), func(tx Tx) error {
		tasks, err := s.Tasks().List(tx, TaskFilter{})
		if err != nil {
			return err
		}
		for _, task := range tasks {
			found[task.Title] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	for title, wantPresent := range want {
		if found[title] != wantPresent {
			t.Errorf("task %q present = %v, want %v", title, found[title], wantPresent)
		}
	}
}
