package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// chatOlderStubService answers ChatList with a canned page and records the
// input it was called with, so tests can pin exactly what the handler
// forwards to the service (the project key, and the "before" cursor as
// service.ChatListInput.Cursor — the seam service.ChatList itself turns
// into a strict "created_at < X OR (created_at = X AND id < Y)" query,
// which is what actually rules out duplicates and gaps at the page
// boundary; see internal/store/chat.go).
type chatOlderStubService struct {
	service.Service
	result   *service.ChatListResult
	err      error
	lastIn   service.ChatListInput
	lastActr service.Actor
	calls    int
}

func (s *chatOlderStubService) ChatList(_ context.Context, a service.Actor, in service.ChatListInput) (*service.ChatListResult, error) {
	s.calls++
	s.lastIn = in
	s.lastActr = a
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func newChatOlderTestWeb(t *testing.T, svc service.Service) (*Web, *domain.Session) {
	t.Helper()
	tpl, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	mgr := newTestManager(t)
	w := New(Deps{
		Service:   svc,
		Auth:      mgr,
		Bus:       events.New(newEmptyHistory(), 4),
		Templates: tpl,
		BaseURL:   "http://127.0.0.1:0",
	})
	sess := sessionFor(t, mgr, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")
	return w, sess
}

// TestChatOlder_RequiresBeforeParam: no "before" is a 400, not a silent
// first-page fetch — the endpoint only ever serves one page relative to a
// caller-supplied cursor.
func TestChatOlder_RequiresBeforeParam(t *testing.T) {
	svc := &chatOlderStubService{}
	w, sess := newChatOlderTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/chat/older", nil, sessionCookie(sess))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", rw.Code, rw.Body.String())
	}
	if svc.calls != 0 {
		t.Fatalf("ChatList called %d times, want 0: a missing cursor must not reach the service", svc.calls)
	}
	if !strings.Contains(rw.Body.String(), "before is required") {
		t.Fatalf("body = %s, want the before-is-required message", rw.Body.String())
	}
}

// TestChatOlder_Unauthenticated: no session and no bearer token gets a
// non-2xx status (this is a JS-fetch endpoint, not a page — no redirect).
func TestChatOlder_Unauthenticated(t *testing.T) {
	svc := &chatOlderStubService{}
	w, _ := newChatOlderTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/chat/older?before=2026-09-12T12%3A00%3A00Z%2Fm1", nil)
	if rw.Code == http.StatusOK || rw.Code == http.StatusFound || rw.Code == http.StatusSeeOther {
		t.Fatalf("status = %d, want a non-2xx, non-redirect status for an unauthenticated fetch", rw.Code)
	}
}

// TestChatOlder_RendersOldestFirstWithNextCursorHeader is the core
// contract: the fragment body holds the page's messages reordered oldest
// first (matching the initial panel's own order), and the next page's
// cursor rides in X-Chat-Next-Cursor, not in the body — the body is meant
// to be inserted straight into an <ol>, which only tolerates <li> children.
func TestChatOlder_RendersOldestFirstWithNextCursorHeader(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	svc := &chatOlderStubService{
		result: &service.ChatListResult{
			// ChatList's own contract: newest first.
			Messages: []domain.ChatMessage{
				{ID: "m2", Author: "agent-b", Body: "newer of this page", CreatedAt: now.Add(-time.Minute)},
				{ID: "m1", Author: "agent-a", Body: "older of this page", CreatedAt: now.Add(-2 * time.Minute)},
			},
			NextCursor: &domain.ChatCursor{CreatedAt: now.Add(-2 * time.Minute), ID: "m1"},
			Cursor:     (&domain.ChatCursor{CreatedAt: now.Add(-2 * time.Minute), ID: "m1"}).String(),
		},
	}
	w, sess := newChatOlderTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/chat/older?before=2026-09-12T12%3A00%3A00Z%2Fm3", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	if svc.lastIn.ProjectKey != "BMB" {
		t.Fatalf("ChatList called with ProjectKey = %q, want BMB", svc.lastIn.ProjectKey)
	}
	if svc.lastIn.Cursor != "2026-09-12T12:00:00Z/m3" {
		t.Fatalf("ChatList called with Cursor = %q, want the decoded before= value", svc.lastIn.Cursor)
	}
	body := rw.Body.String()
	older := strings.Index(body, "older of this page")
	newer := strings.Index(body, "newer of this page")
	if older < 0 || newer < 0 {
		t.Fatalf("page missing a message: %s", body)
	}
	if older > newer {
		t.Fatal("older-page fragment is not oldest-first")
	}
	// It is a bare run of <li>s, not a second <ol>.
	if strings.Contains(body, "<ol") {
		t.Fatal("fragment carries its own <ol>; it must be bare <li> markup for insertAdjacentHTML")
	}
	wantCursor := svc.result.Cursor
	if got := rw.Header().Get("X-Chat-Next-Cursor"); got != wantCursor {
		t.Fatalf("X-Chat-Next-Cursor = %q, want %q", got, wantCursor)
	}
}

// TestChatOlder_ExhaustedHistoryStopsPagination: when the service reports no
// more messages, the fragment is empty and the cursor header is empty too —
// app.js's stop signal for "do not ask again".
func TestChatOlder_ExhaustedHistoryStopsPagination(t *testing.T) {
	svc := &chatOlderStubService{
		result: &service.ChatListResult{Messages: nil},
	}
	w, sess := newChatOlderTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/chat/older?before=2026-09-12T12%3A00%3A00Z%2Fm1", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("X-Chat-Next-Cursor"); got != "" {
		t.Fatalf("X-Chat-Next-Cursor = %q, want empty at exhaustion", got)
	}
	if strings.TrimSpace(rw.Body.String()) != "" {
		t.Fatalf("body = %q, want empty at exhaustion", rw.Body.String())
	}
}

// TestChatOlder_ServiceErrorSurfacesAsAPIError: a project the caller cannot
// access (or that does not exist) reports through apiError, not a page
// redirect or a silent empty page.
func TestChatOlder_ServiceErrorSurfacesAsAPIError(t *testing.T) {
	svc := &chatOlderStubService{err: domain.NotFound("project", "BMB")}
	w, sess := newChatOlderTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/chat/older?before=2026-09-12T12%3A00%3A00Z%2Fm1", nil, sessionCookie(sess))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404\n%s", rw.Code, rw.Body.String())
	}
}
