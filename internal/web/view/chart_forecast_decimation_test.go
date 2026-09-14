package view

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// Decimation and degenerate cases on the forecast chart.
//
// KANB-36 moved the forecast off the percent chart's 0..100 axis onto its own
// date axis. The move dropped chart_forecast_test.go, whose seven tests
// guarded the OLD rendering. Three of those guarded properties that are just
// as live on the new chart, and one of them — decimation — had actually been
// lost together with its test: 500 forecasts rendered 500 polyline points and
// 133 KB of inline SVG, where the percent chart holds to MaxChartPoints.
//
// This file restores the substance of the deleted
// TestChart_ForecastTrack_DecimatesLargeHistory on the new chart, and closes
// the two degenerate cases (a single forecast, and every forecast naming the
// same instant) that were working but unguarded.
//
// Fixtures are built on time.Local, not time.UTC: the renderer formats for the
// reader's clock, and a UTC fixture is green here and red on a machine in
// another zone.
// ---------------------------------------------------------------------------

// forecastMark is the fixture helper: a mark carrying a forecast.
func forecastMark(id, assessor string, pct int, at, eta time.Time) domain.ProgressMark {
	e := eta
	return domain.ProgressMark{ID: id, Assessor: assessor, Percent: pct, CreatedAt: at, ETA: &e}
}

// 1. Hundreds of forecasts must decimate to the same budget every other track
// on this board obeys, must not panic, and every coordinate must be finite.
// This is the deleted test's property, restored on the new chart.
func TestChart_Forecast_DecimatesLargeHistory(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	var marks []domain.ProgressMark
	for i := 0; i < 500; i++ {
		marks = append(marks, forecastMark(
			"m"+strconv.Itoa(i), "heavy-bot", 10+(i*80/500),
			t0.Add(time.Duration(i)*time.Minute),
			t0.Add(time.Duration(30+i)*24*time.Hour),
		))
	}

	svg := string(RenderForecastChart(marks, 600, 200))
	if svg == "" {
		t.Fatal("render returned empty SVG for a 500-forecast history")
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Fatalf("forecast track produced NaN/Inf: %s", svg)
	}

	coords := parsePolylinePoints(t, extractPolylinePoints(t, svg, `data-assessor="heavy-bot"`))
	if len(coords) > MaxChartPoints {
		t.Errorf("forecast track not decimated: got %d points, want <= %d", len(coords), MaxChartPoints)
	}
	if len(coords) < 2 {
		t.Errorf("forecast track over-decimated to %d points", len(coords))
	}
}

// 2a. An isolated slip: the promise held, jumped once, held again.
//
// A lone spike is a local extremum, and the decimator protects extrema in
// BOTH directions, so this shape survives whichever way the polarity is set.
// Worth pinning anyway — it is what a reader most often comes looking for —
// but it is deliberately NOT the test that guards the polarity. See 2b.
func TestChart_Forecast_DecimationKeepsAnIsolatedSlip(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	steady := t0.Add(14 * 24 * time.Hour)
	slipped := t0.Add(90 * 24 * time.Hour)

	const n = 400
	const slipAt = 173

	points := make([]forecastPoint, 0, n)
	for i := 0; i < n; i++ {
		eta := steady
		if i == slipAt {
			eta = slipped
		}
		points = append(points, forecastPoint{
			CreatedAt: t0.Add(time.Duration(i) * time.Minute),
			ETA:       eta,
		})
	}

	kept := decimateForecastPoints(points, MaxChartPoints)
	if len(kept) > MaxChartPoints {
		t.Fatalf("decimation exceeded the budget: %d > %d", len(kept), MaxChartPoints)
	}
	for _, p := range kept {
		if p.ETA.Equal(slipped) {
			return
		}
	}
	t.Fatalf("decimation threw away the slipped forecast, the one point the chart exists to show; kept %d of %d points", len(kept), n)
}

// 2b. A GRADUAL slide: the date creeping later step after step, thirty
// re-forecasts each a little worse than the last. THIS is the shape that
// depends on the polarity.
//
// The rule "a step in the bad direction makes both its ends critical" turns a
// bad-direction ramp into a wall of critical points that all survive. A
// good-direction ramp is just a monotone run with extrema only at its ends,
// and its middle gets thinned by the even spread. So flipping worseIsHigher in
// decimateForecastPoints reddens this test and only this one.
func TestChart_Forecast_DecimationKeepsAGradualSlide(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	base := t0.Add(14 * 24 * time.Hour)

	const n = 400
	const slideFrom = 185
	const slideLen = 30

	points := make([]forecastPoint, 0, n)
	for i := 0; i < n; i++ {
		eta := base
		switch {
		case i >= slideFrom+slideLen:
			eta = base.Add(time.Duration(slideLen) * 24 * time.Hour)
		case i >= slideFrom:
			eta = base.Add(time.Duration(i-slideFrom) * 24 * time.Hour)
		}
		points = append(points, forecastPoint{
			CreatedAt: t0.Add(time.Duration(i) * time.Minute),
			ETA:       eta,
		})
	}

	kept := decimateForecastPoints(points, MaxChartPoints)
	if len(kept) > MaxChartPoints {
		t.Fatalf("decimation exceeded the budget: %d > %d", len(kept), MaxChartPoints)
	}

	survived := map[int]bool{}
	for _, p := range kept {
		d := int(p.ETA.Sub(base).Hours() / 24)
		if d > 0 && d < slideLen {
			survived[d] = true
		}
	}

	if len(survived) < slideLen-5 {
		t.Fatalf("the gradual slide was thinned out: only %d of %d slipping re-forecasts survived decimation. A date creeping later IS the bad news on this chart, so decimateForecastPoints must pass worseIsHigher=true and make every step of a slide critical.", len(survived), slideLen-1)
	}
}

// 3. The mirror of 2b on the PERCENT chart, where bad news points the other
// way: readiness revised DOWN, the fall from 91 the owner wants to see with
// their own eyes. Same shape, opposite polarity.
//
// Together these two pin the polarity argument from both sides: flip it in
// either caller and exactly one of them goes red, so the two charts cannot
// quietly come to share one direction.
func TestChart_Decimation_KeepsAGradualDeclineOnThePercentChart(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)

	const n = 400
	const slideFrom = 185
	const slideLen = 30

	points := make([]chartPoint, 0, n)
	for i := 0; i < n; i++ {
		pct := 91
		switch {
		case i >= slideFrom+slideLen:
			pct = 91 - slideLen
		case i >= slideFrom:
			pct = 91 - (i - slideFrom)
		}
		points = append(points, chartPoint{Time: t0.Add(time.Duration(i) * time.Minute), Percent: pct})
	}

	kept := decimatePoints(points, MaxChartPoints)

	survived := map[int]bool{}
	for _, p := range kept {
		if p.Percent < 91 && p.Percent > 91-slideLen {
			survived[p.Percent] = true
		}
	}
	if len(survived) < slideLen-5 {
		t.Fatalf("the gradual decline was thinned out: only %d of %d revised-down readings survived. The fall is the main event of the percent chart, so decimatePoints must pass worseIsHigher=false.", len(survived), slideLen-1)
	}
}

// 4. Exactly one forecast in an otherwise multi-mark history: a single point
// is a circle marker, never a degenerate one-point polyline, and never a
// divide by zero on a date span of length nothing.
func TestChart_Forecast_SingleForecastRendersCircle(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0},
		forecastMark("m2", "alpha", 60, t0.Add(time.Hour), t0.Add(10*24*time.Hour)),
		{ID: "m3", Assessor: "alpha", Percent: 90, CreatedAt: t0.Add(2 * time.Hour)},
	}

	svg := string(RenderForecastChart(marks, 600, 200))
	if svg == "" {
		t.Fatal("a history with one forecast rendered nothing")
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Fatalf("single forecast produced NaN/Inf: %s", svg)
	}
	if strings.Contains(svg, "<polyline") {
		t.Errorf("a single forecast drew a polyline; one point is a circle marker:\n%s", svg)
	}
	if !strings.Contains(svg, "<circle") {
		t.Errorf("a single forecast drew no circle marker:\n%s", svg)
	}
}

// 5. Every forecast names the exact same instant: the date span is zero, and
// the y projection must fall back to the midline instead of dividing by it.
func TestChart_Forecast_AllSameInstantDoesNotDivideByZero(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	same := t0.Add(48 * time.Hour)
	marks := []domain.ProgressMark{
		forecastMark("m1", "alpha", 20, t0, same),
		forecastMark("m2", "beta", 40, t0.Add(time.Hour), same),
		forecastMark("m3", "alpha", 60, t0.Add(2*time.Hour), same),
		forecastMark("m4", "beta", 80, t0.Add(3*time.Hour), same),
	}

	svg := string(RenderForecastChart(marks, 600, 200))
	if svg == "" {
		t.Fatal("an all-same-instant history rendered nothing")
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Fatalf("all-same-instant forecasts produced NaN/Inf: %s", svg)
	}

	coords := parsePolylinePoints(t, extractPolylinePoints(t, svg, `data-assessor="alpha"`))
	if len(coords) == 0 {
		t.Fatal("expected forecast points for an all-same-instant history")
	}
	midline := (ChartPadTop + (200.0 - ChartPadBottom)) / 2.0
	for _, c := range coords {
		if absFloat(c[1]-midline) > 0.2 {
			t.Fatalf("all-same-instant forecast not on the neutral midline: got %v, want y=%.1f", coords, midline)
		}
	}
}
