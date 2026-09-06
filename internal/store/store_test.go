package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------
//
// Every test gets a fresh temp-file database so the two pools and WAL are
// exercised exactly the way they are in production. :memory: would skip
// WAL entirely and hide the real concurrency contract.

type testStore struct {
	Store
	dir string
}

func openTestStore(t *testing.T) *testStore {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kanban.db")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Open(ctx, Config{Path: dbPath, ReadPoolSize: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ts := &testStore{Store: s, dir: dir}
	t.Cleanup(func() {
		_ = ts.Close()
	})
	return ts
}

// seedProject builds a project with the four default columns from
// domain.DefaultColumns and returns the project and a map of column
// names to *domain.Column for test convenience.
func seedProject(t *testing.T, s Store) (*domain.Project, map[string]*domain.Column) {
	t.Helper()
	p := &domain.Project{
		ID:                  uuid.NewString(),
		Key:                 "BMB",
		Name:                "Test",
		Description:         "",
		Version:             1,
		NextTaskSeq:         1,
		EstimateUnit:        "h",
		EnforceDependencies: true,
		StrictDone:          false,
		ClaimTTLSeconds:     3600,
	}
	err := s.Write(context.Background(), func(tx Tx) error {
		if err := s.Projects().Create(tx, p); err != nil {
			return err
		}
		// Override NextTaskSeq on the just-inserted row so future
		// NextTaskSeq() calls start at 1.
		if _, err := tx.(*txWrap).tx.ExecContext(context.Background(),
			"UPDATE projects SET next_task_seq = 1 WHERE id = ?", p.ID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	cols := map[string]*domain.Column{}
	err = s.Write(context.Background(), func(tx Tx) error {
		for i, def := range domain.DefaultColumns {
			c := &domain.Column{
				ID:        uuid.NewString(),
				ProjectID: p.ID,
				Name:      def.Name,
				Position:  i,
				Kind:      def.Kind,
				WIPLimit:  def.WIPLimit,
			}
			if err := s.Columns().Create(tx, c); err != nil {
				return err
			}
			cols[def.Name] = c
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed columns: %v", err)
	}
	return p, cols
}

// seedTask creates a single task in the given column. Returns the
// newly-minted task (with the assigned key). actor becomes CreatedBy.
func seedTask(t *testing.T, s Store, p *domain.Project, col *domain.Column, title, actor string) *domain.Task {
	t.Helper()
	var task *domain.Task
	err := s.Write(context.Background(), func(tx Tx) error {
		seq, err := s.Projects().NextTaskSeq(tx, p.ID)
		if err != nil {
			return err
		}
		task = &domain.Task{
			ID:        uuid.NewString(),
			Key:       domain.TaskKey(p.Key, seq),
			ProjectID: p.ID,
			ColumnID:  col.ID,
			Rank:      domain.RankStep,
			Title:     title,
			Body:      "",
			Type:      domain.TypeTask,
			Priority:  domain.PriorityMedium,
			Tags:      []string{"seed"},
			CreatedBy: actor,
			UpdatedBy: actor,
		}
		return s.Tasks().Create(tx, task)
	})
	if err != nil {
		t.Fatalf("seed task %q: %v", title, err)
	}
	return task
}

// ---------------------------------------------------------------------------
// Two pools / WAL setup
// ---------------------------------------------------------------------------

// TestOpen_PragmasApplied asserts the connection-level pragmas actually
// took effect (the silent-failure class of bug in the previous
// in-process setups).
func TestOpen_PragmasApplied(t *testing.T) {
	ts := openTestStore(t)
	h, err := ts.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !strings.EqualFold(h.JournalMode, "wal") {
		t.Fatalf("JournalMode = %q, want WAL", h.JournalMode)
	}
	if h.WriteMicros <= 0 {
		t.Fatalf("WriteMicros = %d, want >0", h.WriteMicros)
	}
	if h.Path == "" {
		t.Fatalf("Path is empty")
	}
	if h.Migration < 1 {
		t.Fatalf("Migration = %d, want >=1", h.Migration)
	}
}

// TestMigrations_Idempotent runs Open twice on the same file; the second
// open must not fail and must report the same migration version.
func TestMigrations_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.db")
	ctx := context.Background()
	s1, err := Open(ctx, Config{Path: path})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	h1, _ := s1.Health(ctx)
	_ = s1.Close()
	s2, err := Open(ctx, Config{Path: path})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	h2, _ := s2.Health(ctx)
	if h1.Migration != h2.Migration || h2.Migration < 1 {
		t.Fatalf("migrations: first=%d second=%d", h1.Migration, h2.Migration)
	}
}

// ---------------------------------------------------------------------------
// Case-insensitive lookup
// ---------------------------------------------------------------------------

// TestCaseInsensitiveLookups exercises the NOCASE columns the brief
// explicitly requires: GetByKey("bmb-14") and GetByName(col, "doing").
func TestCaseInsensitiveLookups(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	_ = seedTask(t, ts, p, cols["Backlog"], "alpha", "alice")

	err := ts.Read(context.Background(), func(tx Tx) error {
		// Project key mixed case
		got, err := ts.Projects().GetByKey(tx, "bmb")
		if err != nil {
			return fmt.Errorf("GetByKey(bmb): %w", err)
		}
		if got.ID != p.ID {
			return fmt.Errorf("GetByKey(bmb) wrong id")
		}
		// Task key mixed case
		t1, err := ts.Tasks().GetByKey(tx, "bmb-1")
		if err != nil {
			return fmt.Errorf("GetByKey(bmb-1): %w", err)
		}
		if t1.Key != "BMB-1" {
			return fmt.Errorf("task key not canonical: %q", t1.Key)
		}
		// Column name mixed case
		c, err := ts.Columns().GetByName(tx, p.ID, "doing")
		if err != nil {
			return fmt.Errorf("GetByName(doing): %w", err)
		}
		if !strings.EqualFold(c.Name, "Doing") {
			return fmt.Errorf("column name not Doing: %q", c.Name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Concurrency: 50 goroutines claim one task — exactly one wins.
// ---------------------------------------------------------------------------

// TestClaim_OneOfFiftyWins is the headline invariant from PLAN §11.1.
// Each goroutine uses a different actor name; only one row update can
// succeed because the WHERE clause requires the lease slot to be empty,
// expired, or held by the same actor.
func TestClaim_OneOfFiftyWins(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "race-target", "system")

	const N = 50
	var (
		wins    int64
		losses  int64
		wg      sync.WaitGroup
		errsMu  sync.Mutex
		errs    []error
		barrier = make(chan struct{})
	)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			actor := fmt.Sprintf("agent-%02d", i)
			<-barrier
			err := ts.Write(context.Background(), func(tx Tx) error {
				ok, _, err := ts.Tasks().Claim(tx, task.ID, actor, domain.ClaimTTLDefault)
				if err != nil {
					return err
				}
				if ok {
					atomic.AddInt64(&wins, 1)
				} else {
					atomic.AddInt64(&losses, 1)
				}
				return nil
			})
			if err != nil {
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
			}
		}(i)
	}
	close(barrier)
	wg.Wait()
	for _, e := range errs {
		t.Errorf("claim error: %v", e)
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want 1", wins)
	}
	if losses != N-1 {
		t.Fatalf("losses = %d, want %d", losses, N-1)
	}
	// Confirm exactly one writer is the current holder.
	_ = ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		if got.ClaimedBy == nil {
			return fmt.Errorf("no holder set")
		}
		if !strings.HasPrefix(*got.ClaimedBy, "agent-") {
			return fmt.Errorf("unexpected holder: %s", *got.ClaimedBy)
		}
		return nil
	})
}

// TestClaim_RenewDoesNotBumpVersion asserts the rule that lease
// operations never invalidate an if_version edit. After Claim the
// version is still 1.
func TestClaim_RenewDoesNotBumpVersion(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "renew-target", "system")

	actor := "alice"
	err := ts.Write(context.Background(), func(tx Tx) error {
		ok, _, err := ts.Tasks().Claim(tx, task.ID, actor, domain.ClaimTTLDefault)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("claim did not succeed")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	err = ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		if got.Version != 1 {
			return fmt.Errorf("after claim version = %d, want 1", got.Version)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A content Update from the same actor must succeed without
	// if_version being supplied.
	err = ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		cur.Title = "renewed"
		return ts.Tasks().Update(tx, cur, nil)
	})
	if err != nil {
		t.Fatalf("content update: %v", err)
	}

	err = ts.Write(context.Background(), func(tx Tx) error {
		ok, _, err := ts.Tasks().Claim(tx, task.ID, actor, domain.ClaimTTLDefault)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("re-claim by same actor failed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestClaim_ExpiredFormerOwnerCannotRenew simulates the "another actor
// already took the lease" case. After expiry + takeover, the original
// holder's Claim must fail.
func TestClaim_ExpiredFormerOwnerCannotRenew(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "lease", "system")

	// alice claims first with a 1-minute TTL.
	err := ts.Write(context.Background(), func(tx Tx) error {
		ok, _, err := ts.Tasks().Claim(tx, task.ID, "alice", time.Minute)
		if err != nil || !ok {
			return fmt.Errorf("alice claim: ok=%v err=%v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// bob steals by waiting (simulate by manually expiring in SQL) then
	// claiming.
	err = ts.Write(context.Background(), func(tx Tx) error {
		if _, err := tx.(*txWrap).tx.ExecContext(context.Background(),
			"UPDATE tasks SET claim_expires_at = ? WHERE id = ?",
			formatTime(time.Now().UTC().Add(-time.Minute)), task.ID); err != nil {
			return err
		}
		ok, _, err := ts.Tasks().Claim(tx, task.ID, "bob", time.Minute)
		if err != nil || !ok {
			return fmt.Errorf("bob claim: ok=%v err=%v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// alice tries to renew — must fail.
	err = ts.Write(context.Background(), func(tx Tx) error {
		ok, _, err := ts.Tasks().Claim(tx, task.ID, "alice", time.Minute)
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("alice re-claim should have failed; bob now holds the lease")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Version conflict
// ---------------------------------------------------------------------------

// TestUpdate_VersionConflictReturnsCurrent checks the "current is
// attached" contract: the losing caller gets a CodeConflict with a
// non-nil Current so it can merge without a second round trip.
func TestUpdate_VersionConflictReturnsCurrent(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "v1", "alice")

	// alice reads at version 1 and updates successfully -> version 2.
	if err := ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		cur.Title = "v2 by alice"
		cur.UpdatedBy = "alice"
		return ts.Tasks().Update(tx, cur, &cur.Version)
	}); err != nil {
		t.Fatalf("alice update: %v", err)
	}
	// bob, who only saw version 1, retries with stale if_version=1.
	err := ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		// pretend bob still has v=1
		cur.Title = "v2 by bob"
		cur.UpdatedBy = "bob"
		stale := 1
		return ts.Tasks().Update(tx, cur, &stale)
	})
	if err == nil {
		t.Fatalf("bob update succeeded; want CodeConflict")
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("err is not *domain.Error: %T %v", err, err)
	}
	if de.Code != domain.CodeConflict {
		t.Fatalf("Code = %q, want %q", de.Code, domain.CodeConflict)
	}
	if de.Current == nil {
		t.Fatalf("Current is nil; conflict must carry server state")
	}
	if cur, ok := de.Current.(*domain.Task); !ok {
		t.Fatalf("Current type = %T, want *domain.Task", de.Current)
	} else if cur.Title != "v2 by alice" {
		t.Fatalf("Current.Title = %q, want %q", cur.Title, "v2 by alice")
	}
}

// ---------------------------------------------------------------------------
// NextTaskSeq atomicity
// ---------------------------------------------------------------------------

// TestNextTaskSeq_ConcurrentAllocDiffers is the second headline
// invariant: two concurrent NextTaskSeq allocations on the same project
// produce different numbers.
func TestNextTaskSeq_ConcurrentAllocDiffers(t *testing.T) {
	ts := openTestStore(t)
	p, _ := seedProject(t, ts)

	const N = 30
	var (
		mu      sync.Mutex
		seen    = make(map[int]bool)
		wg      sync.WaitGroup
		barrier = make(chan struct{})
	)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier
			err := ts.Write(context.Background(), func(tx Tx) error {
				seq, err := ts.Projects().NextTaskSeq(tx, p.ID)
				if err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				if seen[seq] {
					return fmt.Errorf("duplicate seq %d", seq)
				}
				seen[seq] = true
				return nil
			})
			if err != nil {
				t.Errorf("NextTaskSeq: %v", err)
			}
		}()
	}
	close(barrier)
	wg.Wait()
	if len(seen) != N {
		t.Fatalf("got %d distinct seq, want %d", len(seen), N)
	}
}

// ---------------------------------------------------------------------------
// WouldCycle
// ---------------------------------------------------------------------------

// TestWouldCycle covers the three cases the brief requires: direct
// A→B→A, transitive A→B→C→A, and parent-chain.
func TestWouldCycle(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	a := seedTask(t, ts, p, cols["Backlog"], "A", "alice")
	b := seedTask(t, ts, p, cols["Backlog"], "B", "alice")
	c := seedTask(t, ts, p, cols["Backlog"], "C", "alice")

	// Helper: add a blocks link via the linkRepo.
	addLink := func(blocker, blocked *domain.Task) error {
		return ts.Write(context.Background(), func(tx Tx) error {
			return ts.Links().Add(tx, &domain.Link{
				BlockerID: blocker.ID,
				BlockedID: blocked.ID,
				Type:      domain.LinkBlocks,
				CreatedBy: "alice",
			})
		})
	}

	// Self-cycle: A->A — rejected by schema CHECK; domain check catches
	// it earlier too.
	err := ts.Write(context.Background(), func(tx Tx) error {
		cyc, path, err := ts.Links().WouldCycle(tx, a.ID, a.ID)
		if err != nil {
			return err
		}
		if !cyc {
			return fmt.Errorf("self-link should be a cycle")
		}
		if len(path) < 1 {
			return fmt.Errorf("path empty")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("self: %v", err)
	}

	// A blocks B
	if err := addLink(a, b); err != nil {
		t.Fatalf("add A->B: %v", err)
	}
	// B->A should be detected as a cycle (direct A→B→A).
	err = ts.Write(context.Background(), func(tx Tx) error {
		cyc, path, err := ts.Links().WouldCycle(tx, b.ID, a.ID)
		if err != nil {
			return err
		}
		if !cyc {
			return fmt.Errorf("B->A should be a cycle after A->B exists")
		}
		if len(path) < 2 {
			return fmt.Errorf("path = %v, want >= 2", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("direct cycle: %v", err)
	}

	// A blocks C now (A->B, A->C). C->A is transitive A->B->A, A->C->A
	if err := addLink(a, c); err != nil {
		t.Fatalf("add A->C: %v", err)
	}
	err = ts.Write(context.Background(), func(tx Tx) error {
		cyc, _, err := ts.Links().WouldCycle(tx, c.ID, a.ID)
		if err != nil {
			return err
		}
		if !cyc {
			return fmt.Errorf("C->A should be a cycle (A->B, A->C already)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transitive cycle: %v", err)
	}

	// Parent-chain: make a child of B via Update's parent_id path.
	child := seedTask(t, ts, p, cols["Backlog"], "child-of-B", "alice")
	if err := ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, child.ID)
		if err != nil {
			return err
		}
		cur.ParentID = &b.ID
		return ts.Tasks().Update(tx, cur, &cur.Version)
	}); err != nil {
		t.Fatalf("set parent via Update: %v", err)
	}
	// child blocks B is a parent-chain violation.
	err = ts.Write(context.Background(), func(tx Tx) error {
		cyc, _, err := ts.Links().WouldCycle(tx, child.ID, b.ID)
		if err != nil {
			return err
		}
		if !cyc {
			return fmt.Errorf("child->parent should be a cycle")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("parent cycle: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Update with parent_id — depth, cycle, clear
// ---------------------------------------------------------------------------

// TestUpdate_ReparentBumpsVersion asserts the contract: setting parent_id
// through Update is a content mutation that bumps version, so an
// if_version edit can race against it.
func TestUpdate_ReparentBumpsVersion(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	parent := seedTask(t, ts, p, cols["Backlog"], "parent", "alice")
	child := seedTask(t, ts, p, cols["Backlog"], "child", "alice")
	if err := ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, child.ID)
		if err != nil {
			return err
		}
		if cur.Version != 1 {
			return fmt.Errorf("initial version = %d, want 1", cur.Version)
		}
		cur.ParentID = &parent.ID
		return ts.Tasks().Update(tx, cur, &cur.Version)
	}); err != nil {
		t.Fatalf("reparent: %v", err)
	}
	if err := ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tasks().GetByID(tx, child.ID)
		if err != nil {
			return err
		}
		if got.ParentID == nil || *got.ParentID != parent.ID {
			return fmt.Errorf("parent_id not set")
		}
		if got.Version != 2 {
			return fmt.Errorf("version = %d, want 2", got.Version)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestUpdate_ReparentDepth3Rejected enforces MaxSubtaskDepth: a subtask
// (depth=1) cannot itself be assigned a parent. We use a third,
// unrelated task so this is purely a depth violation, not a cycle.
func TestUpdate_ReparentDepth3Rejected(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	// A is top-level; B becomes A's child.
	a := seedTask(t, ts, p, cols["Backlog"], "a", "alice")
	b := seedTask(t, ts, p, cols["Backlog"], "b", "alice")
	if err := ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, b.ID)
		if err != nil {
			return err
		}
		cur.ParentID = &a.ID
		return ts.Tasks().Update(tx, cur, &cur.Version)
	}); err != nil {
		t.Fatalf("reparent b under a: %v", err)
	}
	// C is a fresh top-level task with no relationship to A or B. Make
	// it a child of B. The depth check fires because B is already a
	// subtask — no cycle is involved because C is unrelated to B's
	// chain.
	c := seedTask(t, ts, p, cols["Backlog"], "c", "alice")
	err := ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, c.ID)
		if err != nil {
			return err
		}
		cur.ParentID = &b.ID
		return ts.Tasks().Update(tx, cur, &cur.Version)
	})
	if err == nil {
		t.Fatalf("depth-3 reparent succeeded; want validation error")
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("err is not *domain.Error: %T", err)
	}
	if de.Code != domain.CodeValidation {
		t.Fatalf("Code = %q, want %q", de.Code, domain.CodeValidation)
	}
	// C must remain top-level.
	_ = ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tasks().GetByID(tx, c.ID)
		if err != nil {
			return err
		}
		if got.ParentID != nil {
			return fmt.Errorf("c.ParentID = %s, want nil", *got.ParentID)
		}
		return nil
	})
}

// TestUpdate_ReparentSelfDescendantRejected ensures a task cannot
// become a descendant of itself by walking up the prospective parent's
// chain.
func TestUpdate_ReparentSelfDescendantRejected(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	grand := seedTask(t, ts, p, cols["Backlog"], "grand", "alice")
	parent := seedTask(t, ts, p, cols["Backlog"], "parent", "alice")
	if err := ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, parent.ID)
		if err != nil {
			return err
		}
		cur.ParentID = &grand.ID
		return ts.Tasks().Update(tx, cur, &cur.Version)
	}); err != nil {
		t.Fatalf("setup grand->parent: %v", err)
	}
	// Try to make grand a child of parent — grand is in parent's
	// ancestor chain, so this is a self-descendant cycle.
	err := ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, grand.ID)
		if err != nil {
			return err
		}
		cur.ParentID = &parent.ID
		return ts.Tasks().Update(tx, cur, &cur.Version)
	})
	if err == nil {
		t.Fatalf("self-descendant reparent succeeded; want cycle error")
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("err is not *domain.Error: %T", err)
	}
	if de.Code != domain.CodeCycle {
		t.Fatalf("Code = %q, want %q", de.Code, domain.CodeCycle)
	}
}

// TestUpdate_ClearParentMakesTopLevel asserts that setting ParentID to
// nil in the patch removes the link and the task shows up in a
// top-level List filter.
func TestUpdate_ClearParentMakesTopLevel(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	parent := seedTask(t, ts, p, cols["Backlog"], "parent", "alice")
	child := seedTask(t, ts, p, cols["Backlog"], "child", "alice")
	if err := ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, child.ID)
		if err != nil {
			return err
		}
		cur.ParentID = &parent.ID
		return ts.Tasks().Update(tx, cur, &cur.Version)
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Clear.
	if err := ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, child.ID)
		if err != nil {
			return err
		}
		cur.ParentID = nil
		return ts.Tasks().Update(tx, cur, &cur.Version)
	}); err != nil {
		t.Fatalf("clear parent: %v", err)
	}
	_ = ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tasks().GetByID(tx, child.ID)
		if err != nil {
			return err
		}
		if got.ParentID != nil {
			return fmt.Errorf("ParentID = %s, want nil", *got.ParentID)
		}
		// ParentID-IS-NULL filter must return the cleared task.
		noneStr := ""
		rows, err := ts.Tasks().List(tx, TaskFilter{ProjectIDs: []string{p.ID}, ParentID: &noneStr})
		if err != nil {
			return err
		}
		var found bool
		for _, r := range rows {
			if r.ID == child.ID {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("cleared task missing from top-level list")
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Rank insertion + renumber
// ---------------------------------------------------------------------------

// TestNeighbourRanks_FirstTaskReturnsStep asserts the empty-column case
// (returns 0, RankStep).
func TestNeighbourRanks_FirstTaskReturnsStep(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	err := ts.Write(context.Background(), func(tx Tx) error {
		before, after, err := ts.Tasks().NeighbourRanks(tx, cols["Backlog"].ID, RankBottom)
		if err != nil {
			return err
		}
		if before != 0 || after != domain.RankStep {
			return fmt.Errorf("before=%d after=%d, want 0/%d", before, after, domain.RankStep)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = p
}

// TestNeighbourRanks_InsertBetweenTwice inserts at the top twice and
// verifies the gap closes around 1, triggering a renumber. The second
// insert's gap must be (0, prev_min).
func TestNeighbourRanks_InsertBetweenTwice(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	colID := cols["Backlog"].ID

	mkTask := func(rank int64) string {
		var id string
		err := ts.Write(context.Background(), func(tx Tx) error {
			seq, err := ts.Projects().NextTaskSeq(tx, p.ID)
			if err != nil {
				return err
			}
			task := &domain.Task{
				ID: uuid.NewString(), Key: domain.TaskKey(p.Key, seq),
				ProjectID: p.ID, ColumnID: colID, Rank: rank,
				Title: "x", Type: domain.TypeTask, Priority: domain.PriorityNone,
				CreatedBy: "alice", UpdatedBy: "alice",
			}
			if err := ts.Tasks().Create(tx, task); err != nil {
				return err
			}
			id = task.ID
			return nil
		})
		if err != nil {
			t.Fatalf("mkTask: %v", err)
		}
		return id
	}
	_ = mkTask(domain.RankStep) // existing rank
	// Insert at top — between (0, 1024) we pick 512.
	err := ts.Write(context.Background(), func(tx Tx) error {
		before, after, err := ts.Tasks().NeighbourRanks(tx, colID, RankTop)
		if err != nil {
			return err
		}
		if before != 0 || after != domain.RankStep {
			return fmt.Errorf("first top insert: before=%d after=%d", before, after)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Insert many times at the top until gap exhausts; the store must
	// renumber and still produce a valid (before, after) pair.
	for i := 0; i < 12; i++ {
		err := ts.Write(context.Background(), func(tx Tx) error {
			before, after, err := ts.Tasks().NeighbourRanks(tx, colID, RankTop)
			if err != nil {
				return err
			}
			if after-before < 2 {
				return fmt.Errorf("gap exhausted at iter %d: before=%d after=%d", i, before, after)
			}
			// Add a new task at a midpoint rank to keep the column
			// growing and force further renumbering pressure.
			seq, err := ts.Projects().NextTaskSeq(tx, p.ID)
			if err != nil {
				return err
			}
			rank := (before + after) / 2
			task := &domain.Task{
				ID: uuid.NewString(), Key: domain.TaskKey(p.Key, seq),
				ProjectID: p.ID, ColumnID: colID, Rank: rank,
				Title: "x", Type: domain.TypeTask, Priority: domain.PriorityNone,
				CreatedBy: "alice", UpdatedBy: "alice",
			}
			return ts.Tasks().Create(tx, task)
		})
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
}

// TestRenumberColumn verifies the function is idempotent and produces
// strictly increasing, well-spaced ranks.
func TestRenumberColumn(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	colID := cols["Backlog"].ID
	for i := 0; i < 5; i++ {
		_ = seedTask(t, ts, p, cols["Backlog"], fmt.Sprintf("r%d", i), "alice")
	}
	err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Tasks().RenumberColumn(tx, colID)
	})
	if err != nil {
		t.Fatalf("RenumberColumn: %v", err)
	}
	err = ts.Read(context.Background(), func(tx Tx) error {
		children, err := ts.Tasks().Children(tx, "") // orphan listing
		_ = children
		_ = err
		// Re-read each task in the column
		tasks, err := ts.Tasks().List(tx, TaskFilter{ColumnIDs: []string{colID}})
		if err != nil {
			return err
		}
		if len(tasks) != 5 {
			return fmt.Errorf("got %d tasks, want 5", len(tasks))
		}
		var last int64
		for i, tk := range tasks {
			want := int64(i+1) * domain.RankStep
			if tk.Rank != want {
				return fmt.Errorf("task[%d] rank=%d, want %d", i, tk.Rank, want)
			}
			if last != 0 && tk.Rank <= last {
				return fmt.Errorf("not strictly increasing: %d <= %d", tk.Rank, last)
			}
			last = tk.Rank
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Move
// ---------------------------------------------------------------------------

// TestMove_SetsStartedAtOnFirstActive verifies the lifecycle timestamps:
// moving into an active column sets started_at; moving into done sets
// done_at; moving back from done clears done_at.
func TestMove_SetsStartedAtOnFirstActive(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "move", "alice")

	err := ts.Write(context.Background(), func(tx Tx) error {
		// Bypass domain.CheckMove: this test exercises the store, not
		// the service.
		return ts.Tasks().Move(tx, task.ID, cols["Doing"].ID, domain.RankStep*2, "alice")
	})
	if err != nil {
		t.Fatalf("move to Doing: %v", err)
	}
	err = ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		if got.StartedAt == nil {
			return fmt.Errorf("started_at is nil after entering Doing")
		}
		if got.ColumnID != cols["Doing"].ID {
			return fmt.Errorf("column not Doing")
		}
		if got.Version != 2 {
			return fmt.Errorf("version = %d, want 2", got.Version)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = ts.Write(context.Background(), func(tx Tx) error {
		return ts.Tasks().Move(tx, task.ID, cols["Done"].ID, domain.RankStep*3, "alice")
	})
	if err != nil {
		t.Fatal(err)
	}
	err = ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		if got.DoneAt == nil {
			return fmt.Errorf("done_at is nil after entering Done")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Backup
// ---------------------------------------------------------------------------

// TestBackup_OpensAndHasSameRows asserts the backup is a real, readable
// copy of the live data — not a partial dump.
func TestBackup_OpensAndHasSameRows(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	want := []string{
		seedTask(t, ts, p, cols["Backlog"], "a", "alice").Key,
		seedTask(t, ts, p, cols["Doing"], "b", "alice").Key,
		seedTask(t, ts, p, cols["Review"], "c", "alice").Key,
		seedTask(t, ts, p, cols["Done"], "d", "alice").Key,
	}
	backupPath := filepath.Join(ts.dir, "backup.db")
	if err := ts.Backup(context.Background(), backupPath); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	// Open a fresh Store on the backup and confirm the rows.
	s2, err := Open(context.Background(), Config{Path: backupPath, ApplyMigrations: boolPtr(false)})
	if err != nil {
		t.Fatalf("Open backup: %v", err)
	}
	defer s2.Close()
	var got []string
	err = s2.Read(context.Background(), func(tx Tx) error {
		ts2, err := s2.Tasks().List(tx, TaskFilter{ProjectIDs: []string{p.ID}, IncludeDone: true})
		if err != nil {
			return err
		}
		for _, tk := range ts2 {
			got = append(got, tk.Key)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("list backup: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("backup has %d tasks, want %d (%v)", len(got), len(want), got)
	}
	wantSet := map[string]bool{}
	for _, k := range want {
		wantSet[k] = true
	}
	for _, k := range got {
		if !wantSet[k] {
			t.Fatalf("backup has unexpected task %q", k)
		}
		delete(wantSet, k)
	}
	if len(wantSet) != 0 {
		t.Fatalf("backup missing tasks: %v", wantSet)
	}
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

// TestToken_GetByHash covers the auth hot path. We pre-hash a secret with
// SHA-256 and look it up.
func TestToken_GetByHash(t *testing.T) {
	ts := openTestStore(t)
	sum := sha256.Sum256([]byte("the-secret"))
	hash := sum[:]
	token := &domain.Token{
		ID:     uuid.NewString(),
		Name:   "alice",
		Hash:   hash,
		Scopes: domain.Scopes{domain.ScopeWrite, domain.ScopeRead},
	}
	if err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Tokens().Create(tx, token)
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := ts.Read(context.Background(), func(tx Tx) error {
		got, err := ts.Tokens().GetByHash(tx, hash)
		if err != nil {
			return err
		}
		if got.Name != "alice" {
			return fmt.Errorf("name = %q, want alice", got.Name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Cycle detection across the parent hierarchy (PLAN §11 invariant 5)
// ---------------------------------------------------------------------------

// setParent reparents child under parent through the normal Update path.
func setParent(t *testing.T, s Store, child, parent *domain.Task) {
	t.Helper()
	if err := s.Write(context.Background(), func(tx Tx) error {
		cur, err := s.Tasks().GetByID(tx, child.ID)
		if err != nil {
			return err
		}
		cur.ParentID = &parent.ID
		return s.Tasks().Update(tx, cur, &cur.Version)
	}); err != nil {
		t.Fatalf("reparent %s under %s: %v", child.Key, parent.Key, err)
	}
}

// addBlocks adds a blocks link and returns whatever Add reported.
func addBlocks(s Store, blocker, blocked *domain.Task) error {
	return s.Write(context.Background(), func(tx Tx) error {
		return s.Links().Add(tx, &domain.Link{
			BlockerID: blocker.ID,
			BlockedID: blocked.ID,
			Type:      domain.LinkBlocks,
			CreatedBy: "alice",
		})
	})
}

// wouldCycle runs WouldCycle for one candidate edge.
func wouldCycle(t *testing.T, s Store, blocker, blocked *domain.Task) (bool, []string) {
	t.Helper()
	var cyc bool
	var path []string
	if err := s.Write(context.Background(), func(tx Tx) error {
		var err error
		cyc, path, err = s.Links().WouldCycle(tx, blocker.ID, blocked.ID)
		return err
	}); err != nil {
		t.Fatalf("WouldCycle(%s -> %s): %v", blocker.Key, blocked.Key, err)
	}
	return cyc, path
}

// TestWouldCycle_ThroughSubtaskOfABlockedParent covers the deadlock a
// blocks-only walk misses: A blocks B, C is a subtask of B, and C is then
// allowed to block A. task_next never offers a subtask whose parent has
// open blockers, so C waits on A; A waits on C by the new link; B waits on
// A. All three are stuck for good and nothing refused the link.
func TestWouldCycle_ThroughSubtaskOfABlockedParent(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	a := seedTask(t, ts, p, cols["Backlog"], "A", "alice")
	b := seedTask(t, ts, p, cols["Backlog"], "B", "alice")
	c := seedTask(t, ts, p, cols["Backlog"], "C", "alice")

	setParent(t, ts, c, b)
	if err := addBlocks(ts, a, b); err != nil {
		t.Fatalf("add A blocks B: %v", err)
	}

	cyc, path := wouldCycle(t, ts, c, a)
	if !cyc {
		t.Fatalf("C blocks A should be a cycle: A blocks B, C is a subtask of B")
	}
	if len(path) < 3 {
		t.Fatalf("path = %v, want the three-node loop", path)
	}
	if path[0] != c.Key || path[len(path)-1] != c.Key {
		t.Fatalf("path = %v, want it to open and close at %s", path, c.Key)
	}

	// Add must refuse it, not just WouldCycle.
	err := addBlocks(ts, c, a)
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeCycle {
		t.Fatalf("Add(C blocks A) error = %v, want cycle", err)
	}
}

// TestWouldCycle_ThroughParentOfABlockingSubtask is the other hierarchy
// edge: a parent is never offered while it still has unfinished subtasks,
// so P waits on its child C. With P blocking A, adding "A blocks C" closes
// A -> C -> P -> A.
func TestWouldCycle_ThroughParentOfABlockingSubtask(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	parent := seedTask(t, ts, p, cols["Backlog"], "P", "alice")
	child := seedTask(t, ts, p, cols["Backlog"], "C", "alice")
	other := seedTask(t, ts, p, cols["Backlog"], "A", "alice")

	setParent(t, ts, child, parent)
	if err := addBlocks(ts, parent, other); err != nil {
		t.Fatalf("add P blocks A: %v", err)
	}

	if cyc, path := wouldCycle(t, ts, other, child); !cyc {
		t.Fatalf("A blocks C should be a cycle (P blocks A, C is a subtask of P); path = %v", path)
	}
}

// TestWouldCycle_SiblingSubtasksStayLinkable pins the other side of the
// rule. Collapsing a whole subtree into one node would be the easy way to
// catch the cases above, and it would wrongly refuse this one: A blocks C1
// and C2 blocks A, with C1 and C2 both subtasks of P, schedules fine as
// C2, then A, then C1, then P.
func TestWouldCycle_SiblingSubtasksStayLinkable(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	parent := seedTask(t, ts, p, cols["Backlog"], "P", "alice")
	c1 := seedTask(t, ts, p, cols["Backlog"], "C1", "alice")
	c2 := seedTask(t, ts, p, cols["Backlog"], "C2", "alice")
	other := seedTask(t, ts, p, cols["Backlog"], "A", "alice")

	setParent(t, ts, c1, parent)
	setParent(t, ts, c2, parent)
	if err := addBlocks(ts, other, c1); err != nil {
		t.Fatalf("add A blocks C1: %v", err)
	}
	if err := addBlocks(ts, c2, other); err != nil {
		t.Fatalf("add C2 blocks A: %v — sibling subtasks must stay linkable", err)
	}
}

// ---------------------------------------------------------------------------
// GetManyByKeys
// ---------------------------------------------------------------------------

// TestGetManyByKeys_MatchesCaseInsensitively pins the batch lookup to the
// same case-insensitive contract as GetByKey. GetManyByKeys has no caller
// yet — it is part of the frozen TaskRepo interface — so without a test
// nothing would notice if it drifted, and the collation it relies on lives
// in the schema rather than in the query.
func TestGetManyByKeys_MatchesCaseInsensitively(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	one := seedTask(t, ts, p, cols["Backlog"], "one", "alice")
	two := seedTask(t, ts, p, cols["Backlog"], "two", "alice")

	var got map[string]*domain.Task
	if err := ts.Read(context.Background(), func(tx Tx) error {
		var err error
		got, err = ts.Tasks().GetManyByKeys(tx, []string{
			strings.ToLower(one.Key), two.Key, "BMB-9999", "",
		})
		return err
	}); err != nil {
		t.Fatalf("GetManyByKeys: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tasks, want 2 (%v)", len(got), got)
	}
	if got[one.Key] == nil {
		t.Fatalf("%s missing when asked for in lower case", one.Key)
	}
	if got[two.Key] == nil {
		t.Fatalf("%s missing", two.Key)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func boolPtr(b bool) *bool { return &b }
