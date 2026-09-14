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
	points := []service.HistoryPoint{
		{At: itemsAt(1), TotalTasks: 4, OpenTasks: 4},
		{At: itemsAt(2), TotalTasks: 10, OpenTasks: 7},
		{At: itemsAt(3), TotalTasks: 10, OpenTasks: 0},
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

// TestItemsChart_AReopenIsVisibleAsARiseInOpen (KANB-34 acceptance 2): the
// whole point of reading the journal is that a card closed, reopened and
// closed again shows its dip and its recovery. done_at remembers only the
// last closing; if the drawn OPEN curve does not rise at the reopen instant,
// the chart is still drawing the old curve with new plumbing.
//
// The fixture: open 3 -> 2 (one finished) -> 3 (reopened) -> 2 (finished
// again). Every point changes the count, so the step polyline jumps exactly
// at the 2nd, 3rd and 4th instants; with one start point that puts the jump
// destinations at polyline indices 2, 4 and 6.
func TestItemsChart_AReopenIsVisibleAsARiseInOpen(t *testing.T) {
	points := []service.HistoryPoint{
		{At: itemsAt(1), TotalTasks: 3, OpenTasks: 3},
		{At: itemsAt(2), TotalTasks: 3, OpenTasks: 2},
		{At: itemsAt(3), TotalTasks: 3, OpenTasks: 3}, // the reopen: open climbs back
		{At: itemsAt(4), TotalTasks: 3, OpenTasks: 2},
	}
	svg := string(view.RenderItemsChart(points, view.DefaultChartWidth, view.DefaultChartHeight))
	open := polylinePoints(t, svg, "items-open")
	if len(open) != 7 {
		t.Fatalf("open polyline has %d points, want 7 (start + hold/jump per change)", len(open))
	}
	if open[4][1] >= open[2][1] {
		t.Errorf("the reopen did not draw as a rise: open y %.1f at the reopen, %.1f before it", open[4][1], open[2][1])
	}
	if open[6][1] <= open[4][1] {
		t.Errorf("the second closing did not draw as a fall: open y %.1f after, %.1f at the reopen", open[6][1], open[4][1])
	}
}

// TestItemsChart_CountsAreDrawnAsSteps: a count never passes through a
// fractional value, so the line must hold its level and then jump. A step
// emits two points per change (the hold, then the jump), which is what
// distinguishes it from a straight interpolation.
func TestItemsChart_CountsAreDrawnAsSteps(t *testing.T) {
	points := []service.HistoryPoint{
		{At: itemsAt(1), TotalTasks: 1, OpenTasks: 1},
		{At: itemsAt(5), TotalTasks: 9, OpenTasks: 9},
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

// TestItemsChart_ArchivedTickIsDrawnForBothCases (KANB-34 acceptance 5): the
// axis tick must appear whether the archived card was open or finished, and
// the geometry must tell the two apart — the open card's departure drops BOTH
// curves, the finished card's drops ONLY total, leaving open exactly where it
// was. Rendering the second case as an open-drop would be exactly the
// "archival passed off as a completion" lie the tick exists to prevent.
func TestItemsChart_ArchivedTickIsDrawnForBothCases(t *testing.T) {
	cases := []struct {
		name   string
		points []service.HistoryPoint
		// openFell says whether the OPEN curve must fall at the archival.
		openFell bool
	}{
		{
			name: "an open card leaves: total and open both fall",
			points: []service.HistoryPoint{
				{At: itemsAt(1), TotalTasks: 4, OpenTasks: 4},
				{At: itemsAt(2), TotalTasks: 3, OpenTasks: 3, Archived: 1},
			},
			openFell: true,
		},
		{
			name: "a finished card leaves: total falls, open holds",
			points: []service.HistoryPoint{
				{At: itemsAt(1), TotalTasks: 4, OpenTasks: 1},
				{At: itemsAt(2), TotalTasks: 3, OpenTasks: 1, Archived: 1},
			},
			openFell: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svg := string(view.RenderItemsChart(tc.points, view.DefaultChartWidth, view.DefaultChartHeight))
			if !strings.Contains(svg, `data-axis-mark="-1"`) {
				t.Errorf("the archival instant carries no axis tick:\n%s", svg)
			}
			total := polylinePoints(t, svg, "items-total")
			open := polylinePoints(t, svg, "items-open")
			totalFell := total[len(total)-1][1] > total[0][1]
			openFell := open[len(open)-1][1] > open[0][1]
			if !totalFell {
				t.Error("the total curve did not fall at the archival")
			}
			if openFell != tc.openFell {
				t.Errorf("open fell = %v, want %v", openFell, tc.openFell)
			}
		})
	}
}

// TestItemsChart_AnArchivalDropSurvivesDecimation pins the force-keep inside
// decimateCountPoints, on the branch that actually discards interior points.
//
// The fixture is 120 strictly-falling points, all open, so every step is a
// drop — the "worse direction" rule marks all 120 critical, the shared
// decimator overflows its budget and switches to envelope buckets, which keep
// only bucket extremes. Point 60 (the archival) sits mid-bucket on a slope:
// no extremum, no bucket edge, so it survives ONLY through the explicit
// "never drop an archival point" rule. Remove that rule and the tick — drawn
// from the SURVIVING points, so it can never point where nothing fell —
// disappears with it, and the archival is drawn in silence.
func TestItemsChart_AnArchivalDropSurvivesDecimation(t *testing.T) {
	base := time.Date(2026, 9, 12, 0, 0, 0, 0, time.Local)
	points := make([]service.HistoryPoint, 120)
	for i := range points {
		p := service.HistoryPoint{At: base.Add(time.Duration(i) * time.Minute), TotalTasks: 120 - i, OpenTasks: 120 - i}
		if i == 60 {
			p.Archived = 1
		}
		points[i] = p
	}
	svg := string(view.RenderItemsChart(points, view.DefaultChartWidth, view.DefaultChartHeight))
	if !strings.Contains(svg, `data-axis-mark="-1"`) {
		t.Fatalf("the archival tick did not survive decimation of a %d-point curve:\n%s", len(points), svg)
	}

	// The tick must stand on a DRAWN point, not float between them: the
	// archival instant's x must appear among the polyline vertices. The span
	// is 119 minutes mapped onto the plot width, and the archival is 60/119
	// of the way along it.
	wantX := view.ChartPadLeft + (60.0/119.0)*(float64(view.DefaultChartWidth)-view.ChartPadLeft-view.ChartPadRight)
	total := polylinePoints(t, svg, "items-total")
	for _, p := range total {
		d := p[0] - wantX
		if d < 0 {
			d = -d
		}
		if d < 0.6 {
			return // found it
		}
	}
	t.Errorf("no total vertex at the archival instant (x %.1f); the tick would point at a level the line never shows", wantX)
}

// TestItemsChart_TooltipCarriesTheTimeAndBothValues (KANB-34 acceptance:
// the tooltip names the time and the values AT the point). Both numbers, not
// one: a tooltip naming only the total makes the reader subtract to learn
// what is left, which is how "open" and "done" quietly get mixed up.
func TestItemsChart_TooltipCarriesTheTimeAndBothValues(t *testing.T) {
	points := []service.HistoryPoint{
		{At: itemsAt(10), TotalTasks: 4, OpenTasks: 4},
		{At: itemsAt(12), TotalTasks: 9, OpenTasks: 3},
	}
	svg := string(view.RenderItemsChart(points, view.DefaultChartWidth, view.DefaultChartHeight))
	if !strings.Contains(svg, `<title>12:00, total 9, open 3</title>`) {
		t.Errorf("the last point's tooltip does not name the time and both values:\n%s", svg)
	}
	if n := strings.Count(svg, "data-items-point"); n != 2 {
		t.Errorf("%d tooltip points drawn, want one per drawn point (2)", n)
	}
}

// TestItemsChart_SinglePointDrawsNoLine: one instant of history is not a
// trend. Two dots, no polyline — the same rule the progress chart follows.
func TestItemsChart_SinglePointDrawsNoLine(t *testing.T) {
	svg := string(view.RenderItemsChart([]service.HistoryPoint{
		{At: itemsAt(3), TotalTasks: 6, OpenTasks: 2},
	}, view.DefaultChartWidth, view.DefaultChartHeight))
	if strings.Contains(svg, "<polyline") {
		t.Errorf("a single point was drawn as a line:\n%s", svg)
	}
	if !strings.Contains(svg, `data-series="items-total"`) || !strings.Contains(svg, `data-series="items-open"`) {
		t.Errorf("both counts must still be marked:\n%s", svg)
	}
	if !strings.Contains(svg, `<title>03:00, total 6, open 2</title>`) {
		t.Errorf("the single point lost its tooltip:\n%s", svg)
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
// all archived can present a history whose maximum is zero. The archived
// instants must still be ticked — the emptiness of the board today is the
// story of everything that left.
func TestItemsChart_AllZeroCountsDoNotDivideByZero(t *testing.T) {
	svg := string(view.RenderItemsChart([]service.HistoryPoint{
		{At: itemsAt(1), TotalTasks: 0, OpenTasks: 0},
		{At: itemsAt(2), TotalTasks: 0, OpenTasks: 0, Archived: 1},
	}, view.DefaultChartWidth, view.DefaultChartHeight))
	if svg == "" {
		t.Fatal("no SVG rendered")
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Errorf("a zero maximum produced non-finite coordinates:\n%s", svg)
	}
	if !strings.Contains(svg, `data-axis-mark="-1"`) {
		t.Errorf("the archival tick is missing from an all-zero history:\n%s", svg)
	}
}
