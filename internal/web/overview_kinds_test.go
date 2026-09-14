package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// ---------------------------------------------------------------------------
// KANB-52 meets the overview page.
//
// The overview split a project's columns into three named buckets with a
// switch that had a case per kind and no default. When the domain grew a
// fourth kind, every card parked in a waiting column stopped being reported
// anywhere on the page: not as backlog, not as active, not as done, and not
// in the roll-up above the grid. Nothing failed to compile and nothing went
// red - the page simply under-reported the project, which is the worst shape
// a counting bug can take.
// ---------------------------------------------------------------------------

// overviewService answers the overview page's single read with a fixed board.
// It embeds the interface so any other call the handler might make panics
// loudly instead of quietly returning a zero value.
type overviewService struct {
	service.Service
	board *service.Board
}

func (s *overviewService) BoardGet(_ context.Context, _ service.Actor, _ service.BoardGetInput) (*service.Board, error) {
	return s.board, nil
}

func newOverviewWeb(t *testing.T, board *service.Board) (*Web, *domain.Session) {
	t.Helper()
	tpl, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	mgr := newTestManager(t)
	w := New(Deps{
		Service:   &overviewService{board: board},
		Auth:      mgr,
		Bus:       events.New(newEmptyHistory(), 4),
		Templates: tpl,
		BaseURL:   "http://127.0.0.1:0",
	})
	return w, sessionFor(t, mgr, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")
}

// TestOverview_ReportsWaitingColumns pins the page against the defect: a
// project with cards parked in a waiting column must report them, and the
// roll-up must account for every card on the board.
func TestOverview_ReportsWaitingColumns(t *testing.T) {
	board := &service.Board{Projects: []service.BoardProject{{
		Key:  "BMB",
		Name: "Test",
		Columns: []service.BoardColumn{
			{Name: "Backlog", Kind: domain.KindBacklog, Count: 5},
			{Name: "Doing", Kind: domain.KindActive, Count: 2},
			{Name: "Waiting", Kind: domain.KindWaiting, Count: 3},
			{Name: "Done", Kind: domain.KindDone, Count: 4},
		},
	}}}
	w, sess := newOverviewWeb(t, board)

	rw := do(w, "GET", "/", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, "waiting <strong>3</strong>") {
		t.Fatalf("the project cell does not report its 3 parked cards; the waiting bucket is missing from the page:\n%s", body)
	}
	if !strings.Contains(body, "3 waiting") {
		t.Fatalf("the roll-up above the grid does not report the 3 parked cards:\n%s", body)
	}
}

// TestOverview_CountsEveryColumnKind is the drift guard. It walks
// domain.AllKinds rather than a list written out here, so the next kind the
// domain grows cannot be swallowed the way waiting was: a kind with no bucket
// in tallyOverview contributes nothing, the sum falls short, and the build
// goes red at the place that needs changing.
func TestOverview_CountsEveryColumnKind(t *testing.T) {
	const cards = 7
	for _, k := range domain.AllKinds {
		tally := tallyOverview([]service.BoardColumn{{Name: string(k), Kind: k, Count: cards}})
		if got := tally.sum(); got != cards {
			t.Fatalf("column kind %q contributed %d of %d cards to the overview: it has no bucket in tallyOverview, so its cards vanish from the page",
				k, got, cards)
		}
	}
}
