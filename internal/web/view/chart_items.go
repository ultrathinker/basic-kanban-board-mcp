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
// The two curves are REPLAYED FROM THE LIFECYCLE JOURNAL (service.Replay,
// KANB-34), not recomputed from tasks.created_at/done_at. The journal is the
// only source that remembers a reopened card's first completion, an archived
// card's departure, and the estimates as they stood at each instant; the old
// created_at-based curve saw only the last of those and is gone — one
// definition, one implementation.
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
// What the two curves say, and why both are needed. Work gets noticed and
// written down as it goes, so the denominator moves. TOTAL is drawn as its
// own staircase — its rises are scope discovered — and OPEN is what is left
// to do. The gap between them is finished work, and the question "are we
// converging" is the question of whether OPEN is heading for the baseline,
// which is drawn.
//
// Counts change in steps, never gradually, so the lines are drawn as steps
// (hold the old value until the instant it changes). A straight segment
// between two counts would draw a project passing through 6.5 items.
// ---------------------------------------------------------------------------

// itemsOpenDashArray is the open curve's dash. Deliberately not one of
// chartDashPatterns: those identify assessors on the other panel, and reusing
// one here would invite reading this line as a person.
const itemsOpenDashArray = "5 3"

// ChartAxisMark is one event flagged ON the time axis rather than drawn as a
// curve. The archival tick (KANB-34) is the first kind: archiving is the one
// thing that makes the total fall without work finishing, and a line falling
// in silence lets that drop be read as lost data — or worse, as a completion.
// The tick, with the full sentence in its tooltip, is what stops the chart
// from speaking for the data.
type ChartAxisMark struct {
	// At is the instant the tick stands on.
	At time.Time
	// Label is the short text beside the tick, e.g. "-1". Keep it short: the
	// tick sits at the baseline where the time labels live.
	Label string
	// Note is the full sentence carried in the tick's tooltip.
	Note string
	// Href, when set, turns the tick into a link to where the event is
	// written down. The archival ticks have nowhere to go — the fall they
	// explain is right there in the curve — but the scope_change ticks
	// (KANB-38) point at the message in the Thoughts feed that declared the
	// change, so the reader can read the author's own words instead of the
	// chart's one-line summary of them.
	Href string
}

// axisMarksFromPoints derives the archival ticks from the curve itself: any
// point that recorded cards LEAVING the project gets one. Deriving them here
// — rather than accepting them from the caller — means the tick and the fall
// it explains cannot be separated: whoever draws the fall draws the tick,
// from the same points.
func axisMarksFromPoints(points []service.HistoryPoint) []ChartAxisMark {
	var marks []ChartAxisMark
	for _, p := range points {
		if p.Archived <= 0 {
			continue
		}
		noun := "cards"
		if p.Archived == 1 {
			noun = "card"
		}
		marks = append(marks, ChartAxisMark{
			At:    p.At,
			Label: fmt.Sprintf("-%d", p.Archived),
			Note: fmt.Sprintf("%d %s archived at this instant — the total fell because work LEFT the project, not because it was finished",
				p.Archived, noun),
		})
	}
	return marks
}

// scopeChangeMarkLabel is the scope_change tick's label. Deliberately a word,
// not a number: unlike the archival tick's "-1" there is no count to name —
// the message declares a change of plan, and the sizes live in its body, in
// the author's own words.
const scopeChangeMarkLabel = "scope"

// ScopeChangeNote is one scope_change message a caller wants flagged on the
// items chart's time axis (KANB-38). The chart asks for nothing beyond what
// the message already says: the note explains the DECLARATION — it must never
// claim to explain the neighbouring point of the curve, because not every
// card creation is declared, and a declared change need not be what moved
// the total.
type ScopeChangeNote struct {
	// At is the instant the message was posted; the tick stands there.
	At time.Time
	// Author is the message's display signature.
	Author string
	// Body is the message's full text; the tooltip carries an excerpt cut by
	// the same rune rule as the feed's reply quotes.
	Body string
	// Href leads to the message itself — the Thoughts feed renders each
	// entry with a matching id anchor.
	Href string
}

// ScopeChangeMarks turns declared scope changes into axis ticks. The tick's
// tooltip names the author and an excerpt of their body — the reader gets
// the declaration's own words, and the link leads to the message for the
// rest. Nil input renders nil.
func ScopeChangeMarks(notes []ScopeChangeNote) []ChartAxisMark {
	if len(notes) == 0 {
		return nil
	}
	marks := make([]ChartAxisMark, 0, len(notes))
	for _, n := range notes {
		marks = append(marks, ChartAxisMark{
			At:    n.At,
			Label: scopeChangeMarkLabel,
			Note: fmt.Sprintf("scope change declared here by %s: %q — a declared change of plan, not a proof of what moved the curve at this instant",
				n.Author, chatQuoteExcerpt(n.Body)),
			Href: n.Href,
		})
	}
	return marks
}

// marksOnSpan keeps only the marks that stand on the drawn span. The x
// mapping clamps out-of-range instants onto the edges, and a tick pinned to
// an edge would date a declaration to an instant it did not happen at — a
// scope_change posted after the last replayed point is simply off this
// axis, not moved to its border.
func marksOnSpan(marks []ChartAxisMark, tStart, tEnd time.Time) []ChartAxisMark {
	kept := make([]ChartAxisMark, 0, len(marks))
	for _, m := range marks {
		if !m.At.Before(tStart) && !m.At.After(tEnd) {
			kept = append(kept, m)
		}
	}
	return kept
}

// writeAxisMarks draws each mark as a short vertical stroke rising from the
// baseline at its instant, with the label beside it and the full sentence in
// a tooltip. Everything is html-escaped; each group carries data-axis-mark so
// a test can tell a tick from the data curves. A mark with an Href wraps its
// stroke and label in an SVG anchor — no script, the same plain link
// vocabulary as the rest of the page.
func writeAxisMarks(buf *bytes.Buffer, x func(time.Time) float64, yBaseline float64, marks []ChartAxisMark) {
	for _, m := range marks {
		mx := x(m.At)
		fmt.Fprintf(buf, `<g data-axis-mark="%s">`, html.EscapeString(m.Label))
		open, close := "", ""
		if m.Href != "" {
			open = fmt.Sprintf(`<a href="%s">`, html.EscapeString(m.Href))
			close = `</a>`
		}
		fmt.Fprintf(buf, `%s<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-width="1.5" stroke-opacity="0.9"/>`,
			open, mx, yBaseline, mx, yBaseline-10.0)
		fmt.Fprintf(buf, `<text x="%.1f" y="%.1f" font-size="8" fill="currentColor" fill-opacity="0.8">%s</text>%s`,
			mx+2.0, yBaseline-12.0, html.EscapeString(m.Label), close)
		fmt.Fprintf(buf, `<title>%s</title></g>`, html.EscapeString(m.Note))
	}
}

// ItemsChartView holds the rendered item-count SVG for template inclusion.
// A nil *ItemsChartView means there is nothing to draw.
type ItemsChartView struct {
	SVG template.HTML
	// Total and Open are the latest values, rendered as a caption so the
	// reader gets the two numbers without having to measure the lines.
	Total int
	Open  int
	// Note, when set, is an honesty sentence printed under the chart in the
	// panel's chart-note slot: a named limit the reader must see rather
	// than deduce. Today that is the scope_change walk announcing it hit
	// its page cap, so the missing oldest ticks read as "the walk stops
	// here", never as "nothing was declared back then" — the same honesty
	// the export document's truncated field gives the export. Empty for a
	// walk that covered the whole feed.
	Note string
}

// NewItemsChartView builds the item-count chart. Returns nil when the project
// has no tasks at all — an empty panel would claim something was measured.
// extraMarks (KANB-38: scope_change ticks) pass straight through to the
// renderer.
func NewItemsChartView(points []service.HistoryPoint, width, height int, extraMarks ...ChartAxisMark) *ItemsChartView {
	svg := RenderItemsChart(points, width, height, extraMarks...)
	if svg == "" {
		return nil
	}
	last := points[len(points)-1]
	return &ItemsChartView{SVG: svg, Total: last.TotalTasks, Open: last.OpenTasks}
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

// visiblePoints drops the points a reader could not see anyway. The journal
// records EVERY change — an estimate edit, a move between two open columns,
// a reparent — and most of those move no count and archive nothing, so their
// points would draw a flat segment at the level already held. A run of such
// points collapses into its last member, which is lossless for the picture:
// between two kept points the true curve is exactly "hold the earlier level
// until the later instant, then jump", because every dropped point held that
// same level by construction. Keeping every count change, every archival
// point and both endpoints is what makes it lossless.
//
// This is not an optimisation for its own sake: the journal is append-only
// and never pruned, and an unfiltered walk of a busy board would spend the
// whole MaxChartPoints budget on entries the chart cannot show.
func visiblePoints(points []service.HistoryPoint) []service.HistoryPoint {
	if len(points) <= 1 {
		return points
	}
	out := make([]service.HistoryPoint, 0, len(points))
	for i, p := range points {
		switch {
		case i == 0, i == len(points)-1, p.Archived > 0:
			out = append(out, p)
		default:
			prev := out[len(out)-1]
			if p.TotalTasks != prev.TotalTasks || p.OpenTasks != prev.OpenTasks {
				out = append(out, p)
			}
		}
	}
	return out
}

// decimateCountPoints applies the ONE shared decimation rule
// (decimateKeepMask) to each curve and keeps the union of what either
// survives — the total and the open staircase are two independent stories,
// and a point that is the whole story of one of them must not be dropped
// because the other is quiet there. Polarity per curve: a FALL in total is
// the news (it is an archival, and the axis tick exists to say so), a RISE
// in open is the news (work added or reopened). Archival points are kept
// outright: a tick pointing at an instant where the drawn line shows no fall
// would be its own lie.
func decimateCountPoints(points []service.HistoryPoint, maxPoints int) []service.HistoryPoint {
	if len(points) <= maxPoints || maxPoints < 2 {
		return points
	}
	total := make([]int64, len(points))
	open := make([]int64, len(points))
	for i, p := range points {
		total[i] = int64(p.TotalTasks)
		open[i] = int64(p.OpenTasks)
	}
	tKeep := decimateKeepMask(total, maxPoints, false)
	oKeep := decimateKeepMask(open, maxPoints, true)
	out := make([]service.HistoryPoint, 0, maxPoints)
	for i, p := range points {
		if tKeep[i] || oKeep[i] || p.Archived > 0 {
			out = append(out, p)
		}
	}
	return out
}

// RenderItemsChart draws the total/open step curves from journal replay
// points, with a tick on the time axis at every archival, plus any
// extraMarks the caller hands in (KANB-38: the scope_change declarations —
// they stand on the message's own instant, independent of where the curve
// moved, because the tick explains a DECLARATION, not a point). Empty input
// renders "".
func RenderItemsChart(points []service.HistoryPoint, width, height int, extraMarks ...ChartAxisMark) template.HTML {
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

	// The scale and the span come from the WHOLE curve, before collapsing:
	// the drawn range must never be narrower than the data's.
	maxCount := 0
	for _, p := range points {
		if p.TotalTasks > maxCount {
			maxCount = p.TotalTasks
		}
	}
	tStart := points[0].At
	tEnd := points[len(points)-1].At

	series := decimateCountPoints(visiblePoints(points), MaxChartPoints)

	// Archival ticks derive from the drawn points; scope ticks arrive from
	// outside and are kept only where the axis can honestly place them. Both
	// go on last so neither hides under a curve.
	marks := axisMarksFromPoints(series)
	marks = append(marks, marksOnSpan(extraMarks, tStart, tEnd)...)

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

	if len(series) == 1 {
		// One instant after collapsing: two dots, no lines. A polyline needs
		// two points and a single-point "curve" would imply a trend that has
		// not happened.
		p := series[0]
		fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="3" fill="currentColor" data-series="items-total"><title>Total items: %d</title></circle>`,
			b.x(p.At), b.y(p.TotalTasks), p.TotalTasks)
		fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="3" fill="currentColor" fill-opacity="0.55" data-series="items-open"><title>Open items: %d</title></circle>`,
			b.x(p.At), b.y(p.OpenTasks), p.OpenTasks)
		// The tooltip carries the instant and both values, the same sentence
		// a multi-point chart shows at every point.
		fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="2.4" fill="currentColor" data-items-point><title>%s, total %d, open %d</title></circle>`,
			b.x(p.At), b.y(p.TotalTasks), html.EscapeString(formatChartTime(p.At, sameDay)), p.TotalTasks, p.OpenTasks)
		writeAxisMarks(&buf, b.x, yZero, marks)
		buf.WriteString(`</svg>`)
		return template.HTML(buf.String())
	}

	totalPath := itemsStepPath(b, series, func(p service.HistoryPoint) int { return p.TotalTasks })
	openPath := itemsStepPath(b, series, func(p service.HistoryPoint) int { return p.OpenTasks })

	fmt.Fprintf(&buf, `<polyline fill="none" stroke="currentColor" stroke-width="2.0" points="%s" data-series="items-total"><title>Total items (scope, including work added along the way)</title></polyline>`, totalPath)
	fmt.Fprintf(&buf, `<polyline fill="none" stroke="currentColor" stroke-width="1.4" stroke-dasharray="%s" points="%s" data-series="items-open"><title>Items still open (not in a done column)</title></polyline>`,
		itemsOpenDashArray, openPath)

	// One hover target per drawn point, carrying the instant AND both values:
	// the acceptance asks for the time and the numbers at the point, not for
	// a line one has to measure against the axis.
	for _, p := range series {
		fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="2.4" fill="currentColor" data-items-point><title>%s, total %d, open %d</title></circle>`,
			b.x(p.At), b.y(p.TotalTasks), html.EscapeString(formatChartTime(p.At, sameDay)), p.TotalTasks, p.OpenTasks)
	}

	// Archival ticks go on last so a tick is never hidden under a curve it
	// explains.
	writeAxisMarks(&buf, b.x, yZero, marks)

	buf.WriteString(`</svg>`)
	return template.HTML(buf.String())
}

// itemsStepPath renders one count series as a step line: hold the previous
// value across to the next instant, then jump. pick chooses which of the two
// counts to plot.
func itemsStepPath(b itemsBounds, points []service.HistoryPoint, pick func(service.HistoryPoint) int) string {
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
// assessment chart, the forecast chart (KANB-36: now its own panel with a
// real date axis, was previously a track on the assessment chart), and —
// for the project scope only — the item-count chart. Any of the three may
// be nil; a fragment with all three nil renders nothing, which app.js
// treats as "nothing to open".
//
// The three are separate fields rather than one merged picture because they
// answer different questions in different units: Progress is what the
// assessors SAY (percent), Forecast is when they think it'll be done
// (dates), Items is what the board CONTAINS (counts). Two of them were
// already independent; KANB-36 promoted the forecast from "a track on
// progress" to its own chart, leaving the others untouched. Keeping them
// apart is the same discipline that keeps "assessed" and "tasks done" as
// two bars instead of one blended number.
type ProgressChartFragment struct {
	Progress  *ProgressChartView
	Forecast  *ForecastChartView
	Items     *ItemsChartView
	Readiness *ReadinessChartView
}

// Empty reports whether there is nothing at all to render.
func (f *ProgressChartFragment) Empty() bool {
	return f == nil || (f.Progress == nil && f.Forecast == nil && f.Items == nil && f.Readiness == nil)
}
