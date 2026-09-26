package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// LatestByTasks feeds the board's idle-time check: a wrong answer either
// hides a stale card (returning an older note) or flags a live one (dropping
// a task's notes). The notes are inserted out of chronological order so an
// implementation returning the first or last inserted row fails.
func TestNotes_LatestByTasks(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	a := seedTask(t, ts, p, cols["Doing"], "a", "alice")
	b := seedTask(t, ts, p, cols["Doing"], "b", "alice")
	quiet := seedTask(t, ts, p, cols["Doing"], "quiet", "alice")

	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	add := func(taskID string, at time.Time) {
		t.Helper()
		err := ts.Write(context.Background(), func(tx Tx) error {
			return ts.Notes().Add(tx, &domain.Note{ID: uuid.NewString(), TaskID: taskID, Author: "alice", Body: "n", CreatedAt: at})
		})
		if err != nil {
			t.Fatalf("add note: %v", err)
		}
	}
	add(a.ID, base.Add(2*time.Hour))
	add(a.ID, base.Add(5*time.Hour+250*time.Millisecond))
	add(a.ID, base)
	add(b.ID, base.Add(time.Hour))

	var got map[string]time.Time
	err := ts.Read(context.Background(), func(tx Tx) error {
		var err error
		got, err = ts.Notes().LatestByTasks(tx, []string{a.ID, b.ID, quiet.ID})
		return err
	})
	if err != nil {
		t.Fatalf("LatestByTasks: %v", err)
	}
	if want := base.Add(5*time.Hour + 250*time.Millisecond); !got[a.ID].Equal(want) {
		t.Errorf("a: latest = %v, want %v", got[a.ID], want)
	}
	if want := base.Add(time.Hour); !got[b.ID].Equal(want) {
		t.Errorf("b: latest = %v, want %v", got[b.ID], want)
	}
	if v, ok := got[quiet.ID]; ok {
		t.Errorf("a task without notes must be absent, got %v", v)
	}

	err = ts.Read(context.Background(), func(tx Tx) error {
		empty, err := ts.Notes().LatestByTasks(tx, nil)
		if err == nil && len(empty) != 0 {
			t.Errorf("no ids: got %v, want empty", empty)
		}
		return err
	})
	if err != nil {
		t.Fatalf("LatestByTasks(nil): %v", err)
	}
}
