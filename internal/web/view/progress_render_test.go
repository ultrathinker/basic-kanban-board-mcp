package view_test

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// percentPtr is the view_test helper for optional percents in fixtures.
func percentPtr(v int) *int { return &v }

// renderProgress executes the named template (with the optional extra
// template data) and returns its HTML, failing the test on a render error —
// html/template only fails at execution, never at parse, so every assertion
// below goes through a real render.
func renderProgress(t *testing.T, name string, data any) string {
	t.Helper()
	engine, err := templates.NewFromDir(templatesDir)
	if err != nil {
		t.Fatalf("templates.NewFromDir: %v", err)
	}
	var buf bytes.Buffer
	if err := engine.Render(&buf, name, data); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return buf.String()
}

// countOccurrences counts non-overlapping occurrences of substr in s.
func countOccurrences(s, substr string) int {
	return strings.Count(s, substr)
}

// TestProgressBar_PaintsFloorTensPercents pins the bar's shape contract:
// exactly ten squares at every percent, painted floor(percent/10) of them —
// 0% paints none, 45% paints four (never five: no rounding up, no partial
// fill), 100% paints all ten.
func TestProgressBar_PaintsFloorTensPercents(t *testing.T) {
	cases := []struct {
		percent int
		filled  int
	}{
		{0, 0},
		{45, 4},
		{100, 10},
	}
	for _, tc := range cases {
		html := renderProgress(t, "progress-bar", map[string]any{
			"Progress": view.NewAssessedProgress(percentPtr(tc.percent), 3, nil, ""),
		})
		if total := countOccurrences(html, "<i"); total != 10 {
			t.Fatalf("percent %d: %d squares rendered, want exactly 10", tc.percent, total)
		}
		if painted := countOccurrences(html, `<i class="f">`); painted != tc.filled {
			t.Fatalf("percent %d: %d squares painted, want %d (floor(percent/10))", tc.percent, painted, tc.filled)
		}
		if !strings.Contains(html, `data-filled="`+strconv.Itoa(tc.filled)+`"`) {
			t.Fatalf("percent %d: markup does not carry data-filled=%d: %s", tc.percent, tc.filled, html)
		}
		if !strings.Contains(html, "45%") && tc.percent == 45 {
			t.Fatalf("percent 45: label missing: %s", html)
		}
	}

	// The label names the percent AND how many tracks were folded in, so one
	// agent's opinion cannot masquerade as a consensus.
	html := renderProgress(t, "progress-bar", map[string]any{
		"Progress": view.NewAssessedProgress(percentPtr(45), 3, nil, ""),
	})
	if !strings.Contains(html, "45% · 3 assessments") {
		t.Fatalf("label should read percent and track count, got: %s", html)
	}
	single := renderProgress(t, "progress-bar", map[string]any{
		"Progress": view.NewAssessedProgress(percentPtr(70), 1, nil, ""),
	})
	if !strings.Contains(single, "70% · 1 assessment") {
		t.Fatalf("singular label wrong: %s", single)
	}
}

// TestProgressBar_AbsentWithoutAssessment: a task nobody assessed shows NO
// bar at all — not a row of ten empty squares. An empty bar asserts "not
// started", which is a claim about the work; absence asserts "no data",
// which is the only honest rendering.
func TestProgressBar_AbsentWithoutAssessment(t *testing.T) {
	// The constructors must refuse to invent a bar for missing data: nil in,
	// nil out. A zero-percent bar here would claim "not started".
	if got := view.NewAssessedProgress(nil, 2, nil, ""); got != nil {
		t.Fatalf("NewAssessedProgress(nil) = %+v, want nil (no assessment, no bar)", got)
	}
	if got := view.NewDoneShareProgress(nil, 0, 0); got != nil {
		t.Fatalf("NewDoneShareProgress(nil) = %+v, want nil (no tasks, no bar)", got)
	}
	// Whitespace-only output counts as nothing: it is not visible content.
	if html := renderProgress(t, "progress-bar", map[string]any{"Progress": nil}); strings.TrimSpace(html) != "" {
		t.Fatalf("nil progress rendered %q, want nothing at all", html)
	}
	// Through the card, the way the board actually renders it.
	card := view.TaskCard{Key: "BMB-1", Title: "unassessed"}
	html := renderProgress(t, "card", map[string]any{
		"Card":    card,
		"CSRF":    "",
		"Columns": []view.ColumnView{},
	})
	if strings.Contains(html, `class="pbar`) {
		t.Fatalf("card without assessments rendered a progress bar: %s", html)
	}
}

// TestProjectHeader_ShowsBothLabelledMetrics pins the header pair: the manual
// progress ("assessed") and the automatic one ("tasks done") both render,
// each with its own distinguishing label and its own value.
func TestProjectHeader_ShowsBothLabelledMetrics(t *testing.T) {
	m := view.BoardModel{
		Project:        view.ProjectSummary{Key: "BMB", Name: "Test"},
		Columns:        []view.ColumnView{{Name: "Backlog", Kind: domain.KindBacklog}},
		ManualProgress: view.NewAssessedProgress(percentPtr(30), 2, nil, ""),
		AutoProgress:   view.NewDoneShareProgress(percentPtr(50), 2, 4),
	}
	page := view.SamplePage("Test", "board", m)
	html := renderProgress(t, "page-board", page)

	if !strings.Contains(html, "assessed") {
		t.Fatal("header does not label the manual progress metric")
	}
	if !strings.Contains(html, "tasks done") {
		t.Fatal("header does not label the automatic progress metric")
	}
	if !strings.Contains(html, "30% · 2 assessments") {
		t.Fatalf("manual metric value/label missing: %s", html)
	}
	if !strings.Contains(html, "50% · 2/4 tasks") {
		t.Fatalf("automatic metric value/label missing: %s", html)
	}
	// The two bars must actually differ: manual 30% paints 3 squares,
	// automatic 50% paints 5.
	if !strings.Contains(html, `data-filled="3"`) || !strings.Contains(html, `data-filled="5"`) {
		t.Fatalf("header bars do not carry distinct fill counts: %s", html)
	}

	// Neither metric renders when nobody measured it: an empty-project board
	// shows no header bars at all.
	bare := view.BoardModel{
		Project: view.ProjectSummary{Key: "BMB", Name: "Test"},
		Columns: []view.ColumnView{{Name: "Backlog", Kind: domain.KindBacklog}},
	}
	bareHTML := renderProgress(t, "page-board", view.SamplePage("Test", "board", bare))
	if strings.Contains(bareHTML, `class="pbar`) || strings.Contains(bareHTML, ">assessed<") || strings.Contains(bareHTML, ">tasks done<") {
		t.Fatalf("header rendered progress without any data: %s", bareHTML)
	}
}

// TestProgressView_Clickable pins the exact discriminator KANB-13's chart
// click affordance relies on: only a WithTracks'd, marks-derived metric is
// Clickable. A nil view, a bare NewAssessedProgress nobody attached a scope
// to, and the automatic done-share bar must all read false — a false
// positive here would render a dead click (a bar that looks openable but
// has nothing to fetch); a false negative would silently hide the feature
// on a real assessed metric.
func TestProgressView_Clickable(t *testing.T) {
	var nilView *view.ProgressView
	if nilView.Clickable() {
		t.Fatal("nil *ProgressView.Clickable() = true, want false")
	}
	bare := view.NewAssessedProgress(percentPtr(50), 1, nil, "")
	if bare.Clickable() {
		t.Fatal("assessed progress with no WithTracks call is Clickable, want false (nothing to fetch a scope for)")
	}
	auto := view.NewDoneShareProgress(percentPtr(50), 1, 2)
	if auto.Clickable() {
		t.Fatal("automatic done-share bar is Clickable, want false: it is not built from marks and has no history")
	}
	withScope := view.NewAssessedProgress(percentPtr(50), 1, nil, "").WithTracks("BMB", "BMB-1", nil)
	if !withScope.Clickable() {
		t.Fatal("assessed progress with WithTracks is not Clickable, want true")
	}
	// The project-level scope (empty TaskKey) must be Clickable too.
	withProjectScope := view.NewAssessedProgress(percentPtr(50), 1, nil, "").WithTracks("BMB", "", nil)
	if !withProjectScope.Clickable() {
		t.Fatal("project-scope (empty TaskKey) assessed progress is not Clickable, want true")
	}
}

// TestProgressBar_ChartClickTargetDoesNotSwallowTrackDelete: the chart click
// affordance and the delete-track cross are structurally separate elements
// (the toggle attribute lives only on .pbar; the cross lives inside the
// sibling .progress-tracks list) — this pins that shape so a future edit
// cannot accidentally nest one inside the other, which would let a click
// meant for the cross also (or instead) toggle the chart.
func TestProgressBar_ChartClickTargetDoesNotSwallowTrackDelete(t *testing.T) {
	pv := view.NewAssessedProgress(percentPtr(60), 2, nil, "").
		WithTracks("BMB", "BMB-1", []view.ProgressTrack{{Assessor: "alpha", Percent: 60, Count: 3}})
	html := renderProgress(t, "progress-bar", map[string]any{"Progress": pv})

	toggleIdx := strings.Index(html, "data-progress-chart-toggle")
	tracksListIdx := strings.Index(html, `<ul class="progress-tracks"`)
	deleteArmIdx := strings.Index(html, "data-progress-delete-arm")
	if toggleIdx < 0 || tracksListIdx < 0 || deleteArmIdx < 0 {
		t.Fatalf("expected all three markers present: toggle=%d tracksList=%d arm=%d\n%s", toggleIdx, tracksListIdx, deleteArmIdx, html)
	}
	// The chart toggle attribute must be written before the tracks <ul>
	// even starts, and the delete cross must live after that <ul> opens —
	// i.e. the two controls are siblings in document order, the cross is
	// never inside the element the toggle attribute is on. A click handler
	// scoped to [data-progress-chart-toggle] via closest() can therefore
	// never match an event whose target is the cross.
	if toggleIdx > tracksListIdx {
		t.Fatalf("chart toggle attribute (%d) appears after the tracks list starts (%d): it must be on the earlier .pbar span", toggleIdx, tracksListIdx)
	}
	if deleteArmIdx < tracksListIdx {
		t.Fatalf("delete-arm marker (%d) appears before the tracks list starts (%d): expected it inside <ul class=\"progress-tracks\">", deleteArmIdx, tracksListIdx)
	}
}

// TestProgressBar_WrappedInOneStableContainer pins the fix for an
// independent review's headline defect: deleting an assessor's track used to
// leave a stale bar on screen because app.js found "the old bar" by walking
// to the track list's fixed previous sibling, and a later task (the history
// chart, KANB-13) broke that fixed adjacency by inserting a new element
// between them — silently, forever, since no Go test executes app.js. The
// fix wraps the bar, the optional forecast badge, the optional chart
// container and the whole track list in ONE always-present container
// (data-progress-metric) so a client-side delete can replace it wholesale
// instead of depending on sibling order at all.
//
// This is the one part of that fix a Go test CAN pin: the container exists,
// wraps the fragment's ENTIRE output (nothing renders outside it — a stray
// element outside the wrapper would be exactly the kind of thing a future
// edit could leave behind for app.js to orphan again), and carries the
// metric's own scope so a delete POST's (project, task) always matches the
// container the bar itself was scoped to. The actual DOM replacement (app.js's
// replaceProgressMetric) cannot be exercised from Go — see REPORT.md for how
// that side was verified instead.
func TestProgressBar_WrappedInOneStableContainer(t *testing.T) {
	pv := view.NewAssessedProgress(percentPtr(60), 2, nil, "").
		WithTracks("BMB", "BMB-1", []view.ProgressTrack{{Assessor: "alpha", Percent: 60, Count: 3}})
	html := strings.TrimSpace(renderProgress(t, "progress-bar", map[string]any{"Progress": pv}))

	if n := countOccurrences(html, "data-progress-metric"); n != 1 {
		t.Fatalf("expected exactly one data-progress-metric container, found %d: %s", n, html)
	}
	if !strings.HasPrefix(html, `<span class="progress-metric" data-progress-metric data-project="BMB" data-task="BMB-1">`) {
		t.Fatalf("fragment does not open with the progress-metric wrapper carrying the metric's own scope: %s", html)
	}
	if !strings.HasSuffix(html, "</span>") {
		t.Fatalf("fragment does not end with the wrapper's own closing tag — something renders outside the container: %s", html)
	}
	// Both the bar and the track list must sit INSIDE that one wrapper, not
	// as its siblings — otherwise a whole-container replace would still
	// leave one of them behind.
	metricStart := strings.Index(html, "data-progress-metric")
	barIdx := strings.Index(html, `class="pbar`)
	tracksIdx := strings.Index(html, `<ul class="progress-tracks"`)
	if barIdx < metricStart || tracksIdx < metricStart {
		t.Fatalf("bar or tracks list appears before the wrapper opens: metric=%d bar=%d tracks=%d\n%s", metricStart, barIdx, tracksIdx, html)
	}
}

// TestProgressBar_ContainerPresentEvenWithoutTracks proves the wrapper is not
// conditioned on Tracks being non-empty: a Clickable metric with no tracks
// at all (WithTracks called with a nil slice — e.g. every mark for it was
// just deleted except this render still shows the surviving percent) still
// gets the same stable container, so app.js's replaceProgressMetric can rely
// on data-progress-metric being present unconditionally for every metric a
// delete-track POST can ever target.
func TestProgressBar_ContainerPresentEvenWithoutTracks(t *testing.T) {
	pv := view.NewAssessedProgress(percentPtr(30), 1, nil, "").WithTracks("BMB", "", nil)
	html := renderProgress(t, "progress-bar", map[string]any{"Progress": pv})
	if !strings.Contains(html, "data-progress-metric") {
		t.Fatalf("Clickable metric with no tracks did not get a progress-metric container: %s", html)
	}
}

// TestProgressMarkup_NoInlineStylesNoHandlers enforces the CSP constraints on
// the new markup: the painted count is carried by a class/attribute and
// painted in app.css — never a style="width:…", never an inline event
// handler, both dead or forbidden under `script-src 'self'`.
func TestProgressMarkup_NoInlineStylesNoHandlers(t *testing.T) {
	withBar := view.TaskCard{Key: "BMB-2", Title: "assessed", Progress: view.NewAssessedProgress(percentPtr(45), 3, nil, "")}
	for _, tc := range []struct {
		name string
		html string
	}{
		{"progress-bar", renderProgress(t, "progress-bar", map[string]any{"Progress": view.NewAssessedProgress(percentPtr(45), 3, nil, "")})},
		{"card-with-bar", renderProgress(t, "card", map[string]any{
			"Card":    withBar,
			"CSRF":    "",
			"Columns": []view.ColumnView{},
		})},
		{"board-page", func() string {
			m, _, _ := view.SampleBoardModelFor("BMB")
			return renderProgress(t, "page-board", view.SamplePage("Test", "board", m))
		}()},
	} {
		if strings.Contains(tc.html, `style="`) {
			t.Errorf("%s: carries an inline style attribute", tc.name)
		}
		if strings.Contains(tc.html, `style='`) {
			t.Errorf("%s: carries an inline style attribute", tc.name)
		}
		handler := regexp.MustCompile(`\son[a-z]+\s*=`)
		if loc := handler.FindStringIndex(tc.html); loc != nil {
			t.Errorf("%s: carries an inline event handler at %d: %q", tc.name, loc[0], tc.html[max(0, loc[0]-30):loc[1]+20])
		}
	}
}
