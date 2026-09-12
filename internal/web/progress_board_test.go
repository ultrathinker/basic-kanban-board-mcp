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

// progressCountingService answers the board page's reads and counts them.
// It embeds the service.Service interface: any method the board handler
// calls beyond the three overrides panics on a nil embedded method, which is
// exactly the loud failure a hidden per-card query deserves.
type progressCountingService struct {
	service.Service

	board                *service.Board
	taskProgressCalls    int
	projectProgressCalls int
	taskGetCalls         int
	chatListCalls        int
	chatAddCalls         int
	taskProgressKeys     []string
}

func (s *progressCountingService) BoardGet(_ context.Context, _ service.Actor, _ service.BoardGetInput) (*service.Board, error) {
	return s.board, nil
}

func (s *progressCountingService) ProjectProgress(_ context.Context, _ service.Actor, _ service.ProjectProgressInput) (*service.ProjectProgressResult, error) {
	s.projectProgressCalls++
	manual := 40
	auto := 50
	return &service.ProjectProgressResult{
		ProjectKey:      "BMB",
		Manual:          &manual,
		ManualAssessors: 2,
		Auto:            &auto,
		DoneTasks:       30,
		TotalTasks:      60,
	}, nil
}

func (s *progressCountingService) TaskProgress(_ context.Context, _ service.Actor, in service.TaskProgressInput) (*service.TaskProgressResult, error) {
	s.taskProgressCalls++
	s.taskProgressKeys = append(s.taskProgressKeys, in.Keys...)
	res := &service.TaskProgressResult{ProjectKey: in.ProjectKey}
	for _, k := range in.Keys {
		// A deterministic spread so the served markup can be pinned: the
		// first card is 45% by three assessors, everything else is 0% by one.
		p, assessors := 0, 1
		if k == "BMB-1" {
			p, assessors = 45, 3
		}
		percent := p
		res.Items = append(res.Items, service.TaskProgressItem{
			Key: k, TaskID: "id-" + k, Percent: &percent, Assessors: assessors,
		})
	}
	return res, nil
}

func (s *progressCountingService) TaskGet(_ context.Context, _ service.Actor, _ service.TaskGetInput) (*service.TaskGetResult, error) {
	s.taskGetCalls++
	return &service.TaskGetResult{}, nil
}

func (s *progressCountingService) ChatList(_ context.Context, _ service.Actor, in service.ChatListInput) (*service.ChatListResult, error) {
	s.chatListCalls++
	// A full page of messages: the panel must be fed by THIS one call, never
	// by a request per message.
	msgs := make([]domain.ChatMessage, 0, 60)
	for i := 0; i < 60; i++ {
		msgs = append(msgs, domain.ChatMessage{
			ID: strconv.Itoa(i), Author: "agent-alpha",
			Body: "thought " + strconv.Itoa(i), CreatedAt: time.Now().UTC(),
		})
	}
	return &service.ChatListResult{Messages: msgs}, nil
}

func (s *progressCountingService) ChatAdd(_ context.Context, _ service.Actor, _ service.ChatAddInput) (*domain.ChatMessage, error) {
	s.chatAddCalls++
	return &domain.ChatMessage{}, nil
}

// TestBoardPage_ProgressReadsDoNotScale proves the board page never pays a
// query per card for progress: opening a project with 60 tasks issues ONE
// ProjectProgress read and ceil(60/MaxGetKeys) = 2 batched TaskProgress
// reads — the same fixed handful a 6-task board would pay — and never walks
// the cards one TaskGet at a time.
func TestBoardPage_ProgressReadsDoNotScale(t *testing.T) {
	const taskCount = 60

	tasks := make([]domain.TaskView, 0, taskCount)
	for i := 1; i <= taskCount; i++ {
		tasks = append(tasks, domain.TaskView{
			Task:       domain.Task{Key: "BMB-" + strconv.Itoa(i), Title: "card"},
			ColumnName: "Backlog",
		})
	}
	board := &service.Board{Projects: []service.BoardProject{{
		Key:  "BMB",
		Name: "Test",
		Columns: []service.BoardColumn{
			{Name: "Backlog", Kind: domain.KindBacklog, Count: taskCount, Tasks: tasks},
			{Name: "Doing", Kind: domain.KindActive, Count: 0},
			{Name: "Done", Kind: domain.KindDone, Count: 0},
		},
	}}}
	svc := &progressCountingService{board: board}

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

	rw := do(w, "GET", "/p/BMB", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB = %d, want 200\n%s", rw.Code, rw.Body.String())
	}

	// The batched reads, and only them.
	wantChunks := (taskCount + domain.MaxGetKeys - 1) / domain.MaxGetKeys // ceil: 2 for 60
	if svc.taskProgressCalls != wantChunks {
		t.Fatalf("TaskProgress called %d times, want %d (one chunk per %d keys)",
			svc.taskProgressCalls, wantChunks, domain.MaxGetKeys)
	}
	if len(svc.taskProgressKeys) != taskCount {
		t.Fatalf("TaskProgress covered %d keys, want %d", len(svc.taskProgressKeys), taskCount)
	}
	if svc.projectProgressCalls != 1 {
		t.Fatalf("ProjectProgress called %d times, want 1", svc.projectProgressCalls)
	}
	if svc.taskGetCalls != 0 {
		t.Fatalf("TaskGet called %d times, want 0: the board must not query per card", svc.taskGetCalls)
	}
	// The thoughts panel: one ChatList read for the whole feed, no writes.
	if svc.chatListCalls != 1 {
		t.Fatalf("ChatList called %d times, want 1: the feed must arrive in a single read", svc.chatListCalls)
	}
	if svc.chatAddCalls != 0 {
		t.Fatalf("ChatAdd called %d times, want 0: a board read never writes", svc.chatAddCalls)
	}

	body := rw.Body.String()
	// Cards actually carry their bars, painted from the batch data.
	if !strings.Contains(body, `data-filled="4"`) {
		t.Fatal("the 45% card did not paint 4 squares")
	}
	if !strings.Contains(body, "45% · 3 assessments") {
		t.Fatal("the 45% card does not carry its label with the track count")
	}
	// The header pair, labelled and distinct.
	if !strings.Contains(body, "40% · 2 assessments") || !strings.Contains(body, ">assessed<") {
		t.Fatal("header does not show the manual metric with its label")
	}
	if !strings.Contains(body, "50% · 30/60 tasks") || !strings.Contains(body, ">tasks done<") {
		t.Fatal("header does not show the automatic metric with its label")
	}
	// CSP: no inline width styles anywhere on the page.
	if strings.Contains(body, `style="width`) {
		t.Fatal("page carries an inline width style; the fill must be painted by CSS from the class")
	}
	// The thoughts panel is served with its messages: authors and bodies of
	// the one ChatList page reached the page.
	if !strings.Contains(body, `id="chat-panel"`) || !strings.Contains(body, "agent-alpha") {
		t.Fatal("board page does not carry the thoughts panel with its messages")
	}
	if !strings.Contains(body, "thought 59") {
		t.Fatal("the newest chat message is missing from the served panel")
	}
}
