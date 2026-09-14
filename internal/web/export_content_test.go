package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// ---------------------------------------------------------------------------
// KANB-29 criterion 4: the Export button on the project page must hand back
// THE SAME FULL DOCUMENT the CLI export produces.
//
// Until this file existed, the only test touching /p/{key}/export was the
// route-registration check in web_test.go, which asserts nothing but "not a
// 5xx". A review canary proved the gap: making buildWebExportDoc return zero
// progress marks and zero chat messages left the entire suite green. That is
// precisely the failure the card exists to prevent — the panel then shows
// "No thoughts yet", the same string it shows for a board that never had any,
// so the loss is indistinguishable from emptiness.
//
// These tests therefore assert CONTENT, never the status code alone:
// every seeded value has to come back, and the chat has to come back in full
// past the 100-row page size the service reads it in.
// ---------------------------------------------------------------------------

const exportContentAdminSecret = "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"

// chatBeyondOnePage is deliberately larger than the 100-message page
// ChatList serves, so the test fails if buildWebExportDoc ever stops paging
// and hands back only the newest page. A board loses its oldest thinking
// first, which is the half nobody notices is missing.
const chatBeyondOnePage = 143

type exportEnv struct {
	w   *Web
	st  store.Store
	svc service.Service
	mgr *auth.Manager
}

func newExportEnv(t *testing.T) *exportEnv {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	st, err := store.Open(ctx, store.Config{Path: filepath.Join(dir, "kanban.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := service.New(st, nil)
	tpl, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	w := New(Deps{
		Service:   svc,
		Auth:      newTestManager(t),
		Bus:       events.New(newEmptyHistory(), 4),
		Templates: tpl,
		BaseURL:   "http://127.0.0.1:0",
	})
	return &exportEnv{w: w, st: st, svc: svc, mgr: w.d.Auth}
}

// seedExportBoard builds a board whose every append-only value is known, so
// the assertions can compare values rather than counts. Rows go in through
// the store because that is the only layer honouring a caller-supplied
// timestamp — the service stamps now(), and a document full of now() is the
// vertical wall the card warns about.
func (e *exportEnv) seedExportBoard(t *testing.T) (wantETA time.Time, oldestChatBody string) {
	t.Helper()
	ctx := context.Background()
	admin := service.Actor{Name: "seed", Scopes: domain.Scopes{domain.ScopeAdmin}}

	up, err := e.svc.ProjectUpsert(ctx, admin, service.ProjectUpsertInput{
		Mode: service.UpsertCreate, Key: "BMB", Name: "Export content",
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	created, err := e.svc.TaskCreate(ctx, admin, service.TaskCreateInput{
		Tasks: []service.NewTask{{
			ProjectKey: "BMB", Title: "seed task", Type: domain.TypeTask,
		}},
	})
	if err != nil || len(created.Tasks) == 0 {
		t.Fatalf("seed task: %v", err)
	}

	wantETA = time.Date(2026, 8, 15, 18, 0, 0, 0, time.UTC)
	taskID := created.Tasks[0].ID

	if err := e.st.Write(ctx, func(tx store.Tx) error {
		// One project-level mark carrying a forecast, one task-level mark
		// without: the export has to preserve both the value and the absence.
		if err := e.st.Progress().Add(tx, &domain.ProgressMark{
			ID: "pm-proj", ProjectID: up.Project.ID, Assessor: "alex", Percent: 20,
			ETA: &wantETA, CreatedAt: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		}); err != nil {
			return err
		}
		if err := e.st.Progress().Add(tx, &domain.ProgressMark{
			ID: "pm-task", ProjectID: up.Project.ID, TaskID: &taskID, Assessor: "bea", Percent: 75,
			CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		}); err != nil {
			return err
		}
		for i := 0; i < chatBeyondOnePage; i++ {
			if err := e.st.Chat().Add(tx, &domain.ChatMessage{
				ID:        fmt.Sprintf("chat-%03d", i),
				ProjectID: up.Project.ID,
				Author:    "alex",
				Body:      fmt.Sprintf("thought number %d", i),
				// Ascending in time, so index 0 is the OLDEST message —
				// the first thing a truncating export drops.
				CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed append-only rows: %v", err)
	}
	return wantETA, "thought number 0"
}

// postExport drives one of the two export routes as a signed-in admin,
// carrying the session and CSRF pair the handlers require.
func (e *exportEnv) postExport(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	sess := sessionFor(t, e.mgr, exportContentAdminSecret)
	tok := e.mgr.CSRFTokenForSession(sess.ID)

	form := url.Values{"csrf_token": {tok}}
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie(sess))
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: tok})

	rec := httptest.NewRecorder()
	e.w.Handler().ServeHTTP(rec, req)
	return rec
}

// exportDoc is the shape both routes serve, decoded loosely so the test
// depends on the JSON contract (what an importer reads) rather than on the
// unexported Go struct.
type exportDoc struct {
	Projects      []json.RawMessage `json:"projects"`
	ProgressMarks []struct {
		ID        string     `json:"id"`
		Project   string     `json:"project"`
		Task      string     `json:"task"`
		Assessor  string     `json:"assessor"`
		Percent   int        `json:"percent"`
		ETA       *time.Time `json:"eta"`
		CreatedAt time.Time  `json:"created_at"`
	} `json:"progress_marks"`
	ChatMessages []struct {
		ID        string    `json:"id"`
		Project   string    `json:"project"`
		Author    string    `json:"author"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"chat_messages"`
}

func decodeExport(t *testing.T, rec *httptest.ResponseRecorder, route string) exportDoc {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200 (body: %.300s)", route, rec.Code, rec.Body.String())
	}
	var doc exportDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("%s: decode body: %v", route, err)
	}
	return doc
}

// assertFullContent is the shared assertion both routes must satisfy. It is
// deliberately about values: a version of this test that only counted rows
// would pass against an export that returned the right number of blanks.
func assertFullContent(t *testing.T, doc exportDoc, route string, wantETA time.Time, oldestChatBody string) {
	t.Helper()

	if len(doc.Projects) == 0 {
		t.Errorf("%s: no projects in the document", route)
	}

	if len(doc.ProgressMarks) != 2 {
		t.Fatalf("%s: progress_marks = %d, want 2 — the export dropped assessment history", route, len(doc.ProgressMarks))
	}
	var sawForecast, sawAbsent bool
	for _, m := range doc.ProgressMarks {
		switch m.ID {
		case "pm-proj":
			sawForecast = true
			if m.Assessor != "alex" || m.Percent != 20 {
				t.Errorf("%s: pm-proj = %q/%d, want alex/20", route, m.Assessor, m.Percent)
			}
			if m.ETA == nil {
				t.Errorf("%s: pm-proj lost its forecast; owner decision 6 makes the forecast first-class data", route)
			} else if !m.ETA.Equal(wantETA) {
				t.Errorf("%s: pm-proj eta = %s, want %s", route, m.ETA, wantETA)
			}
			if !m.CreatedAt.Equal(time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)) {
				t.Errorf("%s: pm-proj created_at = %s, want the seeded time", route, m.CreatedAt)
			}
		case "pm-task":
			sawAbsent = true
			if m.Task == "" {
				t.Errorf("%s: pm-task lost its task key, so an importer cannot reattach it", route)
			}
			// Every field, not just the task key and the (absent) forecast:
			// a review canary showed that trimming pm-task down to
			// {ID, Project, Task} left this whole test suite green, because
			// nothing here checked the task-scoped mark's author, percent
			// or timestamp — only the project-scoped one was checked in
			// full. KANB-29 criterion 2 names author, percent AND time.
			if m.Assessor != "bea" {
				t.Errorf("%s: pm-task assessor = %q, want bea", route, m.Assessor)
			}
			if m.Percent != 75 {
				t.Errorf("%s: pm-task percent = %d, want 75", route, m.Percent)
			}
			if !m.CreatedAt.Equal(time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)) {
				t.Errorf("%s: pm-task created_at = %s, want the seeded time", route, m.CreatedAt)
			}
			if m.ETA != nil {
				t.Errorf("%s: pm-task eta = %s, want none — an absent forecast must stay absent", route, m.ETA)
			}
		}
	}
	if !sawForecast || !sawAbsent {
		t.Errorf("%s: expected both pm-proj and pm-task in the document", route)
	}

	// The heart of criterion 4: "the same FULL document". The service reads
	// chat a page at a time; an export that stops after one page silently
	// loses the oldest thinking on any board that has been running a while.
	if len(doc.ChatMessages) != chatBeyondOnePage {
		t.Errorf("%s: chat_messages = %d, want %d — the export truncated the feed instead of paging to the end",
			route, len(doc.ChatMessages), chatBeyondOnePage)
	}
	var sawOldest bool
	for _, m := range doc.ChatMessages {
		if m.Body == oldestChatBody {
			sawOldest = true
			if m.Author != "alex" {
				t.Errorf("%s: oldest message author = %q, want alex", route, m.Author)
			}
			if !m.CreatedAt.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("%s: oldest message created_at = %s, want the seeded time", route, m.CreatedAt)
			}
		}
	}
	if !sawOldest {
		t.Errorf("%s: the OLDEST chat message is missing — this is what truncation looks like", route)
	}
}

func TestProjectExportRoute_CarriesTheWholeDocument(t *testing.T) {
	env := newExportEnv(t)
	wantETA, oldest := env.seedExportBoard(t)
	doc := decodeExport(t, env.postExport(t, "/p/BMB/export"), "POST /p/BMB/export")
	assertFullContent(t, doc, "POST /p/BMB/export", wantETA, oldest)
}

func TestAdminExportRoute_CarriesTheWholeDocument(t *testing.T) {
	env := newExportEnv(t)
	wantETA, oldest := env.seedExportBoard(t)
	doc := decodeExport(t, env.postExport(t, "/admin/export"), "POST /admin/export")
	assertFullContent(t, doc, "POST /admin/export", wantETA, oldest)
}
