package app

import (
	"context"
	"errors"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// maintenanceInterval is how often the single maintenance loop runs its sweep.
// 15 minutes keeps a long-lived installation's WAL, expired sessions and
// expired idempotency rows bounded without any operator action — the "one
// binary, zero maintenance" promise. It is a field on App (defaulted to this
// constant) only so tests can shrink it; it is not configuration surface.
const maintenanceInterval = 15 * time.Minute

// maintenanceStopWait bounds how long shutdown waits for an in-flight
// maintenance pass. The pass's context is already cancelled by then, so this
// is a safety valve against a wedged driver, not an expected wait.
const maintenanceStopWait = 5 * time.Second

// MaintenanceStats is a snapshot of the maintenance loop's history: what ran,
// when, and whether the last pass was clean. Errors here never stop the loop —
// the next tick retries — but they must be visible rather than swallowed, so
// they are recorded instead of logged-and-forgotten (this process has no
// logger yet; when one lands, these stats are its feed).
type MaintenanceStats struct {
	Runs    int
	LastAt  time.Time
	LastErr error
}

// startMaintenance launches the single background loop. It is called from
// serve, so an App that never serves also never runs maintenance — there is
// nothing to maintain in a composition used only for wiring.
func (a *App) startMaintenance(ctx context.Context) {
	a.maintWG.Add(1)
	go a.maintenanceLoop(ctx)
}

// stopMaintenance signals the loop to exit and waits for it, bounded by
// maintenanceStopWait. Safe to call multiple times and when the loop was never
// started. Shutdown paths call it BEFORE closing the store: a sweep writing
// to a closed database is exactly the kind of quiet corruption this
// lifecycle exists to prevent.
func (a *App) stopMaintenance() {
	a.maintStopOnce.Do(func() { close(a.maintStop) })
	done := make(chan struct{})
	go func() {
		a.maintWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(maintenanceStopWait):
		// Bounded: shutdown must not hang on a stuck pass. Its context is
		// cancelled, so it cannot commit anything new after this point.
	}
}

// maintenanceLoop is the lifecycle owner of every background job: one
// goroutine, one ticker, one stop signal. Jobs are sequential by design —
// they all want the single writer anyway.
func (a *App) maintenanceLoop(ctx context.Context) {
	defer a.maintWG.Done()
	ticker := time.NewTicker(a.maintInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.maintStop:
			return
		case <-ticker.C:
			a.maintainOnce(ctx)
		}
	}
}

// maintainOnce runs one pass: delete expired session and idempotency rows in
// one write transaction (the database clock decides "expired", never the
// process clock), then checkpoint the WAL. The sweep goes through the
// admission boundary like every other write — if the writer is saturated,
// maintenance yields its turn and retries on the next tick.
//
// The checkpoint runs last so the sweep's own WAL frames are folded into the
// same pass. TRUNCATE can fail while readers are active; that is normal on a
// live server and is recorded, not fatal — the next pass tries again.
func (a *App) maintainOnce(ctx context.Context) {
	sweepErr := a.gate.Write(ctx, func(tx store.Tx) error {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		if _, err := a.gate.Sessions().DeleteExpired(tx, now); err != nil {
			return err
		}
		_, err = a.gate.Idempotency().DeleteExpired(tx, now)
		return err
	})
	checkpointErr := a.gate.Checkpoint(ctx)
	if ctx.Err() != nil {
		// Shutdown-time failures are not maintenance health signals; the
		// context that died took the pass with it.
		return
	}
	a.recordMaintenance(errors.Join(sweepErr, checkpointErr))
}

// recordMaintenance stamps one completed pass. A nil err means a clean pass.
func (a *App) recordMaintenance(err error) {
	a.maintMu.Lock()
	defer a.maintMu.Unlock()
	a.maintRuns++
	a.maintLastAt = time.Now().UTC()
	a.maintLastErr = err
}

// Maintenance snapshots the loop's stats. Intended for tests and a future
// /readyz or doctor wiring inside the serving process.
func (a *App) Maintenance() MaintenanceStats {
	a.maintMu.Lock()
	defer a.maintMu.Unlock()
	return MaintenanceStats{Runs: a.maintRuns, LastAt: a.maintLastAt, LastErr: a.maintLastErr}
}
