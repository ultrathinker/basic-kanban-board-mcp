package domain

import (
	"reflect"
	"testing"
)

// This file used to pin the wording of the wip_exceeded refusal (KANB-28).
// WIP limits were removed from the board on 26.09.2026 (KANB-59): the owner
// found that a column limit only ever blocked the agent doing the work, while
// the cards it was meant to expose — stale ones — are now surfaced by idle
// time. What is left to guard is the absence itself, so a limit cannot creep
// back through a helper nobody reads.

// TestCheckMove_NeverRefusesForOccupancy: moving into an active column is
// allowed no matter how many cards it already holds. MoveCheck no longer has
// a way to say how full the destination is, and this test fails the build if
// one is added back under any of the names it used to have.
func TestCheckMove_NeverRefusesForOccupancy(t *testing.T) {
	rt := reflect.TypeOf(MoveCheck{})
	for _, name := range []string{"ToCount", "Occupants", "WIPLimit"} {
		if _, ok := rt.FieldByName(name); ok {
			t.Errorf("MoveCheck has field %s again: a move must never be refused for column occupancy (KANB-59)", name)
		}
	}
	if _, ok := reflect.TypeOf(Column{}).FieldByName("WIPLimit"); ok {
		t.Errorf("Column has WIPLimit again: the board has no WIP limits since 26.09.2026 (KANB-59)")
	}

	err := CheckMove(MoveCheck{
		TaskKey: "BMB-9",
		From:    Column{Name: "Backlog", Kind: KindBacklog},
		To:      Column{Name: "Doing", Kind: KindActive},
	})
	if err != nil {
		t.Fatalf("a plain move into an active column was refused: %v", err)
	}
}
