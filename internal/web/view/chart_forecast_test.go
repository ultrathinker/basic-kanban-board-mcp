package view

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// etaPtr is the local helper for optional forecast dates in fixtures.
func etaPtr(t time.Time) *time.Time { return &t }

// 1. A promise that slides later and later at every re-forecast must trend
// toward the bottom of the chart (the same direction a percent drop already
// moves), so the reader can tell "sliding" from "holding" by shape alone.
func TestChart_ForecastTrack_SlidesTowardBottom(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "bot", Percent: 10, CreatedAt: t0, ETA: etaPtr(t0.Add(5 * 24 * time.Hour))},
		{ID: "m2", Assessor: "bot", Percent: 30, CreatedAt: t0.Add(time.Hour), ETA: etaPtr(t0.Add(6 * 24 * time.Hour))},
		{ID: "m3", Assessor: "bot", Percent: 50, CreatedAt: t0.Add(2 * time.Hour), ETA: etaPtr(t0.Add(7 * 24 * time.Hour))},
		{ID: "m4", Assessor: "bot", Percent: 70, CreatedAt: t0.Add(3 * time.Hour), ETA: etaPtr(t0.Add(8 * 24 * time.Hour))},
	}

	svg := string(RenderProgressChart(marks, 600, 200))
	if !strings.Contains(svg, `data-series="forecast"`) {
		t.Fatalf("missing forecast track: %s", svg)
	}
	ptsStr := extractPolylinePoints(t, svg, `data-series="forecast"`)
	coords := parsePolylinePoints(t, ptsStr)
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
	// The earliest forecast (best case) must sit at the very top of the plot
	// area (100 -> yMin), the latest (worst case) at the very bottom (0 -> yMax).
	if got, want := coords[0][1], 16.0; absFloat(got-want) > 0.2 {
		t.Fatalf("earliest forecast not at top: got y=%.1f, want %.1f", got, want)
	}
	if got, want := coords[3][1], 176.0; absFloat(got-want) > 0.2 {
		t.Fatalf("latest forecast not at bottom: got y=%.1f, want %.1f", got, want)
	}
}

// 2. A promise that never moves must draw a flat line — the visual contrast
// against "sliding" that makes the shape legible at a glance.
func TestChart_ForecastTrack_HoldingIsFlat(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	fixed := t0.Add(30 * 24 * time.Hour)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "bot", Percent: 10, CreatedAt: t0, ETA: etaPtr(fixed)},
		{ID: "m2", Assessor: "bot", Percent: 30, CreatedAt: t0.Add(time.Hour), ETA: etaPtr(fixed)},
		{ID: "m3", Assessor: "bot", Percent: 50, CreatedAt: t0.Add(2 * time.Hour), ETA: etaPtr(fixed)},
		{ID: "m4", Assessor: "bot", Percent: 70, CreatedAt: t0.Add(3 * time.Hour), ETA: etaPtr(fixed)},
	}

	svg := string(RenderProgressChart(marks, 600, 200))
	ptsStr := extractPolylinePoints(t, svg, `data-series="forecast"`)
	coords := parsePolylinePoints(t, ptsStr)
	if len(coords) != 4 {
		t.Fatalf("expected 4 forecast points, got %d: %v", len(coords), coords)
	}
	first := coords[0][1]
	for i, c := range coords {
		if absFloat(c[1]-first) > 0.01 {
			t.Fatalf("holding forecast is not flat at point %d: %v (first=%.2f)", i, coords, first)
		}
	}
	// x still advances with real time even though y is flat.
	if coords[3][0] <= coords[0][0] {
		t.Fatalf("forecast x coordinates did not advance with time: %v", coords)
	}
}

// 3. Nobody ever gave a forecast: no forecast track at all, no placeholder,
// no panic. Mirrors the badge's "render nothing" rule, extended to the
// chart.
func TestChart_ForecastTrack_AbsentWithoutAnyETA(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0},
		{ID: "m2", Assessor: "beta", Percent: 50, CreatedAt: t0.Add(time.Hour)},
		{ID: "m3", Assessor: "alpha", Percent: 70, CreatedAt: t0.Add(2 * time.Hour)},
	}
	svg := string(RenderProgressChart(marks, 600, 200))
	if strings.Contains(svg, `data-series="forecast"`) {
		t.Fatalf("forecast track rendered with no forecasts in history: %s", svg)
	}
}

// 4. Exactly one forecast in an otherwise multi-mark, multi-point history:
// must render as a single circle marker (like every other one-point track
// on this chart), never a degenerate polyline, and must not panic.
func TestChart_ForecastTrack_SingleForecastRendersCircle(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 30, CreatedAt: t0},
		{ID: "m2", Assessor: "alpha", Percent: 60, CreatedAt: t0.Add(time.Hour), ETA: etaPtr(t0.Add(10 * 24 * time.Hour))},
		{ID: "m3", Assessor: "alpha", Percent: 90, CreatedAt: t0.Add(2 * time.Hour)},
	}
	svg := string(RenderProgressChart(marks, 600, 200))
	if !strings.Contains(svg, `data-series="forecast"`) {
		t.Fatalf("missing forecast marker: %s", svg)
	}
	re := regexp.MustCompile(`<(polyline|circle)[^>]*data-series="forecast"[^>]*>`)
	m := re.FindStringSubmatch(svg)
	if len(m) < 2 {
		t.Fatalf("forecast element not found: %s", svg)
	}
	if m[1] != "circle" {
		t.Fatalf("single forecast rendered a %s, want a circle marker: %s", m[1], svg)
	}
}

// 5. Hundreds of forecasts: must not panic, must decimate to the same
// budget as every other track, and every coordinate must be finite.
func TestChart_ForecastTrack_DecimatesLargeHistory(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	var marks []domain.ProgressMark
	for i := 0; i < 500; i++ {
		marks = append(marks, domain.ProgressMark{
			ID:        "m" + strconv.Itoa(i),
			Assessor:  "heavy-bot",
			Percent:   10 + (i * 80 / 500),
			CreatedAt: t0.Add(time.Duration(i) * time.Minute),
			ETA:       etaPtr(t0.Add(time.Duration(30+i) * 24 * time.Hour)),
		})
	}
	svg := string(RenderProgressChart(marks, 600, 200))
	if svg == "" {
		t.Fatal("render returned empty SVG")
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Fatalf("forecast track produced NaN/Inf: %s", svg)
	}
	ptsStr := extractPolylinePoints(t, svg, `data-series="forecast"`)
	coords := parsePolylinePoints(t, ptsStr)
	if len(coords) > MaxChartPoints {
		t.Fatalf("forecast track not decimated: got %d points, want <= %d", len(coords), MaxChartPoints)
	}
	if len(coords) < 2 {
		t.Fatalf("forecast track over-decimated to %d points", len(coords))
	}
}

// 6. Every forecast lands on the exact same instant (perfect agreement, not
// just "holding steady over separate re-forecasts"): must not divide by
// zero, must not panic, and must still read as flat (midline).
func TestChart_ForecastTrack_AllSameInstantDoesNotPanic(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	same := t0.Add(48 * time.Hour)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 20, CreatedAt: t0, ETA: etaPtr(same)},
		{ID: "m2", Assessor: "beta", Percent: 40, CreatedAt: t0.Add(time.Hour), ETA: etaPtr(same)},
		{ID: "m3", Assessor: "alpha", Percent: 60, CreatedAt: t0.Add(2 * time.Hour), ETA: etaPtr(same)},
		{ID: "m4", Assessor: "beta", Percent: 80, CreatedAt: t0.Add(3 * time.Hour), ETA: etaPtr(same)},
	}
	svg := string(RenderProgressChart(marks, 600, 200))
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Fatalf("all-same-instant forecasts produced NaN/Inf: %s", svg)
	}
	ptsStr := extractPolylinePoints(t, svg, `data-series="forecast"`)
	coords := parsePolylinePoints(t, ptsStr)
	if len(coords) == 0 {
		t.Fatal("expected forecast points for all-same-instant history")
	}
	midline := 16.0 + (50.0/100.0)*160.0 // yMin + (100-50)/100*(plot height)
	for _, c := range coords {
		if absFloat(c[1]-midline) > 0.2 {
			t.Fatalf("all-same-instant forecast not at neutral midline: got %v, want y=%.1f", coords, midline)
		}
	}
}

// 7. The forecast track must never claim the composite's "on top" contract:
// composite is still the last element rendered (existing behaviour,
// TestChart_MultiAssessor_AndHighlightedComposite pins the assessor-vs-
// composite half; this pins forecast-vs-composite).
func TestChart_ForecastTrack_CompositeStillRendersOnTop(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	marks := []domain.ProgressMark{
		{ID: "m1", Assessor: "alpha", Percent: 20, CreatedAt: t0, ETA: etaPtr(t0.Add(24 * time.Hour))},
		{ID: "m2", Assessor: "alpha", Percent: 60, CreatedAt: t0.Add(time.Hour), ETA: etaPtr(t0.Add(48 * time.Hour))},
	}
	svg := string(RenderProgressChart(marks, 600, 200))
	posForecast := strings.Index(svg, `data-series="forecast"`)
	posComposite := strings.Index(svg, `data-series="composite"`)
	if posForecast == -1 || posComposite == -1 {
		t.Fatalf("expected both forecast and composite tracks: %s", svg)
	}
	if posComposite <= posForecast {
		t.Fatalf("composite must still be rendered after (on top of) the forecast track")
	}
}
