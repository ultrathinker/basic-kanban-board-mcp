package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// KANB-40, review debt item 2: the chart toggle, the clickable bar and the
// delete-track arm were shipped with no assertion on the ASSEMBLED page — a
// wiring regression (a renamed attribute, a dropped branch) would have been
// invisible. This walks the real GET /p/{key} with a project that has an
// assessed manual progress behind it, which is the one shape that renders
// all three.
// ---------------------------------------------------------------------------

// progressBoardStub answers one board-page render with a manual progress
// that has assessors (delete-arm rows), scope identifiers (clickable) and a
// standing forecast (the badge).
type progressBoardStub struct {
	service.Service
}

func (s *progressBoardStub) BoardGet(_ context.Context, _ service.Actor, _ service.BoardGetInput) (*service.Board, error) {
	return &service.Board{Projects: []service.BoardProject{{
		Key:  "BMB",
		Name: "BMB",
		Columns: []service.BoardColumn{
			{Name: "Backlog", Kind: domain.KindBacklog, Count: 0},
		},
	}}}, nil
}

// The board page now renders the charts itself (they are no longer fetched
// on a bar click), so it makes this read too. Answering with no history is
// enough: this test is about the wiring on the assembled page, and the chart
// content has its own tests.
func (s *progressBoardStub) ProgressHistory(_ context.Context, _ service.Actor, _ service.ProgressHistoryInput) (*service.ProgressHistoryResult, error) {
	return &service.ProgressHistoryResult{}, nil
}

func (s *progressBoardStub) ChatFeed(_ context.Context, _ service.Actor, _ service.ChatFeedInput) (*service.ChatFeedResult, error) {
	return &service.ChatFeedResult{}, nil
}

func (s *progressBoardStub) ProjectProgress(_ context.Context, _ service.Actor, in service.ProjectProgressInput) (*service.ProjectProgressResult, error) {
	manual := 55
	eta := time.Now().UTC().Add(72 * time.Hour)
	return &service.ProjectProgressResult{
		ProjectKey:        in.ProjectKey,
		Manual:            &manual,
		ManualAssessors:   2,
		ManualForecastETA: &eta,
		ManualForecastBy:  "lead",
		ManualTracks: []service.AssessorTrack{
			{Assessor: "lead", Percent: 60, Count: 3},
			{Assessor: "beta", Percent: 50, Count: 2},
		},
	}, nil
}

func (s *progressBoardStub) ChatList(_ context.Context, _ service.Actor, _ service.ChatListInput) (*service.ChatListResult, error) {
	return &service.ChatListResult{}, nil
}

// TestBoardPage_ChartWiringReachesTheAssembledPage: the attributes app.js
// binds to exist on the page the server actually assembles — the bar is
// clickable and carries the chart toggle's scope, the forecast badge rides
// the same metric, and each assessor's row carries the delete arm.
func TestBoardPage_ChartWiringReachesTheAssembledPage(t *testing.T) {
	w := newTestWeb(t)
	w.d.Service = &progressBoardStub{}
	sess := sessionFor(t, w.d.Auth, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")

	rec := do(w, "GET", "/p/BMB", nil, sessionCookie(sess))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`pbar-openstracks`,
		`data-progress-tracks-open`,
		`data-progress-tracks-dialog`,
		`data-project="BMB" data-task=""`,
		// The delete arm rides each assessor's row, and the confirmation
		// names the point count that would be lost.
		`data-progress-delete-arm`,
		`data-assessor="lead"`,
		`data-assessor="beta"`,
		// The forecast badge, through the real assembled page.
		`class="forecast"`,
		`title="Forecast by lead"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the assembled board page is missing %q", want)
		}
	}
}
