package service

import (
	"sort"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// Replaying the lifecycle journal (KANB-31)
// ---------------------------------------------------------------------------
//
// ONE DEFINITION, NOT TWO. A board that charts its own history grows two ways
// of answering the same question: the live query ("how many are open now")
// and the replay ("how many were open then"). Left alone they drift, and the
// drift shows up in no test — it shows up as a chart that disagrees with the
// board next to it, months after the change that caused it.
//
// The defence here is structural, in three parts:
//
//  1. Both answers are the SAME TYPE, boardCounts, with the same field
//     meanings. There is no "historical total" separate from "the total".
//  2. Both are reduced to that type by the SAME arithmetic — countBoard —
//     which is handed a plain list of card states and does not know or care
//     whether they came from a SELECT or from replaying a journal.
//  3. TestReplayMatchesTheLiveBoard drives a board through every kind of
//     change the journal records and then asserts, field by field, that
//     replaying to "now" equals the live query. Divergence is a red build.
//
// The live path stays a fast query: replaying the whole journal on every
// board render would be absurd. What is fixed is that the two agree, not that
// there is only one of them.

// boardCounts is everything the charts are built from, at one instant. It is
// produced identically by the live query and by a replay.
type boardCounts struct {
	// TotalTasks is every live (unarchived) card. OpenTasks and DoneTasks
	// partition it: OpenTasks == TotalTasks - DoneTasks, always.
	TotalTasks int
	OpenTasks  int
	DoneTasks  int

	// Leaves is the working set for the estimate rollup: live cards with no
	// live children. A parent with no children IS a leaf; an umbrella whose
	// children are all archived becomes one.
	Leaves int
	// DoneLeaves is how many of those sit in a done-kind column.
	DoneLeaves int
	// LeavesEstimated is how many leaves carry an estimate at all — the
	// coverage figure, without which a percentage over a partly-estimated
	// board is a number nobody can calibrate.
	LeavesEstimated int
	// EstimateTotal / EstimateDone sum the estimates of ESTIMATED leaves
	// only. Unestimated leaves are not counted as zero: that would make an
	// unsized new feature silently free.
	EstimateTotal float64
	EstimateDone  float64
}

// cardState is one card reduced to the fields that decide every count. Both
// sources build these, and nothing downstream can tell them apart.
type cardState struct {
	id       string
	parent   *string
	done     bool
	estimate *float64
}

// countBoard is the single piece of arithmetic behind every number on every
// chart. Give it the live cards and it answers for now; give it the cards a
// replay reconstructed and it answers for then.
func countBoard(cards []cardState) boardCounts {
	var c boardCounts
	hasChild := make(map[string]bool, len(cards))
	for _, card := range cards {
		if card.parent != nil {
			hasChild[*card.parent] = true
		}
	}
	for _, card := range cards {
		c.TotalTasks++
		if card.done {
			c.DoneTasks++
		}
		if hasChild[card.id] {
			continue
		}
		c.Leaves++
		if card.done {
			c.DoneLeaves++
		}
		if card.estimate == nil {
			continue
		}
		c.LeavesEstimated++
		c.EstimateTotal += *card.estimate
		if card.done {
			c.EstimateDone += *card.estimate
		}
	}
	c.OpenTasks = c.TotalTasks - c.DoneTasks
	return c
}

// liveBoardCounts is the fast path: one filtered SELECT plus the project's
// columns. It is what the board itself renders from, and what a replay to
// "now" has to agree with.
//
// Archived cards are excluded on every side, which is what makes archival
// behave the way the board already does: total always drops by one, and the
// bucket the card was actually IN drops by one with it — open for an
// unfinished card, done for a finished one. Nothing is ever added to done by
// archiving, and open == total - done survives both cases by construction.
func liveBoardCounts(s *svc, tx store.Tx, projectID string) (boardCounts, error) {
	cols, err := s.store.Columns().ListByProject(tx, projectID)
	if err != nil {
		return boardCounts{}, err
	}
	doneCols := make(map[string]bool, len(cols))
	for _, c := range cols {
		if c.Kind == domain.KindDone {
			doneCols[c.ID] = true
		}
	}
	// IncludeDone is required: the filter's default hides done columns, which
	// is a display rule and would make this metric count nothing.
	tasks, err := s.store.Tasks().List(tx, store.TaskFilter{
		ProjectIDs:  []string{projectID},
		IncludeDone: true,
	})
	if err != nil {
		return boardCounts{}, err
	}
	cards := make([]cardState, 0, len(tasks))
	for _, t := range tasks {
		cards = append(cards, cardState{
			id:       t.ID,
			parent:   t.ParentID,
			done:     doneCols[t.ColumnID],
			estimate: t.Estimate,
		})
	}
	return countBoard(cards), nil
}

// ---------------------------------------------------------------------------
// The replay
// ---------------------------------------------------------------------------

// replayCard is one card's reconstructed state while walking the journal.
type replayCard struct {
	archived bool
	columnID string
	estimate *float64
	parent   *string
}

// replayWalker is the journal reader. It is a running state rather than a
// function over the whole list because the curve needs the board at EVERY
// recorded instant: re-reading the journal from the top for each point would
// turn one chart into a quadratic amount of work over a table that is never
// pruned.
//
// Column kinds are tracked as their own little state machine. Every entry
// that names a column also carries what that column MEANT at the time, so a
// card finished by a column changing kind under it (rather than by moving)
// lands in the same bucket the live board puts it in.
type replayWalker struct {
	cards   map[string]*replayCard
	colKind map[string]domain.Kind
}

func newReplayWalker() *replayWalker {
	return &replayWalker{cards: map[string]*replayCard{}, colKind: map[string]domain.Kind{}}
}

func (w *replayWalker) noteColumn(id *string, kind *domain.Kind) {
	if id != nil && kind != nil {
		w.colKind[*id] = *kind
	}
}

func (w *replayWalker) apply(e store.TaskHistoryEntry) {
	w.noteColumn(e.FromColumn, e.FromKind)
	w.noteColumn(e.ToColumn, e.ToKind)
	if e.Kind == store.HistoryColumnKind {
		return
	}
	id := *e.TaskID
	switch e.Kind {
	case store.HistoryExists, store.HistoryCreated:
		c := &replayCard{estimate: e.NewEstimate, parent: e.NewParent}
		if e.ToColumn != nil {
			c.columnID = *e.ToColumn
		}
		if e.Archived != nil {
			c.archived = *e.Archived
		}
		w.cards[id] = c
		return
	}
	c := w.cards[id]
	if c == nil {
		// An entry about a card whose creation is not in the journal. That
		// cannot happen for a journal read whole — the baseline seeds every
		// card that predates it — so skipping is the honest response to a
		// truncated read rather than inventing a card.
		return
	}
	switch e.Kind {
	case store.HistoryMoved:
		if e.ToColumn != nil {
			c.columnID = *e.ToColumn
		}
	case store.HistoryArchived:
		c.archived = true
	case store.HistoryRestored:
		c.archived = false
	case store.HistoryEstimate:
		c.estimate = e.NewEstimate
	case store.HistoryParent:
		c.parent = e.NewParent
	}
}

// counts reduces the reconstructed board through the very same arithmetic the
// live query uses. Archived cards drop out entirely, which is what makes
// archival behave the way the board already does: total always falls by one,
// and the bucket the card was actually IN falls by one with it — open for an
// unfinished card, done for a finished one. Archiving never adds to done, and
// open == total - done survives both cases by construction.
func (w *replayWalker) counts() boardCounts {
	live := make([]cardState, 0, len(w.cards))
	for id, c := range w.cards {
		if c.archived {
			continue
		}
		live = append(live, cardState{
			id:       id,
			parent:   c.parent,
			done:     w.colKind[c.columnID] == domain.KindDone,
			estimate: c.estimate,
		})
	}
	// countBoard is order-independent, but sorting keeps any future debug
	// output stable rather than reshuffled by map iteration.
	sort.Slice(live, func(i, j int) bool { return live[i].id < live[j].id })
	return countBoard(live)
}

// replayJournal returns the board as it stood after the last entry with
// ID <= upToID. An upToID of 0 or less means "the whole journal", i.e. now.
//
// The cutoff is an entry ID rather than a timestamp because the database
// clock has millisecond resolution: two changes inside the same millisecond
// are ordered by the journal, not by their timestamps, and a replay that
// could not tell them apart would be non-deterministic exactly where a test
// needs it to be exact. replayAt layers the by-time question on top.
func replayJournal(entries []store.TaskHistoryEntry, upToID int64) boardCounts {
	w := newReplayWalker()
	for _, e := range entries {
		if upToID > 0 && e.ID > upToID {
			break
		}
		w.apply(e)
	}
	return w.counts()
}

// replayAt answers for an instant: the board as it stood after the last entry
// timestamped at or before `at`. A zero time means now.
func replayAt(entries []store.TaskHistoryEntry, at time.Time) boardCounts {
	if at.IsZero() {
		return replayJournal(entries, 0)
	}
	var cutoff int64
	for _, e := range entries {
		if e.TS.After(at) {
			break
		}
		cutoff = e.ID
	}
	if cutoff == 0 {
		// Nothing had happened yet. An empty board, not "no answer": the
		// journal genuinely says there were no cards.
		return boardCounts{}
	}
	return replayJournal(entries, cutoff)
}

// HistoryPoint is the replayed state of a project at one instant — every
// number the history charts are drawn from. Points appear only where the
// journal actually recorded something.
type HistoryPoint struct {
	At time.Time
	// TotalTasks, OpenTasks and DoneTasks are the item counts. OpenTasks is
	// always TotalTasks - DoneTasks.
	TotalTasks int
	OpenTasks  int
	DoneTasks  int
	// Leaves / DoneLeaves / LeavesEstimated describe the working set the
	// estimate rollup is computed over: live cards with no live children.
	Leaves          int
	DoneLeaves      int
	LeavesEstimated int
	// EstimateTotal and EstimateDone sum the estimates of estimated leaves
	// AS THEY STOOD AT THIS INSTANT. Editing an estimate today does not move
	// a point drawn for last month — that is the whole reason the journal
	// carries the old value alongside the new one.
	EstimateTotal float64
	EstimateDone  float64
	// Readiness is the estimate-weighted readiness at this instant, computed
	// from the estimates that were current THEN (KANB-35). It carries its own
	// basis, coverage and the sentence a renderer must print beside the curve.
	Readiness EstimateReadiness
}

func pointAt(at time.Time, c boardCounts, unit string) HistoryPoint {
	return HistoryPoint{
		At:              at,
		Readiness:       readinessFrom(c, unit),
		TotalTasks:      c.TotalTasks,
		OpenTasks:       c.OpenTasks,
		DoneTasks:       c.DoneTasks,
		Leaves:          c.Leaves,
		DoneLeaves:      c.DoneLeaves,
		LeavesEstimated: c.LeavesEstimated,
		EstimateTotal:   c.EstimateTotal,
		EstimateDone:    c.EstimateDone,
	}
}

// buildHistoryPoints turns a journal into the curve: one point per instant at
// which something was recorded. Entries sharing a timestamp collapse into the
// state after the last of them — a point per entry would draw vertical
// segments that mean nothing.
//
// Nothing is drawn before the journal's first entry. A board whose journal
// starts at the origin instant has no line before it, which is the honest
// answer: that history was never recorded and cannot be recovered from
// created_at without inventing the half nobody can see.
func buildHistoryPoints(entries []store.TaskHistoryEntry, unit string) []HistoryPoint {
	if len(entries) == 0 {
		return nil
	}
	out := make([]HistoryPoint, 0, len(entries))
	w := newReplayWalker()
	for i, e := range entries {
		w.apply(e)
		if i+1 < len(entries) && entries[i+1].TS.Equal(e.TS) {
			continue
		}
		out = append(out, pointAt(e.TS, w.counts(), unit))
	}
	return out
}
