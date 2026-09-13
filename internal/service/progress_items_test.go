package service

import (
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// buildItemCounts replays task lifespans into the two curves the item chart
// draws. The behaviour worth pinning is not the arithmetic but the SHAPE:
// the total is allowed to rise (work noticed along the way is the normal
// case on this board, and a fixed denominator would hide it), the open count
// follows both forces, and it reaches zero exactly when nothing is left.

func at(h int) time.Time {
	return time.Date(2026, 9, 12, h, 0, 0, 0, time.UTC)
}

func spanAt(created int, done *int) store.ItemLifespan {
	s := store.ItemLifespan{CreatedAt: at(created)}
	if done != nil {
		d := at(*done)
		s.DoneAt = &d
	}
	return s
}

func hour(h int) *int { return &h }

func TestBuildItemCounts_ScopeGrowsAndOpenReachesZero(t *testing.T) {
	// Three tasks up front, all finished; two more noticed later, also
	// finished. This is the owner's own description of how the board is
	// used: the total is not known at the start.
	spans := []store.ItemLifespan{
		spanAt(1, hour(4)),
		spanAt(1, hour(5)),
		spanAt(1, hour(9)),
		spanAt(6, hour(8)),
		spanAt(6, hour(10)),
	}

	got := buildItemCounts(spans)
	if len(got) == 0 {
		t.Fatal("no points produced")
	}

	// The first instant: three items exist, all open.
	if got[0].At != at(1) || got[0].Total != 3 || got[0].Open != 3 {
		t.Fatalf("first point = %+v, want {1h, total 3, open 3}", got[0])
	}

	// The total must RISE when work is added — the whole reason both curves
	// are drawn. At hour 6 two more items appear.
	var atSix *ItemCountPoint
	for i := range got {
		if got[i].At.Equal(at(6)) {
			atSix = &got[i]
		}
	}
	if atSix == nil {
		t.Fatal("no point at the instant two tasks were added")
	}
	if atSix.Total != 5 {
		t.Errorf("total at hour 6 = %d, want 5 (scope grew)", atSix.Total)
	}
	// One had been finished by then (hour 4 and hour 5 both landed), so
	// three of the five are open: 5 total - 2 done.
	if atSix.Open != 3 {
		t.Errorf("open at hour 6 = %d, want 3", atSix.Open)
	}

	// The last point: everything finished, nothing open. A burn-up that
	// cannot reach zero is not answering the question it was asked.
	last := got[len(got)-1]
	if last.Total != 5 {
		t.Errorf("final total = %d, want 5", last.Total)
	}
	if last.Open != 0 {
		t.Errorf("final open = %d, want 0 — every task is in a done column", last.Open)
	}

	// The open count must never go negative or exceed the total at any
	// point: both would mean the replay lost track of an item.
	for i, p := range got {
		if p.Open < 0 {
			t.Errorf("point %d (%+v): open went negative", i, p)
		}
		if p.Open > p.Total {
			t.Errorf("point %d (%+v): more open than exist", i, p)
		}
	}
}

func TestBuildItemCounts_UnfinishedWorkStaysOpen(t *testing.T) {
	got := buildItemCounts([]store.ItemLifespan{
		spanAt(1, hour(2)),
		spanAt(1, nil),
	})
	last := got[len(got)-1]
	if last.Total != 2 || last.Open != 1 {
		t.Fatalf("final = %+v, want total 2 / open 1 (one task is still not done)", last)
	}
}

// TestBuildItemCounts_SameInstantCollapsesToOnePoint: several tasks created
// in one batch share a timestamp. Emitting a point per event would draw a
// vertical segment that says nothing, and would make the chart's point count
// depend on batch size rather than on how often the numbers changed.
func TestBuildItemCounts_SameInstantCollapsesToOnePoint(t *testing.T) {
	got := buildItemCounts([]store.ItemLifespan{
		spanAt(3, nil), spanAt(3, nil), spanAt(3, nil),
	})
	if len(got) != 1 {
		t.Fatalf("points = %d, want 1 — three tasks created at one instant is one change", len(got))
	}
	if got[0].Total != 3 || got[0].Open != 3 {
		t.Fatalf("point = %+v, want total 3 / open 3", got[0])
	}
}

// TestBuildItemCounts_CreationBeforeCompletionAtTheSameInstant: a task
// created and finished at the very same recorded instant (a tiny task on a
// coarse clock) must not make the open count dip below what exists yet.
func TestBuildItemCounts_CreationBeforeCompletionAtTheSameInstant(t *testing.T) {
	got := buildItemCounts([]store.ItemLifespan{spanAt(5, hour(5))})
	if len(got) != 1 {
		t.Fatalf("points = %d, want 1", len(got))
	}
	if got[0].Total != 1 || got[0].Open != 0 {
		t.Fatalf("point = %+v, want total 1 / open 0", got[0])
	}
}

func TestBuildItemCounts_EmptyProjectDrawsNothing(t *testing.T) {
	if got := buildItemCounts(nil); got != nil {
		t.Fatalf("buildItemCounts(nil) = %v, want nil — an empty panel would claim something was measured", got)
	}
}
