package demo

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

func newSvc(t *testing.T) service.Service {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := store.Open(ctx, store.Config{Path: filepath.Join(t.TempDir(), "kanban.db"), ReadPoolSize: 4})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return service.New(st, nil)
}

func TestSeed_CreatesASampleBoard(t *testing.T) {
	t.Parallel()
	svc := newSvc(t)
	ctx := context.Background()

	res, err := Seed(ctx, svc)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if !res.Created {
		t.Fatal("Created = false on an empty database; the seed reported doing nothing")
	}
	if want := len(sampleTasks()); res.Tasks != want {
		t.Fatalf("seeded %d tasks, want %d — a batch item was dropped without an error", res.Tasks, want)
	}

	board, err := svc.BoardGet(ctx, Actor, service.BoardGetInput{
		ProjectKey: ProjectKey, View: service.ViewTasks, DoneLimit: 10,
		Include: service.Includes{service.IncludeLinks},
	})
	if err != nil {
		t.Fatalf("BoardGet: %v", err)
	}
	if len(board.Projects) != 1 {
		t.Fatalf("board has %d projects, want exactly the demo project", len(board.Projects))
	}
	p := board.Projects[0]
	if p.Key != ProjectKey {
		t.Errorf("project key = %q, want %q", p.Key, ProjectKey)
	}

	// The seed exists to show the product's shape, so assert the shape rather
	// than a task count: a reader must land on a board with work in more than
	// one column, a real dependency and a live claim.
	byColumn := map[string][]domain.TaskView{}
	for _, c := range p.Columns {
		byColumn[c.Name] = c.Tasks
	}
	for _, col := range []string{"Backlog", "Doing", "Done"} {
		if len(byColumn[col]) == 0 {
			t.Errorf("column %q is empty; the sample board reads as one long backlog", col)
		}
	}

	var blocked, claimed int
	for _, tasks := range byColumn {
		for _, tv := range tasks {
			if len(tv.BlockedBy) > 0 {
				blocked++
			}
			if tv.ClaimedBy != nil && *tv.ClaimedBy != "" {
				claimed++
			}
		}
	}
	if blocked == 0 {
		t.Error("no task is blocked by another; the dependency chain did not survive the seed")
	}
	if claimed == 0 {
		t.Error("no task carries a claim; the lease is one of the four things the demo is for")
	}
}

func TestSeed_IsIdempotent(t *testing.T) {
	t.Parallel()
	svc := newSvc(t)
	ctx := context.Background()

	first, err := Seed(ctx, svc)
	if err != nil {
		t.Fatalf("first Seed: %v", err)
	}
	second, err := Seed(ctx, svc)
	if err != nil {
		t.Fatalf("second Seed: %v — --demo sits in a compose file and runs on every restart", err)
	}
	if second.Created {
		t.Error("second Seed reported Created = true; the sample board was seeded twice")
	}

	board, err := svc.BoardGet(ctx, Actor, service.BoardGetInput{
		ProjectKey: ProjectKey, View: service.ViewTasks, DoneLimit: 50,
	})
	if err != nil {
		t.Fatalf("BoardGet: %v", err)
	}
	var total int
	for _, c := range board.Projects[0].Columns {
		total += c.Count
	}
	if total != first.Tasks {
		t.Errorf("board holds %d tasks after two seeds, want %d", total, first.Tasks)
	}
}

// TestSeed_LeavesOtherProjectsAlone pins the one destructive thing a seeder
// could plausibly do: --demo runs against a data directory that may already
// hold real work.
func TestSeed_LeavesOtherProjectsAlone(t *testing.T) {
	t.Parallel()
	svc := newSvc(t)
	ctx := context.Background()

	if _, err := svc.ProjectUpsert(ctx, Actor, service.ProjectUpsertInput{
		Mode: service.UpsertCreate, Key: "REAL", Name: "Real work",
	}); err != nil {
		t.Fatalf("create REAL: %v", err)
	}
	if _, err := svc.TaskCreate(ctx, Actor, service.TaskCreateInput{
		Tasks: []service.NewTask{{ProjectKey: "REAL", Ref: "a", Title: "Do not touch me", Type: domain.TypeTask}},
	}); err != nil {
		t.Fatalf("create task in REAL: %v", err)
	}

	if _, err := Seed(ctx, svc); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	board, err := svc.BoardGet(ctx, Actor, service.BoardGetInput{
		ProjectKey: "REAL", View: service.ViewTasks, DoneLimit: 10,
	})
	if err != nil {
		t.Fatalf("BoardGet REAL: %v", err)
	}
	var total int
	for _, c := range board.Projects[0].Columns {
		total += c.Count
	}
	if total != 1 {
		t.Errorf("REAL holds %d tasks after seeding the demo, want 1", total)
	}
}

// TestSampleTasks_RefsAreUniqueAndResolvable guards the batch itself: a
// "@ref" that names nothing is a validation error at seed time, and a
// duplicate ref silently binds the wrong task.
func TestSampleTasks_RefsAreUniqueAndResolvable(t *testing.T) {
	t.Parallel()
	tasks := sampleTasks()
	refs := map[string]bool{}
	for _, task := range tasks {
		if task.Ref == "" {
			t.Errorf("task %q has no ref", task.Title)
			continue
		}
		if refs[task.Ref] {
			t.Errorf("duplicate ref %q", task.Ref)
		}
		refs[task.Ref] = true
	}
	for _, task := range tasks {
		for _, b := range task.BlockedBy {
			if len(b) > 0 && b[0] == '@' && !refs[b[1:]] {
				t.Errorf("task %q is blocked by %q, which names no task in the batch", task.Ref, b)
			}
		}
		if len(task.Parent) > 0 && task.Parent[0] == '@' && !refs[task.Parent[1:]] {
			t.Errorf("task %q has parent %q, which names no task in the batch", task.Ref, task.Parent)
		}
	}
	// advance addresses these two by name; a rename that misses it would leave
	// the sample board flat with no error anywhere.
	for _, ref := range []string{"schema", "api"} {
		if !refs[ref] {
			t.Errorf("advance moves ref %q, which sampleTasks no longer defines", ref)
		}
	}
}
