package view

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"sort"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// The enlarged charts.
//
// Both panels in the left column open into a modal at a size where a reader
// can actually measure something. "Detailed" is deliberately NOT a different
// picture: the same marks, the same projections, the same decimation. What it
// adds is the furniture the small inline panel has no room for — a labelled
// gridline every 25 percent instead of one midline, a midpoint time tick, a
// legend naming every line, and a plain-language range under each chart.
//
// The legend lives in HTML rather than inside the SVG. Two reasons: an SVG
// legend has to lay out its own text (no wrapping, no font metrics, and it
// would have to steal plot area to fit), and the dash swatches are the only
// way to name a line in a design with no colour — so they need to sit in the
// document flow where they can wrap like any other list.
// ---------------------------------------------------------------------------

// Enlarged chart dimensions. Wider than tall, because the axis that carries
// the most information here is time.
const (
	DetailChartWidth  = 900
	DetailChartHeight = 380
)

// percentAxisSteps are the gridlines the enlarged percent chart draws. Five
// lines (0, 25, 50, 75, 100) is the most that stays legible at this height
// without the labels starting to crowd each other.
var percentAxisSteps = []int{0, 25, 50, 75, 100}

// assessorDash is the ONE rule mapping an assessor's position in the sorted
// name list onto a dash pattern. Both the chart and its legend call it, so a
// line and its legend swatch cannot drift apart.
func assessorDash(i int) string {
	return chartDashPatterns[i%len(chartDashPatterns)]
}

// writePercentAxis draws the labelled 0..100 gridlines of the enlarged chart.
func writePercentAxis(buf *bytes.Buffer, b chartBounds) {
	for _, p := range percentAxisSteps {
		y := b.y(p)
		opacity := "0.18"
		if p == 0 || p == 100 {
			opacity = "0.35"
		}
		fmt.Fprintf(buf, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-width="1" stroke-opacity="%s"/>`,
			b.xMin, y, b.xMax, y, opacity)
		fmt.Fprintf(buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="11" fill="currentColor" fill-opacity="0.6">%d%%</text>`,
			b.xMin-6.0, y+4.0, p)
	}
}

// writeTimeMidTick adds the halfway time label to the enlarged chart, so the
// horizontal scale can be read rather than merely bounded by its two ends.
func writeTimeMidTick(buf *bytes.Buffer, b chartBounds, tStart, tEnd time.Time) {
	mid := tStart.Add(tEnd.Sub(tStart) / 2)
	x := b.x(mid)
	fmt.Fprintf(buf, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-width="1" stroke-opacity="0.18"/>`,
		x, b.yMin, x, b.yMax)
	fmt.Fprintf(buf, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`,
		x, b.height-ChartPadBottom+16.0, html.EscapeString(formatChartTime(mid, sameCalendarDay(tStart, tEnd))))
}

// ChartLegendEntry is one line of a chart's legend: a dash swatch, the name
// of the line, and what it did over the charted period.
type ChartLegendEntry struct {
	// Label is what the line is: an assessor's name, "composite", "total".
	Label string
	// Dash is the SVG stroke-dasharray for the swatch, empty for solid.
	Dash string
	// Width is the swatch stroke width, matching the line's own.
	Width float64
	// Note is the range in words, e.g. "91% -> 72%" or "4 -> 151".
	Note string
	// Points is how many data points the line was drawn from, so a reader
	// can tell one confident-looking line from another drawn from two marks.
	Points int
}

// ChartDetailView is one enlarged chart plus everything needed to read it.
type ChartDetailView struct {
	Title    string
	SVG      template.HTML
	Legend   []ChartLegendEntry
	Axis     string // what the vertical axis measures, in words
	Span     string // the time range covered, in words
	Subtitle string // the headline reading, e.g. "72% now, from 91%"
}

// NewProgressChartDetail builds the enlarged assessment chart: the same
// picture as the inline panel, with an axis, a midpoint time tick, a legend
// naming every assessor, and the range each line covered.
//
// Returns nil for an empty history, exactly like the inline view: a modal
// containing an empty frame would be worse than no modal.
func NewProgressChartDetail(history []domain.ProgressMark) *ChartDetailView {
	svg := renderProgressChart(history, DetailChartWidth, DetailChartHeight, true)
	if svg == "" {
		return nil
	}

	marks := make([]domain.ProgressMark, len(history))
	copy(marks, history)
	sort.SliceStable(marks, func(i, j int) bool {
		if marks[i].CreatedAt.Equal(marks[j].CreatedAt) {
			return marks[i].ID < marks[j].ID
		}
		return marks[i].CreatedAt.Before(marks[j].CreatedAt)
	})

	byAssessor := map[string][]domain.ProgressMark{}
	var names []string
	for _, m := range marks {
		if _, seen := byAssessor[m.Assessor]; !seen {
			names = append(names, m.Assessor)
		}
		byAssessor[m.Assessor] = append(byAssessor[m.Assessor], m)
	}
	// The same sort the chart uses to assign dashes, so swatch and line agree.
	sort.Strings(names)

	legend := make([]ChartLegendEntry, 0, len(names)+2)
	for i, name := range names {
		track := byAssessor[name]
		legend = append(legend, ChartLegendEntry{
			Label:  name,
			Dash:   assessorDash(i),
			Width:  1.2,
			Note:   percentRangeNote(track),
			Points: len(track),
		})
	}
	// The composite is drawn on top and is the number the bar itself shows,
	// so it is named last — the reader's eye ends on the summary line.
	if hasForecast(marks) {
		legend = append(legend, ChartLegendEntry{
			Label:  "forecast",
			Dash:   forecastDashArray,
			Width:  1.6,
			Note:   "promised finish date, highest = earliest",
			Points: countForecasts(marks),
		})
	}
	legend = append(legend, ChartLegendEntry{
		Label:  "composite",
		Dash:   "",
		Width:  2.5,
		Note:   percentRangeNote(marks),
		Points: len(marks),
	})

	return &ChartDetailView{
		Title:    "Assessed progress",
		SVG:      svg,
		Legend:   legend,
		Axis:     "percent assessed, 0 to 100",
		Span:     spanNote(marks[0].CreatedAt, marks[len(marks)-1].CreatedAt),
		Subtitle: percentRangeNote(marks),
	}
}

// NewItemsChartDetail builds the enlarged item-count chart.
func NewItemsChartDetail(points []service.ItemCountPoint) *ChartDetailView {
	svg := RenderItemsChart(points, DetailChartWidth, DetailChartHeight)
	if svg == "" {
		return nil
	}
	first, last := points[0], points[len(points)-1]

	maxTotal := 0
	for _, p := range points {
		if p.Total > maxTotal {
			maxTotal = p.Total
		}
	}
	peakOpen := 0
	for _, p := range points {
		if p.Open > peakOpen {
			peakOpen = p.Open
		}
	}

	return &ChartDetailView{
		Title: "Items on the board",
		SVG:   svg,
		Legend: []ChartLegendEntry{
			{
				Label:  "total",
				Width:  2.0,
				Note:   fmt.Sprintf("%d -> %d — every task in the project, including work added along the way", first.Total, last.Total),
				Points: len(points),
			},
			{
				Label:  "open",
				Dash:   itemsOpenDashArray,
				Width:  1.4,
				Note:   fmt.Sprintf("%d -> %d — not yet in a done column; peaked at %d", first.Open, last.Open, peakOpen),
				Points: len(points),
			},
		},
		Axis:     fmt.Sprintf("number of tasks, 0 to %d", maxTotal),
		Span:     spanNote(first.At, last.At),
		Subtitle: fmt.Sprintf("%d of %d done, %d still open", last.Total-last.Open, last.Total, last.Open),
	}
}

// percentRangeNote states where a track started and where it ended, which is
// the one sentence a reader wants before studying the shape.
func percentRangeNote(marks []domain.ProgressMark) string {
	if len(marks) == 0 {
		return ""
	}
	first := clampPercent(marks[0].Percent)
	last := clampPercent(marks[len(marks)-1].Percent)
	if len(marks) == 1 || first == last {
		return fmt.Sprintf("%d%%", last)
	}
	return fmt.Sprintf("%d%% -> %d%%", first, last)
}

func hasForecast(marks []domain.ProgressMark) bool {
	return countForecasts(marks) > 0
}

func countForecasts(marks []domain.ProgressMark) int {
	n := 0
	for _, m := range marks {
		if m.ETA != nil {
			n++
		}
	}
	return n
}

// spanNote renders the charted period the way the axis labels do, so the
// caption and the axis cannot disagree about what "today" means.
func spanNote(from, to time.Time) string {
	sameDay := sameCalendarDay(from, to)
	if from.Equal(to) {
		return formatChartTime(from, sameDay)
	}
	return formatChartTime(from, sameDay) + " – " + formatChartTime(to, sameDay)
}

// ChartDetailFragment is the modal's whole body: one or both enlarged charts.
type ChartDetailFragment struct {
	Progress *ChartDetailView
	Items    *ChartDetailView
}

// Empty reports whether there is nothing to show.
func (f *ChartDetailFragment) Empty() bool {
	return f == nil || (f.Progress == nil && f.Items == nil)
}
