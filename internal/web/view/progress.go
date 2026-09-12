package view

import (
	"fmt"
	"strconv"
)

// ProgressSquares is the bar's only size: ten cells, painted left to right.
const ProgressSquares = 10

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
}

// NewAssessedProgress builds the bar for a marks-derived metric (a task's
// summary progress, the project's manual progress). A nil percent means no
// assessment exists and yields a nil view: nothing is rendered.
func NewAssessedProgress(percent *int, assessors int) *ProgressView {
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

// squaresFilled maps an already-aggregated percent onto the ten-square bar:
// floor division, so 45% paints 4 squares and 100% paints all 10. The clamp
// is defensive — the schema CHECK already holds percent inside 0..100 — but
// an out-of-range value must not paint a negative or overflowing number of
// squares on a rendered page.
func squaresFilled(percent int) int {
	return clampPercent(percent) / 10
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
