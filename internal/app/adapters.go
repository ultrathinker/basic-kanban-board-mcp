package app

import (
	"context"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// storeHistory adapts store.Store to events.HistoryLoader. It lives here,
// in the composition root, exactly per this package's brief: internal/events
// declares HistoryLoader in terms of plain values so it never imports
// internal/store, and the composition root is where the two are wired
// together — the same seam-adapter pattern internal/auth's StoreAdapter
// already uses for TokenLookup/SessionLookup.
//
// events.HistoryLoader has no context parameter, so a bounded background
// context is used for each call rather than blocking forever if the store
// were ever wedged.
type storeHistory struct{ st store.Store }

func (h *storeHistory) Since(projectID string, afterID int64, limit int) ([]domain.Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []domain.Event
	err := h.st.Read(ctx, func(tx store.Tx) error {
		evs, err := h.st.Events().Since(tx, projectID, afterID, limit)
		if err != nil {
			return err
		}
		out = evs
		return nil
	})
	return out, err
}

func (h *storeHistory) MinID() (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id int64
	err := h.st.Read(ctx, func(tx store.Tx) error {
		v, err := h.st.Events().MinID(tx)
		if err != nil {
			return err
		}
		id = v
		return nil
	})
	return id, err
}

// projectLookup resolves a project's human key to its internal storage id,
// implementing web.ProjectResolver. It exists because domain.Event.ProjectID
// (what events.Bus filters subscriptions on) is the internal id, but the web
// layer only ever has the human key from a URL, and the frozen
// service.Service interface has no method that returns it (board_get's
// BoardProject carries Key/Name, never the id).
//
// This is a narrow, read-only lookup with no business rules attached — the
// same class of infrastructure seam AGENTS.md's "only internal/service may
// call internal/store" is not really aimed at (internal/auth's StoreAdapter
// already calls store directly for the same reason: tokens/sessions are not
// board domain objects either). It is still a deviation worth flagging: see
// the task report.
type projectLookup struct{ st store.Store }

func (p projectLookup) ProjectIDForKey(ctx context.Context, key string) (string, bool, error) {
	var (
		id    string
		found bool
	)
	err := p.st.Read(ctx, func(tx store.Tx) error {
		proj, err := p.st.Projects().GetByKey(tx, key)
		if err != nil {
			if de := domain.AsError(err); de != nil && de.Code == domain.CodeNotFound {
				return nil
			}
			return err
		}
		if proj == nil {
			return nil
		}
		id = proj.ID
		found = true
		return nil
	})
	return id, found, err
}
