package app

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ephemeralListener binds a loopback listener on an OS-chosen port. Tests
// must never bind a fixed port.
func ephemeralListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// testToken builds a minimal valid token row for tests that need a token to
// satisfy foreign keys (sessions and idempotency records reference it).
func testToken(name string) *domain.Token {
	return &domain.Token{
		ID:        uuid.NewString(),
		Name:      name,
		Hash:      auth.HashToken("kbn_" + name + "_padding00xx00xx00xx00xx00xx"),
		Scopes:    domain.Scopes{domain.ScopeWrite},
		CreatedAt: time.Now().UTC(),
	}
}

// seedMaintenanceRows inserts, in one transaction, a token plus one expired
// and one live session and idempotency record each, so a sweep has something
// to delete and something that must survive.
func seedMaintenanceRows(t *testing.T, st store.Store, tok *domain.Token, now time.Time) {
	t.Helper()
	past := now.Add(-2 * time.Hour)
	future := now.Add(2 * time.Hour)
	err := st.Write(context.Background(), func(tx store.Tx) error {
		if err := st.Tokens().Create(tx, tok); err != nil {
			return err
		}
		rows := []any{
			&domain.Session{ID: "sess-expired", TokenID: tok.ID, CreatedAt: past, LastSeenAt: past, ExpiresAt: past},
			&domain.Session{ID: "sess-live", TokenID: tok.ID, CreatedAt: now, LastSeenAt: now, ExpiresAt: future},
		}
		for _, r := range rows {
			if err := st.Sessions().Create(tx, r.(*domain.Session)); err != nil {
				return err
			}
		}
		if err := st.Idempotency().Put(tx, &domain.IdempotencyRecord{
			TokenID: tok.ID, Key: "key-expired", RequestHash: "h1", Response: []byte("{}"), ExpiresAt: past,
		}); err != nil {
			return err
		}
		return st.Idempotency().Put(tx, &domain.IdempotencyRecord{
			TokenID: tok.ID, Key: "key-live", RequestHash: "h2", Response: []byte("{}"), ExpiresAt: future,
		})
	})
	if err != nil {
		t.Fatalf("seed maintenance rows: %v", err)
	}
}

// TestMaintainOnce_SweepsExpiredRows is the maintenance contract on real
// SQLite: one pass deletes expired sessions and idempotency records, keeps
// the live ones, and uses the database clock to decide.
func TestMaintainOnce_SweepsExpiredRows(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	defer a.Close()

	// Pin "now" to the database clock so the seeded past/future rows are
	// unambiguous.
	var now time.Time
	if err := a.store.Read(context.Background(), func(tx store.Tx) error {
		ts, err := tx.Now()
		now = ts
		return err
	}); err != nil {
		t.Fatalf("read db clock: %v", err)
	}

	tok := testToken("maint-sweeper")
	seedMaintenanceRows(t, a.store, tok, now)
	a.maintainOnce(context.Background())

	if got := a.Maintenance(); got.Runs != 1 || got.LastErr != nil {
		t.Fatalf("maintenance stats = %+v, want Runs=1 LastErr=nil", got)
	}

	isGone := func(kind, tokenID, key string) bool {
		var missing bool
		err := a.store.Read(context.Background(), func(tx store.Tx) error {
			var err error
			if kind == "session" {
				_, err = a.store.Sessions().Get(tx, key)
			} else {
				_, err = a.store.Idempotency().Get(tx, tokenID, key)
			}
			if de := domain.AsError(err); de != nil && de.Code == domain.CodeNotFound {
				missing = true
				return nil
			}
			return err
		})
		if err != nil {
			t.Fatalf("probe %s %s: %v", kind, key, err)
		}
		return missing
	}
	if !isGone("session", tok.ID, "sess-expired") {
		t.Fatal("expired session survived the sweep")
	}
	if isGone("session", tok.ID, "sess-live") {
		t.Fatal("live session was swept")
	}
	if !isGone("idempotency", tok.ID, "key-expired") {
		t.Fatal("expired idempotency record survived the sweep")
	}
	if isGone("idempotency", tok.ID, "key-live") {
		t.Fatal("live idempotency record was swept")
	}
}

// TestMaintenanceLoop_RunsAndStops proves the lifecycle: the loop ticks,
// records passes, and exits promptly on cancellation — no goroutine outlives
// the shutdown that stops it.
func TestMaintenanceLoop_RunsAndStops(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	a.maintInterval = 2 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	a.startMaintenance(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a.Maintenance().Runs >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := a.Maintenance().Runs; got < 3 {
		t.Fatalf("maintenance ran %d passes in 3s at a 2ms tick, want >= 3", got)
	}

	cancel()
	a.stopMaintenance()

	done := make(chan struct{})
	go func() {
		a.maintWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance loop did not exit within 2s of stop")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close after loop stop: %v", err)
	}
}

// TestServe_RunsMaintenance: the loop is tied to serving — a serve/shutdown
// cycle leaves at least one recorded pass if the tick fires before cancel.
// This is deliberately lenient (timing-dependent otherwise); the strict
// lifecycle assertions live in TestMaintenanceLoop_RunsAndStops.
func TestServe_RunsMaintenance(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	a.maintInterval = 2 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.serve(ctx, ephemeralListener(t))
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && a.Maintenance().Runs == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if a.Maintenance().Runs == 0 {
		t.Fatal("maintenance never ran while serving")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
}
