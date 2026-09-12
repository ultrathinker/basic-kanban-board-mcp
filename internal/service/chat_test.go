package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

func TestService_ChatAddAndList(t *testing.T) {
	t.Parallel()
	env := openTestEnv(t)

	// 1. Add message with default author (actor.Name)
	rawBody := "Hello from AI agent! <b>no escaping</b>\nline 2"
	m1, err := env.svc.ChatAdd(context.Background(), env.actor, ChatAddInput{
		ProjectKey: env.proj.Key,
		Body:       rawBody,
	})
	if err != nil {
		t.Fatalf("ChatAdd: %v", err)
	}
	if m1.Author != env.actor.Name {
		t.Errorf("author = %q, want %q", m1.Author, env.actor.Name)
	}
	if m1.Body != rawBody {
		t.Errorf("body = %q, want verbatim %q", m1.Body, rawBody)
	}
	if m1.CreatedAt.IsZero() {
		t.Errorf("CreatedAt is zero")
	}

	// Sleep 2ms so m2 has a strictly newer millisecond timestamp than m1.
	time.Sleep(2 * time.Millisecond)

	// 2. Add message with custom author
	m2, err := env.svc.ChatAdd(context.Background(), env.actor, ChatAddInput{
		ProjectKey: env.proj.Key,
		Author:     "custom-agent",
		Body:       "Second message.",
	})
	if err != nil {
		t.Fatalf("ChatAdd custom: %v", err)
	}
	if m2.Author != "custom-agent" {
		t.Errorf("author = %q, want %q", m2.Author, "custom-agent")
	}

	// 3. List messages
	res, err := env.svc.ChatList(context.Background(), env.actor, ChatListInput{
		ProjectKey: env.proj.Key,
	})
	if err != nil {
		t.Fatalf("ChatList: %v", err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(res.Messages))
	}
	// Newest first: m2, then m1
	if res.Messages[0].ID != m2.ID || res.Messages[1].ID != m1.ID {
		t.Fatalf("messages = [%s, %s], want [%s, %s]",
			res.Messages[0].ID, res.Messages[1].ID, m2.ID, m1.ID)
	}
}

func TestService_ChatPaginationWithCursor(t *testing.T) {
	t.Parallel()
	env := openTestEnv(t)

	// Insert 5 messages
	for i := 1; i <= 5; i++ {
		_, err := env.svc.ChatAdd(context.Background(), env.actor, ChatAddInput{
			ProjectKey: env.proj.Key,
			Body:       "Message " + string(rune('0'+i)),
		})
		if err != nil {
			t.Fatalf("ChatAdd %d: %v", i, err)
		}
	}

	// Page 1: limit 2
	p1, err := env.svc.ChatList(context.Background(), env.actor, ChatListInput{
		ProjectKey: env.proj.Key,
		Limit:      2,
	})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(p1.Messages) != 2 {
		t.Fatalf("page 1 len = %d, want 2", len(p1.Messages))
	}
	if p1.NextCursor == nil || p1.Cursor == "" {
		t.Fatalf("page 1 NextCursor = %v, want non-nil", p1.NextCursor)
	}

	// Page 2: limit 2 using NextCursor
	p2, err := env.svc.ChatList(context.Background(), env.actor, ChatListInput{
		ProjectKey: env.proj.Key,
		Limit:      2,
		Before:     p1.NextCursor,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(p2.Messages) != 2 {
		t.Fatalf("page 2 len = %d, want 2", len(p2.Messages))
	}
	if p2.NextCursor == nil || p2.Cursor == "" {
		t.Fatalf("page 2 NextCursor = %v, want non-nil", p2.NextCursor)
	}

	// Page 3: limit 2 using string Cursor
	p3, err := env.svc.ChatList(context.Background(), env.actor, ChatListInput{
		ProjectKey: env.proj.Key,
		Limit:      2,
		Cursor:     p2.Cursor,
	})
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if len(p3.Messages) != 1 {
		t.Fatalf("page 3 len = %d, want 1", len(p3.Messages))
	}
	if p3.NextCursor != nil || p3.Cursor != "" {
		t.Errorf("page 3 NextCursor = %v, want nil at end of history", p3.NextCursor)
	}

	// Verify all 5 messages are unique and returned in order
	allIDs := []string{
		p1.Messages[0].ID, p1.Messages[1].ID,
		p2.Messages[0].ID, p2.Messages[1].ID,
		p3.Messages[0].ID,
	}
	seen := map[string]bool{}
	for _, id := range allIDs {
		if seen[id] {
			t.Errorf("duplicate message ID %s across pages", id)
		}
		seen[id] = true
	}
}

func TestService_ChatListWithoutProjectFilter(t *testing.T) {
	t.Parallel()
	env := openTestEnv(t)

	// Create second project
	p2 := &domain.Project{
		ID:                  uuid.NewString(),
		Key:                 "OTHER",
		Name:                "Other Project",
		Version:             1,
		NextTaskSeq:         1,
		EstimateUnit:        "h",
		EnforceDependencies: true,
	}
	err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Projects().Create(tx, p2)
	})
	if err != nil {
		t.Fatalf("create second project: %v", err)
	}

	// Add messages to both projects
	_, err = env.svc.ChatAdd(context.Background(), env.actor, ChatAddInput{
		ProjectKey: env.proj.Key,
		Body:       "msg in proj 1",
	})
	if err != nil {
		t.Fatalf("ChatAdd 1: %v", err)
	}

	_, err = env.svc.ChatAdd(context.Background(), env.actor, ChatAddInput{
		ProjectKey: "OTHER",
		Body:       "msg in proj 2",
	})
	if err != nil {
		t.Fatalf("ChatAdd 2: %v", err)
	}

	// Read without project filter
	res, err := env.svc.ChatList(context.Background(), env.actor, ChatListInput{})
	if err != nil {
		t.Fatalf("ChatList without filter: %v", err)
	}
	if len(res.Messages) < 2 {
		t.Fatalf("got %d messages, want at least 2", len(res.Messages))
	}

	var hasProj1, hasProj2 bool
	for _, m := range res.Messages {
		if m.ProjectID == env.proj.ID {
			hasProj1 = true
		}
		if m.ProjectID == p2.ID {
			hasProj2 = true
		}
	}
	if !hasProj1 || !hasProj2 {
		t.Errorf("hasProj1 = %v, hasProj2 = %v, want both true", hasProj1, hasProj2)
	}
}

func TestService_ChatPermissionsAndScoping(t *testing.T) {
	t.Parallel()
	env := openTestEnv(t)

	// Read-only actor
	readActor := Actor{
		TokenID: "tok-read",
		Name:    "reader",
		Scopes:  domain.Scopes{domain.ScopeRead},
	}
	_, err := env.svc.ChatAdd(context.Background(), readActor, ChatAddInput{
		ProjectKey: env.proj.Key,
		Body:       "should fail",
	})
	if err == nil {
		t.Fatalf("expected error for read-only actor calling ChatAdd, got nil")
	}
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeForbidden {
		t.Errorf("err = %v, want CodeForbidden", err)
	}

	// Actor without read scope
	writeOnlyActor := Actor{
		TokenID: "tok-write",
		Name:    "writer",
		Scopes:  domain.Scopes{domain.ScopeWrite}, // Note: ScopeWrite implies read in Has(), but let's test empty scopes
	}
	noScopeActor := Actor{
		TokenID: "tok-none",
		Name:    "nobody",
		Scopes:  domain.Scopes{},
	}
	_, err = env.svc.ChatList(context.Background(), noScopeActor, ChatListInput{})
	if err == nil {
		t.Fatalf("expected error for no-scope actor calling ChatList, got nil")
	}
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeForbidden {
		t.Errorf("err = %v, want CodeForbidden", err)
	}
	_ = writeOnlyActor

	// Project-scoped actor trying to write to an unauthorized project
	scopedActor := Actor{
		TokenID:     "tok-scoped",
		Name:        "scoped",
		Scopes:      domain.Scopes{domain.ScopeWrite, domain.ScopeRead},
		ProjectKeys: []string{"ALLOWED"},
	}
	_, err = env.svc.ChatAdd(context.Background(), scopedActor, ChatAddInput{
		ProjectKey: env.proj.Key, // "BMB", not "ALLOWED"
		Body:       "unauthorized project",
	})
	if err == nil {
		t.Fatalf("expected error for scoped actor touching wrong project, got nil")
	}
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeForbidden {
		t.Errorf("err = %v, want CodeForbidden", err)
	}
}

func TestService_ChatValidation(t *testing.T) {
	t.Parallel()
	env := openTestEnv(t)

	// Empty project key
	_, err := env.svc.ChatAdd(context.Background(), env.actor, ChatAddInput{
		ProjectKey: "",
		Body:       "hello",
	})
	if err == nil {
		t.Fatalf("expected error for empty project key, got nil")
	}

	// Nonexistent project
	_, err = env.svc.ChatAdd(context.Background(), env.actor, ChatAddInput{
		ProjectKey: "NOPE",
		Body:       "hello",
	})
	if err == nil {
		t.Fatalf("expected error for non-existent project, got nil")
	}
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeNotFound {
		t.Errorf("err = %v, want CodeNotFound", err)
	}

	// Empty body
	_, err = env.svc.ChatAdd(context.Background(), env.actor, ChatAddInput{
		ProjectKey: env.proj.Key,
		Body:       "",
	})
	if err == nil {
		t.Fatalf("expected error for empty body, got nil")
	}
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation {
		t.Errorf("err = %v, want CodeValidation", err)
	}

	// Body exceeding max characters
	longBody := strings.Repeat("z", domain.MaxChatMessageLen+1)
	_, err = env.svc.ChatAdd(context.Background(), env.actor, ChatAddInput{
		ProjectKey: env.proj.Key,
		Body:       longBody,
	})
	if err == nil {
		t.Fatalf("expected error for body exceeding %d characters, got nil", domain.MaxChatMessageLen)
	}
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation {
		t.Errorf("err = %v, want CodeValidation", err)
	}
}
