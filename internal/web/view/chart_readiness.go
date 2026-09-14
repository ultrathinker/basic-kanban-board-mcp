package view

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// The historical readiness chart (KANB-53).
//
// The estimate-weighted readiness of the project at every recorded instant,
// replayed from the lifecycle journal — the same points the item-count chart
// is built from, read through Readiness.Percent instead of the counts.
//
// The chart exists so its caption can exist. The curve is computed from the
// estimates that were CURRENT at each instant and is never recomputed
// backwards, so early points legitimately disagree with a figure recomputed
// from today's estimates. Without the sentence beside the curve, that
// disagreement reads as a bug, and "fixing" it would recompute the past —
// the one thing the journal exists to prevent. The sentence is not kept
// here: the view carries service's own HistoricalReadinessNote through from
// the points, so the wording has exactly one home.
// ---------------------------------------------------------------------------

// ReadinessChartView holds the rendered readiness SVG plus the caption and
// the note a template must print beside it. A nil view means there is
// nothing to draw.
type ReadinessChartView struct {
	SVG template.HTML
	// Caption is the current reading: the latest percent and its own basis,
	// e.g. "38%, 8 of 12 estimated".
	Caption string
	// Note is service.HistoricalReadinessNote, carried verbatim off the
	// replayed points. Not a copy of the sentence — it IS the sentence.
	Note string
}

// NewReadinessChartView builds the historical readiness chart from journal
// replay points. Returns nil when no point carries a readiness percent at
// all (nothing was ever estimated): the "no data, no placeholder" rule the
// other panels follow — a frame around nothing would still need the
// sentence, and the sentence without a curve is a warning label, not a
// chart.
func NewReadinessChartView(points []service.HistoryPoint, width, height int) *ReadinessChartView {
	segments := readinessSegments(points)
	if len(segments) == 0 {
		return nil
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

	svg := renderReadinessChart(segments, width, height)
	if svg == "" {
		return nil
	}
	last := segments[len(segments)-1]
	lastPoint := last[len(last)-1]
	caption := fmt.Sprintf("%d%%, %s", *lastPoint.Readiness.Percent, lastPoint.Readiness.Coverage)
	return &ReadinessChartView{
		SVG:     svg,
		Caption: caption,
		Note:    lastPoint.Readiness.HistoricalNote,
	}
}

// readinessSegments reduces the replay to what the curve can draw: the
// points whose basis produced a percent, split at every gap where the
// percent does not exist (nothing estimated at that instant). A line drawn
// across such a gap would invent a value for the one stretch of history
// that has none.
func readinessSegments(points []service.HistoryPoint) [][]service.HistoryPoint {
	var segments [][]service.HistoryPoint
	var cur []service.HistoryPoint
	for _, p := range points {
		if p.Readiness.Percent == nil {
			if len(cur) > 0 {
				segments = append(segments, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, p)
	}
	if len(cur) > 0 {
		segments = append(segments, cur)
	}
	out := make([][]service.HistoryPoint, 0, len(segments))
	for _, seg := range segments {
		out = append(out, decimateReadinessPoints(visibleReadiness(seg), MaxChartPoints))
	}
	return out
}

// decimateReadinessPoints applies the ONE shared decimation rule
// (decimateKeepMask) to a segment's percents. A fall in readiness is the
// news here, so worseIsHigher is false — the same polarity as the
// assessors' percent chart.
func decimateReadinessPoints(points []service.HistoryPoint, maxPoints int) []service.HistoryPoint {
	if len(points) <= maxPoints || maxPoints < 2 {
		return points
	}
	values := make([]int64, len(points))
	for i, p := range points {
		values[i] = int64(*p.Readiness.Percent)
	}
	keep := decimateKeepMask(values, maxPoints, false)
	out := make([]service.HistoryPoint, 0, maxPoints)
	for i, p := range points {
		if keep[i] {
			out = append(out, p)
		}
	}
	return out
}

// visibleReadiness collapses, within one segment, the points a step line
// could not show anyway: a journal entry that moved no estimate and no count
// leaves the percent where it was, so its point would draw a flat step. The
// same lossless argument as the item chart's visiblePoints applies — between
// two kept points the true curve holds the earlier percent until the later
// instant.
func visibleReadiness(points []service.HistoryPoint) []service.HistoryPoint {
	if len(points) <= 1 {
		return points
	}
	out := make([]service.HistoryPoint, 0, len(points))
	for i, p := range points {
		switch {
		case i == 0, i == len(points)-1:
			out = append(out, p)
		default:
			if *p.Readiness.Percent != *out[len(out)-1].Readiness.Percent {
				out = append(out, p)
			}
		}
	}
	return out
}

// renderReadinessChart draws each segment as a hold-then-jump step line on a
// 0..100 percent axis — readiness is a percent, so it reuses the progress
// chart's bounds and axis vocabulary rather than inventing a third scale.
func renderReadinessChart(segments [][]service.HistoryPoint, width, height int) template.HTML {
	var tStart, tEnd time.Time
	first := true
	for _, seg := range segments {
		for _, p := range seg {
			if first || p.At.Before(tStart) {
				tStart = p.At
			}
			if first || p.At.After(tEnd) {
				tEnd = p.At
			}
			first = false
		}
	}
	b := newChartBounds(width, height, tStart, tEnd)

	var buf bytes.Buffer
	w, h := int(b.width), int(b.height)
	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" class="chart-readiness" role="img" aria-label="Historical readiness: the estimate-weighted percent at each recorded instant">`, w, h, w, h)

	// The percent axis, in the progress chart's inline vocabulary: 0 and 100
	// labelled, the midline drawn. Readiness shares the 0..100 scale, so its
	// reader gets the same landmarks, not a new dialect.
	yZero := b.y(0)
	yHundred := b.y(100)
	fmt.Fprintf(&buf, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-width="1" stroke-opacity="0.35"/>`, b.xMin, yZero, b.xMax, yZero)
	fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.5">0%%</text>`, b.xMin-4.0, yZero+3.0)
	fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.5">100%%</text>`, b.xMin-4.0, yHundred+3.0)

	// Time labels, same placement and vocabulary as the other panels.
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

	for _, seg := range segments {
		if len(seg) == 1 {
			p := seg[0]
			fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="3" fill="currentColor" data-series="readiness"><title>%s, %d%%</title></circle>`,
				b.x(p.At), b.y(*p.Readiness.Percent), html.EscapeString(formatChartTime(p.At, sameDay)), *p.Readiness.Percent)
			continue
		}
		fmt.Fprintf(&buf, `<polyline fill="none" stroke="currentColor" stroke-width="2.0" points="%s" data-series="readiness"><title>Estimate-weighted readiness at the time</title></polyline>`,
			readinessStepPath(b, seg))
	}
	buf.WriteString(`</svg>`)
	return template.HTML(buf.String())
}

// readinessStepPath renders one segment as a step line: hold the previous
// percent across to the next instant, then jump — the readiness changes at
// instants, never gradually between them.
func readinessStepPath(b chartBounds, seg []service.HistoryPoint) string {
	var sb bytes.Buffer
	prevY := b.y(*seg[0].Readiness.Percent)
	for i, p := range seg {
		x := b.x(p.At)
		y := b.y(*p.Readiness.Percent)
		if i > 0 {
			fmt.Fprintf(&sb, " %.1f,%.1f ", x, prevY)
		}
		fmt.Fprintf(&sb, "%.1f,%.1f", x, y)
		prevY = y
	}
	return sb.String()
}
