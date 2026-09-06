package service

import (
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// columnCache and projectCache avoid re-querying the same column/project row
// once per task when hydrating a page of TaskViews (board_get and task_next
// routinely touch the same handful of columns tens of times).

type columnCache struct {
	s    *svc
	tx   store.Tx
	byID map[string]*domain.Column
}

func newColumnCache(s *svc, tx store.Tx) *columnCache {
	return &columnCache{s: s, tx: tx, byID: map[string]*domain.Column{}}
}

func (c *columnCache) get(id string) (*domain.Column, error) {
	if col, ok := c.byID[id]; ok {
		return col, nil
	}
	col, err := c.s.store.Columns().GetByID(c.tx, id)
	if err != nil {
		return nil, err
	}
	c.byID[id] = col
	return col, nil
}

func (c *columnCache) prime(col *domain.Column) { c.byID[col.ID] = col }

type projectCache struct {
	s    *svc
	tx   store.Tx
	byID map[string]*domain.Project
}

func newProjectCache(s *svc, tx store.Tx) *projectCache {
	return &projectCache{s: s, tx: tx, byID: map[string]*domain.Project{}}
}

func (c *projectCache) get(id string) (*domain.Project, error) {
	if p, ok := c.byID[id]; ok {
		return p, nil
	}
	p, err := c.s.store.Projects().GetByID(c.tx, id)
	if err != nil {
		return nil, err
	}
	c.byID[id] = p
	return p, nil
}

func (c *projectCache) prime(p *domain.Project) { c.byID[p.ID] = p }

// hydrateOpts controls the expensive optional parts of a TaskView: notes
// require a separate paginated query, so they are opt-in per PLAN §6.3.
type hydrateOpts struct {
	IncludeNotes bool
	NotesLimit   int
	NotesBefore  *time.Time
}

// hydrateView builds a full domain.TaskView from a stored Task. Body,
// Acceptance and Metadata are always populated here (they cost nothing extra
// — they are columns on the row already loaded); callers that must keep a
// response small (board_get's default, task_next's truncation) trim or
// truncate the fields they return, after hydration, so this function stays
// the single source of truth for what "the full view" means.
func (s *svc) hydrateView(tx store.Tx, cc *columnCache, pc *projectCache, task *domain.Task, now time.Time, opts hydrateOpts) (domain.TaskView, error) {
	col, err := cc.get(task.ColumnID)
	if err != nil {
		return domain.TaskView{}, err
	}
	proj, err := pc.get(task.ProjectID)
	if err != nil {
		return domain.TaskView{}, err
	}
	openBlockers, err := s.store.Links().OpenBlockers(tx, task.ID)
	if err != nil {
		return domain.TaskView{}, err
	}
	blocks, err := s.store.Links().Blocks(tx, task.ID)
	if err != nil {
		return domain.TaskView{}, err
	}
	children, err := s.store.Tasks().Children(tx, task.ID)
	if err != nil {
		return domain.TaskView{}, err
	}
	subTotal, subDone := 0, 0
	for _, ch := range children {
		if ch.ArchivedAt != nil {
			continue
		}
		subTotal++
		chCol, err := cc.get(ch.ColumnID)
		if err != nil {
			return domain.TaskView{}, err
		}
		if chCol.Kind == domain.KindDone {
			subDone++
		}
	}

	tv := domain.TaskView{
		Task:        *task,
		ProjectKey:  proj.Key,
		ColumnName:  col.Name,
		ColumnKind:  col.Kind,
		BlockedBy:   openBlockers,
		Blocks:      blocks,
		SubDone:     subDone,
		SubTotal:    subTotal,
		LeaseRemain: domain.LeaseRemaining(task, now),
	}
	tv.Ready = subTotal == 0 && len(openBlockers) == 0 &&
		domain.ClaimableBy(task, "", now) && col.Kind == domain.KindBacklog

	if opts.IncludeNotes {
		notes, err := s.store.Notes().ListByTask(tx, task.ID, opts.NotesLimit, opts.NotesBefore)
		if err != nil {
			return domain.TaskView{}, err
		}
		tv.Notes = notes
	}
	return tv, nil
}

// stripToCompactDefault removes the fields board_get omits unless the
// caller's `include` widens them (PLAN §6.1: default none). BlockedBy/Blocks
// stay — the compact grammar always shows `blocked-by` when it applies, and
// Ready reasoning depends on the caller seeing them.
func stripToCompactDefault(tv *domain.TaskView, inc Includes) {
	if !inc.Has(IncludeBody) {
		tv.Body = ""
	}
	if !inc.Has(IncludeAcceptance) {
		tv.Acceptance = nil
	}
	if !inc.Has(IncludeMetadata) {
		tv.Metadata = nil
	}
}

// truncateBody bounds body to at most limit bytes, appending a marker with
// the count of characters cut so the agent knows how much it is missing
// (PLAN §6.2: task_next's body[≤2 KB, truncated with "… +N chars"]).
func truncateBody(body string, limit int) string {
	if len(body) <= limit {
		return body
	}
	kept := body[:limit]
	for len(kept) > 0 && !utf8.ValidString(kept) {
		kept = kept[:len(kept)-1]
	}
	remaining := utf8.RuneCountInString(body[len(kept):])
	return fmt.Sprintf("%s… +%d chars", kept, remaining)
}
