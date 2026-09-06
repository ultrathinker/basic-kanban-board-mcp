package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// Cycle detection has no horizon
// ---------------------------------------------------------------------------

// chainLength is deliberately longer than the batch API's 100-task limit,
// and far longer than the depth-16 ceiling the walk used to carry. A cycle
// that closes beyond the horizon used to commit, and the whole component
// then had no runnable leaf: task_next stalled there permanently, across
// restarts, with nothing in the data to explain why.
const chainLength = 120

// TestWouldCycle_ClosesBeyondTheOldHorizon builds one long chain and closes
// it end to end.
func TestWouldCycle_ClosesBeyondTheOldHorizon(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()
	chain := seedChain(t, ts, p, cols["Backlog"], chainLength)

	// tail blocks head closes the loop head → … → tail → head.
	head, tail := chain[0], chain[len(chain)-1]
	var (
		cyc  bool
		path []string
	)
	if err := ts.Read(ctx, func(tx Tx) error {
		var err error
		cyc, path, err = ts.Links().WouldCycle(tx, tail.ID, head.ID)
		return err
	}); err != nil {
		t.Fatalf("WouldCycle: %v", err)
	}
	if !cyc {
		t.Fatalf("a cycle closing after %d hops was not detected", chainLength)
	}
	if len(path) != chainLength+1 {
		t.Fatalf("path has %d keys, want %d (the whole chain plus the closing hop)", len(path), chainLength+1)
	}
	if path[0] != tail.Key || path[len(path)-1] != tail.Key {
		t.Fatalf("path = %v…%v, want the cycle to start and end at %s", path[0], path[len(path)-1], tail.Key)
	}

	// And the link itself must be refused, not merely reported.
	err := ts.Write(ctx, func(tx Tx) error {
		return ts.Links().Add(tx, &domain.Link{
			BlockerID: tail.ID,
			BlockedID: head.ID,
			Type:      domain.LinkBlocks,
			CreatedBy: "alice",
		})
	})
	if got := domain.AsError(err); got == nil || got.Code != domain.CodeCycle {
		t.Fatalf("Add returned %v, want a cycle error", err)
	}
	assertLinkCount(t, ts, chainLength-1)
}

// TestWouldCycle_LongChainStillAcceptsAcyclicLinks is the other half of the
// previous test: removing the ceiling must not turn every long chain into a
// refusal.
func TestWouldCycle_LongChainStillAcceptsAcyclicLinks(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	chain := seedChain(t, ts, p, cols["Backlog"], chainLength)
	extra := seedTask(t, ts, p, cols["Backlog"], "leaf", "alice")

	// The chain's head blocks a fresh leaf: reachable from the head, but
	// nothing leads back.
	if err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Links().Add(tx, &domain.Link{
			BlockerID: chain[len(chain)-1].ID,
			BlockedID: extra.ID,
			Type:      domain.LinkBlocks,
			CreatedBy: "alice",
		})
	}); err != nil {
		t.Fatalf("Add acyclic link across a %d-task chain: %v", chainLength, err)
	}
	assertLinkCount(t, ts, chainLength)
}

// TestWouldCycle_ParallelRoutesDoNotExplode is the reason the walk counts a
// visited set rather than tracking paths. This graph has 2^40 distinct
// simple routes from head to tail; a walk that enumerates paths cannot
// finish it, which is what the depth-16 ceiling was really hiding.
func TestWouldCycle_ParallelRoutesDoNotExplode(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()
	const rungs = 40

	rail := seedChain(t, ts, p, cols["Backlog"], rungs+1)
	// A second route around every rung: rail[i] → side[i] → rail[i+1].
	side := make([]*domain.Task, rungs)
	if err := ts.Write(ctx, func(tx Tx) error {
		for i := 0; i < rungs; i++ {
			task, err := newTaskIn(tx, ts, p, cols["Backlog"], fmt.Sprintf("side-%d", i))
			if err != nil {
				return err
			}
			side[i] = task
		}
		return nil
	}); err != nil {
		t.Fatalf("seed side tasks: %v", err)
	}
	if err := ts.Write(ctx, func(tx Tx) error {
		for i := 0; i < rungs; i++ {
			if err := ts.Links().Add(tx, &domain.Link{
				BlockerID: rail[i].ID, BlockedID: side[i].ID,
				Type: domain.LinkBlocks, CreatedBy: "alice",
			}); err != nil {
				return err
			}
			if err := ts.Links().Add(tx, &domain.Link{
				BlockerID: side[i].ID, BlockedID: rail[i+1].ID,
				Type: domain.LinkBlocks, CreatedBy: "alice",
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed side links: %v", err)
	}

	var cyc bool
	var path []string
	if err := ts.Read(ctx, func(tx Tx) error {
		var err error
		cyc, path, err = ts.Links().WouldCycle(tx, rail[rungs].ID, rail[0].ID)
		return err
	}); err != nil {
		t.Fatalf("WouldCycle: %v", err)
	}
	if !cyc {
		t.Fatalf("closing the ladder was not detected as a cycle")
	}
	// The reported cycle is the shortest one, which runs down the rail
	// rather than through any of the side detours.
	if len(path) != rungs+2 {
		t.Fatalf("path has %d keys, want the shortest route (%d)", len(path), rungs+2)
	}
}

// TestWouldCycle_ReportedPathIsDeterministic pins the tie-break: two equally
// short routes must not make the error message flip between runs.
func TestWouldCycle_ReportedPathIsDeterministic(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()
	head := seedTask(t, ts, p, cols["Backlog"], "head", "alice")
	viaA := seedTask(t, ts, p, cols["Backlog"], "via-a", "alice")
	viaB := seedTask(t, ts, p, cols["Backlog"], "via-b", "alice")
	tail := seedTask(t, ts, p, cols["Backlog"], "tail", "alice")

	if err := ts.Write(ctx, func(tx Tx) error {
		for _, mid := range []*domain.Task{viaA, viaB} {
			if err := ts.Links().Add(tx, &domain.Link{
				BlockerID: head.ID, BlockedID: mid.ID,
				Type: domain.LinkBlocks, CreatedBy: "alice",
			}); err != nil {
				return err
			}
			if err := ts.Links().Add(tx, &domain.Link{
				BlockerID: mid.ID, BlockedID: tail.ID,
				Type: domain.LinkBlocks, CreatedBy: "alice",
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed diamond: %v", err)
	}

	var first []string
	for i := 0; i < 5; i++ {
		var path []string
		if err := ts.Read(ctx, func(tx Tx) error {
			var err error
			_, path, err = ts.Links().WouldCycle(tx, tail.ID, head.ID)
			return err
		}); err != nil {
			t.Fatalf("WouldCycle: %v", err)
		}
		if i == 0 {
			first = path
			continue
		}
		if fmt.Sprint(path) != fmt.Sprint(first) {
			t.Fatalf("run %d reported %v, first run reported %v", i, path, first)
		}
	}
}

// ---------------------------------------------------------------------------
// Project locality — the schema-level backstop
// ---------------------------------------------------------------------------

// TestProjectLocality_LinkAcrossProjectsRefused covers the case the service
// layer now rejects on the way in: this asserts the database refuses it too,
// so an import or an admin path cannot reintroduce the edge.
func TestProjectLocality_LinkAcrossProjectsRefused(t *testing.T) {
	ts := openTestStore(t)
	home, homeCols := seedProject(t, ts)
	away, awayCols := seedProjectKey(t, ts, "AWY")
	mine := seedTask(t, ts, home, homeCols["Backlog"], "mine", "alice")
	theirs := seedTask(t, ts, away, awayCols["Backlog"], "theirs", "alice")

	err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Links().Add(tx, &domain.Link{
			BlockerID: theirs.ID,
			BlockedID: mine.ID,
			Type:      domain.LinkBlocks,
			CreatedBy: "alice",
		})
	})
	assertCrossProjectRefusal(t, err)
	assertLinkCount(t, ts, 0)
}

// TestProjectLocality_ParentAcrossProjectsRefusedOnCreate covers the
// hierarchy half at insert time.
func TestProjectLocality_ParentAcrossProjectsRefusedOnCreate(t *testing.T) {
	ts := openTestStore(t)
	home, homeCols := seedProject(t, ts)
	away, awayCols := seedProjectKey(t, ts, "AWY")
	parent := seedTask(t, ts, away, awayCols["Backlog"], "their parent", "alice")

	err := ts.Write(context.Background(), func(tx Tx) error {
		seq, err := ts.Projects().NextTaskSeq(tx, home.ID)
		if err != nil {
			return err
		}
		return ts.Tasks().Create(tx, &domain.Task{
			ID:        uuid.NewString(),
			Key:       domain.TaskKey(home.Key, seq),
			ProjectID: home.ID,
			ColumnID:  homeCols["Backlog"].ID,
			ParentID:  &parent.ID,
			Rank:      domain.RankStep,
			Title:     "adopted across a border",
			Type:      domain.TypeTask,
			Priority:  domain.PriorityMedium,
			CreatedBy: "alice",
			UpdatedBy: "alice",
		})
	})
	assertCrossProjectRefusal(t, err)
}

// TestProjectLocality_ParentAcrossProjectsRefusedOnUpdate covers the
// reparent path, which passes every Go-side check (the parent exists, is
// top-level and is not a descendant) and is stopped only by the schema.
func TestProjectLocality_ParentAcrossProjectsRefusedOnUpdate(t *testing.T) {
	ts := openTestStore(t)
	home, homeCols := seedProject(t, ts)
	away, awayCols := seedProjectKey(t, ts, "AWY")
	child := seedTask(t, ts, home, homeCols["Backlog"], "child", "alice")
	parent := seedTask(t, ts, away, awayCols["Backlog"], "their parent", "alice")

	err := ts.Write(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, child.ID)
		if err != nil {
			return err
		}
		cur.ParentID = &parent.ID
		return ts.Tasks().Update(tx, cur, nil)
	})
	assertCrossProjectRefusal(t, err)

	if err := ts.Read(context.Background(), func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, child.ID)
		if err != nil {
			return err
		}
		if cur.ParentID != nil {
			t.Errorf("parent_id = %v, want nil", *cur.ParentID)
		}
		return nil
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
}

// TestProjectLocality_MovingATaskWithEdgesRefused is the mirror case: a
// task cannot be walked across the border either, leaving its parent or its
// links pointing home. There is no store API for this — a project move is
// exactly the kind of admin/import path the backstop exists for — so the
// test issues the UPDATE the way such a path would.
func TestProjectLocality_MovingATaskWithEdgesRefused(t *testing.T) {
	ts := openTestStore(t)
	home, homeCols := seedProject(t, ts)
	away, _ := seedProjectKey(t, ts, "AWY")
	blocker := seedTask(t, ts, home, homeCols["Backlog"], "blocker", "alice")
	blocked := seedTask(t, ts, home, homeCols["Backlog"], "blocked", "alice")
	ctx := context.Background()

	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Links().Add(tx, &domain.Link{
			BlockerID: blocker.ID, BlockedID: blocked.ID,
			Type: domain.LinkBlocks, CreatedBy: "alice",
		})
	}); err != nil {
		t.Fatalf("Add link: %v", err)
	}

	err := ts.Write(ctx, func(tx Tx) error {
		tw := tx.(*txWrap)
		_, err := tw.tx.ExecContext(tw.ctx(),
			"UPDATE tasks SET project_id = ? WHERE id = ?", away.ID, blocked.ID)
		return err
	})
	if err == nil {
		t.Fatalf("moving a linked task to another project was allowed")
	}
	if !IsProjectLocalityViolation(err) {
		t.Fatalf("error = %v, want the project-locality trigger", err)
	}
}

// TestProjectLocality_SameProjectEdgesStillWork guards against a backstop
// that refuses everything.
func TestProjectLocality_SameProjectEdgesStillWork(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	_, _ = seedProjectKey(t, ts, "AWY") // a second project exists but is uninvolved
	parent := seedTask(t, ts, p, cols["Backlog"], "parent", "alice")
	child := seedTask(t, ts, p, cols["Backlog"], "child", "alice")
	ctx := context.Background()

	if err := ts.Write(ctx, func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, child.ID)
		if err != nil {
			return err
		}
		cur.ParentID = &parent.ID
		return ts.Tasks().Update(tx, cur, nil)
	}); err != nil {
		t.Fatalf("reparent inside one project: %v", err)
	}
	other := seedTask(t, ts, p, cols["Backlog"], "other", "alice")
	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Links().Add(tx, &domain.Link{
			BlockerID: parent.ID, BlockedID: other.ID,
			Type: domain.LinkBlocks, CreatedBy: "alice",
		})
	}); err != nil {
		t.Fatalf("link inside one project: %v", err)
	}
}

// TestProjectLocality_MissingTaskStillReportsNotFound asserts the trigger
// did not take over the foreign key's job: an edge to a task that does not
// exist must still say so, not blame a project boundary.
func TestProjectLocality_MissingTaskStillReportsNotFound(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	real := seedTask(t, ts, p, cols["Backlog"], "real", "alice")
	ghost := uuid.NewString()

	err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Links().Add(tx, &domain.Link{
			BlockerID: ghost, BlockedID: real.ID,
			Type: domain.LinkBlocks, CreatedBy: "alice",
		})
	})
	if got := domain.AsError(err); got == nil || got.Code != domain.CodeNotFound {
		t.Fatalf("Add with a missing blocker returned %v, want not_found", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// seedChain creates n tasks in one transaction and links each to the next,
// so task i blocks task i+1.
func seedChain(t *testing.T, s Store, p *domain.Project, col *domain.Column, n int) []*domain.Task {
	t.Helper()
	out := make([]*domain.Task, 0, n)
	ctx := context.Background()
	if err := s.Write(ctx, func(tx Tx) error {
		for i := 0; i < n; i++ {
			task, err := newTaskIn(tx, s, p, col, fmt.Sprintf("chain-%03d", i))
			if err != nil {
				return err
			}
			out = append(out, task)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed %d chained tasks: %v", n, err)
	}
	if err := s.Write(ctx, func(tx Tx) error {
		for i := 0; i+1 < n; i++ {
			if err := s.Links().Add(tx, &domain.Link{
				BlockerID: out[i].ID,
				BlockedID: out[i+1].ID,
				Type:      domain.LinkBlocks,
				CreatedBy: "alice",
			}); err != nil {
				return fmt.Errorf("link %d->%d: %w", i, i+1, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed chain links: %v", err)
	}
	return out
}

// seedProjectKey is seedProject with a caller-chosen key, for the tests
// that need two projects.
func seedProjectKey(t *testing.T, s Store, key string) (*domain.Project, map[string]*domain.Column) {
	t.Helper()
	p := &domain.Project{
		ID:                  uuid.NewString(),
		Key:                 key,
		Name:                key,
		Version:             1,
		NextTaskSeq:         1,
		EstimateUnit:        "h",
		EnforceDependencies: true,
		ClaimTTLSeconds:     3600,
	}
	cols := map[string]*domain.Column{}
	if err := s.Write(context.Background(), func(tx Tx) error {
		if err := s.Projects().Create(tx, p); err != nil {
			return err
		}
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
	}); err != nil {
		t.Fatalf("seed project %s: %v", key, err)
	}
	return p, cols
}

func assertLinkCount(t *testing.T, s Store, want int) {
	t.Helper()
	var got int
	if err := s.Read(context.Background(), func(tx Tx) error {
		tw := tx.(*txWrap)
		return tw.tx.QueryRowContext(tw.ctx(), "SELECT COUNT(*) FROM links").Scan(&got)
	}); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if got != want {
		t.Fatalf("links table holds %d rows, want %d", got, want)
	}
}

func assertCrossProjectRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("a cross-project edge was accepted")
	}
	got := domain.AsError(err)
	if got == nil || got.Code != domain.CodeValidation {
		t.Fatalf("error = %v, want a validation error", err)
	}
	if got.Remediation == "" {
		t.Fatalf("error %v carries no remediation", got)
	}
}
