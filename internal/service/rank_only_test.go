package service

import (
	"errors"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// firstInColumn reports whether key holds the lowest rank among keys, all of
// which must sit in the same column.
func (e *execEnv) firstInColumn(t *testing.T, key string, keys ...string) bool {
	t.Helper()
	me := e.get(t, key)
	for _, k := range keys {
		other := e.get(t, k)
		if other.ColumnID != me.ColumnID {
			t.Fatalf("%s and %s are in different columns", key, k)
		}
		if k != key && other.Rank <= me.Rank {
			return false
		}
	}
	return true
}

func TestTaskUpdate_RankWithoutColumnMovesWithinCurrentColumn(t *testing.T) {
	e := openExecEnv(t)
	a, _ := e.card(t, "first", "")
	b, _ := e.card(t, "second", "")
	c, vc := e.card(t, "third", "exec-1")
	if e.firstInColumn(t, c, a, b, c) {
		t.Fatal("setup: the newest card should start at the bottom")
	}

	it := e.update(t, e.actor, TaskPatch{Key: c, IfVersion: &vc, Rank: "top"})
	if !it.OK {
		t.Fatalf("rank:top without column refused: %v", it.Err)
	}
	if it.Task.Version != vc+1 {
		t.Fatalf("version = %d, want %d: a reposition bumps it like any move", it.Task.Version, vc+1)
	}
	if it.Task.ColumnID != e.cols["Backlog"].ID {
		t.Fatalf("card left its column: %s", it.Task.ColumnID)
	}
	if !e.firstInColumn(t, c, a, b, c) {
		t.Fatal("rank:top did not put the card first in its column")
	}

	// bottom, and the executor key may do the same on its own card.
	v := it.Task.Version
	it = e.update(t, e.exec, TaskPatch{Key: c, IfVersion: &v, Rank: "bottom"})
	if !it.OK {
		t.Fatalf("executor rank:bottom on its own card refused: %v", it.Err)
	}
	if it.Task.Version != v+1 || e.firstInColumn(t, c, a, b, c) {
		t.Fatalf("rank:bottom did not reposition: version %d, still first", it.Task.Version)
	}
	for _, k := range []string{a, b} {
		me := e.get(t, k)
		if me.Rank >= it.Task.Rank {
			t.Fatalf("%s ranks below the card sent to the bottom", k)
		}
	}

	// An executor may not re-rank someone else's card.
	va := e.get(t, a).Version
	if it := e.update(t, e.exec, TaskPatch{Key: a, IfVersion: &va, Rank: "top"}); it.OK {
		t.Fatal("executor re-ranked a card that is not assigned to it")
	}
}

func TestTaskUpdate_RankWithoutColumnRefusals(t *testing.T) {
	e := openExecEnv(t)
	key, v := e.card(t, "card", "")
	bad := "sideways"
	cases := []struct {
		name  string
		patch TaskPatch
		field string
	}{
		{"no if_version", TaskPatch{Key: key, Rank: "top"}, "if_version"},
		{"unknown rank", TaskPatch{Key: key, IfVersion: &v, Rank: bad}, "rank"},
	}
	for _, tc := range cases {
		it := e.update(t, e.actor, tc.patch)
		if it.OK {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		var de *domain.Error
		if !errors.As(it.Err, &de) || de.Code != domain.CodeValidation || de.Field != tc.field || de.Remediation == "" {
			t.Errorf("%s: err = %#v, want validation on %s with remediation", tc.name, it.Err, tc.field)
		}
	}
	if got := e.get(t, key).Version; got != v {
		t.Fatalf("refused patches moved the version to %d", got)
	}
}
