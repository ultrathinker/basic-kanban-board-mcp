package service

import (
	"context"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// KANB-12: progress assessments and chat messages must publish a "something
// changed" signal after they commit — that signal is the ONLY thing SSE
// carries; the board's live regions re-read the real data (progress_marks,
// chat_messages) from their own tables, never from the event payload. These
// tests pin both halves of that contract for each of the two new event
// types: the event actually reaches a subscriber (recordingPublisher stands
// in for internal/events.Bus, exactly as batch_isolation_test.go's rollback
// tests already do), and its payload never smuggles the feature data itself.

// eventsInStore reads back every committed event for a project, newest last,
// the same way storedEventsForTask does but without requiring a task id (a
// project-level progress mark has none).
func eventsInStore(t *testing.T, env *testEnv, projectID string) []domain.Event {
	t.Helper()
	var out []domain.Event
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		var err error
		out, err = env.Events().Latest(tx, projectID, 500)
		return err
	}); err != nil {
		t.Fatalf("read events: %v", err)
	}
	return out
}

// TestProgressSet_TaskScope_PublishesSignalOnly pins acceptance criterion 1
// (a progress bar can update live) at its source: recording a task-scoped
// mark must both persist and publish exactly one progress.recorded event
// naming the task, and the event's payload must carry nothing beyond that —
// no percent, no assessor, no ETA. A refresh that needed those fields out of
// the event rather than out of progress_marks would defeat the whole "the
// journal is only a signal" design the pruning test (TestEventsPrune_Spares
// ProgressAndChat, internal/store/progress_test.go) exists to protect.
func TestProgressSet_TaskScope_PublishesSignalOnly(t *testing.T) {
	env := openTestEnv(t)
	rec := &recordingPublisher{}
	svc := New(env.Store, rec)

	task := makeBacklogTask(t, env, "assessed task")

	res, err := svc.ProgressSet(context.Background(), env.actor, ProgressSetInput{
		Assessor: "agent-1",
		Percent:  35,
		TaskKey:  task.Key,
	})
	if err != nil {
		t.Fatalf("ProgressSet: %v", err)
	}
	if res.Mark.Percent != 35 {
		t.Fatalf("mark landed with percent %d, want 35 — the store side of this call", res.Mark.Percent)
	}

	evs := rec.forTask(task.ID)
	if len(evs) != 1 {
		t.Fatalf("published %d events for the task, want exactly 1: %+v", len(evs), evs)
	}
	e := evs[0]
	if e.Type != domain.EventProgressRecorded {
		t.Fatalf("event type = %q, want %q", e.Type, domain.EventProgressRecorded)
	}
	if e.ProjectID != env.proj.ID {
		t.Fatalf("event ProjectID = %q, want %q", e.ProjectID, env.proj.ID)
	}
	if e.TaskID == nil || *e.TaskID != task.ID {
		t.Fatalf("event TaskID = %v, want %s", e.TaskID, task.ID)
	}
	if got := e.Payload["key"]; got != task.Key {
		t.Fatalf("event payload[key] = %v, want %q", got, task.Key)
	}
	for _, field := range []string{"percent", "assessor", "eta"} {
		if _, ok := e.Payload[field]; ok {
			t.Fatalf("event payload carries %q — the mark's own data must live only in progress_marks, never in the event", field)
		}
	}

	// The event is not just in-process; it is the same row a replaying SSE
	// subscriber (Bus's HistoryLoader) would read back.
	stored := eventsInStore(t, env, env.proj.ID)
	found := false
	for _, se := range stored {
		if se.Type == domain.EventProgressRecorded && se.TaskID != nil && *se.TaskID == task.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("progress.recorded event was published but never persisted to the events table")
	}
}

// TestProgressSet_ProjectScope_PublishesSignalWithNoTask covers the other
// half of ProgressSet: an assessment of the project as a whole (no task)
// still fans out a signal so the header's "assessed" bar can refresh without
// the owner reloading — the concrete gap this task closes, since the header
// bars sit outside #board and previously had no event at all keeping them
// live.
func TestProgressSet_ProjectScope_PublishesSignalWithNoTask(t *testing.T) {
	env := openTestEnv(t)
	rec := &recordingPublisher{}
	svc := New(env.Store, rec)

	_, err := svc.ProgressSet(context.Background(), env.actor, ProgressSetInput{
		Assessor:   "project-evaluator",
		Percent:    60,
		ProjectKey: env.proj.Key,
	})
	if err != nil {
		t.Fatalf("ProgressSet: %v", err)
	}

	evs := rec.all()
	if len(evs) != 1 {
		t.Fatalf("published %d events, want exactly 1: %+v", len(evs), evs)
	}
	e := evs[0]
	if e.Type != domain.EventProgressRecorded {
		t.Fatalf("event type = %q, want %q", e.Type, domain.EventProgressRecorded)
	}
	if e.ProjectID != env.proj.ID {
		t.Fatalf("event ProjectID = %q, want %q", e.ProjectID, env.proj.ID)
	}
	if e.TaskID != nil {
		t.Fatalf("event TaskID = %v, want nil for a project-level assessment", e.TaskID)
	}
}

// TestChatAdd_PublishesSignalWithoutTheMessageBody pins acceptance criterion
// 2 (a chat message can appear live) plus the "journal is only a signal"
// rule from the opposite direction of the progress test above: the payload
// must not carry the body, the author, or anything else a live refresh could
// be tempted to render straight from the event instead of re-reading
// chat_messages.
func TestChatAdd_PublishesSignalWithoutTheMessageBody(t *testing.T) {
	env := openTestEnv(t)
	rec := &recordingPublisher{}
	svc := New(env.Store, rec)

	secretBody := "the root cause was a stale cache entry"
	msg, err := svc.ChatAdd(context.Background(), env.actor, ChatAddInput{
		ProjectKey: env.proj.Key,
		Author:     "agent-thinking-aloud",
		Body:       secretBody,
	})
	if err != nil {
		t.Fatalf("ChatAdd: %v", err)
	}
	if msg.Body != secretBody {
		t.Fatalf("stored body = %q, want %q — the store side of this call", msg.Body, secretBody)
	}

	evs := rec.all()
	if len(evs) != 1 {
		t.Fatalf("published %d events, want exactly 1: %+v", len(evs), evs)
	}
	e := evs[0]
	if e.Type != domain.EventChatPosted {
		t.Fatalf("event type = %q, want %q", e.Type, domain.EventChatPosted)
	}
	if e.ProjectID != env.proj.ID {
		t.Fatalf("event ProjectID = %q, want %q", e.ProjectID, env.proj.ID)
	}
	if e.TaskID != nil {
		t.Fatalf("event TaskID = %v, want nil — chat is project-scoped, not task-scoped", e.TaskID)
	}
	for k, v := range e.Payload {
		t.Fatalf("event payload carries %q=%v — a chat.posted event must be an empty signal, the message lives only in chat_messages", k, v)
	}

	stored := eventsInStore(t, env, env.proj.ID)
	found := false
	for _, se := range stored {
		if se.Type == domain.EventChatPosted {
			found = true
			if se.Payload != nil {
				for k := range se.Payload {
					if k == "body" || k == "text" || k == "author" {
						t.Fatalf("persisted event smuggled chat content via payload[%q]", k)
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("chat.posted event was published but never persisted to the events table")
	}
}
