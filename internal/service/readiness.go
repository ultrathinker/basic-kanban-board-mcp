package service

import (
	"fmt"
	"math"
)

// ---------------------------------------------------------------------------
// Estimate-weighted readiness (KANB-35)
// ---------------------------------------------------------------------------
//
// Next to what assessors believe, the board needs a figure derived from the
// board itself. The one it had — the share of cards sitting in done columns —
// weighs a two-line typo fix the same as a month of work.
//
//	P = 100 x (estimates of finished working tasks) / (estimates of all working tasks)
//
// WORKING TASKS ARE THE UNARCHIVED LEAVES of the tree. A parent with no
// children is itself a leaf. An umbrella card is NOT added on top of its
// children's estimates — doing so counts the same work twice and makes the
// denominator grow every time someone groups existing cards.
//
// COVERAGE IS PART OF THE ANSWER, not a footnote. When only some leaves carry
// an estimate the result says "8 of 12 estimated" and marks itself partial.
// Unestimated leaves are left OUT of both sides rather than counted as zero:
// a zero denominator-side contribution makes an unsized new feature silently
// free, which is precisely the case where the number most needs to move.
//
// A ZERO DENOMINATOR IS "NO DATA", never 100%. 0/0 is not "everything is
// finished", and a progress bar that fills up when nobody has estimated
// anything is worse than an empty one.
//
// THE UNIT IS A PROJECT UNIT OF EFFORT, whatever the project decided it is.
// Hours of effort are NOT hours until completion, and nothing here converts
// between them.

// ReadinessBasis says what a readiness figure is actually made of. It is
// returned rather than inferred so no renderer has to guess whether a number
// weighs effort or just counts cards.
type ReadinessBasis string

const (
	// ReadinessEstimates is the real thing: estimate-weighted.
	ReadinessEstimates ReadinessBasis = "estimates"
	// ReadinessTaskCount is the fallback for a board with no estimates at
	// all — the share of finished leaves. It is a legitimate number and it
	// is NOT a calculation by estimates; it carries its own label so it can
	// never be presented as one.
	ReadinessTaskCount ReadinessBasis = "task_count"
	// ReadinessNone means there is nothing to divide: no working tasks, or
	// estimates that sum to zero. Percent is nil.
	ReadinessNone ReadinessBasis = "none"
)

// HistoricalReadinessNote is the single line a surface MUST print beside any
// historical readiness curve. It is a constant here, on the result, rather
// than prose left to each renderer, because without it the early points look
// like a bug: they are computed from the estimates that were current back
// then, so they disagree with the same period recomputed from today's
// numbers, and someone will file that disagreement as a defect and "fix" it
// by recomputing the past — which is the one thing the journal exists to
// prevent.
const HistoricalReadinessNote = "Historical points use the estimates that were current at the time, not today's — early points will not match a figure recomputed from current estimates."

// EstimateReadiness is the estimate-weighted readiness of a project at one
// instant, together with everything needed to read it honestly.
type EstimateReadiness struct {
	// Basis says what the figure is made of; check it before showing Percent.
	Basis ReadinessBasis
	// Percent is nil when Basis is ReadinessNone. It is never a stand-in for
	// "no data".
	Percent *int

	// Leaves is the working set: unarchived cards with no unarchived
	// children. DoneLeaves is how many of those are in a done column.
	Leaves     int
	DoneLeaves int
	// LeavesEstimated is how many working tasks carry an estimate at all.
	LeavesEstimated int
	// Partial is true when the figure is estimate-weighted but some working
	// tasks have no estimate, so the number describes only part of the work.
	Partial bool
	// Coverage is the ready-to-show phrasing of the two numbers above, e.g.
	// "8 of 12 estimated".
	Coverage string

	// EstimateDone / EstimateTotal are the two sides of the fraction, in the
	// project's own unit. They sum ESTIMATED leaves only.
	EstimateDone  float64
	EstimateTotal float64
	// Unit is the project's estimate unit. It is a unit of EFFORT: it may not
	// be read as time remaining.
	Unit string

	// Label names the basis in words, for a caption: "by estimates", "by task
	// count" or "no data".
	Label string
	// HistoricalNote is HistoricalReadinessNote, carried on the result so a
	// renderer showing a curve has the required sentence in hand and cannot
	// draw the curve without it.
	HistoricalNote string
}

// readinessFrom computes the readiness of one already-counted board. It takes
// boardCounts, which means the live figure and every point of the historical
// curve go through this exact function — the live number and the last point
// of the curve cannot say different things.
func readinessFrom(c boardCounts, unit string) EstimateReadiness {
	r := EstimateReadiness{
		Leaves:          c.Leaves,
		DoneLeaves:      c.DoneLeaves,
		LeavesEstimated: c.LeavesEstimated,
		EstimateDone:    c.EstimateDone,
		EstimateTotal:   c.EstimateTotal,
		Unit:            unit,
		HistoricalNote:  HistoricalReadinessNote,
	}
	switch {
	case c.LeavesEstimated > 0 && c.EstimateTotal > 0:
		r.Basis = ReadinessEstimates
		p := percentOfEffort(c.EstimateDone, c.EstimateTotal)
		r.Percent = &p
		r.Partial = c.LeavesEstimated < c.Leaves
		r.Label = "by estimates"
	case c.LeavesEstimated > 0:
		// Estimates exist but add up to nothing. 0/0 is not 100%.
		r.Basis = ReadinessNone
		r.Label = "no data"
	case c.Leaves > 0:
		// Nothing is estimated anywhere. Counting cards is allowed here, and
		// it is labelled as counting cards.
		r.Basis = ReadinessTaskCount
		p := RoundMeanHalfUp(c.DoneLeaves*100, c.Leaves)
		r.Percent = &p
		r.Label = "by task count"
	default:
		r.Basis = ReadinessNone
		r.Label = "no data"
	}
	r.Coverage = coveragePhrase(r.Basis, c.LeavesEstimated, c.Leaves)
	return r
}

// percentOfEffort rounds half up, matching how every other percentage in the
// product rounds, and clamps to 0..100 so a pathological estimate cannot put
// a progress bar past its own end.
func percentOfEffort(done, total float64) int {
	if total <= 0 {
		return 0
	}
	p := int(math.Floor(done/total*100 + 0.5))
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p
}

func coveragePhrase(basis ReadinessBasis, estimated, leaves int) string {
	switch basis {
	case ReadinessTaskCount:
		return fmt.Sprintf("%d working tasks, none estimated", leaves)
	case ReadinessNone:
		if leaves == 0 {
			return "no working tasks"
		}
		return fmt.Sprintf("%d of %d estimated", estimated, leaves)
	default:
		return fmt.Sprintf("%d of %d estimated", estimated, leaves)
	}
}
