package view

import (
	"fmt"
	"strconv"
	"time"
)

// ProgressSquares is the bar's only size: twenty cells, painted left to
// right. Twenty, not ten, because the owner wanted a finer reading; the
// cells are narrower than they are tall (app.css) so twice the count does
// not make the bar twice as long.
const ProgressSquares = 20

// ProgressView is one rendered progress bar: ten square cells plus the label
// printed next to them.
//
// A nil *ProgressView means "nobody measured this", and the templates render
// NOTHING in that case — not an empty bar. A row of unfilled squares claims
// "the work has not started"; the truth may be "no one has assessed it".
// Those are different facts and only one of them is true.
//
// This type draws; it does not compute. Percent arrives already aggregated by
// the service (internal/service/progress.go owns every average and every
// rounding of marks). The one number derived here is the painted-square count,
// which is the bar's own presentation rule — how a finished percent maps onto
// ten squares — not a second opinion about the percent itself.
type ProgressView struct {
	// Percent is the aggregated value as the service reported it.
	Percent int
	// Filled is the painted square count: floor(percent/10). 45% paints 4
	// squares. There is no partial fill — the owner decided that explicitly.
	Filled int
	// Cells carries the painted flags so the template can range and paint
	// without doing any arithmetic of its own: exactly ProgressSquares
	// entries, Filled of them true, in paint order.
	Cells []bool
	// Assessors is how many assessor tracks were folded into the percent.
	Assessors int
	// Label is the text beside the bar, e.g. "45% · 3 assessments" — the
	// percent plus how many tracks it took to produce it, so one agent's
	// opinion never masquerades as a consensus.
	Label string
	// Forecast is the freshest, most pessimistic standing finish-date
	// promise among this metric's assessors, and who gave it. Nil means
	// nobody currently has one: the template renders nothing at all for it —
	// no empty slot, no "not set" placeholder, the same rule the bar itself
	// follows for a missing assessment.
	Forecast *ForecastView

	// ProjectKey and TaskKey identify the scope this metric was computed
	// over, so the delete-track control knows what to post back. TaskKey is
	// empty for the project-level (manual) metric. Both are empty (and
	// Tracks nil) for the automatic done-share bar, which is not built from
	// marks and has nothing a track-delete could remove.
	ProjectKey string
	TaskKey    string
	// Tracks is the per-assessor breakdown behind Percent — one row per
	// assessor, each with its own delete control. Nil when there is no
	// breakdown to show (the automatic bar, or a caller that never attached
	// one).
	Tracks []ProgressTrack
}

// ForecastView is the "who promised what, when" badge shown next to a
// summary progress metric (a task card, the project header's "assessed"
// figure). It is built from the service's own pick-the-latest aggregation
// (service.TaskProgressItem.ForecastETA/ForecastBy,
// service.ProjectProgressResult.ManualForecastETA/ManualForecastBy) — this
// package only formats and decides "overdue", it never re-derives which
// forecast is the current one.
type ForecastView struct {
	// Text is the absolute finish date-time, formatted with the same
	// vocabulary chart.go's own timestamps already use (formatChartTime),
	// rather than inventing a second one. Always the long form
	// ("2006-01-02 15:04"): a forecast can name any day, not just "today
	// relative to the other end of this chart", so there is no safe
	// same-day compression to borrow. The owner decided the value shown is
	// an absolute date-time, never a relative "N hours left".
	Text string
	// By is the assessor whose forecast this is — guaranteed to be the one
	// that produced ETA, never a different assessor's name.
	By string
	// Overdue is true when the forecast date has already passed at render
	// time. The template renders this as an expired promise using weight
	// and a marker glyph (reusing .mark, the board's one existing "cannot
	// miss this" signal) — never colour, since the portal is black and
	// white outside the chat feed.
	Overdue bool
}

// ProgressTrack is one assessor's row in the delete-track control: their
// latest percent (so the row still reads as a metric on its own) and how
// many marks make up their whole track (the confirmation's point count).
type ProgressTrack struct {
	Assessor string
	Percent  int
	Count    int
}

// WithTracks attaches the scope identifiers and per-assessor breakdown the
// delete-track control needs. It is separate from the constructors because
// that data is not known at the point a bare percent/assessors pair is
// turned into a bar — the caller (attachProgress, or the delete handler
// rebuilding one bar after a delete) fetches it once it also has the scope's
// keys in hand. Safe to call on a nil receiver (no assessment => no bar =>
// nothing to attach anything to) so callers can chain it unconditionally.
func (v *ProgressView) WithTracks(projectKey, taskKey string, tracks []ProgressTrack) *ProgressView {
	if v == nil {
		return nil
	}
	v.ProjectKey = projectKey
	v.TaskKey = taskKey
	v.Tracks = tracks
	return v
}

// NewAssessedProgress builds the bar for a marks-derived metric (a task's
// summary progress, the project's manual progress). A nil percent means no
// assessment exists and yields a nil view: nothing is rendered.
//
// forecastETA/forecastBy are the scope's already-picked "freshest, most
// pessimistic" forecast (see service.TaskProgressItem / ProjectProgressResult):
// this constructor only formats it and decides "overdue" against the wall
// clock, it does not re-derive which forecast wins among several. A nil
// forecastETA renders no Forecast at all, matching the "no forecasts, no
// placeholder" rule.
func NewAssessedProgress(percent *int, assessors int, forecastETA *time.Time, forecastBy string) *ProgressView {
	if percent == nil {
		return nil
	}
	v := &ProgressView{
		Percent:   clampPercent(*percent),
		Assessors: assessors,
	}
	v.Filled = squaresFilled(v.Percent)
	v.Cells = progressCells(v.Filled)
	v.Label = fmt.Sprintf("%d%% · %s", v.Percent, plural(assessors, "assessment"))
	if forecastETA != nil {
		v.Forecast = &ForecastView{
			Text:    formatChartTime(*forecastETA, false),
			By:      forecastBy,
			Overdue: forecastETA.Before(time.Now().UTC()),
		}
	}
	return v
}

// NewDoneShareProgress builds the bar for the board-derived metric: the share
// of the project's tasks sitting in done columns. A nil percent means the
// project has no tasks — nothing is measured, so nothing is rendered (0%
// would claim the work has not started, which no one has said).
func NewDoneShareProgress(percent *int, done, total int) *ProgressView {
	if percent == nil {
		return nil
	}
	v := &ProgressView{
		Percent: clampPercent(*percent),
	}
	v.Filled = squaresFilled(v.Percent)
	v.Cells = progressCells(v.Filled)
	v.Label = fmt.Sprintf("%d%% · %d/%d tasks", v.Percent, done, total)
	return v
}

// Clickable reports whether this metric has a mark history behind it worth
// charting — true only for a marks-derived (assessed) metric, never for the
// automatic done-share bar. It is not a new signal: WithTracks is the only
// writer of ProjectKey, and it is only ever called on the assessed
// constructor's output (a nil percent there already yields a nil view, so a
// non-nil, WithTracks'd view always has at least one mark behind it). The
// bar's own template calls this, rather than testing ProjectKey directly, so
// that fact stays documented in one place instead of being tribal knowledge
// the template silently depends on.
//
// A nil receiver is not clickable, matching every other method here that is
// safe to call on "no bar at all".
func (v *ProgressView) Clickable() bool {
	return v != nil && v.ProjectKey != ""
}

// squaresFilled maps an already-aggregated percent onto the bar: floor
// division by the step one cell is worth, so with twenty cells 45% paints 9
// and 100% paints all 20. There is no partial fill, and at a 5% step the
// rounding it would have smoothed is half what it was. The clamp is
// defensive — the schema CHECK already holds percent inside 0..100 — but an
// out-of-range value must not paint a negative or overflowing number of
// cells on a rendered page.
func squaresFilled(percent int) int {
	return clampPercent(percent) * ProgressSquares / 100
}

// progressCells builds the template's paint plan: exactly ProgressSquares
// entries, the first filled true.
func progressCells(filled int) []bool {
	if filled < 0 {
		filled = 0
	}
	if filled > ProgressSquares {
		filled = ProgressSquares
	}
	cells := make([]bool, ProgressSquares)
	for i := 0; i < filled; i++ {
		cells[i] = true
	}
	return cells
}

func clampPercent(p int) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// plural renders "1 assessment" / "3 assessments" for the bar labels.
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
