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
			"Progress": view.NewAssessedProgress(percentPtr(tc.percent), 3),
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
		"Progress": view.NewAssessedProgress(percentPtr(45), 3),
	})
	if !strings.Contains(html, "45% · 3 assessments") {
		t.Fatalf("label should read percent and track count, got: %s", html)
	}
	single := renderProgress(t, "progress-bar", map[string]any{
		"Progress": view.NewAssessedProgress(percentPtr(70), 1),
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
	if got := view.NewAssessedProgress(nil, 2); got != nil {
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
		ManualProgress: view.NewAssessedProgress(percentPtr(30), 2),
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

// TestProgressMarkup_NoInlineStylesNoHandlers enforces the CSP constraints on
// the new markup: the painted count is carried by a class/attribute and
// painted in app.css — never a style="width:…", never an inline event
// handler, both dead or forbidden under `script-src 'self'`.
func TestProgressMarkup_NoInlineStylesNoHandlers(t *testing.T) {
	withBar := view.TaskCard{Key: "BMB-2", Title: "assessed", Progress: view.NewAssessedProgress(percentPtr(45), 3)}
	for _, tc := range []struct {
		name string
		html string
	}{
		{"progress-bar", renderProgress(t, "progress-bar", map[string]any{"Progress": view.NewAssessedProgress(percentPtr(45), 3)})},
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
