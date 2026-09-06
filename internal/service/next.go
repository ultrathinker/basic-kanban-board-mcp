package service

import (
	"context"
	"sort"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// TaskNext implements peek/claim/start (PLAN §6.2). peek is read-only and may
// scan every accessible project; claim and start require a concrete project
// because "the first active column" and its WIP limit are project-scoped —
// domain.NextInput carries a single ActiveWIPFull flag, so a mutating call
// needs one unambiguous column to check and move into.
func (s *svc) TaskNext(ctx context.Context, a Actor, in TaskNextInput) (*NextResult, error) {
	if err := requireRead(a); err != nil {
		return nil, err
	}
	action := in.Action
	if action == "" {
		action = NextPeek
	}
	if action != NextPeek && action != NextClaim && action != NextStart {
		return nil, domain.Invalid("action", "action must be peek, claim or start", "Use one of: peek, claim, start.")
	}
	if action != NextPeek {
		if err := requireWrite(a); err != nil {
			return nil, err
		}
		if in.ProjectKey == "" {
			return nil, domain.Invalid("project", "project is required for claim and start",
				"Only peek may scan every accessible project; pass project for claim/start.")
		}
	}
	if in.Detail != "" && in.Detail != NextDetailSummary && in.Detail != NextDetailFull {
		return nil, domain.Invalid("detail", "detail must be summary or full",
			"Use one of: summary, full. Omit it for summary, and call task_get for a whole card.")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 3
	}
	if limit > domain.MaxNextLimit {
		limit = domain.MaxNextLimit
	}
	ctx = store.WithActor(ctx, a.Name)
	proj := NewNextProjection(in.Include, in.Detail)

	if action == NextPeek {
		return s.taskNextPeek(ctx, a, in, limit, proj)
	}
	return s.taskNextMutate(ctx, a, in, action, limit, proj)
}

// NewNextProjection decides task_next's response shaping once, up front, so
// the decision travels with the result instead of being inferred again by
// every layer that renders it.
//
// Neither tier returns an unbounded body: a candidate list exists to choose
// from, and PLAN §6.2 caps it at domain.NextBodyTruncate even at its widest.
// Metadata is absent from task_next's vocabulary by the same design — per-task
// metadata never helps choose between candidates.
func NewNextProjection(include Includes, detail NextDetail) Projection {
	fields := make(Includes, 0, 4)
	for _, w := range []Include{IncludeBody, IncludeAcceptance, IncludeNotes, IncludeLinks} {
		if include.Has(w) {
			fields = append(fields, w)
		}
	}
	p := Projection{Fields: fields, AcceptanceTotals: map[string]int{}}
	if detail == NextDetailFull {
		p.Body, p.BodyLimitBytes = DetailBounded, domain.NextBodyTruncate
		p.Acceptance, p.AcceptanceLimit = DetailBounded, domain.NextAcceptanceItems
		return p
	}
	p.Body, p.BodyLimitBytes = DetailBounded, NextSummaryBodyBytes
	p.Acceptance, p.AcceptanceLimit = DetailBounded, NextSummaryAcceptanceItems
	return p
}

func (s *svc) taskNextPeek(ctx context.Context, a Actor, in TaskNextInput, limit int, proj Projection) (*NextResult, error) {
	var result NextResult
	err := s.store.Read(ctx, func(tx store.Tx) error {
		projects, err := s.listProjectsForBoard(tx, a, in.ProjectKey)
		if err != nil {
			return err
		}
		now, err := tx.Now()
		if err != nil {
			return err
		}
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)

		var merged domain.NextOutput
		for i, p := range projects {
			pc.prime(p)
			out, err := s.nextForProject(tx, cc, pc, p, a.Name, now, limit)
			if err != nil {
				return err
			}
			mergeNextOutputs(&merged, out, i == 0)
		}
		resortMerged(&merged, limit)

		views := make([]domain.TaskView, 0, len(merged.Ready))
		for i := range merged.Ready {
			tv, err := s.finalizeNextView(tx, &merged.Ready[i], &proj)
			if err != nil {
				return err
			}
			views = append(views, tv)
		}
		result = NextResult{
			Tasks:      views,
			Projection: proj,
			WIPFull:    merged.WIPFull,
			Reasons:    NextReasons(merged.Reasons),
			BlockedTop: toServiceBlocked(merged.BlockedTop),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *svc) taskNextMutate(ctx context.Context, a Actor, in TaskNextInput, action NextAction, limit int, proj Projection) (*NextResult, error) {
	var result NextResult
	var pending []domain.Event
	err := s.store.Write(ctx, func(tx store.Tx) error {
		p, err := s.resolveProject(tx, a, in.ProjectKey)
		if err != nil {
			return err
		}
		now, err := tx.Now()
		if err != nil {
			return err
		}
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)
		pc.prime(p)

		out, err := s.nextForProject(tx, cc, pc, p, a.Name, now, limit)
		if err != nil {
			return err
		}

		var acted *domain.Task
		var actedErr error
		for i := range out.Ready {
			cand := out.Ready[i].Task
			ttl := time.Duration(p.ClaimTTLSeconds) * time.Second
			ok, fresh, err := s.store.Tasks().Claim(tx, cand.ID, a.Name, ttl)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if action == NextStart {
				started, err := s.startClaimedTask(tx, p, cc, fresh, a.Name)
				if err != nil {
					// The claim succeeded but the move did not: release the
					// lease we just took so a WIP refusal does not leave the
					// task claimed for no reason.
					_, _ = s.store.Tasks().Release(tx, fresh.ID, a.Name, false)
					actedErr = err
					break
				}
				acted = started
				if err := s.emit(tx, &pending, a.Name, domain.EventTaskStarted, p.ID, &started.ID,
					map[string]any{"key": started.Key}); err != nil {
					return err
				}
			} else {
				acted = fresh
				if err := s.emit(tx, &pending, a.Name, domain.EventTaskClaimed, p.ID, &fresh.ID,
					map[string]any{"key": fresh.Key}); err != nil {
					return err
				}
			}
			break
		}
		if actedErr != nil {
			return actedErr
		}

		views := make([]domain.TaskView, 0, len(out.Ready))
		for i := range out.Ready {
			tv, err := s.finalizeNextView(tx, &out.Ready[i], &proj)
			if err != nil {
				return err
			}
			views = append(views, tv)
		}
		result = NextResult{
			Tasks:      views,
			Projection: proj,
			WIPFull:    out.WIPFull,
			Reasons:    NextReasons(out.Reasons),
			BlockedTop: toServiceBlocked(out.BlockedTop),
		}
		if acted != nil {
			if action == NextStart {
				result.StartedKey = acted.Key
			} else {
				result.ClaimedKey = acted.Key
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &result, nil
}

// startClaimedTask validates and performs the move into the project's first
// active column, in the same transaction as the claim that preceded it.
func (s *svc) startClaimedTask(tx store.Tx, p *domain.Project, cc *columnCache, t *domain.Task, actor string) (*domain.Task, error) {
	fromCol, err := cc.get(t.ColumnID)
	if err != nil {
		return nil, err
	}
	toCol, err := s.firstActiveColumn(tx, p.ID)
	if err != nil {
		return nil, err
	}
	cnt, err := s.store.Columns().CountTasks(tx, toCol.ID, t.ID)
	if err != nil {
		return nil, err
	}
	openBlocks, err := s.store.Links().OpenBlockers(tx, t.ID)
	if err != nil {
		return nil, err
	}
	if err := domain.CheckMove(domain.MoveCheck{
		TaskKey:             t.Key,
		From:                *fromCol,
		To:                  *toCol,
		OpenBlocks:          openBlocks,
		ToCount:             cnt,
		EnforceDependencies: p.EnforceDependencies,
		StrictDone:          p.StrictDone,
	}); err != nil {
		return nil, err
	}
	before, after, err := s.store.Tasks().NeighbourRanks(tx, toCol.ID, store.RankBottom)
	if err != nil {
		return nil, err
	}
	// The lease taken moments ago must SURVIVE this move: candidates come
	// from backlog columns and firstActiveColumn only returns active-kind
	// columns, so the done boundary is never crossed here — the release rule
	// in applyUpdate deliberately does not apply to start.
	if err := s.store.Tasks().Move(tx, t.ID, toCol.ID, midRank(before, after), actor); err != nil {
		return nil, err
	}
	return s.store.Tasks().GetByID(tx, t.ID)
}

func (s *svc) firstActiveColumn(tx store.Tx, projectID string) (*domain.Column, error) {
	cols, err := s.store.Columns().ListByProject(tx, projectID)
	if err != nil {
		return nil, err
	}
	for _, c := range cols {
		if c.Kind == domain.KindActive {
			return c, nil
		}
	}
	return nil, domain.Invalid("column", "project has no active-kind column",
		"Add one with project_upsert before starting tasks.")
}

func midRank(before, after int64) int64 { return before + (after-before)/2 }

// nextForProject gathers backlog candidates for one project, hydrates them
// into TaskViews and runs the pure domain.NextReady algorithm.
func (s *svc) nextForProject(tx store.Tx, cc *columnCache, pc *projectCache, p *domain.Project, actor string, now time.Time, limit int) (domain.NextOutput, error) {
	cols, err := s.store.Columns().ListByProject(tx, p.ID)
	if err != nil {
		return domain.NextOutput{}, err
	}
	var backlogIDs []string
	wipFull := false
	activeSeen := false
	for _, c := range cols {
		cc.prime(c)
		if c.Kind == domain.KindBacklog {
			backlogIDs = append(backlogIDs, c.ID)
		}
		if c.Kind == domain.KindActive && !activeSeen {
			activeSeen = true
			if c.WIPLimit != nil {
				cnt, err := s.store.Columns().CountTasks(tx, c.ID, "")
				if err != nil {
					return domain.NextOutput{}, err
				}
				wipFull = cnt >= *c.WIPLimit
			}
		}
	}
	if len(backlogIDs) == 0 {
		return domain.NextOutput{WIPFull: wipFull}, nil
	}

	tasks, err := s.store.Tasks().List(tx, store.TaskFilter{
		ProjectIDs: []string{p.ID},
		ColumnIDs:  backlogIDs,
	})
	if err != nil {
		return domain.NextOutput{}, err
	}

	views := make([]domain.TaskView, 0, len(tasks))
	parentBlocked := make(map[string][]string, len(tasks))
	parentBlockersCache := map[string][]string{}
	for _, t := range tasks {
		tv, err := s.hydrateView(tx, cc, pc, t, now, hydrateOpts{})
		if err != nil {
			return domain.NextOutput{}, err
		}
		views = append(views, tv)
		if t.ParentID != nil {
			blockers, ok := parentBlockersCache[*t.ParentID]
			if !ok {
				blockers, err = s.store.Links().OpenBlockers(tx, *t.ParentID)
				if err != nil {
					return domain.NextOutput{}, err
				}
				parentBlockersCache[*t.ParentID] = blockers
			}
			if len(blockers) > 0 {
				parentBlocked[t.Key] = blockers
			}
		}
	}

	return domain.NextReady(domain.NextInput{
		Now:           now,
		Actor:         actor,
		Candidates:    views,
		ParentBlocked: parentBlocked,
		ActiveWIPFull: wipFull,
		Limit:         limit,
	}), nil
}

// finalizeNextView applies the projection to an already hydrated candidate.
// It is the only place task_next's shaping happens, and it records what it cut
// back into the projection so the result can say so.
//
// The projection is passed by pointer because AcceptanceTotals accumulates
// across candidates; everything else on it is read-only here.
func (s *svc) finalizeNextView(tx store.Tx, tv *domain.TaskView, proj *Projection) (domain.TaskView, error) {
	out := *tv
	proj.Apply(&out)
	if proj.Has(IncludeNotes) {
		notes, err := s.store.Notes().ListByTask(tx, out.ID, domain.MaxNotesPerRead, nil)
		if err != nil {
			return domain.TaskView{}, err
		}
		out.Notes = notes
	}
	return out, nil
}

func toServiceBlocked(in []domain.BlockedSample) []BlockedSample {
	out := make([]BlockedSample, 0, len(in))
	for _, b := range in {
		out = append(out, BlockedSample{Key: b.Key, BlockedBy: b.BlockedBy})
	}
	return out
}

// mergeNextOutputs folds one project's NextOutput into the running total for
// a multi-project peek. first resets the accumulator instead of appending to
// stale zero values.
func mergeNextOutputs(acc *domain.NextOutput, out domain.NextOutput, first bool) {
	if first {
		*acc = domain.NextOutput{}
	}
	acc.Ready = append(acc.Ready, out.Ready...)
	acc.BlockedTop = append(acc.BlockedTop, out.BlockedTop...)
	acc.Reasons.BlockedDependency += out.Reasons.BlockedDependency
	acc.Reasons.WIPFull += out.Reasons.WIPFull
	acc.Reasons.ClaimedByOther += out.Reasons.ClaimedByOther
	acc.Reasons.ParentIncomplete += out.Reasons.ParentIncomplete
	acc.Reasons.NotLeaf += out.Reasons.NotLeaf
	acc.WIPFull = acc.WIPFull || out.WIPFull
}

// resortMerged re-applies task_next's ordering (PLAN §6.2) across the
// concatenated per-project Ready/BlockedTop lists and re-caps them, since
// each project only capped against its own Limit.
func resortMerged(acc *domain.NextOutput, limit int) {
	sort.SliceStable(acc.Ready, func(i, j int) bool { return nextLess(&acc.Ready[i], &acc.Ready[j]) })
	if len(acc.Ready) > limit {
		acc.Ready = acc.Ready[:limit]
	}
	// BlockedTop samples do not carry a full TaskView (domain.BlockedSample is
	// just a key + its blockers), so a cross-project re-sort by priority is
	// not possible here without re-fetching; each project's slice was already
	// priority-ordered, so falling back to key order for the merge keeps the
	// result deterministic without a second query.
	sort.SliceStable(acc.BlockedTop, func(i, j int) bool { return acc.BlockedTop[i].Key < acc.BlockedTop[j].Key })
	if len(acc.BlockedTop) > domain.NextBlockedTopSample {
		acc.BlockedTop = acc.BlockedTop[:domain.NextBlockedTopSample]
	}
}

// nextLess duplicates domain's unexported ordering rule (PLAN §6.2:
// priority desc, due_at asc nulls-last, rank asc, created_at asc, key asc).
// It only needs to exist because peek across multiple projects must re-sort
// the concatenation of already-sorted per-project lists, and the algorithm
// that produced those lists is not exported.
func nextLess(a, b *domain.TaskView) bool {
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
