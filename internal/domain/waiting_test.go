package domain

import "testing"

// TestKindWaiting_Valid pins KANB-52's new column kind into Kind.Valid(),
// the single gate validateColumnSpecs (internal/service) relies on to accept
// or reject a caller-supplied kind string.
func TestKindWaiting_Valid(t *testing.T) {
	if !KindWaiting.Valid() {
		t.Fatalf("KindWaiting.Valid() = false, want true")
	}
	if Kind("waiting") != KindWaiting {
		t.Fatalf(`Kind("waiting") != KindWaiting — the wire spelling must be exactly "waiting"`)
	}
}

// TestDefaultColumns_UnaffectedByWaiting is the brief's explicit prohibition
// (KANB-52): adding the KindWaiting constant must not turn it into a fourth
// default column on every new project. DefaultColumns must stay exactly the
// three it already was.
func TestDefaultColumns_UnaffectedByWaiting(t *testing.T) {
	if len(DefaultColumns) != 3 {
		t.Fatalf("len(DefaultColumns) = %d, want 3 (unchanged by KANB-52)", len(DefaultColumns))
	}
	wantKinds := []Kind{KindBacklog, KindActive, KindDone}
	for i, want := range wantKinds {
		if DefaultColumns[i].Kind != want {
			t.Errorf("DefaultColumns[%d].Kind = %q, want %q", i, DefaultColumns[i].Kind, want)
		}
		if DefaultColumns[i].Kind == KindWaiting {
			t.Errorf("DefaultColumns[%d] is KindWaiting; no project may get one by default", i)
		}
	}
}

// TestCheckMove_WaitingAllowsOpenDependencyButActiveRefuses is KANB-52's one
// real trap, pinned directly at the rule that holds it: the dependency check
// in CheckMove refuses every non-backlog destination while a blocker is
// still open, and KindWaiting needs its own carved-out exception the same
// way KindBacklog already has one — miss it, and the very reason the column
// exists (parking a task with an open dependency) is refused right along
// with every other destination.
//
// The test is deliberately PAIRED on the SAME task and the SAME open
// blocker: moving to Waiting must be allowed, and moving to Doing (an
// ordinary active column) must still be refused. Checking only one half
// would pass under either wrong implementation — the check removed
// entirely (both halves green, dependencies silently stop mattering) or the
// exception never added (both halves refuse, and Waiting cannot do the one
// thing it exists for).
func TestCheckMove_WaitingAllowsOpenDependencyButActiveRefuses(t *testing.T) {
	backlog := Column{Name: "Backlog", Kind: KindBacklog}
	doing := Column{Name: "Doing", Kind: KindActive}
	waiting := Column{Name: "Waiting", Kind: KindWaiting}
	openBlocks := []string{"BMB-1"}

	if err := CheckMove(MoveCheck{
		TaskKey:             "BMB-2",
		From:                backlog,
		To:                  waiting,
		OpenBlocks:          openBlocks,
		EnforceDependencies: true,
	}); err != nil {
		t.Fatalf("move to waiting with an open dependency = %v, want allowed", err)
	}

	err := CheckMove(MoveCheck{
		TaskKey:             "BMB-2",
		From:                backlog,
		To:                  doing,
		OpenBlocks:          openBlocks,
		EnforceDependencies: true,
	})
	de := AsError(err)
	if de == nil || de.Code != CodeBlocked {
		t.Fatalf("move to Doing with the same open dependency = %v, want a blocked *Error", err)
	}
}
