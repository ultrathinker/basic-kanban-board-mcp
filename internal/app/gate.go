package app

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// writerQueueDepth is the writer's admission boundary: how many write
// transactions may be admitted (executing on or queued for the single writer
// connection) before new ones are refused outright.
//
// 32 is deliberately generous for the product's scale — a solo owner with a
// handful of agent CLIs. A normal write transaction is milliseconds, so 32
// queued waiters drain in well under a second; one pathological 100-item
// batch costs maybe a few hundred milliseconds. Beyond 32 simultaneous write
// attempts, piling on more goroutines that each hold a request context, memory
// and an eventual busy_timeout failure is strictly worse than refusing fast:
// the caller learns immediately, with a retryable code, while reads keep
// flowing on the reader pool.
const writerQueueDepth = 32

// gatedStore is a store.Store view whose Write passes an admission boundary
// first. Everything else — reads, backup, checkpoint, health — is delegated
// untouched: only the serialized writer needs one.
type gatedStore struct {
	store.Store
	slots    chan struct{}
	admitted atomic.Int64
	refused  atomic.Int64
}

// newGatedStore wraps st with an admission boundary of depth concurrent
// writes. depth < 1 is clamped to 1 rather than refused: the gate must never
// become a second source of "cannot write at all".
func newGatedStore(st store.Store, depth int) *gatedStore {
	if depth < 1 {
		depth = 1
	}
	return &gatedStore{Store: st, slots: make(chan struct{}, depth)}
}

// Write admits the caller through the boundary, then delegates. Cancellation
// is checked before admission so a request whose client already went away
// reports cancellation (the store's own contract) instead of consuming a slot.
// Overflow is refused immediately with rate_limited — a code agents already
// branch on — rather than after a five-second busy_timeout where it is
// indistinguishable from a genuine database conflict.
func (g *gatedStore) Write(ctx context.Context, fn func(store.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case g.slots <- struct{}{}:
		g.admitted.Add(1)
		defer func() { <-g.slots }()
		return g.Store.Write(ctx, fn)
	default:
		g.refused.Add(1)
		return writerOverloaded(len(g.slots), cap(g.slots))
	}
}

// writerOverloaded builds the product-visible refusal. The remediation names
// the actual recovery (back off and retry) because that is the honest answer:
// the server is fine, it is briefly busy.
func writerOverloaded(inFlight, capacity int) *domain.Error {
	return &domain.Error{
		Code: domain.CodeRateLimited,
		Message: fmt.Sprintf(
			"the write queue is full: %d of %d write requests are already admitted and waiting for the single writer",
			inFlight, capacity),
		Remediation: "The server is briefly saturated with writes; this request was refused before touching the database, so nothing was partially applied. Retry the same request after a short backoff (start around 1s and double it). Reads are unaffected.",
	}
}

// WriterStats is a point-in-time view of the admission boundary, for tests and
// future /readyz wiring. A cross-process `kanban doctor` cannot see these —
// they live in the serving process — but they make the boundary observable
// where it lives instead of invisible everywhere.
type WriterStats struct {
	// InFlight is the number of writes currently admitted (executing or
	// queued for the writer connection).
	InFlight int
	// Capacity is the configured admission depth.
	Capacity int
	// Admitted and Refused are lifetime counters.
	Admitted int64
	Refused  int64
}

// stats snapshots the boundary counters. len() on the semaphore channel is
// the current occupancy; buffered-channel len is race-tolerant enough for
// diagnostics (a value off by one writer for a microsecond misleads nobody).
func (g *gatedStore) stats() WriterStats {
	return WriterStats{
		InFlight: len(g.slots),
		Capacity: cap(g.slots),
		Admitted: g.admitted.Load(),
		Refused:  g.refused.Load(),
	}
}
