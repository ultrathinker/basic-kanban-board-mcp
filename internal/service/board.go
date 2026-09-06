package service

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// BoardGet is a single read transaction: PLAN §6.1's compact-by-default
// contract only holds if every field on the resulting Board came from one
// consistent snapshot.
func (s *svc) BoardGet(ctx context.Context, a Actor, in BoardGetInput) (*Board, error) {
	if err := requireRead(a); err != nil {
		return nil, err
	}
	ctx = store.WithActor(ctx, a.Name)

	var board Board
	err := s.store.Read(ctx, func(tx store.Tx) error {
		projects, err := s.listProjectsForBoard(tx, a, in.ProjectKey)
		if err != nil {
			return err
		}

		view := in.View
		if view == "" {
			if in.ProjectKey != "" {
				view = ViewTasks
			} else {
				view = ViewSummary
			}
		}
		doneLimit := in.DoneLimit
		if doneLimit < 0 {
			doneLimit = 0
		}
		if doneLimit > domain.MaxDoneLimit {
			doneLimit = domain.MaxDoneLimit
		}

		now, err := tx.Now()
		if err != nil {
			return err
		}
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)

		board.Projects = make([]BoardProject, 0, len(projects))
		for _, p := range projects {
			pc.prime(p)
			bp, err := s.buildBoardProject(tx, cc, pc, p, view, doneLimit, in.Filter, in.Include, now)
			if err != nil {
				return err
			}
			board.Projects = append(board.Projects, *bp)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &board, nil
}

func (s *svc) listProjectsForBoard(tx store.Tx, a Actor, key string) ([]*domain.Project, error) {
	if key != "" {
		p, err := s.resolveProject(tx, a, key)
		if err != nil {
			return nil, err
		}
		return []*domain.Project{p}, nil
	}
	all, err := s.store.Projects().List(tx, false)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Project, 0, len(all))
	for _, p := range all {
		if actorMayAccessProject(a, p.Key) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *svc) buildBoardProject(
	tx store.Tx, cc *columnCache, pc *projectCache, p *domain.Project,
	view BoardView, doneLimit int, filter BoardFilter, include Includes, now time.Time,
) (*BoardProject, error) {
	cols, err := s.store.Columns().ListByProject(tx, p.ID)
	if err != nil {
		return nil, err
	}
	bp := &BoardProject{
		Key:                 p.Key,
		Name:                p.Name,
		Description:         p.Description,
		Version:             p.Version,
		EstimateUnit:        p.EstimateUnit,
		EnforceDependencies: p.EnforceDependencies,
		StrictDone:          p.StrictDone,
		ClaimTTLSeconds:     p.ClaimTTLSeconds,
		Archived:            p.ArchivedAt != nil,
		Columns:             make([]BoardColumn, 0, len(cols)),
	}
	if p.FocusTaskID != nil {
		if ft, err := s.store.Tasks().GetByID(tx, *p.FocusTaskID); err == nil {
			bp.FocusKey = ft.Key
		}
	}

	type doneCandidate struct {
		task     *domain.Task
		colIndex int
	}
	var doneCandidates []doneCandidate

	for _, c := range cols {
		cc.prime(c)
		bc := BoardColumn{Name: c.Name, Kind: c.Kind, WIPLimit: c.WIPLimit}

		if !columnAllowed(filter, c.Name) {
			bp.Columns = append(bp.Columns, bc)
			continue
		}

		f := boardStoreFilter(filter, p.ID, c.ID)
		tasks, err := s.store.Tasks().List(tx, f)
		if err != nil {
			return nil, err
		}
		bc.Count = len(tasks)

		if c.Kind == domain.KindDone {
			bp.DoneTotal += len(tasks)
			if view == ViewTasks {
				idx := len(bp.Columns)
				for _, t := range tasks {
					doneCandidates = append(doneCandidates, doneCandidate{task: t, colIndex: idx})
				}
			}
			bp.Columns = append(bp.Columns, bc)
			continue
		}

		if view == ViewTasks {
			views := make([]domain.TaskView, 0, len(tasks))
			for _, t := range tasks {
				tv, err := s.hydrateView(tx, cc, pc, t, now, hydrateOpts{IncludeNotes: include.Has(IncludeNotes)})
				if err != nil {
					return nil, err
				}
				stripToCompactDefault(&tv, include)
				views = append(views, tv)
			}
			bc.Tasks = views
		}
		bp.Columns = append(bp.Columns, bc)
	}

	if len(doneCandidates) > 0 && doneLimit > 0 {
		sort.SliceStable(doneCandidates, func(i, j int) bool {
			return doneAtLess(doneCandidates[j].task.DoneAt, doneCandidates[i].task.DoneAt)
		})
		if len(doneCandidates) > doneLimit {
			doneCandidates = doneCandidates[:doneLimit]
		}
		byColumn := make(map[int][]*domain.Task, len(doneCandidates))
		for _, dc := range doneCandidates {
			byColumn[dc.colIndex] = append(byColumn[dc.colIndex], dc.task)
		}
		for idx, tasks := range byColumn {
			views := make([]domain.TaskView, 0, len(tasks))
			for _, t := range tasks {
				tv, err := s.hydrateView(tx, cc, pc, t, now, hydrateOpts{IncludeNotes: include.Has(IncludeNotes)})
				if err != nil {
					return nil, err
				}
				stripToCompactDefault(&tv, include)
				views = append(views, tv)
			}
			bp.Columns[idx].Tasks = views
		}
		bp.DoneShown = len(doneCandidates)
	}

	return bp, nil
}

// doneAtLess orders "most recently done first": a nil DoneAt (should not
// happen for a task actually in a done column, but defends against it) sorts
// as older than any real timestamp.
func doneAtLess(a, b *time.Time) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil {
		return true
	}
	if b == nil {
		return false
	}
	return a.Before(*b)
}

func columnAllowed(bf BoardFilter, name string) bool {
	if len(bf.Columns) == 0 {
		return true
	}
	for _, c := range bf.Columns {
		if strings.EqualFold(c, name) {
			return true
		}
	}
	return false
}

func boardStoreFilter(bf BoardFilter, projectID, columnID string) store.TaskFilter {
	f := store.TaskFilter{
		ProjectIDs:   []string{projectID},
		ColumnIDs:    []string{columnID},
		Types:        bf.Types,
		PriorityMin:  bf.PriorityMin,
		Tags:         bf.Tags,
		Assignee:     bf.Assignee,
		Query:        bf.Query,
		UpdatedSince: bf.UpdatedSince,
		Blocked:      bf.Blocked,
		IncludeDone:  true,
	}
	switch bf.Claimed {
	case "mine":
		f.Claimed = store.ClaimedMine
	case "unclaimed":
		f.Claimed = store.ClaimedUnclaimed
	case "other":
		f.Claimed = store.ClaimedOther
	}
	return f
}
