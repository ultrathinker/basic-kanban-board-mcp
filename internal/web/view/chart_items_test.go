package view_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

func itemsAt(h int) time.Time {
	return time.Date(2026, 9, 12, h, 0, 0, 0, time.Local)
}

// polylinePoints pulls the coordinate list out of the polyline carrying one
// data-series value, so a test can assert geometry rather than a substring.
func polylinePoints(t *testing.T, svg, series string) [][2]float64 {
	t.Helper()
	re := regexp.MustCompile(`<polyline[^>]*points="([^"]*)"[^>]*data-series="` + regexp.QuoteMeta(series) + `"`)
	m := re.FindStringSubmatch(svg)
	if m == nil {
		t.Fatalf("no polyline with data-series=%q in:\n%s", series, svg)
	}
	var out [][2]float64
	for _, pair := range strings.Fields(m[1]) {
		xy := strings.Split(pair, ",")
		if len(xy) != 2 {
			t.Fatalf("malformed point %q", pair)
		}
		x, err1 := strconv.ParseFloat(xy[0], 64)
		y, err2 := strconv.ParseFloat(xy[1], 64)
		if err1 != nil || err2 != nil {
			t.Fatalf("unparseable point %q", pair)
		}
		out = append(out, [2]float64{x, y})
	}
	return out
}

// TestItemsChart_ZeroIsOnTheBaselineAndMaxIsAtTheTop pins the scale, which is
// the whole readability of this panel: the open curve touching the baseline
// has to MEAN zero, and the axis top has to mean the real maximum rather than
// some padded round number.
func TestItemsChart_ZeroIsOnTheBaselineAndMaxIsAtTheTop(t *testing.T) {
	points := []service.ItemCountPoint{
		{At: itemsAt(1), Total: 4, Open: 4},
		{At: itemsAt(2), Total: 10, Open: 7},
		{At: itemsAt(3), Total: 10, Open: 0},
	}
	svg := string(view.RenderItemsChart(points, view.DefaultChartWidth, view.DefaultChartHeight))
	if svg == "" {
		t.Fatal("no SVG rendered")
	}

	// The axis is labelled with the real maximum, not a rounded one.
	if !strings.Contains(svg, ">10</text>") {
		t.Errorf("the axis top is not labelled with the real maximum (10):\n%s", svg)
	}
	if !strings.Contains(svg, ">0</text>") {
		t.Error("the zero baseline is not labelled")
	}

	total := polylinePoints(t, svg, "items-total")
	open := polylinePoints(t, svg, "items-open")

	// The final open value is 0 and the final total is the maximum, so the
	// two curves must end on the two extremes of the axis — the open one on
	// the baseline, the total one at the top. Larger y is lower on screen.
	openEndY := open[len(open)-1][1]
	totalEndY := total[len(total)-1][1]
	if openEndY <= totalEndY {
		t.Errorf("open (%.1f) did not end below total (%.1f) despite ending at 0 of 10", openEndY, totalEndY)
	}
	// Every plotted y must sit inside the drawing box.
	for _, p := range append(append([][2]float64{}, total...), open...) {
		if p[1] < view.ChartPadTop-0.01 || p[1] > float64(view.DefaultChartHeight)-view.ChartPadBottom+0.01 {
			t.Errorf("point %v escaped the plot area", p)
		}
	}

	// A rise in the total must be visible as a rise: the total at hour 1 (4)
	// must sit lower on screen than at hour 2 (10).
	if total[0][1] <= total[len(total)-1][1] {
		t.Errorf("the total curve did not rise: first y %.1f, last y %.1f", total[0][1], total[len(total)-1][1])
	}
}

// TestItemsChart_CountsAreDrawnAsSteps: a count never passes through a
// fractional value, so the line must hold its level and then jump. A step
// emits two points per change (the hold, then the jump), which is what
// distinguishes it from a straight interpolation.
func TestItemsChart_CountsAreDrawnAsSteps(t *testing.T) {
	points := []service.ItemCountPoint{
		{At: itemsAt(1), Total: 1, Open: 1},
		{At: itemsAt(5), Total: 9, Open: 9},
	}
	svg := string(view.RenderItemsChart(points, view.DefaultChartWidth, view.DefaultChartHeight))
	total := polylinePoints(t, svg, "items-total")

	if len(total) != 3 {
		t.Fatalf("total polyline has %d points, want 3 (start, hold at the old level, jump)", len(total))
	}
	// The hold and the jump share an x; the hold shares its y with the start.
	if total[1][0] != total[2][0] {
		t.Errorf("the step does not jump vertically: %v then %v", total[1], total[2])
	}
	if total[0][1] != total[1][1] {
		t.Errorf("the level was not held across to the change: %v then %v", total[0], total[1])
	}
	if total[2][1] >= total[1][1] {
		t.Errorf("the jump did not go up for a rising count: %v then %v", total[1], total[2])
	}
}

// TestItemsChart_SinglePointDrawsNoLine: one instant of history is not a
// trend. Two dots, no polyline — the same rule the progress chart follows.
func TestItemsChart_SinglePointDrawsNoLine(t *testing.T) {
	svg := string(view.RenderItemsChart([]service.ItemCountPoint{
		{At: itemsAt(3), Total: 6, Open: 2},
	}, view.DefaultChartWidth, view.DefaultChartHeight))
	if strings.Contains(svg, "<polyline") {
		t.Errorf("a single point was drawn as a line:\n%s", svg)
	}
	if !strings.Contains(svg, `data-series="items-total"`) || !strings.Contains(svg, `data-series="items-open"`) {
		t.Errorf("both counts must still be marked:\n%s", svg)
	}
}

func TestItemsChart_EmptyRendersNothing(t *testing.T) {
	if got := view.RenderItemsChart(nil, view.DefaultChartWidth, view.DefaultChartHeight); got != "" {
		t.Errorf("RenderItemsChart(nil) = %q, want empty", got)
	}
	if got := view.NewItemsChartView(nil, view.DefaultChartWidth, view.DefaultChartHeight); got != nil {
		t.Errorf("NewItemsChartView(nil) = %+v, want nil", got)
	}
}

// TestItemsChart_AllZeroCountsDoNotDivideByZero: a project whose tasks were
// all archived can present a history whose maximum is zero.
func TestItemsChart_AllZeroCountsDoNotDivideByZero(t *testing.T) {
	svg := string(view.RenderItemsChart([]service.ItemCountPoint{
		{At: itemsAt(1), Total: 0, Open: 0},
		{At: itemsAt(2), Total: 0, Open: 0},
	}, view.DefaultChartWidth, view.DefaultChartHeight))
	if svg == "" {
		t.Fatal("no SVG rendered")
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Errorf("a zero maximum produced non-finite coordinates:\n%s", svg)
	}
}
