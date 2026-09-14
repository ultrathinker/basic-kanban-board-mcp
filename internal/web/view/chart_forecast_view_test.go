package view

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// forecastPt is the local shorthand used by the time fixtures below.
func forecastPt(t time.Time) *time.Time { return &t }

// 1. KANB-36: Y axis is a date axis, not 0..100. Every labelled Y tick on
// the enlarged chart must be a date (matches formatForecastAxisDate's
// grammar), and NO percent label may have leaked onto the forecast chart
// from the old "0..100 normalized" rendering.
func TestChart_Forecast_YAxisIsADateAxis(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0, ETA: forecastPt(t0.Add(5 * 24 * time.Hour))},
		{ID: "m2", Assessor: "alpha", Percent: 50, CreatedAt: t0.Add(time.Hour), ETA: forecastPt(t0.Add(7 * 24 * time.Hour))},
		{ID: "m3", Assessor: "alpha", Percent: 70, CreatedAt: t0.Add(2 * time.Hour), ETA: forecastPt(t0.Add(10 * 24 * time.Hour))},
	}

	svg := string(RenderForecastChartDetailed(marks, 600, 200))

	// Pick any percentage literal — the old forecast rendering drew one.
	for _, pct := range []string{"0%", "25%", "50%", "75%", "100%"} {
		if strings.Contains(svg, pct) {
			t.Errorf("forecast chart Y axis leaked a percent label %q: KANB-36 wants a date axis, not 0..100.\nSVG:\n%s", pct, svg)
		}
	}
	// At least one date-shaped Y label must be present: "Sep" or "2026"
	// (the formatForecastAxisDate rules produce either, depending on
	// span).
	if !strings.Contains(svg, "Sep") && !strings.Contains(svg, "2026") {
		t.Errorf("forecast chart Y axis is missing any date-shaped label.\nSVG:\n%s", svg)
	}
}

// 2. KANB-36: the old forecast track on the percent chart is gone.
// renderProgressChart used to draw a "data-series=\"forecast\"" polyline
// on its own percent axis; it must not anymore. Same expectation for the
// "Forecast history" title text the old rendering carried.
func TestChart_Forecast_TrackGoneFromPercentChart(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0, ETA: forecastPt(t0.Add(5 * 24 * time.Hour))},
		{ID: "m2", Assessor: "alpha", Percent: 70, CreatedAt: t0.Add(time.Hour), ETA: forecastPt(t0.Add(8 * 24 * time.Hour))},
	}

	svg := string(RenderProgressChart(marks, 600, 200))
	if strings.Contains(svg, `data-series="forecast"`) {
		t.Fatalf("percent chart still carries a forecast track: %s", svg)
	}
	if strings.Contains(svg, "Forecast history") {
		t.Fatalf("percent chart still carries a forecast-history tooltip: %s", svg)
	}
}

// 3. KANB-36: a held promise draws as a flat line on the date axis. The
// same fixture the old test used to assert the same shape on the percent
// axis; the new chart should preserve the property (a held promise reads
// as held) under a different visualisation. ETAs are the raw dates from
// the input — no recomputation against "today" or any later baseline.
func TestChart_Forecast_HoldingIsFlatOnDateAxis(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	fixed := t0.Add(30 * 24 * time.Hour)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "bot", Percent: 10, CreatedAt: t0, ETA: forecastPt(fixed)},
		{ID: "m2", Assessor: "bot", Percent: 30, CreatedAt: t0.Add(time.Hour), ETA: forecastPt(fixed)},
		{ID: "m3", Assessor: "bot", Percent: 50, CreatedAt: t0.Add(2 * time.Hour), ETA: forecastPt(fixed)},
		{ID: "m4", Assessor: "bot", Percent: 70, CreatedAt: t0.Add(3 * time.Hour), ETA: forecastPt(fixed)},
	}

	svg := string(RenderForecastChart(marks, 600, 200))
	pts := extractPolylinePoints(t, svg, `data-assessor="bot"`)
	coords := parsePolylinePoints(t, pts)
	if len(coords) != 4 {
		t.Fatalf("expected 4 forecast points, got %d: %v", len(coords), coords)
	}
	first := coords[0][1]
	for i, c := range coords {
		if absFloat(c[1]-first) > 0.01 {
			t.Fatalf("a held forecast did not render flat at point %d: %v (first=%.2f)", i, coords, first)
		}
	}
	// X still advances with real time even though Y is flat.
	if coords[3][0] <= coords[0][0] {
		t.Fatalf("forecast x coordinates did not advance with time: %v", coords)
	}
}

// 4. KANB-36: a sliding promise trends DOWNWARD on the date axis — a
// later ETA sits at a larger SVG Y than an earlier one, exactly the
// direction a percent drop already reads on its own chart. The same
// invariant the old chart promised, just on a different axis. The
// spacing here is deliberately non-uniform (see test 5 for the
// spacing-vs-order argument that motivated this fixture).
func TestChart_Forecast_SlidesTowardBottom(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "bot", Percent: 10, CreatedAt: t0, ETA: forecastPt(t0.Add(5 * 24 * time.Hour))},
		{ID: "m2", Assessor: "bot", Percent: 30, CreatedAt: t0.Add(time.Hour), ETA: forecastPt(t0.Add(6*24*time.Hour + time.Hour))},
		{ID: "m3", Assessor: "bot", Percent: 50, CreatedAt: t0.Add(2 * time.Hour), ETA: forecastPt(t0.Add(7 * 24 * time.Hour))},
		{ID: "m4", Assessor: "bot", Percent: 70, CreatedAt: t0.Add(3 * time.Hour), ETA: forecastPt(t0.Add(8 * 24 * time.Hour))},
	}

	svg := string(RenderForecastChart(marks, 600, 200))
	pts := extractPolylinePoints(t, svg, `data-assessor="bot"`)
	coords := parsePolylinePoints(t, pts)
	if len(coords) != 4 {
		t.Fatalf("expected 4 forecast points, got %d: %v", len(coords), coords)
	}

	// Strictly increasing Y (each later promise sinks further toward the
	// bottom) — a monotonic slide, never flat, never bouncing back up.
	for i := 1; i < len(coords); i++ {
		if coords[i][1] <= coords[i-1][1] {
			t.Fatalf("forecast track did not sink monotonically at step %d: %v", i, coords)
		}
	}
	// The earliest forecast (best case) must sit at the TOP of the date
	// axis (smallest Y, since SVG Y grows downward); the latest at the
	// BOTTOM (largest Y).
	plotTop := ChartPadTop
	plotBottom := 200.0 - ChartPadBottom
	if got := coords[0][1]; absFloat(got-plotTop) > 0.2 {
		t.Fatalf("earliest forecast not at top: got y=%.1f, want %.1f", got, plotTop)
	}
	if got := coords[3][1]; absFloat(got-plotBottom) > 0.2 {
		t.Fatalf("latest forecast not at bottom: got y=%.1f, want %.1f", got, plotBottom)
	}
}

// 5. KANB-36 / REVIEW 13.09: the old forecast test passed because the
// points were UNIFORMLY spaced in time, so any "by value" mapping was
// indistinguishable from a "by index" mapping — both produced the same
// coordinates. The fix is to use NON-uniform spacing: gaps like "+5d,
// +5d1m, +30d" are different lengths, so a "by value" sort must place
// points where their dates put them, not where their position in the
// input did. This test would have caught the bug the review surfaced.
//
// The fixture uses three distinct gaps (5 days, 5 days + 1 month, 30
// days) and one input order (5d, 30d, 5d1m — deliberately NOT sorted).
// A correct chart re-orders by date and plots each point at its real
// ETA coordinate; a buggy chart plots them at indices 0/1/2.
// The chart sorts by WHEN each forecast was made (CreatedAt), not by the date
// it promised: x is real time, so an unsorted input would draw a line that
// jumps back and forth across the chart instead of tracking the promise as
// time goes on.
//
// The name used to say "sort by value not by index", which described neither
// the implementation nor the assertion — the fixture's CreatedAt order and ETA
// order happen to coincide, so the Y check below passes on chronological
// sorting rather than proving any ordering by value.
func TestChart_Forecast_UnorderedInputIsSortedChronologically(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	// Fed out of order on purpose: the marks arrive t0, t0+2h, t0+1h. A
	// chart that plots them in input order draws the third point to the LEFT
	// of the second, so the line doubles back on itself.
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "bot", Percent: 10, CreatedAt: t0, ETA: forecastPt(t0.Add(5 * 24 * time.Hour))},
		{ID: "m3", Assessor: "bot", Percent: 50, CreatedAt: t0.Add(2 * time.Hour), ETA: forecastPt(t0.Add(5*24*time.Hour + 30*24*time.Hour))},
		{ID: "m2", Assessor: "bot", Percent: 30, CreatedAt: t0.Add(time.Hour), ETA: forecastPt(t0.Add(5*24*time.Hour + time.Hour))},
	}

	svg := string(RenderForecastChart(marks, 600, 200))
	pts := extractPolylinePoints(t, svg, `data-assessor="bot"`)
	coords := parsePolylinePoints(t, pts)
	if len(coords) != 3 {
		t.Fatalf("expected 3 forecast points, got %d: %v", len(coords), coords)
	}

	// THE property: x increases along the line, because the line is drawn in
	// the order the forecasts were made. This is what fails when the sort is
	// dropped.
	for i := 1; i < len(coords); i++ {
		if !(coords[i][0] > coords[i-1][0]) {
			t.Fatalf("X is not strictly increasing in the rendered line: %v\n"+
				"The marks were fed out of chronological order and drawn that way, so the line "+
				"doubles back instead of tracking the promise forward in time.\nSVG:\n%s", coords, svg)
		}
	}
	// In THIS fixture the promised dates happen to rise with the times they
	// were made, so y rises along the line too. That is a property of the
	// fixture, not a rule of the chart — the ordering guarantee above is the
	// one being pinned.
	if !(coords[0][1] < coords[1][1]) || !(coords[1][1] < coords[2][1]) {
		t.Fatalf("Y coordinates do not follow the promised dates for this fixture: %v\nSVG:\n%s", coords, svg)
	}
}

// 6. KANB-36: a history with NO forecast renders nothing at all. This
// pins the "no data, no placeholder" rule that the percent chart and
// the items chart already follow: a frame around "no one has forecast
// this scope yet" would be a worse lie than nothing.
func TestChart_Forecast_AbsentWithoutAnyETA(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0},
		{ID: "m2", Assessor: "beta", Percent: 50, CreatedAt: t0.Add(time.Hour)},
	}
	view := NewForecastChartView(marks, 600, 200)
	if view != nil {
		t.Fatalf("NewForecastChartView returned %+v for history with no ETA; want nil", view)
	}
	if svg := string(RenderForecastChart(marks, 600, 200)); svg != "" {
		t.Fatalf("RenderForecastChart rendered %q for history with no ETA; want empty", svg)
	}
	if svg := string(RenderForecastChartDetailed(marks, 600, 200)); svg != "" {
		t.Fatalf("RenderForecastChartDetailed rendered %q for history with no ETA; want empty", svg)
	}
}

// 7. KANB-36: the consensus line is the AVERAGE ETA at each unique time
// point — one polyline (or circle for a one-point consensus), with one
// point per unique CreatedAt that received at least one forecast. Two
// assessors forecasting the same instant must produce a single consensus
// point whose ETA is the mean of theirs.
func TestChart_Forecast_ConsensusIsMeanAtEachTimestamp(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0, ETA: forecastPt(t0.Add(10 * 24 * time.Hour))},
		{ID: "m2", Assessor: "beta", Percent: 50, CreatedAt: t0, ETA: forecastPt(t0.Add(20 * 24 * time.Hour))},
	}

	svg := string(RenderForecastChart(marks, 600, 200))
	if !strings.Contains(svg, `data-series="consensus"`) {
		t.Fatalf("consensus line missing: %s", svg)
	}

	// The one-point case renders as a <circle>, not a <polyline> —
	// same rule the percent chart follows for single-point tracks.
	coord := extractConsensusCoord(t, svg)
	// The consensus point's Y must sit at the mean of the two ETAs: 10d
	// and 20d from t0, averaged = 15d. On the date axis (10d .. 20d),
	// the midpoint is the vertical centre of the plot area.
	plotTop := ChartPadTop
	plotBottom := 200.0 - ChartPadBottom
	wantY := (plotTop + plotBottom) / 2.0
	if absFloat(coord[1]-wantY) > 0.5 {
		t.Fatalf("consensus point not at mean ETA: got y=%.1f, want %.1f (mean of 10d and 20d)", coord[1], wantY)
	}
}

// extractConsensusCoord returns the (x, y) of the single consensus marker,
// whether the renderer drew it as a <polyline> or a <circle>. The percent
// chart's own one-point tracks fall back to <circle> — same vocabulary
// here — so a test that hardcodes one or the other would miss the other
// shape and report a false negative on perfectly correct output.
func extractConsensusCoord(t *testing.T, svg string) [2]float64 {
	t.Helper()
	if strings.Contains(svg, "<polyline") && strings.Contains(svg, `data-series="consensus"`) {
		pts := extractPolylinePoints(t, svg, `data-series="consensus"`)
		coords := parsePolylinePoints(t, pts)
		if len(coords) != 1 {
			t.Fatalf("expected exactly one consensus point, got %d: %v", len(coords), coords)
		}
		return coords[0]
	}
	re := regexp.MustCompile(`<circle[^>]*data-series="consensus"[^>]*>`)
	m := re.FindString(svg)
	if m == "" {
		t.Fatalf("no consensus marker found: %s", svg)
	}
	ptRe := regexp.MustCompile(`cx="([\d.]+)"\s+cy="([\d.]+)"`)
	mm := ptRe.FindStringSubmatch(m)
	if len(mm) < 3 {
		t.Fatalf("could not parse cx/cy from circle: %s", m)
	}
	x, err1 := parseFloat(mm[1])
	y, err2 := parseFloat(mm[2])
	if err1 != nil || err2 != nil {
		t.Fatalf("cx/cy parse failed: %v %v", err1, err2)
	}
	return [2]float64{x, y}
}

// parseFloat is a tiny wrapper around strconv.ParseFloat so the test
// stays self-contained (the package's parsePolylinePoints is internal,
// not reusable from a regex match's capture groups).
func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// 8. KANB-36: tooltips on every forecast point name the promised date AND
// the remaining time at the moment of the forecast — both numbers,
// because "remaining time" alone shrinks every day even when nothing
// changed. This pins the "tooltip has both numbers" half of the brief.
//
// The renderer uses two tooltip shapes: single points carry the date plus
// "(in Xd/m/h)", polylines carry the range ("start -> end"). Both must
// include the date; the single-point ones also carry the "in ..." suffix.
// This test splits them rather than demanding every title satisfy one
// rigid pattern.
func TestChart_Forecast_TooltipHasDateAndRemaining(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0, ETA: forecastPt(t0.Add(5*24*time.Hour + 3*time.Hour))},
		{ID: "m2", Assessor: "alpha", Percent: 50, CreatedAt: t0.Add(time.Hour), ETA: forecastPt(t0.Add(10 * 24 * time.Hour))},
	}

	svg := string(RenderForecastChart(marks, 600, 200))
	titleRe := regexp.MustCompile(`<title>([^<]+)</title>`)
	matches := titleRe.FindAllStringSubmatch(svg, -1)
	if len(matches) == 0 {
		t.Fatal("no <title> elements found in forecast chart")
	}

	dateRe := regexp.MustCompile(`2026-\d{2}-\d{2}`)
	remainingRe := regexp.MustCompile(`in \d+[mhd]`)
	sawSinglePointTooltip := false

	for _, m := range matches {
		text := m[1]
		// Every tooltip must include a date in the expected shape.
		if !dateRe.MatchString(text) {
			t.Errorf("tooltip missing promised-date: %q", text)
		}
		// A single-point tooltip carries the "(in Xd/m/h)" suffix; this
		// test only guarantees ONE such tooltip exists per point, so
		// record that we saw at least one and stop complaining if the
		// rest are polyline tooltips without the suffix.
		if remainingRe.MatchString(text) {
			sawSinglePointTooltip = true
		}
	}
	if !sawSinglePointTooltip {
		t.Error("no tooltip carried the 'in Xd/m/h' remaining-time suffix")
	}
}

// 9. REVIEW C #5: a promise whose date had already passed when it was made
// must read as OVERDUE, not as imminent. humanizeDuration has no sign, so
// the unguarded remainder fell into its "<1m" branch and a month-late
// promise rendered "(in <1m)" — a past date described as arriving in under
// a minute. The service deliberately supports past ETAs (its own forecast
// tests feed a 48h-past eta and require it to reach the view), so this is a
// real path, not a degenerate input.
//
// The future case is asserted TOO, on purpose: a "fix" that flips the
// comparison would move the lie to the other side (every future promise
// suddenly "overdue"), and a past-only test would stay green while the
// tooltip got worse for the common case.
func TestChart_Forecast_TooltipCallsAPastPromiseOverdue(t *testing.T) {
	made := time.Date(2026, 9, 12, 12, 0, 0, 0, time.Local)

	past := tooltipText(forecastPoint{CreatedAt: made, ETA: made.Add(-30 * 24 * time.Hour)})
	if !strings.Contains(past, "2026-08-13") || !strings.Contains(past, "overdue by 30d") {
		t.Errorf("tooltip for a promise 30 days past = %q, want the date plus \"overdue by 30d\"", past)
	}
	if strings.Contains(past, "(in ") {
		t.Errorf("tooltip for a past promise still claims remaining time: %q", past)
	}

	future := tooltipText(forecastPoint{CreatedAt: made, ETA: made.Add(90 * 24 * time.Hour)})
	if !strings.Contains(future, "2026-12-11") || !strings.Contains(future, "(in 90d)") {
		t.Errorf("tooltip for a promise 90 days ahead = %q, want the date plus \"(in 90d)\"", future)
	}

	// End to end: the overdue phrasing must survive into the rendered SVG,
	// not only live in the helper — a renderer that swapped tooltipText for
	// its own inline formatting would otherwise pass unnoticed.
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: made, ETA: forecastPt(made.Add(-30 * 24 * time.Hour))},
	}
	svg := string(RenderForecastChart(marks, 600, 200))
	if !strings.Contains(svg, "overdue by 30d") {
		t.Errorf("rendered forecast chart has no overdue tooltip for the past promise:\n%s", svg)
	}
	if strings.Contains(svg, "(in <1m)") {
		t.Errorf("rendered forecast chart describes a past promise as imminent:\n%s", svg)
	}
}

// 10. REVIEW C #6: no text on a forecast chart may start left of the SVG
// viewport. Both surfaces anchor a full "2006-01-02" label left of the plot
// area — the inline panel's earliest-ETA reference and the modal's date
// axis — and the shared ChartPadLeft (36px) gave a 10-character date less
// than half the room it needs (~60px), so the viewport clipped the left
// half of every axis date.
//
// SVG has no server-side text metrics, so the check estimates each label's
// width at 0.5em per glyph — deliberately UNDER the real width of the
// alphabet these labels use (digits, '-', ':', ' ' measure ~0.53em in the
// system UI fonts, so 0.5 can only understate). An understated width makes
// the check lenient: it may pass a label that is a pixel or two over the
// edge, but it can never fail one that fits, and the two-thirds-of-the-
// label clip the old pad produced is far past any estimation error.
func TestChart_Forecast_DateLabelsAreNotClippedByTheLeftEdge(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	// The ETA span crosses the 31-day switch in formatForecastAxisDate, so
	// the modal's axis prints FULL dates — the widest, most easily clipped
	// case — and always five of them.
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0, ETA: forecastPt(t0.Add(5 * 24 * time.Hour))},
		{ID: "m2", Assessor: "alpha", Percent: 50, CreatedAt: t0.Add(time.Hour), ETA: forecastPt(t0.Add(95 * 24 * time.Hour))},
	}

	textRe := regexp.MustCompile(`<text x="(-?[\d.]+)" y="[-\d.]+" text-anchor="(start|middle|end)" font-size="(\d+)"[^>]*>([^<]+)</text>`)
	check := func(svg, what string) {
		t.Helper()
		matches := textRe.FindAllStringSubmatch(svg, -1)
		if len(matches) == 0 {
			t.Fatalf("%s: no labelled text elements found to check", what)
		}
		dateLabels := 0
		for _, m := range matches {
			x, _ := strconv.ParseFloat(m[1], 64)
			fs, _ := strconv.ParseFloat(m[3], 64)
			label, anchor := m[4], m[2]
			left := x
			switch anchor {
			case "end":
				left = x - 0.5*fs*float64(len(label))
			case "middle":
				left = x - 0.5*fs*float64(len(label))/2
			}
			if left < 0 {
				t.Errorf("%s: label %q at x=%.1f (font %.0f, anchor %s) starts at x=%.1f — left of the SVG edge, the reader sees a clipped tail",
					what, label, x, fs, anchor, left)
			}
			if strings.HasPrefix(label, "2026-") {
				dateLabels++
			}
		}
		// The guard must actually be looking at full dates, not merely at
		// whatever labels happen to be present: a fixture change that
		// shrank the ETA span would otherwise turn this into a test of
		// short "Mon DD" labels and quietly stop covering the defect.
		if what == "modal" && dateLabels < 5 {
			t.Fatalf("modal: expected the five full-date axis labels, saw %d — the fixture no longer covers the 10-character case", dateLabels)
		}
	}

	check(string(RenderForecastChart(marks, DefaultChartWidth, DefaultChartHeight)), "inline panel")
	check(string(RenderForecastChartDetailed(marks, DetailChartWidth, DetailChartHeight)), "modal")
}

// 11. REVIEW C #7: the enlarged forecast chart's legend must name the number
// of points the picture actually draws. The legend used to quote the raw
// series length while the renderer decimated the polyline to MaxChartPoints,
// so a 300-point history shipped "300 pts" beside a 60-point line — and the
// line's own hover tooltip printed the drawn count, making the legend and
// the tooltip on one picture contradict each other. The rule the legend's
// own comment states ("They MUST agree") was true everywhere except the one
// place a reader could check it.
func TestChart_Forecast_DetailLegendPointsMatchTheDrawnPolyline(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	const n = 300
	var marks []domain.ProgressMark
	for i := 0; i < n; i++ {
		marks = append(marks, forecastMark(
			"m"+strconv.Itoa(i), "busy-bot", 10+(i*80/n),
			t0.Add(time.Duration(i)*time.Minute),
			t0.Add(time.Duration(30+i%20)*24*time.Hour),
		))
	}

	detail := NewForecastChartDetail(marks)
	if detail == nil {
		t.Fatal("NewForecastChartDetail returned nil for a 300-forecast history")
	}

	drawn := func(seriesAttr string) int {
		t.Helper()
		coords := parsePolylinePoints(t, extractPolylinePoints(t, string(detail.SVG), seriesAttr))
		return len(coords)
	}
	assessorDrawn := drawn(`data-assessor="busy-bot"`)
	consensusDrawn := drawn(`data-series="consensus"`)

	// The fixture must sit inside the regime the defect lives in — past the
	// decimation budget — so the legend/picture equality below is a check
	// on real disagreement, not an accident of both being equal because
	// nothing was thinned.
	if assessorDrawn >= n || consensusDrawn >= n {
		t.Fatalf("fixture did not decimate: assessor %d, consensus %d of %d raw points", assessorDrawn, consensusDrawn, n)
	}

	legend := map[string]int{}
	for _, e := range detail.Legend {
		legend[e.Label] = e.Points
	}
	if got, want := legend["busy-bot"], assessorDrawn; got != want {
		t.Errorf("legend claims %d points for the assessor but the polyline draws %d — the legend and the picture contradict each other", got, want)
	}
	if got, want := legend["consensus"], consensusDrawn; got != want {
		t.Errorf("legend claims %d points for the consensus but the polyline draws %d", got, want)
	}
}

// 12. REVIEW C #11: the modal's date axis must not print the same label
// twice. pickForecastDateTicks always picks five evenly-spaced ticks, and
// on an ETA span of a couple of days several of those land on the SAME
// calendar day — which day-precision labels ("Sep 13") cannot tell apart.
// The reader got five gridlines signed by two dates, three identical, and
// could not tell which line was which. A tick whose label repeats the
// previous one is now dropped whole; this pins that no printed label
// survives twice on the axis.
func TestChart_Forecast_ModalDateAxisDoesNotRepeatALabel(t *testing.T) {
	// ETA span exactly one day, fixed at noon so no DST shift can move a
	// date under the fixture: the five ticks land two days wide, the very
	// shape the review measured as "Sep 13" three times.
	t0 := time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0, ETA: forecastPt(t0)},
		{ID: "m2", Assessor: "alpha", Percent: 50, CreatedAt: t0.Add(time.Hour), ETA: forecastPt(t0.Add(24 * time.Hour))},
	}

	svg := string(RenderForecastChartDetailed(marks, DetailChartWidth, DetailChartHeight))

	textRe := regexp.MustCompile(`<text[^>]*>([^<]+)</text>`)
	counts := map[string]int{}
	for _, m := range textRe.FindAllStringSubmatch(svg, -1) {
		counts[m[1]]++
	}
	for label, n := range counts {
		if n > 1 {
			t.Errorf("modal date axis printed label %q %d times — repeated labels leave the gridlines indistinguishable:\n%s", label, n, svg)
		}
	}
	// Both end dates must still be named exactly once: the dedupe thins
	// the MIDDLE of the axis, it must not swallow the bounds a reader
	// measures the span by.
	if counts["Sep 13"] != 1 || counts["Sep 14"] != 1 {
		t.Errorf("date axis labels = %v, want Sep 13 and Sep 14 exactly once each", counts)
	}
}
