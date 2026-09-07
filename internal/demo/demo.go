// Package demo seeds one visibly-labeled sample project so a first-time
// reader can see a real board — dependencies, priorities, a live claim, a
// finished task — without inventing content first (PLAN §14: `--demo` is part
// of the sixty-second evaluation path).
//
// It goes through service.Service only, exactly like MCP and the web UI, so
// the sample board is reachable by the same rules as any other board and
// cannot drift into a shape the product itself would refuse to create.
package demo

import (
	"context"
	"errors"
	"fmt"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ProjectKey is the key the sample board is created under. It is fixed so
// Seed can recognise its own previous run, and so "delete the demo" is one
// obvious thing to delete rather than a hunt through the board.
const ProjectKey = "DEMO"

// projectName says out loud that this is sample data. A user who forgets they
// passed --demo must be able to tell at a glance which project is theirs.
const projectName = "Demo — sample data, safe to delete"

// Actor is the identity the seed writes under. It is not a token: Seed is
// called by the composition root, which is already past authentication, and
// the name exists so the activity feed attributes the sample tasks to
// something a reader will not mistake for a real agent.
var Actor = service.Actor{
	Name:   "demo-seed",
	Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
}

// Result reports what a Seed call did, so the caller can print an honest line
// instead of claiming work it skipped.
type Result struct {
	// Created is false when the sample project was already there. Seed is
	// idempotent by design: `--demo` sits in a docker-compose file and runs on
	// every restart, so a second run must not duplicate the board or fail the
	// startup that carries it.
	Created bool
	Tasks   int
}

// Seed creates the sample project and its tasks, and does nothing at all if
// the sample project already exists. It never touches any other project.
//
// A partial failure is reported, not swallowed: the tasks arrive in one batch
// call, so either the whole sample board lands or the project is left empty
// and the error says so.
func Seed(ctx context.Context, svc service.Service) (Result, error) {
	exists, err := projectExists(ctx, svc)
	if err != nil {
		return Result{}, err
	}
	if exists {
		return Result{Created: false}, nil
	}

	if _, err := svc.ProjectUpsert(ctx, Actor, service.ProjectUpsertInput{
		Mode:        service.UpsertCreate,
		Key:         ProjectKey,
		Name:        projectName,
		Description: strptr("Sample data seeded by `kanban demo` / `serve --demo`. Delete this project once you have your own."),
	}); err != nil {
		// A racing second seeder is the expected way this fails; treat the
		// project already being there as success rather than as a startup
		// error, since the outcome the caller wanted is the outcome it got.
		if isConflict(err) {
			return Result{Created: false}, nil
		}
		return Result{}, fmt.Errorf("demo: create project %s: %w", ProjectKey, err)
	}

	tasks := sampleTasks()
	res, err := svc.TaskCreate(ctx, Actor, service.TaskCreateInput{Tasks: tasks})
	if err != nil {
		return Result{}, fmt.Errorf("demo: seed tasks: %w", err)
	}
	out := Result{Created: true, Tasks: len(res.Tasks)}

	// The board is more useful — and the differentiators more visible — with
	// some history on it: one task finished, one claimed and in progress.
	// These run after the batch because they address tasks by the keys the
	// batch just allocated. task_create returns the created tasks in request
	// order, which is what lets the refs be matched back by position.
	byRef := map[string]domain.TaskView{}
	for i, t := range res.Tasks {
		if i < len(tasks) {
			byRef[tasks[i].Ref] = t
		}
	}
	if err := advance(ctx, svc, byRef); err != nil {
		return out, err
	}
	return out, nil
}

// SeedFresh seeds the sample board only when the database holds no projects at
// all — the "brand-new node" case. Once the operator has any project of their
// own (or has kept the demo), it does nothing, so deleting the demo after you
// have your own board does not bring it back on the next restart. This is the
// default path `kanban serve` takes; the plain Seed (and `kanban demo`) still
// force the sample board regardless.
func SeedFresh(ctx context.Context, svc service.Service) (Result, error) {
	empty, err := boardIsEmpty(ctx, svc)
	if err != nil {
		return Result{}, err
	}
	if !empty {
		return Result{Created: false}, nil
	}
	return Seed(ctx, svc)
}

// Clear archives the sample project so it drops off the board. Archiving rather
// than hard-deleting is reversible and needs no new store primitive: an
// archived project is excluded from every board listing, so the board reads as
// clean while the data is still recoverable. It is a no-op when the demo is
// already gone.
func Clear(ctx context.Context, svc service.Service) (bool, error) {
	board, err := svc.BoardGet(ctx, Actor, service.BoardGetInput{
		ProjectKey: ProjectKey, View: service.ViewSummary,
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("demo: look for the %s project to clear: %w", ProjectKey, err)
	}
	if len(board.Projects) == 0 {
		return false, nil
	}
	p := board.Projects[0]
	if p.Archived {
		return false, nil
	}
	archived := true
	version := p.Version
	if _, err := svc.ProjectUpsert(ctx, Actor, service.ProjectUpsertInput{
		Mode:      service.UpsertUpdate,
		Key:       ProjectKey,
		IfVersion: &version,
		Archived:  &archived,
	}); err != nil {
		return false, fmt.Errorf("demo: archive %s: %w", ProjectKey, err)
	}
	return true, nil
}

// boardIsEmpty reports whether the board has no projects at all. An empty
// ProjectKey asks BoardGet for every project the actor can reach; the demo
// actor is unrestricted, so this sees the whole board.
func boardIsEmpty(ctx context.Context, svc service.Service) (bool, error) {
	board, err := svc.BoardGet(ctx, Actor, service.BoardGetInput{View: service.ViewSummary})
	if err != nil {
		return false, fmt.Errorf("demo: check whether the board is empty: %w", err)
	}
	return len(board.Projects) == 0, nil
}

// advance moves two of the seeded tasks out of the backlog so the sample board
// shows a column layout rather than one long list.
func advance(ctx context.Context, svc service.Service, byRef map[string]domain.TaskView) error {
	if done, ok := byRef["schema"]; ok {
		v := done.Version
		if _, err := svc.TaskUpdate(ctx, Actor, service.TaskUpdateInput{
			Patches: []service.TaskPatch{{Key: done.Key, IfVersion: &v, Column: "Done"}},
		}); err != nil {
			return fmt.Errorf("demo: move %s to Done: %w", done.Key, err)
		}
	}
	if doing, ok := byRef["api"]; ok {
		v := doing.Version
		if _, err := svc.TaskUpdate(ctx, Actor, service.TaskUpdateInput{
			Patches: []service.TaskPatch{{Key: doing.Key, IfVersion: &v, Column: "Doing"}},
		}); err != nil {
			return fmt.Errorf("demo: move %s to Doing: %w", doing.Key, err)
		}
		if _, err := svc.TaskClaim(ctx, Actor, service.TaskClaimInput{
			Key: doing.Key, Action: service.ClaimTake,
		}); err != nil {
			return fmt.Errorf("demo: claim %s: %w", doing.Key, err)
		}
	}
	return nil
}

// projectExists asks the board read whether the sample project is already
// present. BoardGet is the only listing the frozen service interface has, and
// its summary view is the cheapest form of it.
func projectExists(ctx context.Context, svc service.Service) (bool, error) {
	board, err := svc.BoardGet(ctx, Actor, service.BoardGetInput{
		ProjectKey: ProjectKey, View: service.ViewSummary,
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("demo: look for an existing %s project: %w", ProjectKey, err)
	}
	return len(board.Projects) > 0, nil
}

func isNotFound(err error) bool { return hasCode(err, domain.CodeNotFound) }
func isConflict(err error) bool { return hasCode(err, domain.CodeConflict) }

func hasCode(err error, code domain.Code) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == code
}

func strptr(s string) *string { return &s }

func f64(v float64) *float64 { return &v }

// sampleTasks is the board itself: a small feature with a real dependency
// chain (schema → API → UI), a bug that jumps the queue, a subtask, and a
// couple of unstarted items so `task_next` has something to choose between.
// Refs are symbolic so the chain survives however keys get allocated.
func sampleTasks() []service.NewTask {
	return []service.NewTask{
		{
			ProjectKey: ProjectKey, Ref: "schema",
			Title:    "Design the notes table",
			Body:     "One table, one index on (user_id, updated_at). Keep it boring.",
			Type:     domain.TypeTask,
			Priority: domain.PriorityHigh,
			Estimate: f64(2),
			Tags:     []string{"backend", "schema"},
			Acceptance: []string{
				"Migration applies on an empty database",
				"Migration applies on a populated database",
			},
		},
		{
			ProjectKey: ProjectKey, Ref: "api",
			Title:     "Notes CRUD endpoints",
			Body:      "GET/POST/PATCH/DELETE /notes. Optimistic concurrency on PATCH.",
			Type:      domain.TypeFeat,
			Priority:  domain.PriorityHigh,
			Estimate:  f64(5),
			Tags:      []string{"backend", "api"},
			BlockedBy: []string{"@schema"},
			Acceptance: []string{
				"PATCH with a stale version returns 409 and the current state",
				"Every endpoint has a test",
			},
		},
		{
			ProjectKey: ProjectKey, Ref: "ui",
			Title:     "Notes list and editor",
			Body:      "Server-rendered. No client framework.",
			Type:      domain.TypeFeat,
			Priority:  domain.PriorityMedium,
			Estimate:  f64(5),
			Tags:      []string{"frontend"},
			BlockedBy: []string{"@api"},
		},
		{
			ProjectKey: ProjectKey, Ref: "ui-empty",
			Title:    "Empty state for the notes list",
			Type:     domain.TypeTask,
			Priority: domain.PriorityLow,
			Estimate: f64(1),
			Tags:     []string{"frontend"},
			Parent:   "@ui",
		},
		{
			ProjectKey: ProjectKey, Ref: "bug",
			Title:    "Timestamps render in UTC for everyone",
			Body:     "Reported twice this week. The server sends UTC and the page never converts it.",
			Type:     domain.TypeBug,
			Priority: domain.PriorityCritical,
			Estimate: f64(1),
			Tags:     []string{"frontend", "bug"},
			Acceptance: []string{
				"A note created at 23:30 local shows as 23:30, not tomorrow",
			},
		},
		{
			ProjectKey: ProjectKey, Ref: "search",
			Title:    "Full-text search over notes",
			Body:     "SQLite FTS5. Decide first whether it is worth the index size.",
			Type:     domain.TypeResearch,
			Priority: domain.PriorityLow,
			Estimate: f64(3),
			Tags:     []string{"backend", "search"},
		},
		{
			ProjectKey: ProjectKey, Ref: "readme",
			Title:    "Write the README",
			Type:     domain.TypeDoc,
			Priority: domain.PriorityMedium,
			Estimate: f64(1),
			Tags:     []string{"docs"},
		},
		{
			ProjectKey: ProjectKey, Ref: "ci",
			Title:    "CI: build, vet, test on every push",
			Type:     domain.TypeChore,
			Priority: domain.PriorityMedium,
			Estimate: f64(2),
			Tags:     []string{"ci"},
		},
	}
}
