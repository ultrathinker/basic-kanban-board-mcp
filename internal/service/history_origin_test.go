package service

import (
	"context"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// readOrigin reads the lifecycle journal's origin straight from the store, so
// a test can check that what the service hands out is that value and not a
// coincidentally similar one.
func readOrigin(t *testing.T, env *testEnv) time.Time {
	t.Helper()
	var at time.Time
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		var err error
		at, err = env.TaskHistory().Origin(tx)
		return err
	}); err != nil {
		t.Fatalf("read journal origin: %v", err)
	}
	return at
}

// KANB-30 acceptance 4 and 6: the moment from which the lifecycle journal is
// trustworthy is handed OUT of the service, through the service.Service
// interface — no caller reaches into the store for it and no caller has to
// type-assert its way to a concrete type to find it.
func TestProjectProgress_ExposesHistoryStart(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	var svc Service = env.svc // the interface is the whole surface
	got, err := svc.ProjectProgress(ctx, env.actor, ProjectProgressInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ProjectProgress: %v", err)
	}
	if got.HistoryStartsAt.IsZero() {
		t.Fatal("HistoryStartsAt is zero: the service is not reporting when accurate data begins")
	}

	// It is the journal's own origin, not some convenient nearby timestamp.
	origin := readOrigin(t, env)
	if !got.HistoryStartsAt.Equal(origin) {
		t.Fatalf("HistoryStartsAt = %s, want the journal origin %s", got.HistoryStartsAt, origin)
	}
}
