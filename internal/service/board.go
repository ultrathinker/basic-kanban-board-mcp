package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// DoneLimitUnlimited passed as BoardGetInput.DoneLimit turns off the done
// column cap entirely. The MCP tool schema for board_get never allows a
// caller to send it (done_limit is bounded to [0, domain.MaxDoneLimit]
// there): the cap exists so that surface stays cheap to call every session.
// The CLI export path is the one caller that needs every done task a
// project has — a backup that silently drops cards is not a backup — and
// it is a direct service.BoardGet caller, not a schema-bound one.
const DoneLimitUnlimited = -1

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
		projects, err := s.listProjectsForBoard(tx, a, in.ProjectKey, in.withArchivedProjects)
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
		switch {
		case doneLimit == DoneLimitUnlimited:
			doneLimit = math.MaxInt
		case doneLimit < 0:
			doneLimit = 0
		case doneLimit > domain.MaxDoneLimit:
			doneLimit = domain.MaxDoneLimit
		}

		// A progress line annotates task lines, and a summary has none: a
		// silently empty answer would read as "no active cards".
		if view == ViewSummary && in.Include.Has(IncludeProgress) {
			return domain.Invalid("include", `include "progress" annotates task lines, and view:"summary" has none`,
				`Use view:"tasks" (the default when project is set), or drop "progress" from include.`)
		}

		now, err := s.now(tx)
		if err != nil {
			return err
		}
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)
		pix, err := s.buildParticipantIndex(tx, now)
		if err != nil {
			return err
		}

		if err := s.validateColumnFilter(tx, projects, in.Filter); err != nil {
			return err
		}

		board.Projects = make([]BoardProject, 0, len(projects))
		for _, p := range projects {
			pc.prime(p)
			bp, err := s.buildBoardProject(tx, cc, pc, pix, p, view, doneLimit, in.Filter, in.Include, now)
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

// participantIndex answers "who may participate in project X" for every
// project of one board read from a single token listing (KANB-44). The
// participant catalogue is DERIVED, never stored: a token participates in
// every project its scope grants access to, so the board read resolves the
// list from the same source of truth authentication checks — one query for
// the whole board, not one per project.
//
// now is the read's database clock: an executor key past its expiry is no
// longer a participant (KANB-60), and it drops out here on its own, with
// nothing to clean up.
type participantIndex struct {
	byID map[string]*domain.Token
	now  time.Time
}

func (s *svc) buildParticipantIndex(tx store.Tx, now time.Time) (*participantIndex, error) {
	toks, err := s.store.Tokens().List(tx)
	if err != nil {
		return nil, err
	}
	ix := &participantIndex{byID: make(map[string]*domain.Token, len(toks)), now: now}
	for _, t := range toks {
		ix.byID[t.ID] = t
	}
	return ix, nil
}

// participantsFor lists who MAY participate in the project: every active
// token with access. Access is permission, not presence — nothing here says
// anyone is working right now, and the two states must not be conflated.
// Names, not secrets: the shape carries an id and a display name only.
func (ix *participantIndex) participantsFor(key string) []Participant {
	out := make([]Participant, 0, len(ix.byID))
	for _, t := range ix.byID {
		if !t.ActiveAt(ix.now) || !t.MayAccessProject(key) {
			continue
		}
		out = append(out, Participant{TokenID: t.ID, Name: t.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// checkAssignee refuses an assignee that is not a participant of the
// project (KANB-60). The participant list is the list of tokens that may
// work there, executor keys included, so a name typed in one of twenty-five
// spellings is caught at the write instead of leaving a card nobody's key
// can touch. The match is exact: the assignee is what an executor key's
// name is compared against, byte for byte.
func (ix *participantIndex) checkAssignee(p *domain.Project, taskKey, name string) error {
	return ix.checkParticipant("assignee", p, taskKey, name,
		"Assign one of the listed participants exactly as written, issue an executor key with that name first (executor_key_issue), add it as a participant without a key (executor_key_issue participant_only:true), or clear the assignee.")
}

// checkReviewer applies the assignee's rule to the reviewer (KANB-68): one
// name per person or agent, so the reviewer of a card is someone the board
// knows under exactly one spelling. A reviewer rarely calls the board
// itself, which is what a participant without a key is for.
func (ix *participantIndex) checkReviewer(p *domain.Project, taskKey, name string) error {
	return ix.checkParticipant("reviewer", p, taskKey, name,
		"Name one of the listed participants exactly as written, add the reviewer as a participant first (executor_key_issue participant_only:true), or clear the reviewer.")
}

func (ix *participantIndex) checkParticipant(field string, p *domain.Project, taskKey, name, remediation string) error {
	parts := ix.participantsFor(p.Key)
	names := make([]string, 0, len(parts))
	for _, pt := range parts {
		if pt.Name == name {
			return nil
		}
		names = append(names, pt.Name)
	}
	known := strings.Join(names, ", ")
	if known == "" {
		known = "(none)"
	}
	where := "a new task"
	if taskKey != "" {
		where = taskKey
	}
	return domain.Invalid(field,
		fmt.Sprintf("%s %q on %s is not a participant of project %s; participants: %s",
			field, name, where, p.Key, known),
		remediation)
}

// coordinator resolves the project's appointed coordinator to a Participant.
// A token id survives secret rotation by construction (rotation rewrites the
// hash on the same row), so this lookup keeps naming the same actor across
// rotations. A revoked appointee is still reported: the appointment is a
// fact about configuration, and pretending it is empty would hide why
// commands are being refused.
func (ix *participantIndex) coordinator(p *domain.Project) *Participant {
	if p.CoordinatorTokenID == "" {
		return nil
	}
	if t, ok := ix.byID[p.CoordinatorTokenID]; ok {
		return &Participant{TokenID: t.ID, Name: t.Name}
	}
	return &Participant{TokenID: p.CoordinatorTokenID}
}

func (s *svc) listProjectsForBoard(tx store.Tx, a Actor, key string, withArchived bool) ([]*domain.Project, error) {
	if key != "" {
		p, err := s.resolveProject(tx, a, key)
		if err != nil {
			return nil, err
		}
		return []*domain.Project{p}, nil
	}
	all, err := s.store.Projects().List(tx, withArchived)
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
	tx store.Tx, cc *columnCache, pc *projectCache, pix *participantIndex, p *domain.Project,
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
		IdleAfterSeconds:    p.IdleAfterSeconds,
		Archived:            p.ArchivedAt != nil,
		Coordinator:         pix.coordinator(p),
		Participants:        pix.participantsFor(p.Key),
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
	var active []*domain.Task

	for _, c := range cols {
		cc.prime(c)
		bc := BoardColumn{Name: c.Name, Kind: c.Kind}

		if !columnAllowed(filter, c) {
			bp.Columns = append(bp.Columns, bc)
			continue
		}

		f := boardStoreFilter(filter, p.ID, c.ID)
		tasks, err := s.store.Tasks().List(tx, f)
		if err != nil {
			return nil, err
		}
		bc.Count = len(tasks)
		if c.Kind == domain.KindActive {
			active = append(active, tasks...)
		}

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

	bp.Attention, bp.Activity, err = s.boardActivity(tx, p, active, include.Has(IncludeProgress), now)
	if err != nil {
		return nil, err
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

func columnAllowed(bf BoardFilter, col *domain.Column) bool {
	if len(bf.ColumnKinds) > 0 {
		ok := false
		for _, k := range bf.ColumnKinds {
			if k == col.Kind {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(bf.Columns) == 0 {
		return true
	}
	for _, c := range bf.Columns {
		if strings.EqualFold(c, col.Name) {
			return true
		}
	}
	return false
}

// validateColumnFilter refuses a filter that can only ever return an empty
// board: a column kind that does not exist, or a column name that none of
// the projects being read has. The IAMT retrospective counted six requests
// that came back empty for exactly this reason and were read as "nothing
// there" (KANB-59). The error lists the real names so the next call is right.
func (s *svc) validateColumnFilter(tx store.Tx, projects []*domain.Project, bf BoardFilter) error {
	for _, k := range bf.ColumnKinds {
		if !k.Valid() {
			return domain.Invalid("filter.column_kinds",
				fmt.Sprintf("unknown column kind %q", k),
				"Use one of: backlog, active, waiting, done.")
		}
	}
	if len(bf.Columns) == 0 || len(projects) == 0 {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	for _, p := range projects {
		cols, err := s.store.Columns().ListByProject(tx, p.ID)
		if err != nil {
			return err
		}
		for _, c := range cols {
			if k := strings.ToLower(c.Name); !seen[k] {
				seen[k] = true
				names = append(names, c.Name)
			}
		}
	}
	var unknown []string
	for _, want := range bf.Columns {
		if !seen[strings.ToLower(strings.TrimSpace(want))] {
			unknown = append(unknown, fmt.Sprintf("%q", want))
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	where := "these projects"
	if len(projects) == 1 {
		where = "project " + projects[0].Key
	}
	return domain.Invalid("filter.columns",
		fmt.Sprintf("no column named %s in %s", strings.Join(unknown, ", "), where),
		"Columns that exist: "+strings.Join(names, ", ")+". To filter by what a column means rather than its name, use filter.column_kinds.")
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
