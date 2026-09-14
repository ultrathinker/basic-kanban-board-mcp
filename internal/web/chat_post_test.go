package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// chatPostStubService answers ChatAdd with a canned message and records the
// input it was called with, so the tests can pin exactly what the composer's
// POST forwards to the service — body verbatim, and all four protocol
// fields, the idempotency key included (KANB-48 makes that key the UI's
// obligation; the handler is where its travel can actually be observed).
type chatPostStubService struct {
	service.Service
	out    *domain.ChatMessage
	err    error
	lastIn service.ChatAddInput
	calls  int
}

func (s *chatPostStubService) ChatAdd(_ context.Context, _ service.Actor, in service.ChatAddInput) (*domain.ChatMessage, error) {
	s.calls++
	s.lastIn = in
	if s.err != nil {
		return nil, s.err
	}
	if s.out != nil {
		return s.out, nil
	}
	return &domain.ChatMessage{ID: "chat-1", Body: in.Body}, nil
}

func newChatPostTestWeb(t *testing.T, svc service.Service) (*Web, *domain.Session) {
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

// postChat drives one composer POST the way the browser does: session
// cookie plus the CSRF pair (the cookie and the form field), body
// urlencoded.
func postChat(t *testing.T, w *Web, sess *domain.Session, tok string, fields url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/p/BMB/chat", strings.NewReader(fields.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie(sess))
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: tok})
	if tok != "" {
		fields.Set("csrf_token", tok)
		req = httptest.NewRequest("POST", "/p/BMB/chat", strings.NewReader(fields.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(sessionCookie(sess))
		req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: tok})
	}
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	return rec
}

// TestChatPost_ForwardsEveryComposerField is the heart of the UI's contract:
// the body travels verbatim (the service owns trimming rules, the handler
// must not pre-trim a message the user wrote), and kind / recipient /
// reply_to / idempotency_key all reach ChatAdd. Losing the idempotency key
// here would silently strip the double-send protection the composer built
// its whole retry story on.
func TestChatPost_ForwardsEveryComposerField(t *testing.T) {
	svc := &chatPostStubService{}
	w, sess := newChatPostTestWeb(t, svc)
	csrf := w.d.Auth.CSRFTokenForSession(sess.ID)

	rec := postChat(t, w, sess, csrf, url.Values{
		"body":            {"  two  lines\nverbatim  "},
		"kind":            {"command"},
		"recipient":       {"tok-exec"},
		"reply_to":        {"chat-42"},
		"idempotency_key": {"a1b2c3d4e5f60718"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if svc.calls != 1 {
		t.Fatalf("ChatAdd called %d times, want 1", svc.calls)
	}
	in := svc.lastIn
	if in.ProjectKey != "BMB" {
		t.Errorf("project = %q, want BMB", in.ProjectKey)
	}
	if in.Body != "  two  lines\nverbatim  " {
		t.Errorf("body = %q, want verbatim", in.Body)
	}
	if in.Kind != "command" || in.Recipient != "tok-exec" || in.ReplyTo != "chat-42" {
		t.Errorf("protocol fields = kind:%q recipient:%q reply_to:%q", in.Kind, in.Recipient, in.ReplyTo)
	}
	if in.IdempotencyKey != "a1b2c3d4e5f60718" {
		t.Errorf("idempotency_key = %q, want the composer's key", in.IdempotencyKey)
	}

	var out struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not the JSON envelope: %s", rec.Body.String())
	}
	if !out.OK || out.ID != "chat-1" {
		t.Errorf("response = ok:%v id:%q", out.OK, out.ID)
	}
}

// TestChatPost_UnauthenticatedNeverReachesTheService: like every JS-driven
// fragment endpoint, a missing session is a non-2xx status for app.js to
// catch — never a redirect, never a silent no-op.
func TestChatPost_UnauthenticatedNeverReachesTheService(t *testing.T) {
	svc := &chatPostStubService{}
	w, _ := newChatPostTestWeb(t, svc)
	req := httptest.NewRequest("POST", "/p/BMB/chat", strings.NewReader("body=hi"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || rec.Code == http.StatusFound || rec.Code == http.StatusSeeOther {
		t.Fatalf("status = %d, want a non-2xx, non-redirect status", rec.Code)
	}
	if svc.calls != 0 {
		t.Fatalf("ChatAdd called %d times, want 0", svc.calls)
	}
}

// TestChatPost_ServiceErrorSurfacesAsAPIError: a refused send (bad kind,
// unknown recipient, a duplicate idempotency key with different content…)
// reaches the composer as the standard error envelope, whose message and
// remediation the UI prints next to the form — the draft survives.
func TestChatPost_ServiceErrorSurfacesAsAPIError(t *testing.T) {
	svc := &chatPostStubService{err: domain.Invalid("kind", "message kind \"nope\" is invalid", "Use one of: update, scope_change, question, command.")}
	w, sess := newChatPostTestWeb(t, svc)
	csrf := w.d.Auth.CSRFTokenForSession(sess.ID)
	rec := postChat(t, w, sess, csrf, url.Values{"body": {"hi"}, "kind": {"nope"}})
	if rec.Code == http.StatusOK {
		t.Fatalf("a refused send answered 200: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "idempotency_key") && !strings.Contains(rec.Body.String(), "kind") {
		t.Errorf("error envelope lost its field name: %s", rec.Body.String())
	}
}

// TestChatPost_MissingCSRFIsRefused: the composer posts through the browser
// session, so the double-submit check applies — a POST with the session
// cookie but no matching CSRF pair is refused before the service sees it.
func TestChatPost_MissingCSRFIsRefused(t *testing.T) {
	svc := &chatPostStubService{}
	w, sess := newChatPostTestWeb(t, svc)
	rec := postChat(t, w, sess, "", url.Values{"body": {"hi"}})
	if rec.Code == http.StatusOK {
		t.Fatalf("a CSRF-less post was accepted: %s", rec.Body.String())
	}
	if svc.calls != 0 {
		t.Fatalf("ChatAdd called %d times, want 0", svc.calls)
	}
}

// ---------------------------------------------------------------------------
// The board page itself: the composer and the acceptance mark reach the
// real route. This is the wiring the unit tests cannot see — handleBoard
// resolving participants and done-states and handing them to the panel —
// and it is exactly the wiring a refactor could silently drop.
// ---------------------------------------------------------------------------

// chatBoardStubService answers the reads one board page render makes, with
// a project that has a coordinator, a participant, and one accepted
// command on the feed whose linked tasks straddle the done boundary.
type chatBoardStubService struct {
	service.Service
	taskGetInputs []service.TaskGetInput
}

func (s *chatBoardStubService) BoardGet(_ context.Context, _ service.Actor, in service.BoardGetInput) (*service.Board, error) {
	return &service.Board{Projects: []service.BoardProject{{
		Key:  "BMB",
		Name: "BMB",
		Coordinator: &service.Participant{
			TokenID: "tok-lead", Name: "lead",
		},
		Participants: []service.Participant{
			{TokenID: "tok-lead", Name: "lead"},
			{TokenID: "tok-exec", Name: "executor"},
		},
		Columns: []service.BoardColumn{
			{Name: "Backlog", Kind: domain.KindBacklog, Count: 0},
		},
	}}}, nil
}

func (s *chatBoardStubService) ProjectProgress(_ context.Context, _ service.Actor, in service.ProjectProgressInput) (*service.ProjectProgressResult, error) {
	return &service.ProjectProgressResult{ProjectKey: in.ProjectKey}, nil
}

func (s *chatBoardStubService) TaskProgress(_ context.Context, _ service.Actor, in service.TaskProgressInput) (*service.TaskProgressResult, error) {
	return &service.TaskProgressResult{ProjectKey: in.ProjectKey}, nil
}

func (s *chatBoardStubService) ChatFeed(_ context.Context, _ service.Actor, _ service.ChatFeedInput) (*service.ChatFeedResult, error) {
	return nil, ErrServiceUnavailable
}

func (s *chatBoardStubService) ChatList(_ context.Context, _ service.Actor, _ service.ChatListInput) (*service.ChatListResult, error) {
	return &service.ChatListResult{
		Messages: []domain.ChatMessage{
			{
				ID: "m2", Author: "lead", Body: "and sweep the floor",
				CreatedAt: wNow(), Kind: domain.MessageCommand,
				Recipient: "tok-exec", AuthorTokenID: "tok-lead",
			},
			{
				ID: "m1", Author: "lead", Body: "ship the report",
				CreatedAt: wNow(), Kind: domain.MessageCommand,
				Recipient: "tok-exec", AuthorTokenID: "tok-lead",
			},
		},
		Meta: map[string]service.ChatListEntryMeta{
			// m2 carries no acceptance yet — the feed must show it awaiting one.
			"m1": {
				RecipientName: "executor",
				ExecutorName:  "executor",
				Acceptance: &domain.CommandAcceptance{
					MessageID: "m1",
					TaskKeys:  []string{"BMB-1", "BMB-2"},
				},
			},
		},
	}, nil
}

func (s *chatBoardStubService) TaskGet(_ context.Context, _ service.Actor, in service.TaskGetInput) (*service.TaskGetResult, error) {
	s.taskGetInputs = append(s.taskGetInputs, in)
	tasks := make([]domain.TaskView, 0, len(in.Keys))
	for _, k := range in.Keys {
		kind := domain.KindActive
		if strings.ToUpper(k) == "BMB-2" {
			kind = domain.KindDone
		}
		tasks = append(tasks, domain.TaskView{
			Task: domain.Task{Key: strings.ToUpper(k)}, ProjectKey: "BMB", ColumnKind: kind,
		})
	}
	return &service.TaskGetResult{Tasks: tasks}, nil
}

// wNow anchors the fixture message's timestamp to the server's clock; the
// panel renders times relative to Deps.Now, which defaults to time.Now.
func wNow() time.Time { return time.Now() }

// TestChatBoard_ComposerAndAcceptanceMarkReachThePage (KANB-48): a signed-in
// session with the write scope sees the composer with its visible type
// select and the full recipient list, AND the accepted command wearing its
// mark with links to the tasks its acceptance created — one task already
// done, one not, both rendered from the linked tasks' own state.
func TestChatBoard_ComposerAndAcceptanceMarkReachThePage(t *testing.T) {
	w := newTestWeb(t)
	svc := &chatBoardStubService{}
	w.d.Service = svc
	sess := sessionFor(t, w.d.Auth, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")

	rec := do(w, "GET", "/p/BMB", nil, sessionCookie(sess))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		// The composer, with the always-visible type select and the
		// resolved participants.
		`data-chat-compose`,
		`<option value="command">command</option>`,
		`<option value="tok-exec">executor</option>`,
		`<option value="">coordinator (lead)</option>`,
		`data-can-post`,
		// The accepted command: executor named, tasks linked, the done one
		// marked — the aggregate straight off the linked tasks.
		`awaiting acceptance`,
		`<a href="/t/BMB-1">BMB-1</a>`,
		`<a href="/t/BMB-2">BMB-2</a> (done)`,
		`&rarr; executor`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board page is missing %q", want)
		}
	}
	// The done-state lookup really went to the service as one batched read
	// carrying BOTH acceptance keys — never a query per task.
	merged := map[string]bool{}
	for _, in := range svc.taskGetInputs {
		for _, k := range in.Keys {
			merged[strings.ToUpper(k)] = true
		}
	}
	if !merged["BMB-1"] || !merged["BMB-2"] {
		t.Errorf("acceptance keys were not resolved for done-state: %v", svc.taskGetInputs)
	}
}
