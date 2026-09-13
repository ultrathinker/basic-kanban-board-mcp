package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// progressChartStubService answers ProgressHistory with a canned result and
// records what it was called with, so tests can pin exactly what the
// handler forwards to the service layer (the project key and the "task"
// query parameter as service.ProgressHistoryInput.TaskKey).
type progressChartStubService struct {
	service.Service
	result *service.ProgressHistoryResult
	err    error
	lastIn service.ProgressHistoryInput
	calls  int
}

func (s *progressChartStubService) ProgressHistory(_ context.Context, _ service.Actor, in service.ProgressHistoryInput) (*service.ProgressHistoryResult, error) {
	s.calls++
	s.lastIn = in
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func newProgressChartTestWeb(t *testing.T, svc service.Service) (*Web, *domain.Session) {
	t.Helper()
	tpl, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	mgr := newTestManager(t)
	w := New(Deps{
		Service:   svc,
		Auth:      mgr,
		Bus:       events.New(newEmptyHistory(), 4),
		Templates: tpl,
		BaseURL:   "http://127.0.0.1:0",
	})
	sess := sessionFor(t, mgr, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")
	return w, sess
}

// TestProgressChart_Unauthenticated: no session and no bearer token gets a
// non-2xx status — this is a JS-fetch endpoint, not a page, so no redirect
// to /login.
func TestProgressChart_Unauthenticated(t *testing.T) {
	svc := &progressChartStubService{}
	w, _ := newProgressChartTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/progress/chart", nil)
	if rw.Code == http.StatusOK || rw.Code == http.StatusFound || rw.Code == http.StatusSeeOther {
		t.Fatalf("status = %d, want a non-2xx, non-redirect status for an unauthenticated fetch", rw.Code)
	}
	if svc.calls != 0 {
		t.Fatalf("ProgressHistory called %d times, want 0: an unauthenticated request must never reach the service", svc.calls)
	}
}

// TestProgressChart_ProjectScope_EmptyTaskParam: no "task" query param means
// the project-level (manual) scope, the same convention
// service.ProgressTrackDeleteInput.TaskKey and the delete-track control
// already use — never a distinct "missing parameter" error.
func TestProgressChart_ProjectScope_EmptyTaskParam(t *testing.T) {
	svc := &progressChartStubService{result: &service.ProgressHistoryResult{ProjectKey: "BMB"}}
	w, sess := newProgressChartTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/progress/chart", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	if svc.lastIn.ProjectKey != "BMB" {
		t.Fatalf("ProgressHistory called with ProjectKey = %q, want BMB", svc.lastIn.ProjectKey)
	}
	if svc.lastIn.TaskKey != "" {
		t.Fatalf("ProgressHistory called with TaskKey = %q, want empty (project scope)", svc.lastIn.TaskKey)
	}
}

// TestProgressChart_TaskScope: "?task=BMB-1" is forwarded verbatim as
// service.ProgressHistoryInput.TaskKey.
func TestProgressChart_TaskScope(t *testing.T) {
	svc := &progressChartStubService{result: &service.ProgressHistoryResult{ProjectKey: "BMB", TaskKey: "BMB-1"}}
	w, sess := newProgressChartTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/progress/chart?task=BMB-1", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	if svc.lastIn.TaskKey != "BMB-1" {
		t.Fatalf("ProgressHistory called with TaskKey = %q, want BMB-1", svc.lastIn.TaskKey)
	}
}

// TestProgressChart_RendersSVG: the fragment body wraps chart.go's own SVG
// output, built from the marks the service returned — this is the one place
// that proves the web layer actually calls the existing renderer instead of
// reinventing one.
func TestProgressChart_RendersSVG(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	svc := &progressChartStubService{result: &service.ProgressHistoryResult{
		ProjectKey: "BMB",
		Marks: []domain.ProgressMark{
			{ID: "m1", Assessor: "alpha", Percent: 91, CreatedAt: t0},
			{ID: "m2", Assessor: "alpha", Percent: 72, CreatedAt: t0.Add(30 * time.Minute)},
		},
	}}
	w, sess := newProgressChartTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/progress/chart?task=BMB-1", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, "<svg") {
		t.Fatalf("body does not contain a rendered chart: %s", body)
	}
	if !strings.Contains(body, `data-assessor="alpha"`) {
		t.Fatalf("body missing the assessor's track: %s", body)
	}
	if !strings.Contains(body, "progress-chart-inner") {
		t.Fatalf("body missing the fragment's own wrapper: %s", body)
	}
}

// TestProgressChart_NoHistoryRendersEmptyBody: an empty (or vanished)
// history is not an error — the fragment is simply empty, and app.js treats
// that as a no-op, the same way handleChatOlder's exhausted page does.
func TestProgressChart_NoHistoryRendersEmptyBody(t *testing.T) {
	svc := &progressChartStubService{result: &service.ProgressHistoryResult{ProjectKey: "BMB", Marks: nil}}
	w, sess := newProgressChartTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/progress/chart", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	if strings.TrimSpace(rw.Body.String()) != "" {
		t.Fatalf("body = %q, want empty when there is no history to chart", rw.Body.String())
	}
}

// TestProgressChart_ServiceErrorSurfacesAsAPIError: a project the caller
// cannot access (or that does not exist) reports through apiError, not a
// page redirect or a silently empty fragment.
func TestProgressChart_ServiceErrorSurfacesAsAPIError(t *testing.T) {
	svc := &progressChartStubService{err: domain.NotFound("project", "BMB")}
	w, sess := newProgressChartTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/progress/chart", nil, sessionCookie(sess))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404\n%s", rw.Code, rw.Body.String())
	}
}

// TestProgressChart_InvalidProjectKey: a malformed project key in the path
// is rejected before the service is ever called.
func TestProgressChart_InvalidProjectKey(t *testing.T) {
	svc := &progressChartStubService{}
	w, sess := newProgressChartTestWeb(t, svc)
	rw := do(w, "GET", "/p/1BAD/progress/chart", nil, sessionCookie(sess))
	if rw.Code == http.StatusOK {
		t.Fatalf("status = %d, want a non-200 for an invalid project key\n%s", rw.Code, rw.Body.String())
	}
	if svc.calls != 0 {
		t.Fatalf("ProgressHistory called %d times, want 0: an invalid key must never reach the service", svc.calls)
	}
}

// ---------------------------------------------------------------------------
// detail=<which>: the modal's enlarged chart.
//
// One click opens ONE chart, because the modal is sized to fill the viewport
// so nothing inside it has to be scrolled, and two charts sharing that height
// would each get half of it — which is the scrolling being removed. So the
// routing from "which panel was clicked" to "which chart comes back" is
// load-bearing, and neither half of it is visible in a rendered SVG: a
// mis-wired case would simply open the wrong chart, with no error anywhere.
// ---------------------------------------------------------------------------

func chartDetailFixture() *service.ProgressHistoryResult {
	base := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	return &service.ProgressHistoryResult{
		ProjectKey: "BMB",
		Marks: []domain.ProgressMark{
			{ID: "m1", Assessor: "alpha", Percent: 91, CreatedAt: base},
			{ID: "m2", Assessor: "alpha", Percent: 72, CreatedAt: base.Add(2 * time.Hour)},
		},
		Total: 2,
		Items: []service.ItemCountPoint{
			{At: base, Total: 4, Open: 4},
			{At: base.Add(2 * time.Hour), Total: 9, Open: 3},
		},
	}
}

func TestProgressChartDetail_ProgressOpensOnlyTheAssessmentChart(t *testing.T) {
	svc := &progressChartStubService{result: chartDetailFixture()}
	w, sess := newProgressChartTestWeb(t, svc)

	rw := do(w, "GET", "/p/BMB/progress/chart?detail=progress", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, `data-chart-title="Assessed progress"`) {
		t.Errorf("detail=progress did not return the assessment chart:\n%s", body)
	}
	if strings.Contains(body, "Items on the board") || strings.Contains(body, `data-series="items-total"`) {
		t.Errorf("detail=progress also returned the item chart; one click must open one chart:\n%s", body)
	}
	// The enlarged version is the one with a real axis, not the inline
	// panel's single midline.
	if !strings.Contains(body, ">0%<") || !strings.Contains(body, ">100%<") {
		t.Errorf("the enlarged chart is missing its labelled percent axis:\n%s", body)
	}
}

func TestProgressChartDetail_ItemsOpensOnlyTheItemChart(t *testing.T) {
	svc := &progressChartStubService{result: chartDetailFixture()}
	w, sess := newProgressChartTestWeb(t, svc)

	rw := do(w, "GET", "/p/BMB/progress/chart?detail=items", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, `data-chart-title="Items on the board"`) {
		t.Errorf("detail=items did not return the item chart:\n%s", body)
	}
	if !strings.Contains(body, `data-series="items-total"`) || !strings.Contains(body, `data-series="items-open"`) {
		t.Errorf("the item chart is missing one of its two curves:\n%s", body)
	}
	if strings.Contains(body, "Assessed progress") {
		t.Errorf("detail=items also returned the assessment chart:\n%s", body)
	}
}

// TestProgressChartDetail_ItemsAreReadOnlyForTheProjectScope: an item count
// is a property of a project, not of one task, so the second read must not
// be requested on a per-task chart.
func TestProgressChartDetail_ItemsAreReadOnlyForTheProjectScope(t *testing.T) {
	svc := &progressChartStubService{result: chartDetailFixture()}
	w, sess := newProgressChartTestWeb(t, svc)

	do(w, "GET", "/p/BMB/progress/chart", nil, sessionCookie(sess))
	if !svc.lastIn.IncludeItems {
		t.Error("the project-scope chart did not ask for the item counts")
	}
	do(w, "GET", "/p/BMB/progress/chart?task=BMB-1", nil, sessionCookie(sess))
	if svc.lastIn.IncludeItems {
		t.Error("a per-task chart asked for the project's item counts, paying for a read it cannot use")
	}
}

// TestProgressChartDetail_UnknownDetailIsRefused: a typo must not silently
// fall through to the inline panels, which would look like the modal
// mysteriously showing the small chart.
func TestProgressChartDetail_UnknownDetailIsRefused(t *testing.T) {
	svc := &progressChartStubService{result: chartDetailFixture()}
	w, sess := newProgressChartTestWeb(t, svc)

	rw := do(w, "GET", "/p/BMB/progress/chart?detail=1", nil, sessionCookie(sess))
	if rw.Code == http.StatusOK {
		t.Fatalf("an unknown detail value was accepted: status = %d, body=%s", rw.Code, rw.Body.String())
	}
}

// TestProgressChartDetail_NoHistoryRendersNothing: app.js prints its own
// "no history yet" into the modal, so the server must send an empty body
// rather than a frame around nothing.
func TestProgressChartDetail_NoHistoryRendersNothing(t *testing.T) {
	svc := &progressChartStubService{result: &service.ProgressHistoryResult{ProjectKey: "BMB"}}
	w, sess := newProgressChartTestWeb(t, svc)

	rw := do(w, "GET", "/p/BMB/progress/chart?detail=progress", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if strings.TrimSpace(rw.Body.String()) != "" {
		t.Errorf("an empty history rendered a frame:\n%s", rw.Body.String())
	}
}
