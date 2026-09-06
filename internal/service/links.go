package service

import (
	"context"
	"sort"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// TaskLink adds and removes `blocks` edges atomically (PLAN §6.6). Both
// endpoints of every touched link bump their version — a dependency changed,
// which changes readiness, which is exactly what an if_version-aware caller
// needs to know about.
func (s *svc) TaskLink(ctx context.Context, a Actor, in TaskLinkInput) (*TaskLinkResult, error) {
	if err := requireWrite(a); err != nil {
		return nil, err
	}
	if len(in.Add) == 0 && len(in.Remove) == 0 {
		return nil, domain.Invalid("add", "add or remove must contain at least one pair",
			"Pass at least one {blocker, blocked} pair in add or remove.")
	}
	ctx = store.WithActor(ctx, a.Name)

	var result TaskLinkResult
	var pending []domain.Event
	touched := map[string]*domain.Task{}
	err := s.store.Write(ctx, func(tx store.Tx) error {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)

		for _, pair := range in.Add {
			blocker, blocked, err := s.resolveLinkPair(tx, a, pair)
			if err != nil {
				return err
			}
			if err := s.store.Links().Add(tx, &domain.Link{
				BlockerID: blocker.ID, BlockedID: blocked.ID, Type: domain.LinkBlocks, CreatedBy: a.Name,
			}); err != nil {
				return err
			}
			if err := s.bumpVersion(tx, blocker, a.Name); err != nil {
				return err
			}
			if err := s.bumpVersion(tx, blocked, a.Name); err != nil {
				return err
			}
			if err := s.emit(tx, &pending, a.Name, domain.EventLinkAdded, blocker.ProjectID, &blocked.ID,
				map[string]any{"blocker": blocker.Key, "blocked": blocked.Key}); err != nil {
				return err
			}
			touched[blocker.Key] = blocker
			touched[blocked.Key] = blocked
		}

		for _, pair := range in.Remove {
			blocker, blocked, err := s.resolveLinkPair(tx, a, pair)
			if err != nil {
				return err
			}
			existingBlocks, err := s.store.Links().Blocks(tx, blocker.ID)
			if err != nil {
				return err
			}
			existed := containsKey(existingBlocks, blocked.Key)
			if err := s.store.Links().Remove(tx, blocker.ID, blocked.ID, domain.LinkBlocks); err != nil {
				return err
			}
			if !existed {
				// PLAN §6.6: removing a link that never existed is a no-op
				// success — it must not bump a version nor log an event.
				touched[blocker.Key] = blocker
				touched[blocked.Key] = blocked
				continue
			}
			if err := s.bumpVersion(tx, blocker, a.Name); err != nil {
				return err
			}
			if err := s.bumpVersion(tx, blocked, a.Name); err != nil {
				return err
			}
			if err := s.emit(tx, &pending, a.Name, domain.EventLinkRemoved, blocker.ProjectID, &blocked.ID,
				map[string]any{"blocker": blocker.Key, "blocked": blocked.Key}); err != nil {
				return err
			}
			touched[blocker.Key] = blocker
			touched[blocked.Key] = blocked
		}

		keys := make([]string, 0, len(touched))
		for k := range touched {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		result.Tasks = make([]domain.TaskView, 0, len(keys))
		for _, k := range keys {
			tv, err := s.hydrateView(tx, cc, pc, touched[k], now, hydrateOpts{})
			if err != nil {
				return err
			}
			result.Tasks = append(result.Tasks, tv)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &result, nil
}

func (s *svc) resolveLinkPair(tx store.Tx, a Actor, pair LinkPair) (blocker, blocked *domain.Task, err error) {
	if pair.Blocker == "" || pair.Blocked == "" {
		return nil, nil, domain.Invalid("link", "blocker and blocked are both required", "Pass both task keys.")
	}
	blocker, err = s.resolveTask(tx, a, pair.Blocker)
	if err != nil {
		return nil, nil, err
	}
	// The blocked end is resolved relative to the blocker, so the pair is
	// refused when the two live in different projects. Checking the actor's
	// access to both keys is not enough on its own: two projects a token may
	// read are still two closed aggregates in v1 (PLAN §18 / architecture
	// review #18), and a cross-project edge makes readiness depend on a task
	// that a differently-scoped token cannot see.
	blocked, err = s.resolveRelatedTask(tx, a, "blocked", pair.Blocked, blocker.Key, blocker.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	return blocker, blocked, nil
}

// bumpVersion re-writes a task's own current field values so the store's
// Update path increments Version without changing content — the mechanism
// PLAN §7 calls for when a structural change (a link) affects readiness
// without touching the task's own fields.
func (s *svc) bumpVersion(tx store.Tx, t *domain.Task, actor string) error {
	t.UpdatedBy = actor
	return s.store.Tasks().Update(tx, t, nil)
}

func containsKey(keys []string, key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}
