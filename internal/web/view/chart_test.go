package view

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// helper to parse SVG polyline points "x1,y1 x2,y2 ..." into pairs of float64.
func parsePolylinePoints(t *testing.T, pointsAttr string) [][2]float64 {
	t.Helper()
	var result [][2]float64
	pairs := strings.Fields(pointsAttr)
	for _, pair := range pairs {
		parts := strings.Split(pair, ",")
		if len(parts) != 2 {
			t.Fatalf("malformed point pair %q in %q", pair, pointsAttr)
		}
		x, err1 := strconv.ParseFloat(parts[0], 64)
		y, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 != nil || err2 != nil {
			t.Fatalf("failed parsing coords %q: %v, %v", pair, err1, err2)
		}
		result = append(result, [2]float64{x, y})
	}
	return result
}

// extractPolylineByAttr finds a polyline matching an attribute snippet and returns its points attribute.
func extractPolylinePoints(t *testing.T, svg, attrSnippet string) string {
	t.Helper()
	re := regexp.MustCompile(`<polyline[^>]*` + regexp.QuoteMeta(attrSnippet) + `[^>]*>`)
	match := re.FindString(svg)
	if match == "" {
		t.Fatalf("polyline matching snippet %q not found in SVG:\n%s", attrSnippet, svg)
	}
	ptsRe := regexp.MustCompile(`points="([^"]+)"`)
	ptsMatch := ptsRe.FindStringSubmatch(match)
	if len(ptsMatch) < 2 {
		t.Fatalf("polyline %q has no points attribute", match)
	}
	return ptsMatch[1]
}

// 1. Golden test: fixed set of points producing deterministic SVG.
func TestChart_GoldenFixedPoints(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0},
		{ID: "m2", Assessor: "beta", Percent: 50, CreatedAt: t0.Add(15 * time.Minute)},
		{ID: "m3", Assessor: "alpha", Percent: 91, CreatedAt: t0.Add(30 * time.Minute)},
		{ID: "m4", Assessor: "alpha", Percent: 72, CreatedAt: t0.Add(45 * time.Minute)},
		{ID: "m5", Assessor: "beta", Percent: 80, CreatedAt: t0.Add(60 * time.Minute)},
	}

	svg := string(RenderProgressChart(marks, 600, 200))
	if svg == "" {
		t.Fatal("RenderProgressChart returned empty SVG")
	}

	// Verify outer SVG header and dimensions
	if !strings.Contains(svg, `viewBox="0 0 600 200"`) {
		t.Fatalf("expected viewBox 0 0 600 200, got: %s", svg)
	}
	if !strings.Contains(svg, `width="600" height="200"`) {
		t.Fatalf("expected width 600 height 200, got: %s", svg)
	}

	// Verify horizontal 50% line and label
	if !strings.Contains(svg, `50%</text>`) {
		t.Fatalf("missing 50%% label: %s", svg)
	}
	if !strings.Contains(svg, `stroke-dasharray="2 3"`) {
		t.Fatalf("missing 50%% guideline dash pattern: %s", svg)
	}

	// Verify time labels
	if !strings.Contains(svg, ">10:00<") {
		t.Fatalf("missing start time label 10:00: %s", svg)
	}
	if !strings.Contains(svg, ">11:00<") {
		t.Fatalf("missing end time label 11:00: %s", svg)
	}

	// Verify all tracks exist
	alphaPts := extractPolylinePoints(t, svg, `data-assessor="alpha"`)
	betaPts := extractPolylinePoints(t, svg, `data-assessor="beta"`)
	compPts := extractPolylinePoints(t, svg, `data-series="composite"`)

	if alphaPts == "" || betaPts == "" || compPts == "" {
		t.Fatalf("one or more series missing points: alpha=%q beta=%q comp=%q", alphaPts, betaPts, compPts)
	}

	// Check exact golden structure
	expectedSubstring := `data-series="composite"><title>Composite Progress</title></polyline>`
	if !strings.Contains(svg, expectedSubstring) {
		t.Fatalf("expected golden composite marker %q in %s", expectedSubstring, svg)
	}
}

// 2. Drop 91 -> 72 from the same assessor is preserved as a drop (Y coordinate grows downward).
func TestChart_Drop91To72_PreservedAsYGrowth(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "evaluator-1", Percent: 50, CreatedAt: t0},
		{ID: "m2", Assessor: "evaluator-1", Percent: 91, CreatedAt: t0.Add(30 * time.Minute)},
		{ID: "m3", Assessor: "evaluator-1", Percent: 72, CreatedAt: t0.Add(60 * time.Minute)},
	}

	const w = 600
	const h = 200
	svg := string(RenderProgressChart(marks, w, h))

	ptsStr := extractPolylinePoints(t, svg, `data-assessor="evaluator-1"`)
	coords := parsePolylinePoints(t, ptsStr)
	if len(coords) != 3 {
		t.Fatalf("expected 3 points for evaluator-1, got %d: %v", len(coords), coords)
	}

	p91 := coords[1]
	p72 := coords[2]

	// Time must advance horizontally: x(72) > x(91)
	if p72[0] <= p91[0] {
		t.Fatalf("x coordinate did not advance: x(91)=%.1f, x(72)=%.1f", p91[0], p72[0])
	}

	// In SVG coordinate space, Y increases downward.
	// Therefore, a drop in progress (91% -> 72%) MUST produce an increase in Y (y(72) > y(91)).
	if p72[1] <= p91[1] {
		t.Fatalf("drop 91 -> 72 failed: y(72) <= y(91): y(91)=%.1f, y(72)=%.1f (progress must fall, so Y must increase)", p91[1], p72[1])
	}

	// Verify expected Y coordinates within 0.2 precision.
	// Plot height = 200 - 16 - 24 = 160.
	// y(91) = 16 + (100 - 91)/100 * 160 = 16 + 14.4 = 30.4
	// y(72) = 16 + (100 - 72)/100 * 160 = 16 + 44.8 = 60.8
	expectedY91 := 16.0 + (9.0/100.0)*160.0
	expectedY72 := 16.0 + (28.0/100.0)*160.0

	if absFloat(p91[1]-expectedY91) > 0.2 {
		t.Fatalf("y(91) got %.1f, want %.1f", p91[1], expectedY91)
	}
	if absFloat(p72[1]-expectedY72) > 0.2 {
		t.Fatalf("y(72) got %.1f, want %.1f", p72[1], expectedY72)
	}
}

// 3. Time scale is proportional to real elapsed time, not point index.
func TestChart_TimeScale_ProportionalToRealTime(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	// Points at t=0min, 10min, 20min, 40min.
	// Elapsed times from t0:
	// t1: 10m
	// t2: 20m (2x the interval of t1)
	// t3: 40m (4x the interval of t1)
	marks := []domain.ProgressMark{
		{ID: "m0", Assessor: "tracker", Percent: 10, CreatedAt: t0},
		{ID: "m1", Assessor: "tracker", Percent: 30, CreatedAt: t0.Add(10 * time.Minute)},
		{ID: "m2", Assessor: "tracker", Percent: 50, CreatedAt: t0.Add(20 * time.Minute)},
		{ID: "m3", Assessor: "tracker", Percent: 70, CreatedAt: t0.Add(40 * time.Minute)},
	}

	svg := string(RenderProgressChart(marks, 600, 200))
	ptsStr := extractPolylinePoints(t, svg, `data-assessor="tracker"`)
	coords := parsePolylinePoints(t, ptsStr)
	if len(coords) != 4 {
		t.Fatalf("expected 4 points, got %d", len(coords))
	}

	x0 := coords[0][0]
	x1 := coords[1][0]
	x2 := coords[2][0]
	x3 := coords[3][0]

	dist1 := x1 - x0
	dist2 := x2 - x0
	dist3 := x3 - x0

	ratio2 := dist2 / dist1
	ratio3 := dist3 / dist1

	// In real time scaling:
	// dist2 / dist1 must be 20 / 10 = 2.0.
	// dist3 / dist1 must be 40 / 10 = 4.0.
	// (Under index scaling, dist2/dist1 would be 2.0, but dist3/dist1 would be 3/1 = 3.0).
	if absFloat(ratio2-2.0) > 0.05 {
		t.Fatalf("ratio dist2/dist1 = %.3f, want 2.0 (real time proportional)", ratio2)
	}
	if absFloat(ratio3-4.0) > 0.05 {
		t.Fatalf("ratio dist3/dist1 = %.3f, want 4.0 (real time proportional, caught index-based scaling)", ratio3)
	}
}

// 4. Polylines for each assessor plus separate highlighted polyline for composite indicator.
func TestChart_MultiAssessor_AndHighlightedComposite(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "agent-a", Percent: 40, CreatedAt: t0},
		{ID: "m2", Assessor: "agent-b", Percent: 60, CreatedAt: t0.Add(10 * time.Minute)},
		{ID: "m3", Assessor: "agent-a", Percent: 70, CreatedAt: t0.Add(20 * time.Minute)},
		{ID: "m4", Assessor: "agent-b", Percent: 80, CreatedAt: t0.Add(30 * time.Minute)},
	}

	svg := string(RenderProgressChart(marks, 600, 200))

	// Check polyline for agent-a
	if !strings.Contains(svg, `data-assessor="agent-a"`) {
		t.Fatal("missing polyline for agent-a")
	}
	// Check polyline for agent-b
	if !strings.Contains(svg, `data-assessor="agent-b"`) {
		t.Fatal("missing polyline for agent-b")
	}
	// Check polyline for composite
	if !strings.Contains(svg, `data-series="composite"`) {
		t.Fatal("missing polyline for composite indicator")
	}

	// Check that composite polyline has stroke-width 2.5 (highlighted)
	compRe := regexp.MustCompile(`<polyline[^>]*data-series="composite"[^>]*>`)
	compMatch := compRe.FindString(svg)
	if !strings.Contains(compMatch, `stroke-width="2.5"`) {
		t.Fatalf("composite line should have stroke-width=2.5, got: %s", compMatch)
	}

	// Check that individual assessors have stroke-width 1.2 (thinner)
	assessorRe := regexp.MustCompile(`<polyline[^>]*data-assessor="agent-a"[^>]*>`)
	assessorMatch := assessorRe.FindString(svg)
	if !strings.Contains(assessorMatch, `stroke-width="1.2"`) {
		t.Fatalf("assessor line should have stroke-width=1.2, got: %s", assessorMatch)
	}

	// Check distinct dash patterns between assessors
	assessorBRe := regexp.MustCompile(`<polyline[^>]*data-assessor="agent-b"[^>]*>`)
	assessorBMatch := assessorBRe.FindString(svg)

	dashA := extractAttr(assessorMatch, "stroke-dasharray")
	dashB := extractAttr(assessorBMatch, "stroke-dasharray")
	if dashA == "" || dashB == "" || dashA == dashB {
		t.Fatalf("expected distinct dash patterns for assessors: A=%q, B=%q", dashA, dashB)
	}

	// Check order: composite line rendered after assessor lines so it is on top
	posAssessor := strings.Index(svg, `data-assessor="agent-a"`)
	posComp := strings.Index(svg, `data-series="composite"`)
	if posComp <= posAssessor {
		t.Fatalf("composite line should be rendered after (on top of) assessor lines")
	}
}

// 5. Degenerate cases: empty history, single point, all points in one second.
func TestChart_DegenerateCases(t *testing.T) {
	// 5a. Empty history returns empty string
	if res := RenderProgressChart(nil, 600, 200); res != "" {
		t.Fatalf("RenderProgressChart(nil) = %q, want empty string", res)
	}
	if res := RenderProgressChart([]domain.ProgressMark{}, 600, 200); res != "" {
		t.Fatalf("RenderProgressChart([]) = %q, want empty string", res)
	}
	if view := NewProgressChartView(nil, 600, 200); view != nil {
		t.Fatalf("NewProgressChartView(nil) = %+v, want nil", view)
	}

	// 5b. Single point renders a circle point marker, NOT a polyline
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	single := []domain.ProgressMark{
		{ID: "m1", Assessor: "solo", Percent: 65, CreatedAt: t0},
	}
	svgSingle := string(RenderProgressChart(single, 600, 200))
	if strings.Contains(svgSingle, "<polyline") {
		t.Fatalf("single point history rendered a polyline: %s", svgSingle)
	}
	if !strings.Contains(svgSingle, "<circle") {
		t.Fatalf("single point history did not render a circle: %s", svgSingle)
	}
	if !strings.Contains(svgSingle, `data-assessor="solo"`) {
		t.Fatalf("single point circle missing data-assessor: %s", svgSingle)
	}
	if !strings.Contains(svgSingle, "50%") {
		t.Fatalf("single point chart missing 50%% mark: %s", svgSingle)
	}

	// 5c. All points in one second: duration == 0, must not divide by zero or panic
	sameSec := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0},
		{ID: "m2", Assessor: "beta", Percent: 45, CreatedAt: t0},
		{ID: "m3", Assessor: "alpha", Percent: 60, CreatedAt: t0},
		{ID: "m4", Assessor: "beta", Percent: 75, CreatedAt: t0},
	}
	svgSameSec := string(RenderProgressChart(sameSec, 600, 200))
	if svgSameSec == "" {
		t.Fatal("empty svg for all points in one second")
	}
	if strings.Contains(svgSameSec, "NaN") || strings.Contains(svgSameSec, "Inf") {
		t.Fatalf("svg contains NaN or Inf: %s", svgSameSec)
	}
}

// 6. 500 points: render does not crash, point count is decimated, drop 91 -> 72 survives.
func TestChart_500Points_DecimationPreservesDrop(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	var marks []domain.ProgressMark

	// 0..399: monotonic climb from 10% to 90%
	for i := 0; i < 400; i++ {
		p := 10 + (i * 80 / 400)
		marks = append(marks, domain.ProgressMark{
			ID:        fmt.Sprintf("m%03d", i),
			Assessor:  "heavy-bot",
			Percent:   p,
			CreatedAt: t0.Add(time.Duration(i) * time.Minute),
		})
	}

	// Point 400: 91%
	marks = append(marks, domain.ProgressMark{
		ID:        "m400",
		Assessor:  "heavy-bot",
		Percent:   91,
		CreatedAt: t0.Add(400 * time.Minute),
	})

	// Point 401: 72% (the sharp drop!)
	marks = append(marks, domain.ProgressMark{
		ID:        "m401",
		Assessor:  "heavy-bot",
		Percent:   72,
		CreatedAt: t0.Add(401 * time.Minute),
	})

	// 402..499: climb from 73% to 98%
	for i := 402; i < 500; i++ {
		p := 73 + ((i - 402) * 25 / 98)
		marks = append(marks, domain.ProgressMark{
			ID:        fmt.Sprintf("m%03d", i),
			Assessor:  "heavy-bot",
			Percent:   p,
			CreatedAt: t0.Add(time.Duration(i) * time.Minute),
		})
	}

	if len(marks) != 500 {
		t.Fatalf("expected 500 marks, got %d", len(marks))
	}

	// Render must not crash or hang
	svg := string(RenderProgressChart(marks, 600, 200))
	if svg == "" {
		t.Fatal("render returned empty SVG")
	}

	ptsStr := extractPolylinePoints(t, svg, `data-assessor="heavy-bot"`)
	coords := parsePolylinePoints(t, ptsStr)

	// Decimation check: count of points rendered must be far less than 500
	if len(coords) > MaxChartPoints {
		t.Fatalf("points not decimated: got %d points, want <= %d", len(coords), MaxChartPoints)
	}

	// The drop 91 -> 72 must survive decimation.
	// Search for the 91% coordinate followed by the 72% coordinate.
	expectedY91 := 16.0 + (9.0/100.0)*160.0
	expectedY72 := 16.0 + (28.0/100.0)*160.0

	foundDrop := false
	for i := 0; i < len(coords)-1; i++ {
		curr := coords[i]
		next := coords[i+1]
		if absFloat(curr[1]-expectedY91) < 0.2 && absFloat(next[1]-expectedY72) < 0.2 {
			if next[1] > curr[1] && next[0] > curr[0] {
				foundDrop = true
				break
			}
		}
	}

	if !foundDrop {
		t.Fatalf("drop 91 -> 72 did not survive decimation! Coordinates:\n%v", coords)
	}
}

// 7. Evaluator name with <, &, quotes is properly escaped.
func TestChart_EscapesAssessorNames(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: `<evaluator & "special'quotes">`, Percent: 45, CreatedAt: t0},
		{ID: "m2", Assessor: `<evaluator & "special'quotes">`, Percent: 65, CreatedAt: t0.Add(10 * time.Minute)},
	}

	svg := string(RenderProgressChart(marks, 600, 200))

	// Must NOT contain raw unescaped characters
	if strings.Contains(svg, `<evaluator`) {
		t.Fatalf("raw unescaped '<evaluator' leaked into SVG markup: %s", svg)
	}
	if strings.Contains(svg, `"special'quotes"`) {
		t.Fatalf("raw unescaped quotes leaked into SVG markup: %s", svg)
	}

	// Must contain safely escaped entity representation
	if !strings.Contains(svg, "&lt;evaluator &amp; &#34;special&#39;quotes&#34;&gt;") {
		t.Fatalf("escaped entity missing in SVG: %s", svg)
	}
}

// 8. CSP compliance: no inline style attributes and no inline event handlers.
func TestChart_NoInlineStylesOrHandlers(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0},
		{ID: "m2", Assessor: "beta", Percent: 70, CreatedAt: t0.Add(20 * time.Minute)},
	}

	svg := string(RenderProgressChart(marks, 600, 200))

	if strings.Contains(svg, `style="`) || strings.Contains(svg, `style='`) {
		t.Fatalf("SVG contains inline style attribute: %s", svg)
	}

	handlerRe := regexp.MustCompile(`\son[a-z]+\s*=`)
	if loc := handlerRe.FindStringIndex(svg); loc != nil {
		t.Fatalf("SVG contains inline event handler at %d: %s", loc[0], svg)
	}
}

// 9. Input sorting: marks out of chronological order are sorted by time.
func TestChart_SortsUnsortedMarksChronologically(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	// Provide marks in reverse order
	marks := []domain.ProgressMark{
		{ID: "m3", Assessor: "bot", Percent: 80, CreatedAt: t0.Add(40 * time.Minute)},
		{ID: "m2", Assessor: "bot", Percent: 50, CreatedAt: t0.Add(20 * time.Minute)},
		{ID: "m1", Assessor: "bot", Percent: 20, CreatedAt: t0},
	}

	svg := string(RenderProgressChart(marks, 600, 200))
	ptsStr := extractPolylinePoints(t, svg, `data-assessor="bot"`)
	coords := parsePolylinePoints(t, ptsStr)

	if len(coords) != 3 {
		t.Fatalf("expected 3 points, got %d", len(coords))
	}
	// Coordinates must be in chronological X order
	if coords[0][0] >= coords[1][0] || coords[1][0] >= coords[2][0] {
		t.Fatalf("X coordinates not sorted: %v", coords)
	}
}

// 10. Dimensions and clamping: zero or negative dimensions fallback to defaults.
func TestChart_DimensionsAndPercentClamping(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "clamped", Percent: -20, CreatedAt: t0},
		{ID: "m2", Assessor: "clamped", Percent: 150, CreatedAt: t0.Add(10 * time.Minute)},
	}

	svg := string(RenderProgressChart(marks, 0, -50))
	if !strings.Contains(svg, fmt.Sprintf(`width="%d" height="%d"`, DefaultChartWidth, DefaultChartHeight)) {
		t.Fatalf("fallback dimensions missing in SVG: %s", svg)
	}

	ptsStr := extractPolylinePoints(t, svg, `data-assessor="clamped"`)
	coords := parsePolylinePoints(t, ptsStr)

	// -20% should clamp to 0% -> yMax (176.0)
	// 150% should clamp to 100% -> yMin (16.0)
	if absFloat(coords[0][1]-176.0) > 0.2 {
		t.Fatalf("negative percent coordinate got %.1f, want 176.0 (clamped to 0%%)", coords[0][1])
	}
	if absFloat(coords[1][1]-16.0) > 0.2 {
		t.Fatalf("over-100 percent coordinate got %.1f, want 16.0 (clamped to 100%%)", coords[1][1])
	}
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func extractAttr(tag, attrName string) string {
	re := regexp.MustCompile(attrName + `="([^"]*)"`)
	m := re.FindStringSubmatch(tag)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}
