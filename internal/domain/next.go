// Package domain: the next-task algorithm.
//
// NextReady is the pure heart of task_next: given a snapshot of the backlog
// candidates plus a few pieces of context the service layer has already
// resolved, it returns the deterministic ready list, the per-rule exclusion
// counts, and a sample of blocked tasks with their blockers.
//
// The algorithm never touches the database. The service layer is responsible
// for collecting Candidates (unarchived tasks in backlog-kind columns),
// computing ParentBlocked (the open blockers on each candidate's parent), and
// reporting whether the first active column is at its WIP limit.
//
// Determinism is a contract. The service layer retries `task_next(action:
// claim|start)` by walking the returned Ready list when a concurrent writer
// wins the race for the first item; if ordering is not deterministic the
// retries can spin forever. Every comparison below is strict, every tie has
// a final tie-break on Key, and every loop walks Candidates in their input
// order so a stable sort is not required.
package domain

import (
	"sort"
	"time"
)

// Reasons counts why candidates were excluded from the ready list. The field
// names mirror service.NextReasons so the service layer can copy values
// verbatim; they live in domain so the algorithm does not depend on service.
//
// WIPFull is the count of ready tasks that exist when the first active column
// is at its WIP limit — peek returns them, start refuses them, and an agent
// handed an empty peek result with a non-zero WIPFull has actionable context.
type Reasons struct {
	BlockedDependency int
	WIPFull           int
	ClaimedByOther    int
	ParentIncomplete  int
	NotLeaf           int
}

// BlockedSample is one blocked task with its open blockers, surfaced so an
// agent handed an empty ready list knows what to work on first.
type BlockedSample struct {
	Key       string
	BlockedBy []string
}

// NextInput is the pure input to NextReady. The service layer pre-filters
// Candidates to unarchived tasks in backlog-kind columns; the algorithm does
// not touch the database.
type NextInput struct {
	Now   time.Time
	Actor string
	// Candidates is the unarchived, backlog-kind slice for the requested
	// project(s). The algorithm preserves no order assumption.
	Candidates []TaskView
	// ParentBlocked maps each candidate key to the open blockers on its parent
	// task. A missing key, a nil value, or an empty slice all mean "no parent
	// or parent not blocked". The service layer is the only thing that knows
	// the parent task; this side channel keeps the algorithm pure.
	ParentBlocked map[string][]string
	// ActiveWIPFull is true when the first active column is at its WIP limit.
	// peek still returns ready work when true; only start is refused.
	ActiveWIPFull bool
	// Limit caps the size of Ready. Values <= 0 default to 3 (the service's
	// documented default).
	Limit int
}

// NextOutput is the pure result of NextReady. Same input → same output, every
// call. The service layer wraps this into NextResult alongside the claim or
// start side effects it ran.
type NextOutput struct {
	Ready      []TaskView
	Reasons    Reasons
	BlockedTop []BlockedSample
	WIPFull    bool
}

// NextReady selects up to Limit ready tasks in the order mandated by PLAN §6.2
// and reports why the rest were excluded.
//
// Ordering: priority desc → due_at asc (nulls last) → rank asc → created_at
// asc → key asc (final tie-break). The function is pure.
func NextReady(in NextInput) NextOutput {
	if in.Limit <= 0 {
		in.Limit = 3
	}
	out := NextOutput{
		WIPFull: in.ActiveWIPFull,
	}

	var ready []TaskView
	var depBlocked []BlockedSample

	for _, tv := range in.Candidates {
		// Subtree: a task with incomplete subtasks is not itself a leaf; the
		// subtasks are the runnable leaves instead.
		if tv.SubTotal > 0 && tv.SubDone < tv.SubTotal {
			out.Reasons.NotLeaf++
			continue
		}
		// Lease: only a free, expired, or self-claimed task is a candidate.
		// An idempotent re-claim must succeed.
		if !ClaimableBy(&tv.Task, in.Actor, in.Now) {
			out.Reasons.ClaimedByOther++
			continue
		}
		// Parent: a leaf whose parent has open blockers cannot be picked —
		// working on it would make progress the parent cannot keep up with.
		if pblocks, ok := in.ParentBlocked[tv.Key]; ok && len(pblocks) > 0 {
			out.Reasons.ParentIncomplete++
			continue
		}
		// Dependency: an open `blocks` predecessor is a hard blocker.
		if len(tv.BlockedBy) > 0 {
			out.Reasons.BlockedDependency++
			depBlocked = append(depBlocked, BlockedSample{
				Key:       tv.Key,
				BlockedBy: append([]string(nil), tv.BlockedBy...),
			})
			continue
		}
		ready = append(ready, tv)
	}

	sort.Slice(ready, func(i, j int) bool { return nextLess(&ready[i], &ready[j]) })

	if len(ready) > in.Limit {
		out.Ready = ready[:in.Limit]
	} else {
		out.Ready = ready
	}

	// Sample up to NextBlockedTopSample blocked-by-dependency tasks, highest
	// priority first (matches Ready ordering), so the agent's "what's blocked"
	// view is also deterministic. Build the lookup once so the sort comparator
	// stays O(1) — calling findTaskView per comparison made this O(n²).
	lookup := make(map[string]*TaskView, len(in.Candidates))
	for i := range in.Candidates {
		lookup[in.Candidates[i].Key] = &in.Candidates[i]
	}
	sort.Slice(depBlocked, func(i, j int) bool {
		return blockedLess(depBlocked[i], depBlocked[j], lookup)
	})
	if len(depBlocked) > NextBlockedTopSample {
		depBlocked = depBlocked[:NextBlockedTopSample]
	}
	out.BlockedTop = depBlocked

	// WIPFull in Reasons is the count of ready tasks that exist when WIP is
	// full. peek returns them, start refuses them — having a non-zero count
	// tells the agent there is work waiting once WIP frees up.
	if out.WIPFull {
		out.Reasons.WIPFull = len(ready)
	}

	return out
}

// nextLess orders two candidates by PLAN §6.2, with a final tie-break on Key
// so the output is byte-stable across calls.
func nextLess(a, b *TaskView) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	aHas, bHas := a.DueAt != nil, b.DueAt != nil
	if aHas != bHas {
		return aHas
	}
	if aHas && !a.DueAt.Equal(*b.DueAt) {
		return a.DueAt.Before(*b.DueAt)
	}
	if a.Rank != b.Rank {
		return a.Rank < b.Rank
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.Key < b.Key
}

// blockedLess orders blocked samples by the same priority ordering used for
// Ready, so BlockedTop is in the same order an agent would walk the list. The
// lookup map is built once by NextReady so this comparator is O(1).
func blockedLess(a, b BlockedSample, lookup map[string]*TaskView) bool {
	ai := lookup[a.Key]
	bi := lookup[b.Key]
	if ai == nil || bi == nil {
		return a.Key < b.Key
	}
	return nextLess(ai, bi)
}
