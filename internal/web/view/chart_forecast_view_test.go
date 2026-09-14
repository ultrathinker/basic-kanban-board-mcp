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
