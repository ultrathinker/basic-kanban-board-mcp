package service

import (
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// EventPublisher receives events after their transaction has committed. The
// composition root wires a real implementation (internal/events.Bus); the
// service never calls it from inside store.Write, because a subscriber must
// never learn about state a rollback later undid.
type EventPublisher interface {
	Publish(domain.Event)
}

// svc is the concrete Service. Every exported method opens exactly one
// store.Read or store.Write, so the transaction boundary is always visible
// at the call site (PLAN §7, AGENTS.md house style).
type svc struct {
	store store.Store
	pub   EventPublisher
}

// New builds the Service. pub may be nil (tests, or a CLI path that has no
// live subscribers); a nil publisher just means events are appended to the
// database and never fanned out.
func New(st store.Store, pub EventPublisher) Service {
	return &svc{store: st, pub: pub}
}

func newID() string { return uuid.NewString() }

// publishAll hands committed events to the wired publisher. Called only
// after store.Write has returned nil.
func (s *svc) publishAll(events []domain.Event) {
	if s.pub == nil {
		return
	}
	for _, e := range events {
		s.pub.Publish(e)
	}
}

// emit appends an event inside the transaction and stages it for
// post-commit publication. The append gives the event its ID, so the staged
// copy (taken after Append) carries it.
func (s *svc) emit(tx store.Tx, pending *[]domain.Event, actor string, typ domain.EventType, projectID string, taskID *string, payload map[string]any) error {
	e := &domain.Event{Actor: actor, Type: typ, ProjectID: projectID, TaskID: taskID, Payload: payload}
	if err := s.store.Events().Append(tx, e); err != nil {
		return err
	}
	*pending = append(*pending, *e)
	return nil
}

// ---------------------------------------------------------------------------
// Actor / scope / access helpers
// ---------------------------------------------------------------------------

func actorMayAccessProject(a Actor, key string) bool {
	if len(a.ProjectKeys) == 0 {
		return true
	}
	for _, k := range a.ProjectKeys {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

func requireRead(a Actor) error {
	if !a.CanRead() {
		return domain.Forbidden("token lacks read scope", "Use a token with at least the read scope.")
	}
	return nil
}

func requireWrite(a Actor) error {
	if !a.CanWrite() {
		return domain.Forbidden("token lacks write scope", "Use a token with the write scope.")
	}
	return nil
}

func requireProjectAccess(a Actor, key string) error {
	if !actorMayAccessProject(a, key) {
		return domain.Forbidden(
			fmt.Sprintf("token is not scoped to project %q", key),
			"Use a token whose project_keys include this project, or ask an admin to widen it.")
	}
	return nil
}

// requireForce enforces PLAN §6.5's force rule: admin scope plus a non-empty
// reason, both of which end up in the event payload. Used by task_update's
// column-move force path.
func requireForce(a Actor, force bool, reason string) error {
	if !force {
		return nil
	}
	if !a.IsAdmin() {
		return domain.Forbidden("force requires admin scope",
			"Ask an admin, or satisfy the rule instead: finish the blockers, free WIP, or tick the acceptance items.")
	}
	if strings.TrimSpace(reason) == "" {
		return domain.Invalid("reason", "force requires a reason",
			"Say why the rule is being bypassed; it is written to the event log.")
	}
	return nil
}

// requireAdminForce is the task_claim variant: TaskClaimInput has no Reason
// field (a frozen contract detail), so stealing a lease only needs admin
// scope.
func requireAdminForce(a Actor, force bool) error {
	if !force {
		return nil
	}
	if !a.IsAdmin() {
		return domain.Forbidden("force requires admin scope",
			"Ask an admin, or wait for the lease to expire.")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Resolution helpers
// ---------------------------------------------------------------------------

// resolveProject loads a project by key after checking the actor's project
// scope. Errors are loud: an inaccessible project is Forbidden, a missing
// one is NotFound (from the store).
func (s *svc) resolveProject(tx store.Tx, a Actor, key string) (*domain.Project, error) {
	if err := requireProjectAccess(a, key); err != nil {
		return nil, err
	}
	return s.store.Projects().GetByKey(tx, key)
}

// resolveTask loads a task by key. The project-key prefix is parsed out of
// the task key itself so the scope check happens before any query runs.
func (s *svc) resolveTask(tx store.Tx, a Actor, key string) (*domain.Task, error) {
	pk, _, err := domain.ParseTaskKey(key)
	if err != nil {
		return nil, err
	}
	if err := requireProjectAccess(a, pk); err != nil {
		return nil, err
	}
	return s.store.Tasks().GetByKey(tx, key)
}

// resolveColumn finds a column by name, defaulting to the project's first
// backlog-kind column (in position order) when name is empty.
func (s *svc) resolveColumn(tx store.Tx, project *domain.Project, name string) (*domain.Column, error) {
	if name == "" {
		cols, err := s.store.Columns().ListByProject(tx, project.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range cols {
			if c.Kind == domain.KindBacklog {
				return c, nil
			}
		}
		return nil, domain.Invalid("column", "project has no backlog-kind column",
			"Add one with project_upsert before creating tasks.")
	}
	return s.store.Columns().GetByName(tx, project.ID, name)
}
