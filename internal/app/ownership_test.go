package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// TestOwnerLock_SecondAcquireRefused is the core lock property: two open
// handles on the same lock file cannot both hold it, even inside one process
// (distinct descriptors), which is what makes the double-New test meaningful
// on a single machine.
func TestOwnerLock_SecondAcquireRefused(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireOwnerLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := AcquireOwnerLock(dir)
	if err == nil {
		_ = second.Release()
		t.Fatal("second acquire must refuse")
	}
	if !errors.Is(err, ErrOwnerHeld) {
		t.Fatalf("err = %v, want ErrOwnerHeld", err)
	}
	var held *OwnerHeldError
	if !errors.As(err, &held) {
		t.Fatalf("err = %T, want *OwnerHeldError", err)
	}
	if want := filepath.Join(dir, "kanban.lock"); held.LockPath != want {
		t.Fatalf("LockPath = %q, want %q", held.LockPath, want)
	}
	if want := filepath.Join(dir, "kanban.db"); held.DBPath != want {
		t.Fatalf("DBPath = %q, want %q", held.DBPath, want)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	third, err := AcquireOwnerLock(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v (a released lock must not linger)", err)
	}
	_ = third.Release()
}

// TestOwnerLock_StaleFileDoesNotBrick proves the crash-safety property: a
// lock file left behind by a killed process is inert. Ownership lives in the
// OS lock, never in the file's existence or contents.
func TestOwnerLock_StaleFileDoesNotBrick(t *testing.T) {
	dir := t.TempDir()
	// Simulate the aftermath of a kill: the lock file exists, possibly with a
	// stale PID, and nobody holds the OS lock.
	lockPath := filepath.Join(dir, ownerLockName)
	if err := os.WriteFile(lockPath, []byte("pid=999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireOwnerLock(dir)
	if err != nil {
		t.Fatalf("a stale lock file must not block startup: %v", err)
	}
	_ = lock.Release()
}

// TestOpenCLIStore_FreeDirectoryMigratesAndHoldsLock: when no server owns the
// directory, a CLI command takes ownership, applies migrations (a fresh
// install must work from the CLI alone) and holds the lock until Close.
func TestOpenCLIStore_FreeDirectoryMigratesAndHoldsLock(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	st, err := OpenCLIStore(ctx, dir)
	if err != nil {
		t.Fatalf("OpenCLIStore on a free dir: %v", err)
	}

	// The schema must already exist — migrations ran under the CLI's own
	// ownership.
	var tokens int
	if err := st.Read(ctx, func(tx store.Tx) error {
		n, err := st.Tokens().Count(tx)
		tokens = n
		return err
	}); err != nil {
		t.Fatalf("count tokens (schema missing?): %v", err)
	}
	if tokens != 0 {
		t.Fatalf("fresh database has %d tokens, want 0", tokens)
	}

	// While the CLI store is open, it owns the directory.
	if _, err := AcquireOwnerLock(dir); !errors.Is(err, ErrOwnerHeld) {
		t.Fatalf("expected ErrOwnerHeld while CLI store open, got %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	lock, err := AcquireOwnerLock(dir)
	if err != nil {
		t.Fatalf("ownership not released on Close: %v", err)
	}
	_ = lock.Release()
}

// TestOpenCLIStore_AttachesUnderLiveServer: when a server owns the
// directory, a CLI command must NOT refuse (minting a token beside a live
// server is normal operations) and must NOT migrate under it — it attaches
// read/write and lets SQLite's cross-process serialization do its job.
func TestOpenCLIStore_AttachesUnderLiveServer(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// The "server": a migrated database plus a held ownership lock, exactly
	// what a running kanban serve looks like from outside.
	server, err := store.Open(ctx, store.Config{Path: databasePath(dir)})
	if err != nil {
		t.Fatalf("server store open: %v", err)
	}
	_ = server.Close()
	owner, err := AcquireOwnerLock(dir)
	if err != nil {
		t.Fatalf("server takes ownership: %v", err)
	}
	defer owner.Release()

	st, err := OpenCLIStore(ctx, dir)
	if err != nil {
		t.Fatalf("OpenCLIStore under a live server must attach, got %v", err)
	}
	defer st.Close()

	// Attached ≠ crippled: a full write round trip works.
	tok := &domain.Token{
		ID:        uuid.NewString(),
		Name:      "cli-attached",
		Hash:      auth.HashToken("kbn_cli_attached00xx00xx00xx00xx00xx00xx"),
		Scopes:    domain.Scopes{domain.ScopeWrite},
		CreatedAt: time.Now().UTC(),
	}
	if err := st.Write(ctx, func(tx store.Tx) error {
		return st.Tokens().Create(tx, tok)
	}); err != nil {
		t.Fatalf("attached write: %v", err)
	}
	var got *domain.Token
	if err := st.Read(ctx, func(tx store.Tx) error {
		g, err := st.Tokens().GetByName(tx, "cli-attached")
		got = g
		return err
	}); err != nil || got == nil {
		t.Fatalf("attached read: (%v, %v)", got, err)
	}
}
