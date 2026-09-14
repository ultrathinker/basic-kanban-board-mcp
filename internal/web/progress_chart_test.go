package web

import (
	"context"
	"errors"
	"html"
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
// query parameter as service.ProgressHistoryInput.TaskKey). feedPages are
// served in order, one per ChatFeed call (KANB-38's scope-change walk); an
// exhausted list answers with an empty page.
type progressChartStubService struct {
	service.Service
	result    *service.ProgressHistoryResult
	err       error
	lastIn    service.ProgressHistoryInput
	calls     int
	feedPages []*service.ChatFeedResult
	feedErr   error
	feedIns   []service.ChatFeedInput
	feedPage  int
}

func (s *progressChartStubService) ProgressHistory(_ context.Context, _ service.Actor, in service.ProgressHistoryInput) (*service.ProgressHistoryResult, error) {
	s.calls++
	s.lastIn = in
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *progressChartStubService) ChatFeed(_ context.Context, _ service.Actor, in service.ChatFeedInput) (*service.ChatFeedResult, error) {
	s.feedIns = append(s.feedIns, in)
	if s.feedErr != nil {
		return nil, s.feedErr
	}
	if s.feedPage < len(s.feedPages) {
		res := s.feedPages[s.feedPage]
		s.feedPage++
		return res, nil
	}
	return &service.ChatFeedResult{}, nil
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
			{ID: "m1", Assessor: "alpha", Percent: 91, CreatedAt: base, ETA: etaAt(base, 7*24*time.Hour)},
			{ID: "m2", Assessor: "alpha", Percent: 72, CreatedAt: base.Add(2 * time.Hour), ETA: etaAt(base, 10*24*time.Hour)},
		},
		Total: 2,
		Replay: []service.HistoryPoint{
			{At: base, TotalTasks: 4, OpenTasks: 4, DoneTasks: 0,
				Readiness: service.EstimateReadiness{Basis: service.ReadinessEstimates,
					Percent: readinessPct(25), Coverage: "2 of 4 estimated",
					HistoricalNote: service.HistoricalReadinessNote}},
			{At: base.Add(2 * time.Hour), TotalTasks: 9, OpenTasks: 3, DoneTasks: 6,
				Readiness: service.EstimateReadiness{Basis: service.ReadinessEstimates,
					Percent: readinessPct(67), Coverage: "4 of 5 estimated",
					HistoricalNote: service.HistoricalReadinessNote}},
		},
	}
}

// readinessPct is the local shorthand for the optional readiness percent.
func readinessPct(v int) *int { return &v }

// etaAt is the local shorthand for optional forecast dates in fixtures.
func etaAt(t time.Time, d time.Duration) *time.Time {
	v := t.Add(d)
	return &v
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
// is a property of a project, not of one task, so the journal replay behind
// it must not be requested on a per-task chart.
func TestProgressChartDetail_ItemsAreReadOnlyForTheProjectScope(t *testing.T) {
	svc := &progressChartStubService{result: chartDetailFixture()}
	w, sess := newProgressChartTestWeb(t, svc)

	do(w, "GET", "/p/BMB/progress/chart", nil, sessionCookie(sess))
	if !svc.lastIn.IncludeReplay {
		t.Error("the project-scope chart did not ask for the journal replay")
	}
	do(w, "GET", "/p/BMB/progress/chart?task=BMB-1", nil, sessionCookie(sess))
	if svc.lastIn.IncludeReplay {
		t.Error("a per-task chart asked for the project's journal replay, paying for a read it cannot use")
	}
}

// TestProgressChart_ReadinessNoteReachesTheFragment (KANB-53): the sentence
// the service stamps on every replayed point must reach the page standing
// next to the readiness curve — the whole point of the note is to be in
// plain sight where the early, "wrong-looking" points are visible, not in a
// tooltip behind a click.
func TestProgressChart_ReadinessNoteReachesTheFragment(t *testing.T) {
	svc := &progressChartStubService{result: chartDetailFixture()}
	w, sess := newProgressChartTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/progress/chart", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, `data-series="readiness"`) {
		t.Errorf("the fragment has no readiness curve:\n%s", body)
	}
	// html/template escapes the apostrophe mechanically; the comparison is
	// still against the service constant, so a reworded note would not match.
	if !strings.Contains(body, html.EscapeString(service.HistoricalReadinessNote)) {
		t.Errorf("the readiness note did not reach the fragment verbatim:\n%s", body)
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

// TestProgressChartDetail_ForecastOpensOnlyTheForecastChart (KANB-36):
// the forecast has its own chart now, with its own modal route. One click
// must open ONE chart, exactly like progress / items already do. A
// mis-routed case would silently open the wrong chart; this test pins the
// routing so a future change cannot drift without breaking it.
func TestProgressChartDetail_ForecastOpensOnlyTheForecastChart(t *testing.T) {
	svc := &progressChartStubService{result: chartDetailFixture()}
	w, sess := newProgressChartTestWeb(t, svc)

	rw := do(w, "GET", "/p/BMB/progress/chart?detail=forecast", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, `data-chart-title="Promised finish date"`) {
		t.Errorf("detail=forecast did not return the forecast chart:\n%s", body)
	}
	// A misrouted forecast detail must not silently return the other
	// two charts either — same one-click-one-chart discipline.
	if strings.Contains(body, "Assessed progress") || strings.Contains(body, `data-series="composite"`) {
		t.Errorf("detail=forecast also returned the assessment chart:\n%s", body)
	}
	if strings.Contains(body, "Items on the board") || strings.Contains(body, `data-series="items-total"`) {
		t.Errorf("detail=forecast also returned the item chart:\n%s", body)
	}
	// The enlarged forecast chart's Y axis is a date axis (KANB-36),
	// not 0..100 percent. A label that looks like "2026-09-XX" must be
	// present, and no percent label can have leaked through.
	if !strings.Contains(body, "2026-") {
		t.Errorf("the enlarged forecast chart is missing its date axis:\n%s", body)
	}
	for _, pct := range []string{">0%<", ">25%<", ">50%<", ">75%<", ">100%<"} {
		if strings.Contains(body, pct) {
			t.Errorf("forecast detail leaked a percent label %q:\n%s", pct, body)
		}
	}
}

// TestProgressChartDetail_ForecastAbsentWithoutETARendersEmpty: a history
// of percent marks with NO ETA anywhere renders nothing for the forecast
// detail — the same "no data, no placeholder" rule the percent and items
// charts already follow (a modal frame around "nothing here" would be
// a worse lie than no modal).
func TestProgressChartDetail_ForecastAbsentWithoutETARendersEmpty(t *testing.T) {
	base := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	svc := &progressChartStubService{result: &service.ProgressHistoryResult{
		ProjectKey: "BMB",
		Marks: []domain.ProgressMark{
			{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: base},
			{ID: "m2", Assessor: "alpha", Percent: 50, CreatedAt: base.Add(2 * time.Hour)},
		},
	}}
	w, sess := newProgressChartTestWeb(t, svc)

	rw := do(w, "GET", "/p/BMB/progress/chart?detail=forecast", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if strings.TrimSpace(rw.Body.String()) != "" {
		t.Errorf("a history with no ETA rendered a frame for detail=forecast:\n%s", rw.Body.String())
	}
}

// ---------------------------------------------------------------------------
// KANB-38: the scope_change declarations ride the items chart's time axis.
// These tests walk the real endpoint: the feed read, the mark in the markup,
// and the two ways the feature must degrade (feed pages past the first, and
// a feed read that fails).
// ---------------------------------------------------------------------------

// scopeChartFixture is a two-point items curve spanning three days, the
// smallest axis a scope tick can honestly stand on.
func scopeChartFixture() *service.ProgressHistoryResult {
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.Local)
	return &service.ProgressHistoryResult{
		ProjectKey: "BMB",
		Replay: []service.HistoryPoint{
			{At: base, TotalTasks: 1, OpenTasks: 1},
			{At: base.Add(72 * time.Hour), TotalTasks: 4, OpenTasks: 3},
		},
	}
}

// TestProgressChart_ScopeChangeTickReachesTheFragment: a declared scope
// change on the feed becomes a linked tick on the axis; an ordinary update
// on the same page of the feed becomes nothing.
func TestProgressChart_ScopeChangeTickReachesTheFragment(t *testing.T) {
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.Local)
	svc := &progressChartStubService{
		result: scopeChartFixture(),
		feedPages: []*service.ChatFeedResult{{
			Messages: []service.ChatFeedMessage{
				{Message: domain.ChatMessage{
					ID: "msg-scope", Author: "lead", Body: "we took on the export",
					CreatedAt: base.Add(24 * time.Hour), Kind: domain.MessageScopeChange,
				}},
				{Message: domain.ChatMessage{
					ID: "msg-plain", Author: "lead", Body: "status: fine",
					CreatedAt: base.Add(24 * time.Hour), Kind: domain.MessageUpdate,
				}},
			},
		}},
	}
	w, sess := newProgressChartTestWeb(t, svc)

	rw := do(w, "GET", "/p/BMB/progress/chart", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	for _, want := range []string{
		`data-axis-mark="scope"`,
		`<a href="/p/BMB#chat-msg-scope">`,
		"we took on the export",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("items chart fragment is missing %q", want)
		}
	}
	if strings.Contains(body, "msg-plain") || strings.Contains(body, "status: fine") {
		t.Error("an ordinary update was drawn as a scope tick")
	}
	if len(svc.feedIns) == 0 {
		t.Error("the feed was never asked for the scope declarations")
	}
}

// TestProgressChart_ScopeWalkPagesTheWholeFeed: the marks must cover the
// journal's whole span, and the feed's first page is the freshest, not the
// oldest — the walk that stops after one page would draw only yesterday's
// declarations and lose the project's early history.
func TestProgressChart_ScopeWalkPagesTheWholeFeed(t *testing.T) {
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.Local)
	svc := &progressChartStubService{
		result: scopeChartFixture(),
		feedPages: []*service.ChatFeedResult{
			{
				Messages: []service.ChatFeedMessage{
					{Message: domain.ChatMessage{
						ID: "msg-new", Author: "lead", Body: "recent chatter",
						CreatedAt: base.Add(71 * time.Hour), Kind: domain.MessageUpdate,
					}},
				},
				NextCursor: "10/msg-new",
				HasMore:    true,
			},
			{
				Messages: []service.ChatFeedMessage{
					{Message: domain.ChatMessage{
						ID: "msg-old", Author: "lead", Body: "we took on the export",
						CreatedAt: base.Add(time.Hour), Kind: domain.MessageScopeChange,
					}},
				},
			},
		},
	}
	w, sess := newProgressChartTestWeb(t, svc)

	rw := do(w, "GET", "/p/BMB/progress/chart", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), `<a href="/p/BMB#chat-msg-old">`) {
		t.Errorf("the declaration on the second page never reached the axis:\n%s", rw.Body.String())
	}
	// The walk carried the cursor forward verbatim: page one started at the
	// beginning of history, page two at where page one stopped.
	if len(svc.feedIns) != 2 {
		t.Fatalf("feed read %d times, want exactly the two pages", len(svc.feedIns))
	}
	if svc.feedIns[0].After != "" || svc.feedIns[1].After != "10/msg-new" {
		t.Errorf("cursor walk = %q then %q, want empty then the returned cursor",
			svc.feedIns[0].After, svc.feedIns[1].After)
	}
}

// TestProgressChart_FailedFeedKeepsTheChart: the marks are best-effort, the
// same vocabulary as every other chat read feeding a page — a failed feed
// costs the ticks, never the chart itself.
func TestProgressChart_FailedFeedKeepsTheChart(t *testing.T) {
	svc := &progressChartStubService{
		result:  scopeChartFixture(),
		feedErr: errors.New("chat feed is down"),
	}
	w, sess := newProgressChartTestWeb(t, svc)

	rw := do(w, "GET", "/p/BMB/progress/chart", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the chart must survive a failed feed read\n%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, "<svg") {
		t.Error("the items chart itself did not render")
	}
	if strings.Contains(body, `data-axis-mark="scope"`) {
		t.Error("a failed feed read still produced ticks")
	}
}

// TestProgressChart_TaskScopeNeverWalksTheFeed: the per-task charts have no
// items panel, so the extra feed read must not happen there at all.
func TestProgressChart_TaskScopeNeverWalksTheFeed(t *testing.T) {
	svc := &progressChartStubService{result: scopeChartFixture()}
	w, sess := newProgressChartTestWeb(t, svc)

	rw := do(w, "GET", "/p/BMB/progress/chart?task=BMB-1", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	if len(svc.feedIns) != 0 {
		t.Errorf("the feed was read %d times on a task scope, want 0", len(svc.feedIns))
	}
}
