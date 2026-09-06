package auth

import (
	"context"
	"net"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// StoreAdapter bridges the auth layer's context-based TokenLookup /
// SessionLookup interfaces to the store layer's transactional repos. It
// lives in the auth package because the auth layer is the only consumer
// that needs Tx-by-call translation; the store never imports auth.
//
// Reads use store.Read (concurrent pool); writes use store.Write (single
// connection, BEGIN IMMEDIATE). Every method honours the caller's context
// for cancellation.
type StoreAdapter struct {
	S store.Store
}

// Compile-time check: adapter implements both lookup surfaces.
var (
	_ TokenLookup   = (*StoreAdapter)(nil)
	_ SessionLookup = (*StoreAdapter)(nil)
)

// NewStoreAdapter is a thin constructor. The caller passes the fully-opened
// store.Store (typically via auth.NewManagerFromStore).
func NewStoreAdapter(s store.Store) *StoreAdapter { return &StoreAdapter{S: s} }

func (a *StoreAdapter) GetByHash(ctx context.Context, hash []byte) (*domain.Token, error) {
	var out *domain.Token
	err := a.S.Read(ctx, func(tx store.Tx) error {
		t, e := a.S.Tokens().GetByHash(tx, hash)
		if e != nil {
			return e
		}
		out = t
		return nil
	})
	return out, err
}

func (a *StoreAdapter) GetByName(ctx context.Context, name string) (*domain.Token, error) {
	var out *domain.Token
	err := a.S.Read(ctx, func(tx store.Tx) error {
		t, e := a.S.Tokens().GetByName(tx, name)
		if e != nil {
			return e
		}
		out = t
		return nil
	})
	return out, err
}

func (a *StoreAdapter) List(ctx context.Context) ([]*domain.Token, error) {
	var out []*domain.Token
	err := a.S.Read(ctx, func(tx store.Tx) error {
		ts, e := a.S.Tokens().List(tx)
		if e != nil {
			return e
		}
		out = ts
		return nil
	})
	return out, err
}

func (a *StoreAdapter) Count(ctx context.Context) (int, error) {
	var out int
	err := a.S.Read(ctx, func(tx store.Tx) error {
		n, e := a.S.Tokens().Count(tx)
		if e != nil {
			return e
		}
		out = n
		return nil
	})
	return out, err
}

func (a *StoreAdapter) Create(ctx context.Context, t *domain.Token) error {
	return a.S.Write(ctx, func(tx store.Tx) error {
		return a.S.Tokens().Create(tx, t)
	})
}

func (a *StoreAdapter) UpdateHash(ctx context.Context, id string, hash []byte) error {
	return a.S.Write(ctx, func(tx store.Tx) error {
		return a.S.Tokens().UpdateHash(tx, id, hash)
	})
}

func (a *StoreAdapter) Revoke(ctx context.Context, name string) error {
	return a.S.Write(ctx, func(tx store.Tx) error {
		return a.S.Tokens().Revoke(tx, name)
	})
}

func (a *StoreAdapter) CreateSession(ctx context.Context, s *domain.Session) error {
	return a.S.Write(ctx, func(tx store.Tx) error {
		return a.S.Sessions().Create(tx, s)
	})
}

func (a *StoreAdapter) GetSession(ctx context.Context, id string) (*domain.Session, error) {
	var out *domain.Session
	err := a.S.Read(ctx, func(tx store.Tx) error {
		s, e := a.S.Sessions().Get(tx, id)
		if e != nil {
			return e
		}
		out = s
		return nil
	})
	return out, err
}

func (a *StoreAdapter) TouchSession(ctx context.Context, id string, now time.Time) error {
	return a.S.Write(ctx, func(tx store.Tx) error {
		return a.S.Sessions().Touch(tx, id, now)
	})
}

func (a *StoreAdapter) DeleteSession(ctx context.Context, id string) error {
	return a.S.Write(ctx, func(tx store.Tx) error {
		return a.S.Sessions().Delete(tx, id)
	})
}

// NewManagerFromStore is the production constructor. It takes a
// store.Store, builds the adapter, and returns a Manager wired against the
// real database. baseURL / insecure / trusted / now follow the same
// semantics as NewManager.
func NewManagerFromStore(st store.Store, trusted []*net.IPNet, baseURL string, insecure bool, now ClockFunc) *Manager {
	sa := NewStoreAdapter(st)
	return NewManager(sa, sa, trusted, baseURL, insecure, now)
}
