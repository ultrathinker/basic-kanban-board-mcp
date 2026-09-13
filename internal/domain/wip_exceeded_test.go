package domain

import (
	"fmt"
	"strings"
	"testing"
)

// TestWIPExceeded_NamesOccupants covers KANB-28: the reporter's complaint was
// not that the WIP refusal is wrong (it isn't — the owner confirmed neither
// the limit nor the batch's non-atomicity should change), only that its
// message did not say what was occupying the column, forcing a separate
// board read. This pins the message actually naming the occupants passed in.
func TestWIPExceeded_NamesOccupants(t *testing.T) {
	e := WIPExceeded("Doing", 3, []string{"BMB-1", "BMB-2", "BMB-3"})
	if e.Code != CodeWIPExceeded {
		t.Fatalf("code = %v, want wip_exceeded", e.Code)
	}
	if !strings.Contains(e.Message, "Doing") || !strings.Contains(e.Message, "3") {
		t.Fatalf("message %q lost the column name or limit", e.Message)
	}
	for _, key := range []string{"BMB-1", "BMB-2", "BMB-3"} {
		if !strings.Contains(e.Message, key) {
			t.Errorf("message %q does not name occupant %s", e.Message, key)
		}
	}
}

// TestWIPExceeded_NoOccupants_MessageUnchanged is the backward-compatibility
// half: task_next's own CheckMove call site does not (yet) pass Occupants,
// and that must keep behaving exactly as before — no trailing ", occupied by"
// clause hanging off an empty list.
func TestWIPExceeded_NoOccupants_MessageUnchanged(t *testing.T) {
	e := WIPExceeded("Doing", 3, nil)
	want := `column "Doing" is at its WIP limit of 3`
	if e.Message != want {
		t.Fatalf("message = %q, want %q", e.Message, want)
	}
}

// TestWIPExceeded_SampleCapped_NotAColumnDump is the brief's explicit
// concern: a project may set wip_limit far past a handful, and the message
// must stay a short pointer, never a dump of the whole column. It also
// checks the "and N more" note names how many were left out, since a caller
// reading only the sample should still learn the true size.
func TestWIPExceeded_SampleCapped_NotAColumnDump(t *testing.T) {
	occupants := make([]string, 50)
	for i := range occupants {
		occupants[i] = fmt.Sprintf("BMB-%d", i+1)
	}
	e := WIPExceeded("Doing", 50, occupants)

	shown := 0
	for _, key := range occupants {
		if strings.Contains(e.Message, key+",") || strings.Contains(e.Message, key+" ") || strings.HasSuffix(e.Message, key) {
			shown++
		}
	}
	if shown > WIPExceededKeySample {
		t.Fatalf("message names %d occupants, want at most WIPExceededKeySample=%d: %q", shown, WIPExceededKeySample, e.Message)
	}
	if !strings.Contains(e.Message, fmt.Sprintf("%d more", len(occupants)-WIPExceededKeySample)) {
		t.Errorf("message %q does not say how many more were left out", e.Message)
	}
	// The very first occupant must still be named — a sample, not an
	// arbitrary or unstable subset.
	if !strings.Contains(e.Message, occupants[0]) {
		t.Errorf("message %q dropped the first occupant entirely", e.Message)
	}
}

// TestCheckMove_WIPExceeded_CarriesOccupants verifies MoveCheck.Occupants
// actually reaches the WIPExceeded error CheckMove returns, not just that
// WIPExceeded itself formats correctly in isolation.
func TestCheckMove_WIPExceeded_CarriesOccupants(t *testing.T) {
	limit := 2
	err := CheckMove(MoveCheck{
		TaskKey:   "BMB-9",
		From:      Column{Name: "Backlog", Kind: KindBacklog},
		To:        Column{Name: "Doing", Kind: KindActive, WIPLimit: &limit},
		ToCount:   2,
		Occupants: []string{"BMB-1", "BMB-2"},
	})
	de := AsError(err)
	if de == nil || de.Code != CodeWIPExceeded {
		t.Fatalf("err = %v, want a wip_exceeded *Error", err)
	}
	if !strings.Contains(de.Message, "BMB-1") || !strings.Contains(de.Message, "BMB-2") {
		t.Fatalf("message %q does not name the occupants CheckMove was given", de.Message)
	}
}
