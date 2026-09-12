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
// history over time for each assessor and the composite progress metric.
// If history is empty, it returns empty HTML ("").
func RenderProgressChart(history []domain.ProgressMark, width, height int) template.HTML {
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
		return renderSinglePointChart(marks[0], bounds)
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
			DashArray:   chartDashPatterns[i%len(chartDashPatterns)],
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

	return renderFullChart(bounds, seriesList, tStart, tEnd)
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
func renderSinglePointChart(m domain.ProgressMark, b chartBounds) template.HTML {
	var buf bytes.Buffer
	w := int(b.width)
	h := int(b.height)
	y50 := (b.yMin + b.yMax) / 2.0
	x := (b.xMin + b.xMax) / 2.0
	y := b.y(m.Percent)
	escapedAssessor := html.EscapeString(m.Assessor)
	timeLabel := formatChartTime(m.CreatedAt, true)

	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" class="chart-progress" role="img" aria-label="Progress history">`, w, h, w, h)
	fmt.Fprintf(&buf, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-dasharray="2 3" stroke-width="1" stroke-opacity="0.3"/>`, b.xMin, y50, b.xMax, y50)
	fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.5">50%%</text>`, b.xMin-4.0, y50+3.0)
	fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, x, b.height-ChartPadBottom+16.0, timeLabel)
	fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="4" fill="currentColor" data-assessor="%s"><title>%s: %d%%</title></circle>`, x, y, escapedAssessor, escapedAssessor, clampPercent(m.Percent))
	buf.WriteString(`</svg>`)

	return template.HTML(buf.String())
}

// renderFullChart renders the complete SVG with 50% guideline, time labels, and polylines.
func renderFullChart(b chartBounds, seriesList []chartSeries, tStart, tEnd time.Time) template.HTML {
	var buf bytes.Buffer
	w := int(b.width)
	h := int(b.height)
	y50 := (b.yMin + b.yMax) / 2.0
	sameDay := tStart.Year() == tEnd.Year() && tStart.YearDay() == tEnd.YearDay()
	startLabel := formatChartTime(tStart, sameDay)
	endLabel := formatChartTime(tEnd, sameDay)

	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" class="chart-progress" role="img" aria-label="Progress history">`, w, h, w, h)

	// Minimal horizontal 50% guideline and label.
	fmt.Fprintf(&buf, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-dasharray="2 3" stroke-width="1" stroke-opacity="0.3"/>`, b.xMin, y50, b.xMax, y50)
	fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.5">50%%</text>`, b.xMin-4.0, y50+3.0)

	// Minimal time labels at first and last points.
	if b.durationNan <= 0 {
		midX := (b.xMin + b.xMax) / 2.0
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, midX, b.height-ChartPadBottom+16.0, startLabel)
	} else {
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="start" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, b.xMin, b.height-ChartPadBottom+16.0, startLabel)
		fmt.Fprintf(&buf, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.6">%s</text>`, b.xMax, b.height-ChartPadBottom+16.0, endLabel)
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
			if s.IsComposite {
				fmt.Fprintf(&buf, `<circle cx="%.1f" cy="%.1f" r="4" fill="currentColor" data-series="composite"><title>Composite: %d%%</title></circle>`, cx, cy, pt.Percent)
			} else {
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

		if s.IsComposite {
			fmt.Fprintf(&buf, `<polyline fill="none" stroke="currentColor" stroke-width="%.1f" points="%s" data-series="composite"><title>Composite Progress</title></polyline>`, s.StrokeWidth, pointsBuf.String())
		} else {
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

// decimatePoints downsamples dense point sequences for rendering while strictly
// protecting all downward drops and local minima.
func decimatePoints(points []chartPoint, maxPoints int) []chartPoint {
	n := len(points)
	if n <= maxPoints || maxPoints < 2 {
		return points
	}

	// Mark critical points that MUST NOT be dropped:
	// 1. Endpoints (start and finish).
	// 2. Any drop: if points[i].Percent < points[i-1].Percent, both points[i-1] (peak)
	//    and points[i] (valley/trough) are strictly critical.
	// 3. Local minima: points where progress fell and now levels off or rises.
	// 4. Local maxima: points where progress rose and now levels off or falls.
	isCritical := make([]bool, n)
	isCritical[0] = true
	isCritical[n-1] = true

	for i := 1; i < n; i++ {
		if points[i].Percent < points[i-1].Percent {
			isCritical[i-1] = true
			isCritical[i] = true
		}
	}

	for i := 1; i < n-1; i++ {
		prev := points[i-1].Percent
		curr := points[i].Percent
		next := points[i+1].Percent
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

	// If critical points alone exceed maxPoints, prioritize largest drops and extrema.
	if criticalCount > maxPoints {
		type scoredIndex struct {
			idx   int
			score int
		}
		var scored []scoredIndex
		for i := 1; i < n-1; i++ {
			if isCritical[i] {
				prevDiff := absInt(points[i].Percent - points[i-1].Percent)
				nextDiff := 0
				if i+1 < n {
					nextDiff = absInt(points[i].Percent - points[i+1].Percent)
				}
				scored = append(scored, scoredIndex{idx: i, score: prevDiff + nextDiff})
			}
		}
		sort.SliceStable(scored, func(i, j int) bool {
			return scored[i].score > scored[j].score
		})

		keep := make([]bool, n)
		keep[0] = true
		keep[n-1] = true
		budget := maxPoints - 2
		if budget > len(scored) {
			budget = len(scored)
		}
		for i := 0; i < budget; i++ {
			keep[scored[i].idx] = true
		}

		var result []chartPoint
		for i := 0; i < n; i++ {
			if keep[i] {
				result = append(result, points[i])
			}
		}
		return result
	}

	// Critical points fit in budget: distribute remaining budget across non-critical points.
	remainingBudget := maxPoints - criticalCount
	nonCriticalIndices := make([]int, 0, n-criticalCount)
	for i := 0; i < n; i++ {
		if !isCritical[i] {
			nonCriticalIndices = append(nonCriticalIndices, i)
		}
	}

	keep := make([]bool, n)
	for i := 0; i < n; i++ {
		if isCritical[i] {
			keep[i] = true
		}
	}

	if remainingBudget > 0 && len(nonCriticalIndices) > 0 {
		step := float64(len(nonCriticalIndices)) / float64(remainingBudget)
		for j := 0; j < remainingBudget; j++ {
			idx := nonCriticalIndices[int(float64(j)*step)]
			keep[idx] = true
		}
	}

	var result []chartPoint
	for i := 0; i < n; i++ {
		if keep[i] {
			result = append(result, points[i])
		}
	}
	return result
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func formatChartTime(t time.Time, sameDay bool) string {
	if sameDay {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}
