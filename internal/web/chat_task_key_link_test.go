package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// chatKeyLinkService is a minimal board+chat+TaskGet stub for KANB-11: it
// answers BoardGet with one project, ChatList with a canned page of
// messages, and TaskGet by treating exactly the keys in existingKeys as
// real (everything else comes back NotFound) — while counting how many
// times TaskGet is called, so a test can pin "exactly once per page", not
// once per message or per mention.
type chatKeyLinkService struct {
	service.Service
	chatMsgs     []domain.ChatMessage
	existingKeys map[string]bool

	taskGetCalls int
	lastKeys     []string
}

func (s *chatKeyLinkService) BoardGet(_ context.Context, _ service.Actor, _ service.BoardGetInput) (*service.Board, error) {
	return &service.Board{Projects: []service.BoardProject{{
		Key:  "BMB",
		Name: "Test",
		Columns: []service.BoardColumn{
			{Name: "Backlog", Kind: domain.KindBacklog},
		},
	}}}, nil
}

func (s *chatKeyLinkService) ProjectProgress(_ context.Context, _ service.Actor, _ service.ProjectProgressInput) (*service.ProjectProgressResult, error) {
	return nil, nil
}

func (s *chatKeyLinkService) TaskProgress(_ context.Context, _ service.Actor, _ service.TaskProgressInput) (*service.TaskProgressResult, error) {
	return nil, nil
}

func (s *chatKeyLinkService) ChatList(_ context.Context, _ service.Actor, _ service.ChatListInput) (*service.ChatListResult, error) {
	return &service.ChatListResult{Messages: s.chatMsgs}, nil
}

func (s *chatKeyLinkService) TaskGet(_ context.Context, _ service.Actor, in service.TaskGetInput) (*service.TaskGetResult, error) {
	s.taskGetCalls++
	s.lastKeys = append([]string{}, in.Keys...)
	res := &service.TaskGetResult{}
	for _, k := range in.Keys {
		if s.existingKeys[strings.ToUpper(k)] {
			res.Tasks = append(res.Tasks, domain.TaskView{Task: domain.Task{Key: strings.ToUpper(k), Title: "t"}})
		} else {
			res.NotFound = append(res.NotFound, k)
		}
	}
	return res, nil
}

func newChatKeyLinkTestWeb(t *testing.T, svc service.Service) (*Web, *domain.Session) {
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

// TestBoardChat_ExistingKeyBecomesLink_UnknownStaysPlain proves the core of
// KANB-11 part 2 end to end through the real board handler: a mentioned key
// that IS a real task becomes a link, and a mentioned key that is NOT a real
// task is left as plain, unlinked text — never a broken link.
func TestBoardChat_ExistingKeyBecomesLink_UnknownStaysPlain(t *testing.T) {
	svc := &chatKeyLinkService{
		existingKeys: map[string]bool{"BMB-1": true},
		chatMsgs: []domain.ChatMessage{
			{ID: "m1", Author: "agent-a", Body: "created BMB-1, and also mentioned BMB-999 which does not exist.", CreatedAt: time.Now().UTC()},
		},
	}
	w, sess := newChatKeyLinkTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()

	if !strings.Contains(body, `<a href="/t/BMB-1">BMB-1</a>`) {
		t.Fatalf("known key BMB-1 did not become a link:\n%s", body)
	}
	if strings.Contains(body, `<a href="/t/BMB-999"`) {
		t.Fatalf("unknown key BMB-999 became a link (a broken link):\n%s", body)
	}
	if !strings.Contains(body, "BMB-999") {
		t.Fatal("unknown key BMB-999 vanished from the message instead of staying as plain text")
	}
	if svc.taskGetCalls != 1 {
		t.Fatalf("TaskGet called %d times, want exactly 1 (one batched call per page, never per message)", svc.taskGetCalls)
	}
}

// TestBoardChat_TaskGetNeverCalledWhenNoKeyMentioned pins the cheap path:
// when nothing in the page's messages looks like a task key, the handler
// must not call TaskGet at all — the existence lookup is opportunistic, not
// an unconditional extra query on every board render.
func TestBoardChat_TaskGetNeverCalledWhenNoKeyMentioned(t *testing.T) {
	svc := &chatKeyLinkService{
		existingKeys: map[string]bool{},
		chatMsgs: []domain.ChatMessage{
			{ID: "m1", Author: "agent-a", Body: "just chatting, nothing to link here", CreatedAt: time.Now().UTC()},
		},
	}
	w, sess := newChatKeyLinkTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB = %d, want 200", rw.Code)
	}
	if svc.taskGetCalls != 0 {
		t.Fatalf("TaskGet called %d times, want 0 when no message mentions a key-shaped token", svc.taskGetCalls)
	}
}

// TestBoardChat_OneBatchedCallCoversManyMessages proves the call count does
// not scale with the number of mentions: many messages, many distinct
// candidate keys, still exactly one TaskGet call for the whole page.
func TestBoardChat_OneBatchedCallCoversManyMessages(t *testing.T) {
	existing := map[string]bool{}
	var msgs []domain.ChatMessage
	for i := 1; i <= 40; i++ {
		key := "BMB-" + strconv.Itoa(i)
		existing[key] = i%2 == 0 // half real, half not
		msgs = append(msgs, domain.ChatMessage{
			ID: strconv.Itoa(i), Author: "agent-a",
			Body: "working on " + key, CreatedAt: time.Now().UTC(),
		})
	}
	svc := &chatKeyLinkService{existingKeys: existing, chatMsgs: msgs}
	w, sess := newChatKeyLinkTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB = %d, want 200", rw.Code)
	}
	if svc.taskGetCalls != 1 {
		t.Fatalf("TaskGet called %d times across 40 mentions, want exactly 1", svc.taskGetCalls)
	}
	if len(svc.lastKeys) != 40 {
		t.Fatalf("TaskGet asked about %d keys, want all 40 distinct candidates in one call", len(svc.lastKeys))
	}
	body := rw.Body.String()
	if !strings.Contains(body, `<a href="/t/BMB-2">BMB-2</a>`) {
		t.Fatal("an existing even-numbered key did not link")
	}
	if strings.Contains(body, `<a href="/t/BMB-1"`) {
		t.Fatal("BMB-1 does not exist in this fixture and must not have linked")
	}
}

// TestBoardChat_InjectionIsEscapedAndCannotEscapeTheLinkAttribute is the
// mandatory security test: a message body that mixes a real task key with an
// HTML/script injection attempt, INCLUDING an attempt to break out of the
// <a href="..."> attribute right after the recognized key, must render with
// the script inert and visible as text, and the link's href must still be
// exactly the key-derived URL — never anything spliced from the message.
func TestBoardChat_InjectionIsEscapedAndCannotEscapeTheLinkAttribute(t *testing.T) {
	svc := &chatKeyLinkService{
		existingKeys: map[string]bool{"BMB-1": true},
		chatMsgs: []domain.ChatMessage{
			{
				ID:     "m1",
				Author: "agent-a",
				// The quote+angle-bracket right after BMB-1 is an attempt to
				// break out of the href attribute the link renders into; it
				// must land as inert escaped text, never inside the tag.
				Body:      `see BMB-1"><script>alert(1)</script> and <img src=x onerror=alert(2)>`,
				CreatedAt: time.Now().UTC(),
			},
		},
	}
	w, sess := newChatKeyLinkTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB = %d, want 200\n%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()

	if strings.Contains(body, "<script>alert(1)") {
		t.Fatal("script tag reached the page live")
	}
	if strings.Contains(body, "<img src=x onerror") {
		t.Fatal("img onerror handler reached the page live")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatal("script tag was not visible as escaped text")
	}
	if !strings.Contains(body, "&lt;img src=x onerror=alert(2)&gt;") {
		t.Fatal("img tag was not visible as escaped text")
	}
	// The link itself must be exactly the key-derived anchor, immediately
	// followed by the escaped quote/angle-bracket as plain text — proving
	// the injected `">` never became part of the tag.
	if !strings.Contains(body, `<a href="/t/BMB-1">BMB-1</a>&#34;&gt;&lt;script&gt;`) {
		t.Fatalf("the injection attempt broke out of (or altered) the link markup:\n%s", body)
	}
}
