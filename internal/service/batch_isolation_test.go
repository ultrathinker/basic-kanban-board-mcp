package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// Tests for the per-item unit of work (architecture review #6). A batch that
// reports "nine ok, one failed" has to mean it: the failed item leaves no
// row, no note and no event behind, and the nine that worked stay committed.
//
// The interesting cases are the ones where an item writes and THEN fails —
// exactly the shape the old "validate everything before touching a row"
// convention could not defend against as the patch command grew.

// recordingPublisher captures what the service fans out after a commit.
// Asserting on it is the only way to see the in-memory half of a rollback:
// the database rows of a failed item are gone either way, but its staged
// events live in this process until the outer transaction commits, and a
// rolled-back item that still publishes announces work that never happened.
type recordingPublisher struct {
	mu     sync.Mutex
	events []domain.Event
}

func (p *recordingPublisher) Publish(e domain.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}

func (p *recordingPublisher) all() []domain.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]domain.Event(nil), p.events...)
}

func (p *recordingPublisher) forTask(id string) []domain.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []domain.Event
	for _, e := range p.events {
		if e.TaskID != nil && *e.TaskID == id {
			out = append(out, e)
		}
	}
	return out
}

// storedEventsForTask counts the events that actually reached the event table.
func storedEventsForTask(t *testing.T, env *testEnv, projectID, taskID string) int {
	t.Helper()
	n := 0
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		evs, err := env.Events().Latest(tx, projectID, 500)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if e.TaskID != nil && *e.TaskID == taskID {
				n++
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read events: %v", err)
	}
	return n
}

func noteCount(t *testing.T, env *testEnv, taskID string) int {
	t.Helper()
	n := 0
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		notes, err := env.Notes().ListByTask(tx, taskID, domain.MaxNotesPerRead, nil)
		if err != nil {
			return err
		}
		n = len(notes)
		return nil
	}); err != nil {
		t.Fatalf("read notes: %v", err)
	}
	return n
}

// makeSubtask returns the key of a task that is itself a subtask, which makes
// it an illegal parent (max depth 2). Reparenting onto it is refused by the
// store inside Tasks().Update — i.e. AFTER an item's earlier mutations have
// already been written.
func makeSubtask(t *testing.T, env *testEnv) string {
	t.Helper()
	res, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{
		Tasks: []NewTask{
			{ProjectKey: env.proj.Key, Title: "top level", Type: domain.TypeTask,
				Priority: domain.PriorityMedium, Ref: "top"},
			{ProjectKey: env.proj.Key, Title: "a subtask", Type: domain.TypeTask,
				Priority: domain.PriorityMedium, Parent: "@top"},
		},
	})
	if err != nil {
		t.Fatalf("create parent/child: %v", err)
	}
	return res.Tasks[1].Key
}

// TestTaskUpdate_FailedItemRollsBackItsOwnWritesOnly is the headline test for
// review finding #6. The middle item moves the card, appends a note and
// stages an event, and only then hits a rule the store enforces — the exact
// half-applied shape the old convention could not prevent.
func TestTaskUpdate_FailedItemRollsBackItsOwnWritesOnly(t *testing.T) {
	env := openTestEnv(t)
	illegalParent := makeSubtask(t, env)

	rec := &recordingPublisher{}
	svc := New(env.Store, rec)

	first := makeBacklogTask(t, env, "first")
	middle := makeBacklogTask(t, env, "middle")
	last := makeBacklogTask(t, env, "last")

	before := freshView(t, env, middle.Key)

	res, err := svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{
			{Key: first.Key, IfVersion: intPtrLocal(first.Version), Title: strPtrLocal("first edited")},
			{
				Key:       middle.Key,
				IfVersion: intPtrLocal(middle.Version),
				Column:    "Doing",                                      // writes: move + version bump
				Note:      "starting work",                              // writes: note row + staged event
				Parent:    FieldString{Set: true, Value: illegalParent}, // refused inside Tasks().Update
			},
			{Key: last.Key, IfVersion: intPtrLocal(last.Version), Title: strPtrLocal("last edited")},
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.Items[0].OK || !res.Items[2].OK {
		t.Fatalf("neighbours did not survive the failing item: %+v / %+v", res.Items[0], res.Items[2])
	}
	if res.Items[1].OK {
		t.Fatalf("middle item reported ok; the illegal reparent must refuse it")
	}
	if res.Items[1].Err == nil || res.Items[1].Err.Code != domain.CodeValidation {
		t.Fatalf("middle item error = %+v, want a validation refusal", res.Items[1].Err)
	}

	// The middle task must be untouched in every respect.
	after := freshView(t, env, middle.Key)
	if after.ColumnName != before.ColumnName {
		t.Fatalf("middle moved to %q despite failing; want %q", after.ColumnName, before.ColumnName)
	}
	if after.Version != before.Version {
		t.Fatalf("middle version = %d, want %d unchanged — a rolled-back item must not consume a version",
			after.Version, before.Version)
	}
	if after.ParentID != nil {
		t.Fatalf("middle acquired a parent from a refused patch")
	}
	if n := noteCount(t, env, middle.ID); n != 0 {
		t.Fatalf("middle kept %d note(s) from a refused patch", n)
	}
	if n := storedEventsForTask(t, env, env.proj.ID, middle.ID); n != 0 {
		t.Fatalf("middle left %d event(s) in the event table", n)
	}
	if evs := rec.forTask(middle.ID); len(evs) != 0 {
		t.Fatalf("middle published %d event(s) after rolling back: %+v", len(evs), evs)
	}

	// The neighbours' writes are still there, and still announced.
	if got := freshView(t, env, first.Key).Title; got != "first edited" {
		t.Fatalf("first title = %q, want %q", got, "first edited")
	}
	if got := freshView(t, env, last.Key).Title; got != "last edited" {
		t.Fatalf("last title = %q, want %q", got, "last edited")
	}
	if len(rec.forTask(first.ID)) == 0 || len(rec.forTask(last.ID)) == 0 {
		t.Fatalf("truncating the failed item's events also ate its neighbours'")
	}
}

// TestTaskUpdate_AtomicBatchWithHalfAppliedItemLandsNothing is the same
// half-applied item under Atomic: true, where the promise is the opposite —
// the whole batch must disappear, including the items that succeeded.
func TestTaskUpdate_AtomicBatchWithHalfAppliedItemLandsNothing(t *testing.T) {
	env := openTestEnv(t)
	illegalParent := makeSubtask(t, env)

	rec := &recordingPublisher{}
	svc := New(env.Store, rec)

	first := makeBacklogTask(t, env, "first")
	middle := makeBacklogTask(t, env, "middle")

	_, err := svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Atomic: true,
		Patches: []TaskPatch{
			{Key: first.Key, IfVersion: intPtrLocal(first.Version), Title: strPtrLocal("first edited")},
			{
				Key:       middle.Key,
				IfVersion: intPtrLocal(middle.Version),
				Column:    "Doing",
				Note:      "starting work",
				Parent:    FieldString{Set: true, Value: illegalParent},
			},
		},
	})
	if err == nil {
		t.Fatalf("atomic batch with a failing item succeeded")
	}
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation {
		t.Fatalf("atomic error = %v, want a validation refusal", err)
	}
	if got := freshView(t, env, first.Key).Title; got != "first" {
		t.Fatalf("first title = %q; an atomic batch must roll back its successful items too", got)
	}
	if len(rec.all()) != 0 {
		t.Fatalf("atomic batch published %d event(s) after rolling back", len(rec.all()))
	}
}

// ---------------------------------------------------------------------------
// task_remove: the same guarantee, plus the cascade
// ---------------------------------------------------------------------------

// failingTaskRepo delegates everything to the real repository but refuses
// Archive for one task id. task_remove has no naturally reachable failure
// after its cascade has written, so the failure is injected — the point under
// test is the rollback boundary, not the reason the item failed.
type failingTaskRepo struct {
	store.TaskRepo
	failFor string
	err     error
}

func (r *failingTaskRepo) Archive(tx store.Tx, id string, archived bool, actor string) error {
	if id == r.failFor {
		return r.err
	}
	return r.TaskRepo.Archive(tx, id, archived, actor)
}

// patchedStore swaps in a doctored TaskRepo and passes everything else
// through to the real store.
type patchedStore struct {
	store.Store
	tasks store.TaskRepo
}

func (s *patchedStore) Tasks() store.TaskRepo { return s.tasks }

// makeParentWithChild returns a top-level task and its single subtask.
func makeParentWithChild(t *testing.T, env *testEnv) (parent, child domain.TaskView) {
	t.Helper()
	res, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{
		Tasks: []NewTask{
			{ProjectKey: env.proj.Key, Title: "epic", Type: domain.TypeTask,
				Priority: domain.PriorityMedium, Ref: "epic"},
			{ProjectKey: env.proj.Key, Title: "subtask", Type: domain.TypeTask,
				Priority: domain.PriorityMedium, Parent: "@epic"},
		},
	})
	if err != nil {
		t.Fatalf("create epic/subtask: %v", err)
	}
	return res.Tasks[0], res.Tasks[1]
}

// TestTaskRemove_FailedCascadeLeavesNoArchivedChildren covers the worst
// half-applied shape task_remove can produce: the cascade archives the
// children, then the parent's own archive fails and the item reports
// ok:false — with the children still archived, and an archive event already
// staged for each of them.
func TestTaskRemove_FailedCascadeLeavesNoArchivedChildren(t *testing.T) {
	env := openTestEnv(t)
	parent, child := makeParentWithChild(t, env)
	survivor := makeBacklogTask(t, env, "unrelated")

	rec := &recordingPublisher{}
	svc := New(&patchedStore{
		Store: env.Store,
		tasks: &failingTaskRepo{
			TaskRepo: env.Tasks(),
			failFor:  parent.ID,
			err:      domain.Invalid("test", "injected archive failure", "None; this is a test."),
		},
	}, rec)

	// The pair was created through the service, so both already carry a
	// task.created event; the rollback must leave that count untouched.
	childEvents := storedEventsForTask(t, env, env.proj.ID, child.ID)
	parentEvents := storedEventsForTask(t, env, env.proj.ID, parent.ID)

	res, err := svc.TaskRemove(context.Background(), env.actor, TaskRemoveInput{
		CascadeSubtasks: true,
		Items: []RemoveItem{
			{Key: parent.Key},
			{Key: survivor.Key},
		},
	})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if res.Items[0].OK {
		t.Fatalf("parent item reported ok despite the injected archive failure")
	}
	if res.Items[0].Err == nil || res.Items[0].Err.Code != domain.CodeValidation {
		t.Fatalf("parent item error = %+v, want the injected validation error", res.Items[0].Err)
	}
	if !res.Items[1].OK {
		t.Fatalf("the following item did not survive: %+v", res.Items[1].Err)
	}

	if got := freshView(t, env, child.Key); got.ArchivedAt != nil {
		t.Fatalf("child stayed archived after its parent's item rolled back")
	}
	if got := freshView(t, env, parent.Key); got.ArchivedAt != nil {
		t.Fatalf("parent is archived after a failed archive")
	}
	if n := storedEventsForTask(t, env, env.proj.ID, child.ID); n != childEvents {
		t.Fatalf("child event count = %d, want %d — the cascade's archive event was not rolled back", n, childEvents)
	}
	if n := storedEventsForTask(t, env, env.proj.ID, parent.ID); n != parentEvents {
		t.Fatalf("parent event count = %d, want %d", n, parentEvents)
	}
	if evs := rec.forTask(child.ID); len(evs) != 0 {
		t.Fatalf("child published %d event(s) from a rolled-back cascade: %+v", len(evs), evs)
	}
	// The survivor's own archive committed, so it is off the board — the
	// proof that the batch really did continue past the failed item.
	if boardHasTask(t, env, survivor.Key) {
		t.Fatalf("%s is still on the board; the item after the failure did not commit", survivor.Key)
	}
	if len(rec.forTask(survivor.ID)) == 0 {
		t.Fatalf("the successful item published nothing")
	}
}

// TestTaskRemove_NonDomainFailureAbortsTheWholeCall pins the deliberate
// exception in runItem: an error that is not a *domain.Error is an I/O or
// driver failure, not a refusal. Reporting it as ok:false with no code and no
// remediation would be the silent downgrade AGENTS.md forbids, so the call
// fails loudly and nothing commits.
func TestTaskRemove_NonDomainFailureAbortsTheWholeCall(t *testing.T) {
	env := openTestEnv(t)
	first := makeBacklogTask(t, env, "first")
	boom := makeBacklogTask(t, env, "boom")

	rec := &recordingPublisher{}
	svc := New(&patchedStore{
		Store: env.Store,
		tasks: &failingTaskRepo{
			TaskRepo: env.Tasks(),
			failFor:  boom.ID,
			err:      errors.New("disk went away"),
		},
	}, rec)

	_, err := svc.TaskRemove(context.Background(), env.actor, TaskRemoveInput{
		Items: []RemoveItem{{Key: first.Key}, {Key: boom.Key}},
	})
	if err == nil {
		t.Fatalf("a driver-level failure was reported as a per-item result")
	}
	if !boardHasTask(t, env, first.Key) {
		t.Fatalf("the earlier item stayed archived after the call aborted")
	}
	if len(rec.all()) != 0 {
		t.Fatalf("aborted call published %d event(s)", len(rec.all()))
	}
}
