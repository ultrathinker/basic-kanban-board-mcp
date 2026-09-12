package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// TestChat_SaveAndReadWithAuthorAndTime verifies that a message is saved
// and read back with its author, body, and timestamp preserved. It also
// checks that a zero-valued CreatedAt is populated from tx.Now().
func TestChat_SaveAndReadWithAuthorAndTime(t *testing.T) {
	t.Parallel()
	ts := openTestStore(t)
	p, _ := seedProject(t, ts)

	fixedTime := time.Date(2026, 9, 12, 10, 30, 0, 0, time.UTC)
	msg1 := &domain.ChatMessage{
		ID:        "msg-1",
		ProjectID: p.ID,
		Author:    "agent-claude",
		Body:      "Starting task analysis.",
		CreatedAt: fixedTime,
	}

	err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Chat().Add(tx, msg1)
	})
	if err != nil {
		t.Fatalf("Chat().Add msg1: %v", err)
	}

	// Message with zero CreatedAt should be populated from tx.Now().
	msg2 := &domain.ChatMessage{
		ID:        "msg-2",
		ProjectID: p.ID,
		Author:    "agent-codex",
		Body:      "Implementation complete.",
	}
	err = ts.Write(context.Background(), func(tx Tx) error {
		return ts.Chat().Add(tx, msg2)
	})
	if err != nil {
		t.Fatalf("Chat().Add msg2: %v", err)
	}
	if msg2.CreatedAt.IsZero() {
		t.Fatalf("Chat().Add did not set CreatedAt on msg2")
	}

	err = ts.Read(context.Background(), func(tx Tx) error {
		msgs, err := ts.Chat().List(tx, ChatFilter{ProjectID: &p.ID})
		if err != nil {
			return err
		}
		if len(msgs) != 2 {
			t.Fatalf("got %d messages, want 2", len(msgs))
		}

		// Find msg1
		var found1, found2 *domain.ChatMessage
		for i := range msgs {
			if msgs[i].ID == "msg-1" {
				found1 = &msgs[i]
			}
			if msgs[i].ID == "msg-2" {
				found2 = &msgs[i]
			}
		}

		if found1 == nil {
			t.Fatalf("msg-1 not found in chat read")
		}
		if found1.Author != "agent-claude" {
			t.Errorf("msg-1 author = %q, want %q", found1.Author, "agent-claude")
		}
		if found1.Body != "Starting task analysis." {
			t.Errorf("msg-1 body = %q, want %q", found1.Body, "Starting task analysis.")
		}
		if !found1.CreatedAt.Equal(fixedTime) {
			t.Errorf("msg-1 CreatedAt = %v, want %v", found1.CreatedAt, fixedTime)
		}

		if found2 == nil {
			t.Fatalf("msg-2 not found in chat read")
		}
		if found2.Author != "agent-codex" {
			t.Errorf("msg-2 author = %q, want %q", found2.Author, "agent-codex")
		}
		if found2.Body != "Implementation complete." {
			t.Errorf("msg-2 body = %q, want %q", found2.Body, "Implementation complete.")
		}
		if found2.CreatedAt.IsZero() {
			t.Errorf("msg-2 CreatedAt is zero")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Chat().List: %v", err)
	}
}

// TestChat_ReadOrderNewestFirst verifies that chat messages are returned
// newest first (descending by created_at).
func TestChat_ReadOrderNewestFirst(t *testing.T) {
	t.Parallel()
	ts := openTestStore(t)
	p, _ := seedProject(t, ts)

	tOld := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	tMid := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)
	tNew := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

	err := ts.Write(context.Background(), func(tx Tx) error {
		if err := ts.Chat().Add(tx, &domain.ChatMessage{
			ID: "msg-old", ProjectID: p.ID, Author: "a", Body: "old", CreatedAt: tOld,
		}); err != nil {
			return err
		}
		if err := ts.Chat().Add(tx, &domain.ChatMessage{
			ID: "msg-mid", ProjectID: p.ID, Author: "b", Body: "mid", CreatedAt: tMid,
		}); err != nil {
			return err
		}
		if err := ts.Chat().Add(tx, &domain.ChatMessage{
			ID: "msg-new", ProjectID: p.ID, Author: "c", Body: "new", CreatedAt: tNew,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed messages: %v", err)
	}

	err = ts.Read(context.Background(), func(tx Tx) error {
		msgs, err := ts.Chat().List(tx, ChatFilter{ProjectID: &p.ID})
		if err != nil {
			return err
		}
		if len(msgs) != 3 {
			t.Fatalf("got %d messages, want 3", len(msgs))
		}
		if msgs[0].ID != "msg-new" || msgs[1].ID != "msg-mid" || msgs[2].ID != "msg-old" {
			t.Fatalf("order = [%s, %s, %s], want [msg-new, msg-mid, msg-old]",
				msgs[0].ID, msgs[1].ID, msgs[2].ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Chat().List: %v", err)
	}
}

// TestChat_CursorPaginationTieBreak verifies that keyset pagination with
// cursor by time breaks ties deterministically by ID without omitting or
// duplicating messages, especially when multiple messages land on the exact
// same timestamp.
func TestChat_CursorPaginationTieBreak(t *testing.T) {
	t.Parallel()
	ts := openTestStore(t)
	p, _ := seedProject(t, ts)

	t1 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 12, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

	// Six messages:
	// M1, M2, M3 have timestamp t1 (ties broken by ID)
	// M4, M5 have timestamp t2
	// M6 has timestamp t3
	messages := []*domain.ChatMessage{
		{ID: "id-1", ProjectID: p.ID, Author: "a", Body: "t1-1", CreatedAt: t1},
		{ID: "id-2", ProjectID: p.ID, Author: "a", Body: "t1-2", CreatedAt: t1},
		{ID: "id-3", ProjectID: p.ID, Author: "a", Body: "t1-3", CreatedAt: t1},
		{ID: "id-4", ProjectID: p.ID, Author: "b", Body: "t2-4", CreatedAt: t2},
		{ID: "id-5", ProjectID: p.ID, Author: "b", Body: "t2-5", CreatedAt: t2},
		{ID: "id-6", ProjectID: p.ID, Author: "c", Body: "t3-6", CreatedAt: t3},
	}

	err := ts.Write(context.Background(), func(tx Tx) error {
		for _, m := range messages {
			if err := ts.Chat().Add(tx, m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed messages: %v", err)
	}

	// Expected total ordering newest first (created_at DESC, id DESC):
	// 1: id-6 (t3)
	// 2: id-5 (t2)
	// 3: id-4 (t2)
	// 4: id-3 (t1)
	// 5: id-2 (t1)
	// 6: id-1 (t1)
	expectedIDs := []string{"id-6", "id-5", "id-4", "id-3", "id-2", "id-1"}

	// Page through with limit = 2
	var collectedIDs []string
	var cursor *ChatCursor

	for page := 1; page <= 5; page++ {
		var pageMsgs []domain.ChatMessage
		err := ts.Read(context.Background(), func(tx Tx) error {
			var err error
			pageMsgs, err = ts.Chat().List(tx, ChatFilter{
				ProjectID: &p.ID,
				Limit:     2,
				Before:    cursor,
			})
			return err
		})
		if err != nil {
			t.Fatalf("page %d read: %v", page, err)
		}

		if len(pageMsgs) == 0 {
			break
		}

		for _, m := range pageMsgs {
			collectedIDs = append(collectedIDs, m.ID)
		}

		last := pageMsgs[len(pageMsgs)-1]
		cursor = &ChatCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	if len(collectedIDs) != len(expectedIDs) {
		t.Fatalf("collected %d messages %v, want %d %v",
			len(collectedIDs), collectedIDs, len(expectedIDs), expectedIDs)
	}

	for i := range expectedIDs {
		if collectedIDs[i] != expectedIDs[i] {
			t.Errorf("at index %d got %s, want %s (all collected: %v)",
				i, collectedIDs[i], expectedIDs[i], collectedIDs)
		}
	}
}

// TestChat_ReadWithoutProjectFilter verifies that reading without a project
// filter returns messages across all projects.
func TestChat_ReadWithoutProjectFilter(t *testing.T) {
	t.Parallel()
	ts := openTestStore(t)

	// Seed two distinct projects: BMB and KAN
	p1, _ := seedProject(t, ts)

	p2 := &domain.Project{
		ID:                  uuid.NewString(),
		Key:                 "KAN",
		Name:                "Kanban",
		Version:             1,
		NextTaskSeq:         1,
		EstimateUnit:        "h",
		EnforceDependencies: true,
	}
	err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Projects().Create(tx, p2)
	})
	if err != nil {
		t.Fatalf("create project 2: %v", err)
	}

	now := time.Now().UTC()
	err = ts.Write(context.Background(), func(tx Tx) error {
		if err := ts.Chat().Add(tx, &domain.ChatMessage{
			ID: "p1-msg", ProjectID: p1.ID, Author: "bot1", Body: "project 1 msg", CreatedAt: now,
		}); err != nil {
			return err
		}
		if err := ts.Chat().Add(tx, &domain.ChatMessage{
			ID: "p2-msg", ProjectID: p2.ID, Author: "bot2", Body: "project 2 msg", CreatedAt: now.Add(time.Second),
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed messages: %v", err)
	}

	// Read with ProjectID: nil (all projects)
	err = ts.Read(context.Background(), func(tx Tx) error {
		msgs, err := ts.Chat().List(tx, ChatFilter{})
		if err != nil {
			return err
		}
		if len(msgs) < 2 {
			t.Fatalf("got %d messages, want at least 2 across projects", len(msgs))
		}
		var foundP1, foundP2 bool
		for _, m := range msgs {
			if m.ID == "p1-msg" && m.ProjectID == p1.ID {
				foundP1 = true
			}
			if m.ID == "p2-msg" && m.ProjectID == p2.ID {
				foundP2 = true
			}
		}
		if !foundP1 || !foundP2 {
			t.Fatalf("foundP1=%v foundP2=%v, want both true", foundP1, foundP2)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Chat().List: %v", err)
	}
}

// TestChat_Validation_EmptyBodyAndLengthLimit verifies that empty bodies and
// bodies exceeding 4000 characters are rejected with validation errors, while
// valid bodies (including max length) succeed.
func TestChat_Validation_EmptyBodyAndLengthLimit(t *testing.T) {
	t.Parallel()
	ts := openTestStore(t)
	p, _ := seedProject(t, ts)

	// 1. Empty body
	err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Chat().Add(tx, &domain.ChatMessage{
			ID: "msg-empty", ProjectID: p.ID, Author: "bot", Body: "",
		})
	})
	if err == nil {
		t.Fatalf("expected error for empty body, got nil")
	}
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation {
		t.Errorf("empty body err = %v, want CodeInvalid", err)
	}

	// 2. Whitespace-only body
	err = ts.Write(context.Background(), func(tx Tx) error {
		return ts.Chat().Add(tx, &domain.ChatMessage{
			ID: "msg-ws", ProjectID: p.ID, Author: "bot", Body: "   \n\t  ",
		})
	})
	if err == nil {
		t.Fatalf("expected error for whitespace-only body, got nil")
	}
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation {
		t.Errorf("whitespace body err = %v, want CodeInvalid", err)
	}

	// 3. Exceeding limit (4001 characters)
	longBody := strings.Repeat("x", domain.MaxChatMessageLen+1)
	err = ts.Write(context.Background(), func(tx Tx) error {
		return ts.Chat().Add(tx, &domain.ChatMessage{
			ID: "msg-too-long", ProjectID: p.ID, Author: "bot", Body: longBody,
		})
	})
	if err == nil {
		t.Fatalf("expected error for body exceeding %d characters, got nil", domain.MaxChatMessageLen)
	}
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation {
		t.Errorf("too long body err = %v, want CodeInvalid", err)
	}

	// 4. Max allowed limit (exactly 4000 characters)
	maxBody := strings.Repeat("y", domain.MaxChatMessageLen)
	err = ts.Write(context.Background(), func(tx Tx) error {
		return ts.Chat().Add(tx, &domain.ChatMessage{
			ID: "msg-max-ok", ProjectID: p.ID, Author: "bot", Body: maxBody,
		})
	})
	if err != nil {
		t.Fatalf("expected success for exactly %d characters, got %v", domain.MaxChatMessageLen, err)
	}
}

// TestChat_CascadeDeleteOnProject verifies that deleting a project cascades
// and removes all of its chat messages.
func TestChat_CascadeDeleteOnProject(t *testing.T) {
	t.Parallel()
	ts := openTestStore(t)
	p, _ := seedProject(t, ts)

	err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Chat().Add(tx, &domain.ChatMessage{
			ID: "msg-cascade", ProjectID: p.ID, Author: "bot", Body: "will be deleted",
		})
	})
	if err != nil {
		t.Fatalf("Chat().Add: %v", err)
	}

	// Verify message exists
	err = ts.Read(context.Background(), func(tx Tx) error {
		msgs, err := ts.Chat().List(tx, ChatFilter{ProjectID: &p.ID})
		if err != nil {
			return err
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages before delete, want 1", len(msgs))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify msg exists: %v", err)
	}

	// Delete project
	err = ts.Write(context.Background(), func(tx Tx) error {
		return ts.Projects().Delete(tx, p.ID)
	})
	if err != nil {
		t.Fatalf("Projects().Delete: %v", err)
	}

	// Verify message is gone
	err = ts.Read(context.Background(), func(tx Tx) error {
		msgs, err := ts.Chat().List(tx, ChatFilter{ProjectID: &p.ID})
		if err != nil {
			return err
		}
		if len(msgs) != 0 {
			t.Fatalf("got %d messages after project delete, want 0", len(msgs))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify msg deleted: %v", err)
	}
}
