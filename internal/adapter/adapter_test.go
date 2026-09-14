package adapter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeRunnerUsesSavedSession(t *testing.T) {
	runner := ClaudeRunner{}
	if err := runner.Deliver(context.Background(), "", QueuedMessage{}); err == nil {
		t.Fatal("delivery without a session succeeded")
	}
}

func TestDecodeFeedAcceptsEmptyPageWithCursor(t *testing.T) {
	page, err := decodeFeed(map[string]any{"messages": map[string]any{"messages": []any{}, "next_cursor": "cursor-1", "has_more": false}})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "cursor-1" || len(page.Messages) != 0 {
		t.Fatalf("page = %#v", page)
	}
}

func TestWorkerSessionAndCheckpointSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path, "https://board.example", "KANB", "worker-token")
	if err != nil {
		t.Fatal(err)
	}
	worker := Worker{WorkerID: "worker-a", Role: "implementer", Provider: "claude", SessionID: "session-a", WorkDir: "C:/work"}
	if err := store.UpsertWorker(worker); err != nil {
		t.Fatal(err)
	}
	checkpoint := Checkpoint{Done: "parser", Version: "v7", Unresolved: "review", NextStep: "run tests"}
	if err := store.RecordCheckpoint("worker-a", checkpoint); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, "https://board.example", "KANB", "worker-token")
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Snapshot().Workers
	if len(got) != 1 || got[0].SessionID != "session-a" || got[0].Checkpoint != checkpoint {
		t.Fatalf("workers = %#v", got)
	}
}

func TestRelatedTaskPrefersSavedSession(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"), "https://board.example", "KANB", "worker-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorker(Worker{WorkerID: "worker-a", Role: "implementer", Provider: "claude", SessionID: "session-a", WorkDir: "C:/work"}); err != nil {
		t.Fatal(err)
	}
	a, err := store.PrepareAssignment("worker-a", "KANB-50", true)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Resume || a.Worker.SessionID != "session-a" {
		t.Fatalf("assignment = %#v", a)
	}
}

func TestUnavailableResumeUsesVisibleCheckpointFallback(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"), "https://board.example", "KANB", "worker-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorker(Worker{WorkerID: "worker-a", Role: "implementer", Provider: "claude", SessionID: "session-a", WorkDir: "C:/work", Checkpoint: Checkpoint{Done: "done", NextStep: "test"}}); err != nil {
		t.Fatal(err)
	}
	a, err := store.PrepareAssignment("worker-a", "KANB-50", false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Resume || a.Fallback != "resume_unavailable_checkpoint_loaded" || a.Prompt == "" {
		t.Fatalf("assignment = %#v", a)
	}
	if got := store.Snapshot().Journal; len(got) != 1 || got[0].Kind != a.Fallback {
		t.Fatalf("journal = %#v", got)
	}
}

func TestSecondInstanceForSameIdentityRefuses(t *testing.T) {
	workDir := t.TempDir()
	first, err := AcquireInstance("worker-token", "session-a", workDir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := AcquireInstance("worker-token", "session-a", workDir); !errors.Is(err, ErrInstanceHeld) {
		t.Fatalf("second instance error = %v", err)
	}
}

func TestCapabilityTableHasNoImplicitSupport(t *testing.T) {
	capabilities := ConfirmedCapabilities()
	if len(capabilities) != 1 || capabilities[0].InputWhileActive == "" || capabilities[0].CrashRecovery == "" {
		t.Fatalf("capabilities = %#v", capabilities)
	}
}

func TestReceiveSurvivesCrashAfterPagePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path, "https://board.example", "KANB", "worker-token")
	if err != nil {
		t.Fatal(err)
	}
	page := FeedPage{Messages: []FeedMessage{{ID: "m1", Kind: "command", ResolvedExecutor: "worker-token", Body: "do the task"}}, NextCursor: "cursor-1"}
	if err := store.Receive(page, "session-a"); err != nil {
		t.Fatal(err)
	}
	// Reopening models a crash between receiving the page and contacting a CLI.
	reopened, err := OpenStore(path, "https://board.example", "KANB", "worker-token")
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Snapshot()
	if got.Cursor != "cursor-1" || len(got.Queue) != 1 || got.Queue[0].Delivery != DeliveryPending {
		t.Fatalf("state after crash = %#v", got)
	}
}

func TestAttemptIsDurableAndNeverAutomaticallyRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path, "https://board.example", "KANB", "worker-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Receive(FeedPage{Messages: []FeedMessage{{ID: "m1", Kind: "command", ResolvedExecutor: "worker-token", Body: "do work"}}, NextCursor: "c1"}, "session-a"); err != nil {
		t.Fatal(err)
	}
	runner := failingRunner{}
	if err := DeliverNext(context.Background(), store, runner, func() time.Time { return time.Unix(1, 0) }); err == nil {
		t.Fatal("DeliverNext succeeded")
	}
	reopened, err := OpenStore(path, "https://board.example", "KANB", "worker-token")
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Snapshot().Queue[0].Delivery; got != DeliveryAttempting {
		t.Fatalf("delivery = %q, want attempting", got)
	}
	called := false
	if err := DeliverNext(context.Background(), reopened, runnerFunc(func(context.Context, string, QueuedMessage) error { called = true; return nil }), time.Now); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("an unconfirmed attempt was retried automatically")
	}
}

func TestReceiveDeduplicatesMessageAndSession(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"), "https://board.example", "KANB", "worker-token")
	if err != nil {
		t.Fatal(err)
	}
	page := FeedPage{Messages: []FeedMessage{{ID: "m1", Kind: "command", ResolvedExecutor: "worker-token"}}, NextCursor: "c1"}
	if err := store.Receive(page, "session-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Receive(page, "session-a"); err != nil {
		t.Fatal(err)
	}
	if got := len(store.Snapshot().Queue); got != 1 {
		t.Fatalf("queue has %d entries, want 1", got)
	}
}

func TestStopCommandsArePrioritized(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"), "https://board.example", "KANB", "worker-token")
	if err != nil {
		t.Fatal(err)
	}
	page := FeedPage{Messages: []FeedMessage{{ID: "work", Kind: "command", ResolvedExecutor: "worker-token", Body: "work"}, {ID: "stop", Kind: "command", ResolvedExecutor: "worker-token", Body: "stop now"}}, NextCursor: "c1"}
	if err := store.Receive(page, "session-a"); err != nil {
		t.Fatal(err)
	}
	if got := store.NextDeliverable(); got == nil || got.ID != "stop" {
		t.Fatalf("next = %#v", got)
	}
}

type failingRunner struct{}

func (failingRunner) Deliver(context.Context, string, QueuedMessage) error {
	return errors.New("crashed after CLI handoff")
}

type runnerFunc func(context.Context, string, QueuedMessage) error

func (f runnerFunc) Deliver(ctx context.Context, session string, msg QueuedMessage) error {
	return f(ctx, session, msg)
}
