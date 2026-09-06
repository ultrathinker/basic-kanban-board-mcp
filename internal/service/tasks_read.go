package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// TaskGet resolves 1..50 keys in the caller's order. A key that does not
// exist, does not parse, or falls outside the actor's project scope becomes
// a not_found entry rather than aborting the whole read or silently
// dropping it (PLAN §6.3): the caller asked for N keys and gets N answers.
func (s *svc) TaskGet(ctx context.Context, a Actor, in TaskGetInput) (*TaskGetResult, error) {
	if err := requireRead(a); err != nil {
		return nil, err
	}
	if len(in.Keys) == 0 {
		return nil, domain.Invalid("keys", "at least one key is required", "Pass 1-50 task keys.")
	}
	if len(in.Keys) > domain.MaxGetKeys {
		return nil, domain.Invalid("keys",
			fmt.Sprintf("%d keys, the limit is %d", len(in.Keys), domain.MaxGetKeys),
			"Split the request into multiple calls.")
	}
	ctx = store.WithActor(ctx, a.Name)

	var result TaskGetResult
	err := s.store.Read(ctx, func(tx store.Tx) error {
		now := tx.Now()
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)
		result.Tasks = make([]domain.TaskView, 0, len(in.Keys))

		for _, rawKey := range in.Keys {
			pk, _, err := domain.ParseTaskKey(rawKey)
			if err != nil {
				result.NotFound = append(result.NotFound, rawKey)
				continue
			}
			if !actorMayAccessProject(a, pk) {
				result.NotFound = append(result.NotFound, strings.ToUpper(rawKey))
				continue
			}
			t, err := s.store.Tasks().GetByKey(tx, rawKey)
			if err != nil {
				if de := domain.AsError(err); de != nil && de.Code == domain.CodeNotFound {
					canon, cErr := domain.NormalizeTaskKey(rawKey)
					if cErr != nil {
						canon = strings.ToUpper(rawKey)
					}
					result.NotFound = append(result.NotFound, canon)
					continue
				}
				return err
			}
			tv, err := s.hydrateView(tx, cc, pc, t, now, hydrateOpts{
				IncludeNotes: in.Include.Has(IncludeNotes),
				NotesLimit:   domain.MaxNotesPerRead,
			})
			if err != nil {
				return err
			}
			if !in.Include.Has(IncludeMetadata) {
				tv.Metadata = nil
			}
			result.Tasks = append(result.Tasks, tv)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
