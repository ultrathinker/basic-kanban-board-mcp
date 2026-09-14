package view

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// Default dimensions and layout constants for the progress history chart.
const (
	DefaultChartWidth  = 600
	DefaultChartHeight = 200
	MinChartWidth      = 200
	MinChartHeight     = 100

	ChartPadLeft   = 36.0
	ChartPadRight  = 24.0
	ChartPadTop    = 16.0
	ChartPadBottom = 24.0

	MaxChartPoints = 60
)

// chartDashPatterns provides a deterministic monochrome palette of line dash patterns
// so individual assessors are distinguished by stroke dash and width rather than color.
var chartDashPatterns = []string{
	"4 2",         // dashed
	"2 2",         // dotted
	"6 2 2 2",     // dash-dot
	"8 3",         // long dash
	"2 4",         // sparse dots
	"5 2 1 2 1 2", // dash-dot-dot
}

// chartPoint represents a single discrete progress mark in time and percent.
type chartPoint struct {
	Time    time.Time
	Percent int
}

// chartSeries represents a single polyline track to render.
type chartSeries struct {
	Name        string
	IsComposite bool
	// KANB-36: IsForecast is gone. The forecast is no longer drawn on the
	// percent chart — it lives in its own chart with a real date axis
	// (chart_forecast.go).
	Points      []chartPoint
	DashArray   string
	StrokeWidth float64
}

// ProgressChartView holds the rendered SVG chart for template inclusion.
// A nil *ProgressChartView means no assessment history exists.
type ProgressChartView struct {
	SVG template.HTML
}

// NewProgressChartView builds a ProgressChartView from history and dimensions.
// Returns nil if history is empty.
func NewProgressChartView(history []domain.ProgressMark, width, height int) *ProgressChartView {
	svg := RenderProgressChart(history, width, height)
	if svg == "" {
		return nil
	}
	return &ProgressChartView{SVG: svg}
}

// ProgressChartSVG renders an inline SVG representing the progress history over time.
// It is a pure function that returns safe template.HTML.
func ProgressChartSVG(history []domain.ProgressMark, width, height int) template.HTML {
	return RenderProgressChart(history, width, height)
}

// RenderProgressChart builds an inline SVG line chart showing progress assessment
// history over time for each assessor and the composite progress metric, plus
// (when the history contains any) a forecast track showing how the promised
// finish date moved over time — see buildForecastSeries for its mapping onto
// the shared 0..100 vertical axis.
// If history is empty, it returns empty HTML ("").
func RenderProgressChart(history []domain.ProgressMark, width, height int) template.HTML {
	return renderProgressChart(history, width, height, false)
}

// renderProgressChart is RenderProgressChart with the detail switch the
// enlarged, modal version needs: axis ticks and labelled gridlines instead of
// the single 50% guideline the inline panel carries. The data, the series and
// every projection are identical — "detailed" adds furniture around the same
// picture, it never changes the picture.
func renderProgressChart(history []domain.ProgressMark, width, height int, detailed bool) template.HTML {
	if len(history) == 0 {
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

	// Defensive copy and deterministic sort: chronological by CreatedAt, then by ID.
	marks := make([]domain.ProgressMark, len(history))
	copy(marks, history)
	sort.SliceStable(marks, func(i, j int) bool {
		if marks[i].CreatedAt.Equal(marks[j].CreatedAt) {
			return marks[i].ID < marks[j].ID
		}
		return marks[i].CreatedAt.Before(marks[j].CreatedAt)
	})

	tStart := marks[0].CreatedAt
	tEnd := marks[len(marks)-1].CreatedAt
	bounds := newChartBounds(width, height, tStart, tEnd)

	// Degenerate case: exactly one point in history.
	// Render a point marker, 50% guideline, and timestamp label — never a polyline.
	if len(marks) == 1 {
		return renderSinglePointChart(marks[0], bounds, detailed)
	}

	// Group marks by assessor and compute composite progress over time.
	assessorMarks := make(map[string][]chartPoint)
	latestByAssessor := make(map[string]int)
	var compositePoints []chartPoint

	for _, m := range marks {
		p := clampPercent(m.Percent)
		assessorMarks[m.Assessor] = append(assessorMarks[m.Assessor], chartPoint{
			Time:    m.CreatedAt,
			Percent: p,
		})

		latestByAssessor[m.Assessor] = p
		sum := 0
		for _, v := range latestByAssessor {
			sum += v
		}
		compPercent := service.RoundMeanHalfUp(sum, len(latestByAssessor))
		compositePoints = append(compositePoints, chartPoint{
			Time:    m.CreatedAt,
			Percent: compPercent,
		})
	}

	// Deduplicate points with identical timestamps within each series.
	for k, pts := range assessorMarks {
		assessorMarks[k] = deduplicateSameTimestamp(pts)
	}
	compositePoints = deduplicateSameTimestamp(compositePoints)

	// Decimate series to protect against dense / 500-point histories while strictly
	// preserving local minima and downward drops.
	for k, pts := range assessorMarks {
		assessorMarks[k] = decimatePoints(pts, MaxChartPoints)
	}
	compositePoints = decimatePoints(compositePoints, MaxChartPoints)

	// Forecast track removed (KANB-36): the forecast was drawn on the
	// percent chart's 0..100 axis, normalized so the EARLIEST promise sat at
	// 100 and the LATEST at 0. That mixed a date with a percent and read
	// badly. Forecasts now live in their own chart — see RenderForecastChart
	// in chart_forecast.go — with the predicted date on a real date axis,
	// one series per assessor, plus a calculated consensus at each time
	// point. The data itself is unchanged (m.ETA is still stored on every
	// mark); only its rendering moved.

	// Deterministically order assessor names.
	var assessorNames []string
	for name := range assessorMarks {
		assessorNames = append(assessorNames, name)
	}
	sort.Strings(assessorNames)

	var seriesList []chartSeries
	for i, name := range assessorNames {
		seriesList = append(seriesList, chartSeries{
			Name:        name,
			IsComposite: false,
			Points:      assessorMarks[name],
			DashArray:   assessorDash(i),
			StrokeWidth: 1.2,
		})
	}

	// Composite line is rendered last with a heavier, solid stroke to stand out prominently.
	seriesList = append(seriesList, chartSeries{
		Name:        "composite",
		IsComposite: true,
		Points:      compositePoints,
		DashArray:   "none",
		StrokeWidth: 2.5,
	})

	return renderFullChart(bounds, seriesList, tStart, tEnd, detailed)
}

// chartBounds encapsulates SVG viewport dimensions and coordinate projections.
type chartBounds struct {
	width       float64
	height      float64
	xMin        float64
	xMax        float64
	yMin        float64
	yMax        float64
	tStart      time.Time
	tEnd        time.Time
	durationNan float64
}

func newChartBounds(w, h int, tStart, tEnd time.Time) chartBounds {
	width := float64(w)
	height := float64(h)
	xMin := ChartPadLeft
	xMax := width - ChartPadRight
	yMin := ChartPadTop
	yMax := height - ChartPadBottom

	dur := float64(tEnd.Sub(tStart).Nanoseconds())
	return chartBounds{
		width:       width,
		height:      height,
		xMin:        xMin,
		xMax:        xMax,
		yMin:        yMin,
		yMax:        yMax,
		tStart:      tStart,
		tEnd:        tEnd,
		durationNan: dur,
	}
}

// x projects a timestamp onto the horizontal SVG coordinate space.
// If all points share the same timestamp (duration == 0), they are placed in the center.
func (b chartBounds) x(t time.Time) float64 {
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

// y projects a percent (0..100) onto the vertical SVG coordinate space.
// In SVG, Y increases downward: 100% maps to yMin (top), 0% maps to yMax (bottom).
// A drop in percentage (e.g. 91% -> 72%) results in an increasing Y coordinate.
func (b chartBounds) y(percent int) float64 {
	p := clampPercent(percent)
	return b.yMin + float64(100-p)/100.0*(b.yMax-b.yMin)
}

// renderSinglePointChart renders the degenerate single-point history.
func renderSinglePointChart(m domain.ProgressMark, b chartBounds, detailed bool) template.HTML {
	var buf bytes.Buffer
	w := int(b.width)
	h := int(b.height)
	y50 := (b.yMin + b.yMax) / 2.0
	x := (b.xMin + b.xMax) / 2.0
	y := b.y(m.Percent)
	escapedAssessor := html.EscapeString(m.Assessor)
	timeLabel := formatChartTime(m.CreatedAt, true)

	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" class="chart-progress" role="img" aria-label="Progress history">`, w, h, w, h)
	if detailed {
		writePercentAxis(&buf, b)
	} else {
		fmt.Fprintf(&buf, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-dasharray="2 3" stroke-width="1" stroke-opacity="0.3"/>`, b.xMin, y50, b.xMax, y50)
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.5">50%%</text>`, b.xMin-4.0, y50+3.0)
	}
	fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, x, b.height-ChartPadBottom+16.0, timeLabel)
	fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="4" fill="currentColor" data-assessor="%s"><title>%s: %d%%</title></circle>`, x, y, escapedAssessor, escapedAssessor, clampPercent(m.Percent))
	buf.WriteString(`</svg>`)

	return template.HTML(buf.String())
}

// renderFullChart renders the complete SVG with 50% guideline, time labels, and polylines.
func renderFullChart(b chartBounds, seriesList []chartSeries, tStart, tEnd time.Time, detailed bool) template.HTML {
	var buf bytes.Buffer
	w := int(b.width)
	h := int(b.height)
	y50 := (b.yMin + b.yMax) / 2.0
	sameDay := sameCalendarDay(tStart, tEnd)
	startLabel := formatChartTime(tStart, sameDay)
	endLabel := formatChartTime(tEnd, sameDay)

	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" class="chart-progress" role="img" aria-label="Progress history">`, w, h, w, h)

	if detailed {
		// The enlarged chart gets a real percent axis: a labelled gridline
		// every 25 points, so a reader can tell 72% from 61% by eye instead
		// of guessing against a single midline.
		writePercentAxis(&buf, b)
	} else {
		// The inline panel is 600x200 in a narrow column; one guideline is
		// all that fits without the labels colliding.
		fmt.Fprintf(&buf, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-dasharray="2 3" stroke-width="1" stroke-opacity="0.3"/>`, b.xMin, y50, b.xMax, y50)
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.5">50%%</text>`, b.xMin-4.0, y50+3.0)
	}

	// Time labels at first and last points, plus a midpoint tick when there
	// is room for one (detailed only) so the horizontal scale is readable
	// rather than merely bounded.
	if b.durationNan <= 0 {
		midX := (b.xMin + b.xMax) / 2.0
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, midX, b.height-ChartPadBottom+16.0, startLabel)
	} else {
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="start" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, b.xMin, b.height-ChartPadBottom+16.0, startLabel)
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, b.xMax, b.height-ChartPadBottom+16.0, endLabel)
		if detailed {
			writeTimeMidTick(&buf, b, tStart, tEnd)
		}
	}

	// Render each series track.
	for _, s := range seriesList {
		if len(s.Points) == 0 {
			continue
		}
		escapedName := html.EscapeString(s.Name)

		if len(s.Points) == 1 {
			// Single point track for an individual assessor within a multi-mark history.
			pt := s.Points[0]
			cx := b.x(pt.Time)
			cy := b.y(pt.Percent)
			switch {
			case s.IsComposite:
				fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="4" fill="currentColor" data-series="composite"><title>Composite: %d%%</title></circle>`, cx, cy, pt.Percent)
			default:
				fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="3" fill="currentColor" data-assessor="%s"><title>%s: %d%%</title></circle>`, cx, cy, escapedName, escapedName, pt.Percent)
			}
			continue
		}

		var pointsBuf strings.Builder
		for i, pt := range s.Points {
			if i > 0 {
				pointsBuf.WriteByte(' ')
			}
			fmt.Fprintf(&pointsBuf, "%.1f,%.1f", b.x(pt.Time), b.y(pt.Percent))
		}

		switch {
		case s.IsComposite:
			fmt.Fprintf(&buf, `<polyline fill="none" stroke="currentColor" stroke-width="%.1f" points="%s" data-series="composite"><title>Composite Progress</title></polyline>`, s.StrokeWidth, pointsBuf.String())
		default:
			dashAttr := ""
			if s.DashArray != "" && s.DashArray != "none" {
				dashAttr = fmt.Sprintf(` stroke-dasharray="%s"`, s.DashArray)
			}
			fmt.Fprintf(&buf, `<polyline fill="none" stroke="currentColor" stroke-width="%.1f"%s points="%s" data-assessor="%s"><title>%s</title></polyline>`, s.StrokeWidth, dashAttr, pointsBuf.String(), escapedName, escapedName)
		}
	}

	buf.WriteString(`</svg>`)
	return template.HTML(buf.String())
}

// deduplicateSameTimestamp collapses consecutive points sharing the same timestamp,
// keeping the final (latest) point at that instant.
func deduplicateSameTimestamp(points []chartPoint) []chartPoint {
	if len(points) <= 1 {
		return points
	}
	out := make([]chartPoint, 0, len(points))
	for _, pt := range points {
		if len(out) > 0 && out[len(out)-1].Time.Equal(pt.Time) {
			out[len(out)-1] = pt
		} else {
			out = append(out, pt)
		}
	}
	return out
}

// decimatePoints downsamples dense percent sequences for rendering while
// strictly protecting all downward drops and local extrema. On the percent
// chart the bad news is a FALL (readiness revised down), so worseIsHigher is
// false — see decimateKeepMask for the shared rule.
func decimatePoints(points []chartPoint, maxPoints int) []chartPoint {
	if len(points) <= maxPoints || maxPoints < 2 {
		return points
	}
	values := make([]int64, len(points))
	for i, p := range points {
		values[i] = int64(p.Percent)
	}
	keep := decimateKeepMask(values, maxPoints, false)
	var result []chartPoint
	for i := range points {
		if keep[i] {
			result = append(result, points[i])
		}
	}
	return result
}

// decimateKeepMask is the ONE decimation rule both charts obey. It takes the
// series as plain int64 values and answers which indices survive.
//
// Why one function and not one per chart: the two charts disagree about which
// direction is bad news, and that disagreement is exactly ONE comparison (the
// "a step in the bad direction is critical" rule below). Everything else —
// endpoints, local extrema, the scoring when criticals alone overflow the
// budget, the even spread of whatever budget is left — is identical. Two
// copies would drift, and the drift would be invisible: both charts would
// still render, just one of them would have quietly started throwing away the
// points the owner looks at the chart to see.
//
// worseIsHigher says which way is bad news:
//   - percent chart: readiness revised DOWN is the bad news -> false
//   - forecast chart: the promised date sliding LATER is the bad news -> true
//
// The owner's rule this protects: the drop from 91% to 72% (and the forecast
// that slid from "in two weeks" to "in three months") is the main event of the
// chart, never noise. A naive "every Nth point" would throw away precisely
// that.
func decimateKeepMask(values []int64, maxPoints int, worseIsHigher bool) []bool {
	n := len(values)
	keep := make([]bool, n)
	if n <= maxPoints || maxPoints < 2 {
		for i := range keep {
			keep[i] = true
		}
		return keep
	}

	// worse reports whether the step from a to b is the bad direction.
	worse := func(a, b int64) bool {
		if worseIsHigher {
			return b > a
		}
		return b < a
	}

	// Mark critical points that MUST NOT be dropped:
	// 1. Endpoints (start and finish).
	// 2. Any step in the bad direction: BOTH the point before it (the peak the
	//    series fell from / the date it slid from) and the point after it.
	// 3. Local minima and local maxima.
	isCritical := make([]bool, n)
	isCritical[0] = true
	isCritical[n-1] = true

	for i := 1; i < n; i++ {
		if worse(values[i-1], values[i]) {
			isCritical[i-1] = true
			isCritical[i] = true
		}
	}

	for i := 1; i < n-1; i++ {
		prev, curr, next := values[i-1], values[i], values[i+1]
		if (curr <= prev && curr < next) || (curr < prev && curr <= next) {
			isCritical[i] = true
		}
		if (curr >= prev && curr > next) || (curr > prev && curr >= next) {
			isCritical[i] = true
		}
	}

	criticalCount := 0
	for _, c := range isCritical {
		if c {
			criticalCount++
		}
	}

	// If the critical points alone overflow the budget, ranking them by how
	// sharp each one is LOCALLY throws the worst news away. Measured on a
	// history of 400 forecasts with early churn and a late slide: the slide to
	// 90 days did not survive at all and the reader saw a ceiling of 40 days,
	// while the percent chart drew a floor of 20% over data that reached 10%.
	//
	// Three things conspired. A local score (this step plus the next) rates a
	// long gentle slide below a short violent wobble, even when the slide is the
	// larger move overall. sort.SliceStable on equal scores keeps the earlier
	// index, so survival drifts systematically toward the start of the history.
	// And nothing outside the critical set survived at all, so whole stretches
	// of the timeline went unrepresented.
	//
	// The replacement is envelope decimation: cut the timeline into equal
	// buckets and keep each bucket's highest and lowest value. That preserves
	// the outline of the series rather than its sharpest corners, and — the
	// property that matters here — the global extreme of EITHER side is the
	// extreme of its own bucket, so it always survives. The drawn range can
	// therefore never be narrower than the data's, which is exactly the promise
	// the owner cares about: the fall from 91% to 72%, and the promise that
	// slid from two weeks to three months, stay on the chart.
	//
	// Polarity plays no part here: keeping both ends of the envelope protects
	// the bad side whichever way it points.
	if criticalCount > maxPoints {
		keep[0] = true
		keep[n-1] = true

		// Each bucket spends at most two of the remaining budget (its min and
		// its max), so the number of buckets is half of what is left.
		buckets := (maxPoints - 2) / 2
		if buckets < 1 {
			buckets = 1
		}
		for b := 0; b < buckets; b++ {
			lo := 1 + (n-2)*b/buckets
			hi := 1 + (n-2)*(b+1)/buckets
			if hi > n-1 {
				hi = n - 1
			}
			if lo >= hi {
				continue
			}
			minIdx, maxIdx := lo, lo
			for i := lo; i < hi; i++ {
				if values[i] < values[minIdx] {
					minIdx = i
				}
				if values[i] > values[maxIdx] {
					maxIdx = i
				}
			}
			keep[minIdx] = true
			keep[maxIdx] = true
		}

		// Belt and braces: pin the series-wide extremes outright. The bucket
		// pass already reaches them, and saying so here means a later change to
		// the bucketing cannot quietly drop the one guarantee this branch is
		// for.
		gMin, gMax := 0, 0
		for i := 1; i < n; i++ {
			if values[i] < values[gMin] {
				gMin = i
			}
			if values[i] > values[gMax] {
				gMax = i
			}
		}
		keep[gMin] = true
		keep[gMax] = true
		return keep
	}

	// Critical points fit in budget: spread the rest across non-critical points.
	remainingBudget := maxPoints - criticalCount
	nonCriticalIndices := make([]int, 0, n-criticalCount)
	for i := 0; i < n; i++ {
		if !isCritical[i] {
			nonCriticalIndices = append(nonCriticalIndices, i)
		}
	}
	for i := 0; i < n; i++ {
		if isCritical[i] {
			keep[i] = true
		}
	}
	if remainingBudget > 0 && len(nonCriticalIndices) > 0 {
		step := float64(len(nonCriticalIndices)) / float64(remainingBudget)
		for j := 0; j < remainingBudget; j++ {
			keep[nonCriticalIndices[int(float64(j)*step)]] = true
		}
	}
	return keep
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func formatChartTime(t time.Time, sameDay bool) string {
	t = t.Local()
	if sameDay {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// sameCalendarDay reports whether two instants fall on the same calendar day
// for the person reading the page, which is the only sense in which "today"
// means anything here.
//
// Both sides are converted to the SAME zone first, and the zone is the
// server's local one because that is what formatChartTime prints. Getting
// this wrong is not academic: every CreatedAt is forced to UTC by the store
// (internal/store/scan.go), while the clock the callers pass comes from
// Deps.Now, which defaults to time.Now — local. Comparing one against the
// other straddles two zones, so around local midnight a message posted five
// minutes ago rendered with a full date ("yesterday" to the reader), and one
// posted late in the local evening rendered as a bare clock time that was
// hours behind the one the author wrote it by.
func sameCalendarDay(a, b time.Time) bool {
	a, b = a.Local(), b.Local()
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}
