package view

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"sort"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// The forecast chart.
//
// KANB-36 reversed the previous forecast rendering — the forecast used to
// share the percent chart's 0..100 axis, normalized so the earliest promise
// sat at 100 (top) and the latest at 0 (bottom). That mixed a date with a
// percent and read badly: a held promise on the percent chart's normal scale
// is indistinguishable from "stuck at the top because it's done", and a
// promise that drifted from "in two weeks" to "in three months" slid
// diagonally the same way a percent drop does. Two different facts, one
// vocabulary.
//
// This file is the new forecast chart, with a real date axis: x is when the
// forecast was made (CreatedAt), y is the promised finish date (ETA), one
// line per assessor plus a consensus line that averages the ETAs at every
// unique time point. The two charts are now answerable independently — the
// percent chart answers "what does the assessor think the work is", the
// forecast chart answers "when do they think it'll be done".
//
// The data is unchanged. Every forecast mark still carries ETA on the same
// domain.ProgressMark shape; only the rendering moved.
// ---------------------------------------------------------------------------

// forecastPoint is a single forecast data point: when it was made and the
// date it promised.
type forecastPoint struct {
	CreatedAt time.Time
	ETA       time.Time
}

// forecastSeries is one polyline track on the forecast chart. The same
// shape as chartSeries (chart.go) but with forecastPoint instead of
// chartPoint — the projections are different enough that reusing the type
// would force one chart to know the other's coordinate space.
type forecastSeries struct {
	Name        string
	IsComposite bool
	Points      []forecastPoint
	DashArray   string
	StrokeWidth float64
}

// ForecastChartView holds the rendered SVG chart for the forecast history.
// A nil *ForecastChartView means "nobody has ever forecast this scope" —
// the template renders nothing at all, the same "no data, no placeholder"
// rule the percent chart and the items chart already follow.
type ForecastChartView struct {
	SVG template.HTML
}

// NewForecastChartView builds a ForecastChartView from history and
// dimensions. Returns nil if no mark in history carries an ETA: rendering
// a frame around "we know nothing" would be a worse lie than nothing.
func NewForecastChartView(marks []domain.ProgressMark, width, height int) *ForecastChartView {
	svg := RenderForecastChart(marks, width, height)
	if svg == "" {
		return nil
	}
	return &ForecastChartView{SVG: svg}
}

// RenderForecastChart builds the inline-panel forecast SVG: smaller
// (DefaultChartWidth x DefaultChartHeight), no axis labels, just a midline
// reference (the date the EARLIEST forecast named, so a held promise
// against that reference reads as flat). Returns "" for empty history —
// the caller already handles the "render nothing" rule.
func RenderForecastChart(marks []domain.ProgressMark, width, height int) template.HTML {
	return renderForecastChartImpl(marks, width, height, false)
}

// RenderForecastChartDetailed is the enlarged, modal version: same data
// and same projections, with the labelled date Y axis and the midpoint
// time tick the inline panel has no room for. Kept as its own entry point
// so chart_detail.go does not need to know the internal "detail" switch.
func RenderForecastChartDetailed(marks []domain.ProgressMark, width, height int) template.HTML {
	return renderForecastChartImpl(marks, width, height, true)
}

// renderForecastChartImpl is the underlying renderer with the detail
// switch the enlarged, modal version needs: a labelled date Y axis
// (writeDateAxis) and a midpoint time tick on X, instead of the one-line
// references the inline panel carries. Same data, same projections, just
// more furniture. RenderForecastChart / RenderForecastChartDetailed wrap
// it with the right flag.
func renderForecastChartImpl(marks []domain.ProgressMark, width, height int, detailed bool) template.HTML {
	// Filter to marks with a forecast. Not every mark carries one (ETA is
	// optional on progress_set), and plotting a chart that silently drops
	// every mark without an ETA would be a different lie from "render
	// nothing". The "render nothing" case is reserved for "no mark had an
	// ETA at all"; the inline-panel case carries the per-assessor shapes
	// from the marks that did.
	forecasts := make([]domain.ProgressMark, 0, len(marks))
	for _, m := range marks {
		if m.ETA != nil {
			forecasts = append(forecasts, m)
		}
	}
	if len(forecasts) == 0 {
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

	// Defensive copy + chronological sort. The chart's x axis is real time,
	// so order matters: an unsorted input would draw a polyline that jumps
	// back and forth instead of tracking "the promise moving as time goes
	// on". The tiebreaker on equal CreatedAt is the mark id, the same
	// order chart.go uses for the percent chart, so the two charts agree
	// on which mark comes first when two landed in the same instant.
	sorted := make([]domain.ProgressMark, len(forecasts))
	copy(sorted, forecasts)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})

	tStart := sorted[0].CreatedAt
	tEnd := sorted[len(sorted)-1].CreatedAt
	etaMin, etaMax := forecastEtaRange(sorted)

	b := forecastBoundsOf(width, height, tStart, tEnd, etaMin, etaMax)

	// Group forecasts by assessor, in chronological order. The same dash
	// assignment rule the percent chart uses (assessorDash), so a reader
	// who knows one chart already knows the other: the alpha on the percent
	// chart and the alpha on the forecast chart use the same dash.
	byAssessor := map[string][]forecastPoint{}
	var names []string
	for _, m := range sorted {
		if _, seen := byAssessor[m.Assessor]; !seen {
			names = append(names, m.Assessor)
		}
		byAssessor[m.Assessor] = append(byAssessor[m.Assessor], forecastPoint{
			CreatedAt: m.CreatedAt,
			ETA:       *m.ETA,
		})
	}
	sort.Strings(names)

	// Consensus: the average ETA across all assessors at each unique
	// CreatedAt. When only one assessor forecasted at a time point, the
	// consensus IS that forecast. When several did, it's their mean — the
	// one shape that answers "what is the project's standing promise" at
	// that instant, without favouring any single assessor.
	consensus := buildConsensusSeries(sorted)

	var seriesList []forecastSeries
	for i, name := range names {
		seriesList = append(seriesList, forecastSeries{
			Name:        name,
			IsComposite: false,
			Points:      decimateForecastPoints(byAssessor[name], MaxChartPoints),
			DashArray:   assessorDash(i),
			StrokeWidth: 1.2,
		})
	}
	// Consensus last, on top of the per-assessor tracks. The visual grammar
	// matches the percent chart: assessors underneath, summary on top.
	if len(consensus) > 0 {
		seriesList = append(seriesList, forecastSeries{
			Name:        "consensus",
			IsComposite: true,
			Points:      decimateForecastPoints(consensus, MaxChartPoints),
			DashArray:   "none",
			StrokeWidth: 2.5,
		})
	}

	return renderFullForecastChart(b, seriesList, etaMin, etaMax, detailed)
}

// buildConsensusSeries averages ETA across all assessors at each unique
// CreatedAt. Used by both the chart and the legend, so the two cannot
// drift apart (the legend's "consensus" note has to read what the chart
// actually draws — see chart_detail.go's countConsensusPoints).
//
// Why a separate function, not inlined: the legend note for the consensus
// line (forecastConsensusNote) needs the same series the chart drew, with
// the same definition of "consensus", and there is exactly one answer
// here. One function = one definition.
func buildConsensusSeries(marks []domain.ProgressMark) []forecastPoint {
	if len(marks) == 0 {
		return nil
	}
	// Bucket by CreatedAt; each bucket is one point on the consensus line.
	type bucket struct {
		at  time.Time
		sum int64 // nanoseconds since epoch, summed for averaging
		n   int
	}
	buckets := map[int64]*bucket{}
	order := []int64{}
	for _, m := range marks {
		if m.ETA == nil {
			continue
		}
		key := m.CreatedAt.UnixNano()
		b, ok := buckets[key]
		if !ok {
			b = &bucket{at: m.CreatedAt}
			buckets[key] = b
			order = append(order, key)
		}
		b.sum += m.ETA.UnixNano()
		b.n++
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]forecastPoint, 0, len(order))
	for _, k := range order {
		b := buckets[k]
		avg := time.Unix(0, b.sum/int64(b.n))
		out = append(out, forecastPoint{CreatedAt: b.at, ETA: avg})
	}
	return out
}

// forecastBounds encapsulates the SVG viewport and the two coordinate
// projections: time on X (same shape as chartBounds.x), date on Y (its
// own mapping). It is its own type because the percent chart's bounds
// hard-code a percent axis; sharing the type would force the forecast
// chart to fake one.
type forecastBounds struct {
	width, height  float64
	xMin, xMax     float64
	yMin, yMax     float64
	tStart, tEnd   time.Time
	durationNan    float64
	etaMin, etaMax time.Time
	etaSpan        time.Duration
}

// forecastChartPadLeft is the forecast chart's own left padding, wider than
// the shared ChartPadLeft. Both forecast surfaces anchor a full
// "2006-01-02" date label just left of the plot area — the inline panel's
// earliest-ETA reference and the modal's whole date axis — and a
// 10-character date at the axis font size is ~60px wide, about twice what
// ChartPadLeft (36, sized for the percent chart's "100%") leaves beside the
// plot. With the shared pad the label began at a negative SVG x and the
// viewport clipped it: a date axis a reader could not read.
const forecastChartPadLeft = 72.0

func forecastBoundsOf(w, h int, tStart, tEnd time.Time, etaMin, etaMax time.Time) forecastBounds {
	width := float64(w)
	height := float64(h)
	xMin := forecastChartPadLeft
	xMax := width - ChartPadRight
	yMin := ChartPadTop
	yMax := height - ChartPadBottom
	span := etaMax.Sub(etaMin)
	return forecastBounds{
		width:       width,
		height:      height,
		xMin:        xMin,
		xMax:        xMax,
		yMin:        yMin,
		yMax:        yMax,
		tStart:      tStart,
		tEnd:        tEnd,
		durationNan: float64(tEnd.Sub(tStart).Nanoseconds()),
		etaMin:      etaMin,
		etaMax:      etaMax,
		etaSpan:     span,
	}
}

// x: time -> horizontal SVG coordinate, with a single-instant fallback
// to the centre so a history that all happened in one second does not
// divide by zero.
func (b forecastBounds) x(t time.Time) float64 {
	if b.durationNan <= 0 {
		return (b.xMin + b.xMax) / 2.0
	}
	diff := float64(t.Sub(b.tStart).Nanoseconds())
	if diff < 0 {
		diff = 0
	}
	if diff > b.durationNan {
		diff = b.durationNan
	}
	return b.xMin + (diff/b.durationNan)*(b.xMax-b.xMin)
}

// y: date -> vertical SVG coordinate. Earlier ETA sits at the top
// (smaller Y), later ETA at the bottom — "promised sooner" reads above
// "promised later", the same direction the percent chart uses for
// "more done" above "less done". A single-instant fallback (etaMin == etaMax)
// centres the point so a one-forecast history does not divide by zero.
func (b forecastBounds) y(t time.Time) float64 {
	if b.etaSpan <= 0 {
		return (b.yMin + b.yMax) / 2.0
	}
	diff := float64(t.Sub(b.etaMin))
	if diff < 0 {
		diff = 0
	}
	if diff > float64(b.etaSpan) {
		diff = float64(b.etaSpan)
	}
	return b.yMin + (diff/float64(b.etaSpan))*(b.yMax-b.yMin)
}

// renderFullForecastChart writes the SVG: axes, gridlines, series tracks.
// The series shape is the same vocabulary as the percent chart (one
// polyline per multi-point series, one circle per single-point series),
// only the per-point tooltip and the Y axis differ — see tooltipText.
func renderFullForecastChart(b forecastBounds, seriesList []forecastSeries, etaMin, etaMax time.Time, detailed bool) template.HTML {
	var buf bytes.Buffer
	w := int(b.width)
	h := int(b.height)

	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" class="chart-forecast" role="img" aria-label="Forecast history">`, w, h, w, h)

	if detailed {
		writeDateAxis(&buf, b.yMin, b.yMax, etaMin, etaMax, b.xMin, b.xMax)
	} else {
		// The inline panel only has room for one reference line. The
		// earliest-ETA reference is what a held promise rests on: a
		// flat line across it reads as "the promise has not moved",
		// and a downward slope reads as "the promise is sliding later".
		yRef := b.y(etaMin)
		fmt.Fprintf(&buf, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-dasharray="2 3" stroke-width="1" stroke-opacity="0.3"/>`, b.xMin, yRef, b.xMax, yRef)
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.5">%s</text>`, b.xMin-4.0, yRef+3.0, html.EscapeString(etaMin.Format("2006-01-02")))
	}

	// X axis: time. Always labelled (the inline panel carries start/end
	// times, the enlarged version also adds the midpoint tick).
	sameDay := sameCalendarDay(b.tStart, b.tEnd)
	startLabel := formatChartTime(b.tStart, sameDay)
	endLabel := formatChartTime(b.tEnd, sameDay)
	if b.durationNan <= 0 {
		midX := (b.xMin + b.xMax) / 2.0
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, midX, b.height-ChartPadBottom+16.0, html.EscapeString(startLabel))
	} else {
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="start" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, b.xMin, b.height-ChartPadBottom+16.0, html.EscapeString(startLabel))
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, b.xMax, b.height-ChartPadBottom+16.0, html.EscapeString(endLabel))
		if detailed {
			mid := b.tStart.Add(b.tEnd.Sub(b.tStart) / 2)
			midX := b.x(mid)
			fmt.Fprintf(&buf, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-width="1" stroke-opacity="0.18"/>`, midX, b.yMin, midX, b.yMax)
			fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, midX, b.height-ChartPadBottom+16.0, html.EscapeString(formatChartTime(mid, sameDay)))
		}
	}

	for _, s := range seriesList {
		if len(s.Points) == 0 {
			continue
		}
		escapedName := html.EscapeString(s.Name)

		if len(s.Points) == 1 {
			pt := s.Points[0]
			cx := b.x(pt.CreatedAt)
			cy := b.y(pt.ETA)
			seriesAttr := ""
			if s.IsComposite {
				seriesAttr = ` data-series="consensus"`
			} else {
				seriesAttr = fmt.Sprintf(` data-assessor="%s"`, escapedName)
			}
			fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="3" fill="currentColor"%s><title>%s: %s</title></circle>`,
				cx, cy, seriesAttr, escapedName, html.EscapeString(tooltipText(pt)))
			continue
		}

		var pointsBuf bytes.Buffer
		for i, pt := range s.Points {
			if i > 0 {
				pointsBuf.WriteByte(' ')
			}
			fmt.Fprintf(&pointsBuf, "%.1f,%.1f", b.x(pt.CreatedAt), b.y(pt.ETA))
		}

		dashAttr := ""
		if s.DashArray != "" && s.DashArray != "none" {
			dashAttr = fmt.Sprintf(` stroke-dasharray="%s"`, s.DashArray)
		}
		if s.IsComposite {
			fmt.Fprintf(&buf, `<polyline fill="none" stroke="currentColor" stroke-width="%.1f" points="%s" data-series="consensus"><title>Consensus: %s</title></polyline>`,
				s.StrokeWidth, pointsBuf.String(), html.EscapeString(consensusTooltip(s.Points)))
			// KANB-36: per-point tooltip targets (invisible circles) on
			// top of the consensus polyline, so a hover on a single point
			// shows "promised date (in Xd)" rather than the polyline's
			// range summary. The circles are tiny and transparent so the
			// line beneath them stays exactly the line; only the hover
			// area changes.
			for _, pt := range s.Points {
				cx := b.x(pt.CreatedAt)
				cy := b.y(pt.ETA)
				fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="6" fill="transparent" data-series="consensus"><title>%s</title></circle>`,
					cx, cy, html.EscapeString(tooltipText(pt)))
			}
		} else {
			fmt.Fprintf(&buf, `<polyline fill="none" stroke="currentColor" stroke-width="%.1f"%s points="%s" data-assessor="%s"><title>%s</title></polyline>`,
				s.StrokeWidth, dashAttr, pointsBuf.String(), escapedName, html.EscapeString(forecastLineTooltip(s.Name, s.Points)))
			// KANB-36: per-point tooltip targets (invisible circles) on
			// top of the per-assessor polyline, same shape as for the
			// consensus track above. The polyline's title still names the
			// line; the circles carry the per-point info.
			for _, pt := range s.Points {
				cx := b.x(pt.CreatedAt)
				cy := b.y(pt.ETA)
				fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="6" fill="transparent" data-assessor="%s"><title>%s</title></circle>`,
					cx, cy, escapedName, html.EscapeString(tooltipText(pt)))
			}
		}
	}

	buf.WriteString(`</svg>`)
	return template.HTML(buf.String())
}

// tooltipText is the title text on a single forecast point: the promised
// date and the remaining time AT THE MOMENT of the forecast, both numbers
// because "remaining time" alone shrinks every day even when nothing
// changed (the same reason the Y axis is absolute dates, not "days
// remaining from now").
//
// A promise whose date had already passed when it was made is OVERDUE, not
// imminent. humanizeDuration has no sign, so the raw negative remainder
// falls into its "<1m" branch and a month-late promise would read
// "(in <1m)" — the exact lie the board's forecast badge already avoids by
// carrying an "overdue" mark for the same situation (view.ForecastView).
func tooltipText(pt forecastPoint) string {
	rem := pt.ETA.Sub(pt.CreatedAt)
	if rem < 0 {
		return pt.ETA.Format("2006-01-02") + " (overdue by " + humanizeDuration(-rem) + ")"
	}
	return pt.ETA.Format("2006-01-02") + " (in " + humanizeDuration(rem) + ")"
}

// forecastLineTooltip is the title text on an assessor's polyline: where
// their promises started and where they ended, plus the point count.
// One sentence, not a sentence per point — tooltips on a polyline replace
// themselves with the marker's hover, not a long per-point log.
func forecastLineTooltip(name string, pts []forecastPoint) string {
	if len(pts) == 0 {
		return name
	}
	first := pts[0].ETA.Format("2006-01-02")
	last := pts[len(pts)-1].ETA.Format("2006-01-02")
	if first == last {
		return fmt.Sprintf("%s: %s (%d forecast%s)", name, first, len(pts), pluralS(len(pts)))
	}
	return fmt.Sprintf("%s: %s -> %s (%d forecast%s)", name, first, last, len(pts), pluralS(len(pts)))
}

// consensusTooltip is the title text on the consensus polyline. Range +
// point count, same shape as forecastLineTooltip so the legend and the
// tooltip agree. The renderer prepends "Consensus: " once; the function
// itself returns just the range so the two do not collide.
func consensusTooltip(pts []forecastPoint) string {
	if len(pts) == 0 {
		return "no consensus"
	}
	first := pts[0].ETA.Format("2006-01-02")
	last := pts[len(pts)-1].ETA.Format("2006-01-02")
	if first == last {
		return fmt.Sprintf("%s (%d point%s)", first, len(pts), pluralS(len(pts)))
	}
	return fmt.Sprintf("%s -> %s (%d point%s)", first, last, len(pts), pluralS(len(pts)))
}

// humanizeDuration renders a duration in the compact form the rest of
// the board already uses (formatDuration in view.go: "12m", "3h", "2d").
// The function is unexported in view.go, so this is its own one-line
// version with the same vocabulary — staying in sync is testable, but
// the two definitions would drift apart quietly if they differed.
func humanizeDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// decimateForecastPoints downsamples a dense forecast track for rendering,
// protecting the moves a reader opens this chart to see.
//
// It delegates the rule to decimateKeepMask (chart.go) — the SAME rule the
// percent chart uses, with the same MaxChartPoints budget, so the two charts
// cannot drift apart in either the limit or the definition of "critical".
//
// The one thing that differs is which direction is bad news, and that is the
// worseIsHigher argument. On the percent chart bad news is a FALL: readiness
// revised down, 91% -> 72%. On this chart the value is a promised DATE, so bad
// news is the value going UP: the promise sliding from "in two weeks" to "in
// three months". Passing false here would protect the wrong extrema — it would
// faithfully preserve every time the team pulled the date IN and quietly throw
// away every slip, which is the exact opposite of what the chart is for.
func decimateForecastPoints(points []forecastPoint, maxPoints int) []forecastPoint {
	if len(points) <= maxPoints || maxPoints < 2 {
		return points
	}
	values := make([]int64, len(points))
	for i, p := range points {
		values[i] = p.ETA.UnixNano()
	}
	keep := decimateKeepMask(values, maxPoints, true)
	var result []forecastPoint
	for i := range points {
		if keep[i] {
			result = append(result, points[i])
		}
	}
	return result
}
