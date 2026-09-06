package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// blockingStore is a fake store whose Write parks until released, so tests
// can hold admission slots open deterministically. Every method except Write
// would panic if called — the tests never call them.
type blockingStore struct {
	store.Store
	entered chan struct{}
	release chan struct{}
}

func newBlockingStore() *blockingStore {
	return &blockingStore{entered: make(chan struct{}, 16), release: make(chan struct{})}
}

func (b *blockingStore) Write(ctx context.Context, fn func(store.Tx) error) error {
	b.entered <- struct{}{}
	<-b.release
	return nil
}

// TestGatedStore_OverflowRefusedRateLimited is the admission-boundary
// contract: with every slot taken, the next write is refused IMMEDIATELY
// (before the database) with the rate_limited code agents retry on — not
// after a five-second busy_timeout where it looks like a conflict.
func TestGatedStore_OverflowRefusedRateLimited(t *testing.T) {
	fake := newBlockingStore()
	gate := newGatedStore(fake, 2)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := gate.Write(ctx, func(store.Tx) error { return nil }); err != nil {
				t.Errorf("admitted write returned %v, want nil", err)
			}
		}()
	}
	// Wait until both writes are inside the fake store (slots consumed).
	for i := 0; i < 2; i++ {
		select {
		case <-fake.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("writes did not reach the underlying store within 2s")
		}
	}

	start := time.Now()
	err := gate.Write(ctx, func(store.Tx) error { return nil })
	if err == nil {
		t.Fatal("overflow write must be refused")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("overflow refusal took %v; it must be immediate", time.Since(start))
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeRateLimited {
		t.Fatalf("err = %v, want domain rate_limited", err)
	}
	if !errors.Is(err, domain.ErrRateLimited) {
		t.Fatal("errors.Is(err, domain.ErrRateLimited) must hold for adapter mapping")
	}
	if de.Remediation == "" {
		t.Fatal("rate_limited refusal must carry a remediation")
	}

	stats := gate.stats()
	if stats.InFlight != 2 || stats.Capacity != 2 {
		t.Fatalf("stats = %+v, want InFlight=2 Capacity=2", stats)
	}
	if stats.Admitted != 2 || stats.Refused != 1 {
		t.Fatalf("stats = %+v, want Admitted=2 Refused=1", stats)
	}

	close(fake.release)
	wg.Wait()
	// Slots return as the writes complete; the drain is observable.
	deadline := time.Now().Add(2 * time.Second)
	for gate.stats().InFlight != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := gate.stats().InFlight; got != 0 {
		t.Fatalf("InFlight = %d after all writes returned, want 0", got)
	}
}

// TestGatedStore_AdmitsWithinCapacity: below the boundary, writes pass
// straight through and their results (including errors) are untouched.
func TestGatedStore_AdmitsWithinCapacity(t *testing.T) {
	fake := newBlockingStore()
	gate := newGatedStore(fake, 4)
	ctx := context.Background()

	go func() {
		<-fake.entered
		close(fake.release)
	}()
	if err := gate.Write(ctx, func(store.Tx) error { return nil }); err != nil {
		t.Fatalf("in-capacity write: %v", err)
	}
	if s := gate.stats(); s.Admitted != 1 || s.Refused != 0 {
		t.Fatalf("stats = %+v, want Admitted=1 Refused=0", s)
	}
}

// TestGatedStore_CancelledContext: a request whose context already died
// reports cancellation without consuming a slot — matching the underlying
// store's own contract.
func TestGatedStore_CancelledContext(t *testing.T) {
	gate := newGatedStore(newBlockingStore(), 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := gate.Write(ctx, func(store.Tx) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if s := gate.stats(); s.Admitted != 0 && s.Refused != 0 {
		t.Fatalf("cancelled request consumed a slot: %+v", s)
	}
}

// TestGatedStore_ClampsDepthToAtLeastOne: a misconfigured depth must never
// become "nothing can ever write".
func TestGatedStore_ClampsDepthToAtLeastOne(t *testing.T) {
	fake := newBlockingStore()
	gate := newGatedStore(fake, 0)
	if got := cap(gate.slots); got != 1 {
		t.Fatalf("cap = %d, want clamped to 1", got)
	}
}

// TestGatedStore_RealStorePassThrough runs the gate around a real SQLite
// store and proves ordinary writes and reads are unaffected, plus that
// Store-level operations (Checkpoint) delegate.
func TestGatedStore_RealStorePassThrough(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	raw, err := store.Open(ctx, store.Config{Path: databasePath(dir)})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer raw.Close()

	gate := newGatedStore(raw, writerQueueDepth)
	tok := testToken("gate-write")
	if err := gate.Write(ctx, func(tx store.Tx) error {
		return gate.Tokens().Create(tx, tok)
	}); err != nil {
		t.Fatalf("gated write: %v", err)
	}
	var count int
	if err := gate.Read(ctx, func(tx store.Tx) error {
		n, err := gate.Tokens().Count(tx)
		count = n
		return err
	}); err != nil {
		t.Fatalf("gated read: %v", err)
	}
	if count != 1 {
		t.Fatalf("tokens = %d, want 1", count)
	}
	if err := gate.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint through the gate: %v", err)
	}
	if s := gate.stats(); s.Admitted != 1 {
		t.Fatalf("stats = %+v, want Admitted=1", s)
	}
}
