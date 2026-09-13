package view

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// The item-count chart: how much work the project holds, and how much of it
// is still open.
//
// This is a SECOND chart rather than two more tracks on the progress chart,
// and that is the whole design decision. The progress chart's vertical axis
// is a percent — every track on it, including the forecast's normalised
// placement, is read against 0..100. Item counts are not percents and have no
// ceiling: plotting "47 items" against a 0..100 axis would either lie (clamp
// at 100) or silently re-scale the axis under the percent tracks sharing it.
// Two panels, each honest about its own units, stacked so the time axis lines
// up by construction.
//
// What the two curves say, and why both are needed. The owner's complaint was
// that a burn-down against a fixed total hides the thing that actually
// happens on this board: work gets noticed and written down as it goes, so
// the denominator moves. TOTAL is therefore drawn as its own staircase — its
// rises are scope discovered — and OPEN is what is left to do. The gap
// between them is finished work, and the question "are we converging" is the
// question of whether OPEN is heading for the baseline, which is drawn.
//
// Counts change in steps, never gradually, so the lines are drawn as steps
// (hold the old value until the instant it changes). A straight segment
// between two counts would draw a project passing through 6.5 items.
// ---------------------------------------------------------------------------

// itemsOpenDashArray is the open curve's dash. Deliberately not one of
// chartDashPatterns: those identify assessors on the other panel, and reusing
// one here would invite reading this line as a person.
const itemsOpenDashArray = "5 3"

// ItemsChartView holds the rendered item-count SVG for template inclusion.
// A nil *ItemsChartView means there is nothing to draw.
type ItemsChartView struct {
	SVG template.HTML
	// Total and Open are the latest values, rendered as a caption so the
	// reader gets the two numbers without having to measure the lines.
	Total int
	Open  int
}

// NewItemsChartView builds the item-count chart. Returns nil when the project
// has no tasks at all — an empty panel would claim something was measured.
func NewItemsChartView(points []service.ItemCountPoint, width, height int) *ItemsChartView {
	svg := RenderItemsChart(points, width, height)
	if svg == "" {
		return nil
	}
	last := points[len(points)-1]
	return &ItemsChartView{SVG: svg, Total: last.Total, Open: last.Open}
}

// itemsBounds is the item chart's own coordinate mapping: time on x exactly
// like chartBounds, but y runs 0..maxTotal instead of 0..100.
type itemsBounds struct {
	width, height float64
	xMin, xMax    float64
	yMin, yMax    float64
	tStart        time.Time
	durationNan   float64
	maxCount      int
}

func newItemsBounds(w, h int, tStart, tEnd time.Time, maxCount int) itemsBounds {
	b := itemsBounds{
		width:       float64(w),
		height:      float64(h),
		xMin:        ChartPadLeft,
		xMax:        float64(w) - ChartPadRight,
		yMin:        ChartPadTop,
		yMax:        float64(h) - ChartPadBottom,
		tStart:      tStart,
		durationNan: float64(tEnd.Sub(tStart)),
		maxCount:    maxCount,
	}
	if b.maxCount < 1 {
		// A project whose every count is zero still gets a sane axis rather
		// than a division by zero.
		b.maxCount = 1
	}
	return b
}

func (b itemsBounds) x(t time.Time) float64 {
	if b.durationNan <= 0 {
		return (b.xMin + b.xMax) / 2.0
	}
	frac := float64(t.Sub(b.tStart)) / b.durationNan
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return b.xMin + frac*(b.xMax-b.xMin)
}

// y maps a count onto the vertical axis: 0 sits on the baseline, maxCount at
// the top. The axis always includes zero, because "open reached zero" is the
// one reading this chart exists to make possible.
func (b itemsBounds) y(count int) float64 {
	if count < 0 {
		count = 0
	}
	frac := float64(count) / float64(b.maxCount)
	if frac > 1 {
		frac = 1
	}
	return b.yMax - frac*(b.yMax-b.yMin)
}

// RenderItemsChart draws the total/open step curves. Empty input renders "".
func RenderItemsChart(points []service.ItemCountPoint, width, height int) template.HTML {
	if len(points) == 0 {
		return ""
	}
	if width <= 0 {
		width = DefaultChartWidth
	}
	if height <= 0 {
		height = DefaultChartHeight
	}
	if width < MinChartWidth {
		width = MinChartWidth
	}
	if height < MinChartHeight {
		height = MinChartHeight
	}

	maxCount := 0
	for _, p := range points {
		if p.Total > maxCount {
			maxCount = p.Total
		}
	}
	tStart := points[0].At
	tEnd := points[len(points)-1].At
	b := newItemsBounds(width, height, tStart, tEnd, maxCount)

	var buf bytes.Buffer
	w, h := int(b.width), int(b.height)
	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" class="chart-items" role="img" aria-label="Item count history: total items and items still open">`, w, h, w, h)

	// The zero baseline. Unlike the progress chart's 50% guideline this is
	// not a reference value, it is the target: the open curve touching it is
	// the definition of finished.
	yZero := b.y(0)
	fmt.Fprintf(&buf, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-width="1" stroke-opacity="0.35"/>`, b.xMin, yZero, b.xMax, yZero)
	fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.5">0</text>`, b.xMin-4.0, yZero+3.0)
	// The top of the axis, labelled with the real number so the scale is
	// readable without a gridline for every step.
	fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.5">%d</text>`, b.xMin-4.0, b.y(b.maxCount)+3.0, b.maxCount)

	// Time labels, same vocabulary and placement as the progress chart's.
	sameDay := sameCalendarDay(tStart, tEnd)
	if b.durationNan <= 0 {
		midX := (b.xMin + b.xMax) / 2.0
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`,
			midX, b.height-ChartPadBottom+16.0, html.EscapeString(formatChartTime(tStart, sameDay)))
	} else {
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="start" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`,
			b.xMin, b.height-ChartPadBottom+16.0, html.EscapeString(formatChartTime(tStart, sameDay)))
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`,
			b.xMax, b.height-ChartPadBottom+16.0, html.EscapeString(formatChartTime(tEnd, sameDay)))
	}

	last := points[len(points)-1]
	if len(points) == 1 {
		// One instant: two dots, no lines. A polyline needs two points and a
		// single-point "curve" would imply a trend that has not happened.
		fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="3" fill="currentColor" data-series="items-total"><title>Total items: %d</title></circle>`,
			b.x(last.At), b.y(last.Total), last.Total)
		fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="3" fill="currentColor" fill-opacity="0.55" data-series="items-open"><title>Open items: %d</title></circle>`,
			b.x(last.At), b.y(last.Open), last.Open)
		buf.WriteString(`</svg>`)
		return template.HTML(buf.String())
	}

	totalPath := itemsStepPath(b, points, func(p service.ItemCountPoint) int { return p.Total })
	openPath := itemsStepPath(b, points, func(p service.ItemCountPoint) int { return p.Open })

	fmt.Fprintf(&buf, `<polyline fill="none" stroke="currentColor" stroke-width="2.0" points="%s" data-series="items-total"><title>Total items (scope, including work added along the way)</title></polyline>`, totalPath)
	fmt.Fprintf(&buf, `<polyline fill="none" stroke="currentColor" stroke-width="1.4" stroke-dasharray="%s" points="%s" data-series="items-open"><title>Items still open (not in a done column)</title></polyline>`,
		itemsOpenDashArray, openPath)

	buf.WriteString(`</svg>`)
	return template.HTML(buf.String())
}

// itemsStepPath renders one count series as a step line: hold the previous
// value across to the next instant, then jump. pick chooses which of the two
// counts to plot.
func itemsStepPath(b itemsBounds, points []service.ItemCountPoint, pick func(service.ItemCountPoint) int) string {
	var sb strings.Builder
	prevY := b.y(pick(points[0]))
	for i, p := range points {
		x := b.x(p.At)
		y := b.y(pick(p))
		if i > 0 {
			// The horizontal hold at the OLD value, up to this instant.
			fmt.Fprintf(&sb, " %.1f,%.1f", x, prevY)
		}
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%.1f,%.1f", x, y)
		prevY = y
	}
	return sb.String()
}

// ProgressChartFragment is what "GET /p/{key}/progress/chart" renders: the
// assessment chart, and — for the project scope only — the item-count chart
// beneath it. Either may be nil; a fragment with both nil renders nothing,
// which app.js treats as "nothing to open".
//
// The two are separate fields rather than one merged picture because they
// answer different questions in different units: Progress is what the
// assessors SAY, Items is what the board CONTAINS. Keeping them apart is the
// same discipline that keeps "assessed" and "tasks done" as two bars instead
// of one blended number.
type ProgressChartFragment struct {
	Progress *ProgressChartView
	Items    *ItemsChartView
}

// Empty reports whether there is nothing at all to render.
func (f *ProgressChartFragment) Empty() bool {
	return f == nil || (f.Progress == nil && f.Items == nil)
}
