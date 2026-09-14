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

// dateAxisSteps is the maximum number of labelled date gridlines the
// enlarged forecast chart draws. Fewer than this is fine; more makes the
// labels collide. The exact picks are derived from the data range in
// writeDateAxis.
const forecastDateAxisMaxSteps = 5

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

// writeDateAxis draws the labelled date gridlines of the enlarged forecast
// chart. The picks (min / max / evenly-spaced between) come from
// pickForecastDateTicks, which honours the forecastDateAxisMaxSteps ceiling
// so labels never crowd each other. The Y axis on a forecast chart is a date
// axis (KANB-36): an absolute "promised finish" calendar, NOT a percent
// and NOT "days remaining from now". Plotting absolute dates is the only
// mapping that renders a genuinely held promise as flat — "days remaining"
// would shrink every day even when nothing changed.
func writeDateAxis(buf *bytes.Buffer, yMin, yMax float64, etaMin, etaMax time.Time, xMin, xMax float64) {
	for _, t := range pickForecastDateTicks(etaMin, etaMax) {
		y := yForForecastTime(t, etaMin, etaMax, yMin, yMax)
		opacity := "0.18"
		if t.Equal(etaMin) || t.Equal(etaMax) {
			opacity = "0.35"
		}
		fmt.Fprintf(buf, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-width="1" stroke-opacity="%s"/>`,
			xMin, y, xMax, y, opacity)
		fmt.Fprintf(buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="11" fill="currentColor" fill-opacity="0.6">%s</text>`,
			xMin-6.0, y+4.0, html.EscapeString(formatForecastAxisDate(t, etaMin, etaMax)))
	}
}

// yForForecastTime projects a date onto the forecast chart's vertical SVG
// range. Same shape as forecastBounds.y, but standalone so the enlarged
// forecast chart does not have to construct a full forecastBounds (the
// percent chart's bounds). Earlier ETA sits at the top (smaller Y in SVG
// terms); later ETA at the bottom — "promised sooner" reads above
// "promised later", the direction a percent drop already reads on its
// own chart.
func yForForecastTime(t, etaMin, etaMax time.Time, yMin, yMax float64) float64 {
	span := etaMax.Sub(etaMin)
	if span <= 0 {
		return (yMin + yMax) / 2.0
	}
	frac := float64(t.Sub(etaMin)) / float64(span)
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return yMin + frac*(yMax-yMin)
}

// pickForecastDateTicks returns the dates the forecast chart labels on its
// Y axis: at most forecastDateAxisMaxSteps, picked to include etaMin and
// etaMax and fill the gap with evenly-spaced points. Picking the labelled
// dates this way (rather than every day in range) is what keeps the axis
// legible for a 60-day span AND a 6-month span alike: the same chart
// rendering with different label counts, never a label collision.
func pickForecastDateTicks(etaMin, etaMax time.Time) []time.Time {
	if !etaMax.After(etaMin) {
		return []time.Time{etaMin}
	}
	out := []time.Time{etaMin}
	for i := 1; i < forecastDateAxisMaxSteps-1; i++ {
		frac := float64(i) / float64(forecastDateAxisMaxSteps-1)
		out = append(out, forecastTickAt(etaMin, etaMax, frac))
	}
	out = append(out, etaMax)
	return out
}

// forecastTickAt returns the time `frac` of the way from start to end.
func forecastTickAt(start, end time.Time, frac float64) time.Time {
	return start.Add(time.Duration(float64(end.Sub(start)) * frac))
}

// formatForecastAxisDate chooses the label format that stays readable for
// the charted span: short "Mon DD" for spans of weeks, "YYYY-MM-DD" for
// spans of months. The rule is "one label width per step", not "always
// full dates", so a 14-day span does not produce 14 wide labels.
func formatForecastAxisDate(t, etaMin, etaMax time.Time) string {
	if etaMax.Sub(etaMin) <= 31*24*time.Hour {
		return t.Format("Jan 02")
	}
	return t.Format("2006-01-02")
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
//
// KANB-36: the forecast is no longer a track on this chart — it has its
// own chart, its own modal, and its own legend (NewForecastChartDetail).
// The progress chart is now strictly the assessors' percent marks.
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

	legend := make([]ChartLegendEntry, 0, len(names)+1)
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

// NewForecastChartDetail builds the enlarged forecast chart: one line per
// assessor showing the dates they promised over time, plus a "consensus"
// line at each unique time point that averages the assessors' promises. The
// vertical axis is a real date axis, not a percent (KANB-36). Tooltips on
// each point name the promised date AND the remaining time AT THE MOMENT of
// the forecast — both numbers, never just one, because "remaining time"
// alone shrinks every day even when nothing changed.
//
// Returns nil for a history with no forecast marks: the same "no data, no
// placeholder" rule the percent chart and the items chart already follow.
func NewForecastChartDetail(history []domain.ProgressMark) *ChartDetailView {
	// Filter to marks that actually carry a forecast (KANB-36: history is
	// stored in full on the mark, only some marks carry an ETA). Plotting
	// a chart that silently drops every mark without an ETA would be a
	// different lie from "render nothing".
	forecastMarks := make([]domain.ProgressMark, 0, len(history))
	for _, m := range history {
		if m.ETA != nil {
			forecastMarks = append(forecastMarks, m)
		}
	}
	if len(forecastMarks) == 0 {
		return nil
	}

	svg := RenderForecastChartDetailed(forecastMarks, DetailChartWidth, DetailChartHeight)
	if svg == "" {
		return nil
	}

	sorted := make([]domain.ProgressMark, len(forecastMarks))
	copy(sorted, forecastMarks)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})

	byAssessor := map[string][]domain.ProgressMark{}
	var names []string
	for _, m := range sorted {
		if _, seen := byAssessor[m.Assessor]; !seen {
			names = append(names, m.Assessor)
		}
		byAssessor[m.Assessor] = append(byAssessor[m.Assessor], m)
	}
	sort.Strings(names)

	etaMin, etaMax := forecastEtaRange(sorted)
	spanStart := sorted[0].CreatedAt
	spanEnd := sorted[len(sorted)-1].CreatedAt

	legend := make([]ChartLegendEntry, 0, len(names)+1)
	for i, name := range names {
		track := byAssessor[name]
		legend = append(legend, ChartLegendEntry{
			Label:  name,
			Dash:   assessorDash(i),
			Width:  1.2,
			Note:   forecastRangeNote(track),
			Points: len(track),
		})
	}
	// Consensus is computed in chart_forecast.go and rendered last; legend
	// follows the chart's own order so a reader pairs swatch with line in
	// the same direction.
	legend = append(legend, ChartLegendEntry{
		Label:  "consensus",
		Dash:   "",
		Width:  2.5,
		Note:   forecastConsensusNote(sorted),
		Points: countConsensusPoints(sorted),
	})

	return &ChartDetailView{
		Title:    "Promised finish date",
		SVG:      svg,
		Legend:   legend,
		Axis:     "promised finish date, " + etaMin.Format("2006-01-02") + " to " + etaMax.Format("2006-01-02"),
		Span:     spanNote(spanStart, spanEnd),
		Subtitle: forecastConsensusNote(sorted),
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

// forecastRangeNote states where one assessor's promises moved over the
// charted period: "earliest -> latest" of the ETAs they actually gave.
// A single forecast shows just the date; same shape as percentRangeNote so
// the legend reads consistently across the two charts.
func forecastRangeNote(marks []domain.ProgressMark) string {
	if len(marks) == 0 {
		return ""
	}
	var first, last time.Time
	haveETA := false
	for _, m := range marks {
		if m.ETA == nil {
			continue
		}
		if !haveETA {
			first, last = *m.ETA, *m.ETA
			haveETA = true
			continue
		}
		if m.ETA.Before(first) {
			first = *m.ETA
		}
		if m.ETA.After(last) {
			last = *m.ETA
		}
	}
	if !haveETA {
		return ""
	}
	if first.Equal(last) {
		return first.Format("2006-01-02")
	}
	return first.Format("2006-01-02") + " -> " + last.Format("2006-01-02")
}

// forecastEtaRange finds the min/max ETA across the forecast marks. Both
// bounds are inclusive: a single forecast returns that exact instant on
// both ends, which the chart renders as a single point on the midline.
func forecastEtaRange(marks []domain.ProgressMark) (time.Time, time.Time) {
	if len(marks) == 0 {
		return time.Time{}, time.Time{}
	}
	var etaMin, etaMax time.Time
	haveETA := false
	for _, m := range marks {
		if m.ETA == nil {
			continue
		}
		if !haveETA {
			etaMin, etaMax = *m.ETA, *m.ETA
			haveETA = true
			continue
		}
		if m.ETA.Before(etaMin) {
			etaMin = *m.ETA
		}
		if m.ETA.After(etaMax) {
			etaMax = *m.ETA
		}
	}
	return etaMin, etaMax
}

// forecastConsensusNote is the legend note for the consensus line — "the
// average promised date, computed at every time point where at least one
// assessor forecast". For a one-point consensus it is just the date; for
// multiple it is the first -> last average.
func forecastConsensusNote(marks []domain.ProgressMark) string {
	consensus := buildConsensusSeries(marks)
	if len(consensus) == 0 {
		return "no forecasts"
	}
	if len(consensus) == 1 {
		return consensus[0].ETA.Format("2006-01-02")
	}
	first := consensus[0].ETA.Format("2006-01-02")
	last := consensus[len(consensus)-1].ETA.Format("2006-01-02")
	if first == last {
		return first + " (held)"
	}
	return first + " -> " + last
}

// countConsensusPoints mirrors chart_forecast.go's own buildConsensusSeries
// length so the legend and the chart agree on how many points the consensus
// line actually carries. They MUST agree: an entry that claims "5 pts" while
// the polyline has 3 is a worse lie than the chart alone.
func countConsensusPoints(marks []domain.ProgressMark) int {
	return len(buildConsensusSeries(marks))
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
