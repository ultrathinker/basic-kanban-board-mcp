package view_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

func readinessAt(h int, percent int, note string) service.HistoryPoint {
	p := percent
	return service.HistoryPoint{
		At: time.Date(2026, 9, 12, h, 0, 0, 0, time.Local),
		Readiness: service.EstimateReadiness{
			Basis:          service.ReadinessEstimates,
			Percent:        &p,
			Coverage:       "8 of 12 estimated",
			HistoricalNote: note,
		},
	}
}

// TestReadinessChart_CarriesTheServiceSentenceVerbatim is the heart of
// KANB-53: the note beside the historical readiness curve must be
// service.HistoricalReadinessNote ITSELF, carried through the points — not a
// paraphrase kept in a second place that will drift away from the service's
// wording. The comparison is against the constant, so any rewording anywhere
// between the service and the view turns the test red.
func TestReadinessChart_CarriesTheServiceSentenceVerbatim(t *testing.T) {
	v := view.NewReadinessChartView([]service.HistoryPoint{
		readinessAt(10, 25, service.HistoricalReadinessNote),
		readinessAt(11, 40, service.HistoricalReadinessNote),
	}, view.DefaultChartWidth, view.DefaultChartHeight)
	if v == nil {
		t.Fatal("no view built")
	}
	if v.Note != service.HistoricalReadinessNote {
		t.Errorf("the note is not the service sentence verbatim:\n  got  %q\n  want %q", v.Note, service.HistoricalReadinessNote)
	}
	// The note must have a curve to stand beside: the panel is a chart with
	// a caption, not a warning label on its own.
	if !strings.Contains(string(v.SVG), `data-series="readiness"`) {
		t.Errorf("no readiness curve in the SVG:\n%s", v.SVG)
	}
	if !strings.Contains(v.Caption, "40%") || !strings.Contains(v.Caption, "8 of 12 estimated") {
		t.Errorf("caption %q does not name the current percent and its basis", v.Caption)
	}
}

// TestReadinessChart_NilPercentLeavesAGapNotAFakeLine: where nothing was
// estimated the percent does not exist, and a line drawn across that stretch
// would invent a value for history that has none. Two points per side keep
// both segments drawable as lines rather than lone dots.
func TestReadinessChart_NilPercentLeavesAGapNotAFakeLine(t *testing.T) {
	none := service.HistoryPoint{At: readinessAt(11, 25, "").At} // Percent nil
	v := view.NewReadinessChartView([]service.HistoryPoint{
		readinessAt(9, 25, service.HistoricalReadinessNote),
		readinessAt(10, 30, service.HistoricalReadinessNote),
		none,
		readinessAt(12, 50, service.HistoricalReadinessNote),
		readinessAt(13, 60, service.HistoricalReadinessNote),
	}, view.DefaultChartWidth, view.DefaultChartHeight)
	if v == nil {
		t.Fatal("no view built despite drawable points")
	}
	svg := string(v.SVG)
	if n := strings.Count(svg, `<polyline`); n != 2 {
		t.Fatalf("%d polylines drawn across a nil-percent gap, want 2 segments (one per side):\n%s", n, svg)
	}
}

// TestReadinessChart_NothingEverEstimatedRendersNothing: the same "no data,
// no placeholder" rule every other panel follows — and no note without a
// curve, which would be a warning label where a chart was promised.
func TestReadinessChart_NothingEverEstimatedRendersNothing(t *testing.T) {
	if v := view.NewReadinessChartView(nil, view.DefaultChartWidth, view.DefaultChartHeight); v != nil {
		t.Errorf("NewReadinessChartView(nil) = %+v, want nil", v)
	}
	none := service.HistoryPoint{At: readinessAt(10, 0, "").At}
	if v := view.NewReadinessChartView([]service.HistoryPoint{none}, view.DefaultChartWidth, view.DefaultChartHeight); v != nil {
		t.Errorf("a replay with no readiness percent built %+v, want nil", v)
	}
}
