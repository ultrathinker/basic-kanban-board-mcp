package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// ---------------------------------------------------------------------------
// handleProgressTrackDelete — the owner's real, permanent delete of one
// assessor's whole progress track. These tests run the FULL stack (a real
// SQLite store, the real service, the real web handler tree) rather than a
// fake service: the acceptance bar for this feature is proving the track is
// actually gone by reading it back from the store, and a fake service could
// not tell us that.
// ---------------------------------------------------------------------------

// progressDeleteEnv is the wiring every test in this file starts from: a
// real store on a temp-dir SQLite file, the real service on top of it, and
// a Web built exactly like production (auth.Manager, event bus, templates).
type progressDeleteEnv struct {
	w    *Web
	st   store.Store
	svc  service.Service
	mgr  *auth.Manager
	proj domain.Project
	task domain.TaskView
}

const progressDeleteAdminSecret = "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"

func newProgressDeleteEnv(t *testing.T) *progressDeleteEnv {
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
	// newTestManager bootstraps progressDeleteAdminSecret as the one admin
	// token, exactly the secret this file's tests sign in with.
	mgr := newTestManager(t)
	w := New(Deps{
		Service:   svc,
		Auth:      mgr,
		Bus:       events.New(newEmptyHistory(), 4),
		Templates: tpl,
		BaseURL:   "http://127.0.0.1:0",
	})

	admin := service.Actor{Name: "seed", Scopes: domain.Scopes{domain.ScopeAdmin}}
	up, err := svc.ProjectUpsert(ctx, admin, service.ProjectUpsertInput{
		Mode: service.UpsertCreate, Key: "BMB", Name: "Test project",
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	created, err := svc.TaskCreate(ctx, admin, service.TaskCreateInput{
		Tasks: []service.NewTask{{
			ProjectKey: "BMB", Title: "seed task", Type: domain.TypeTask, Priority: domain.PriorityMedium,
		}},
	})
	if err != nil || len(created.Tasks) == 0 {
		t.Fatalf("seed task: %v", err)
	}

	return &progressDeleteEnv{w: w, st: st, svc: svc, mgr: mgr, proj: up.Project, task: created.Tasks[0]}
}

// addMark writes one progress mark directly through the store — the same
// path an assessing AI would take (service.ProgressSet), skipped here only
// because these tests want full control over which assessor logged how
// many marks, not the service's own validation of that path (covered
// elsewhere).
func (e *progressDeleteEnv) addMark(t *testing.T, taskID *string, assessor string, percent int) {
	t.Helper()
	if err := e.st.Write(context.Background(), func(tx store.Tx) error {
		return e.st.Progress().Add(tx, &domain.ProgressMark{
			ID:        "pm-" + assessor + "-" + taskIDOrProject(taskID) + "-" + randSuffix(),
			ProjectID: e.proj.ID, TaskID: taskID, Assessor: assessor, Percent: percent,
		})
	}); err != nil {
		t.Fatalf("seed mark for %s: %v", assessor, err)
	}
}

func taskIDOrProject(taskID *string) string {
	if taskID == nil {
		return "proj"
	}
	return *taskID
}

var randCounter int

// randSuffix keeps every seeded mark id unique within a test without pulling
// in a real random source — these tests never assert on ids, only on counts
// and survivors, so a monotonic counter is all uniqueness requires.
func randSuffix() string {
	randCounter++
	return "n" + itoa(randCounter)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// countMarks reads back the raw row count for one (task, assessor) scope
// directly through the store's History — this is the "prove it by reading
// from the store" the brief asks for, not an inspection of the HTTP
// response.
func (e *progressDeleteEnv) countMarks(t *testing.T, taskID *string, assessor string) int {
	t.Helper()
	var n int
	if err := e.st.Read(context.Background(), func(tx store.Tx) error {
		hist, err := e.st.Progress().History(tx, e.proj.ID, taskID)
		if err != nil {
			return err
		}
		for _, m := range hist {
			if m.Assessor == assessor {
				n++
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read back history: %v", err)
	}
	return n
}

// csrfPair returns the cookie and field value a real browser session would
// carry. The token is the one DERIVED from this session, not a freshly minted
// random one: an authenticated POST is now verified against
// CSRFTokenForSession, so a random pair that merely agrees with itself is
// exactly what an attacker who planted a cookie would have, and is refused.
func (e *progressDeleteEnv) csrfPair(t *testing.T, sess *domain.Session) (*http.Cookie, string) {
	t.Helper()
	tok := e.mgr.CSRFTokenForSession(sess.ID)
	return &http.Cookie{Name: auth.CSRFCookieName, Value: tok}, tok
}

func postDelete(w *Web, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/fragments/progress/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	return rec
}

// TestProgressTrackDelete_RemovesOnlyTheNamedAssessor is the acceptance
// test: after a real POST through the handler, alpha's marks are gone and
// beta's are untouched — proved by reading the store back directly, not by
// inspecting the response body.
func TestProgressTrackDelete_RemovesOnlyTheNamedAssessor(t *testing.T) {
	env := newProgressDeleteEnv(t)
	env.addMark(t, &env.task.ID, "alpha", 10)
	env.addMark(t, &env.task.ID, "alpha", 80)
	env.addMark(t, &env.task.ID, "beta", 60)

	if got := env.countMarks(t, &env.task.ID, "alpha"); got != 2 {
		t.Fatalf("seed: alpha has %d marks, want 2", got)
	}

	sess := sessionFor(t, env.mgr, progressDeleteAdminSecret)
	csrfCookie, csrfTok := env.csrfPair(t, sess)
	body := "csrf_token=" + csrfTok + "&project=BMB&task=" + env.task.Key + "&assessor=alpha"
	rec := postDelete(env.w, body, sessionCookie(sess), csrfCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}

	// The load-bearing assertion: read the store back directly.
	if got := env.countMarks(t, &env.task.ID, "alpha"); got != 0 {
		t.Fatalf("alpha's marks after delete = %d rows, want 0 (a real delete, not hidden)", got)
	}
	if got := env.countMarks(t, &env.task.ID, "beta"); got != 1 {
		t.Fatalf("beta's marks after deleting alpha = %d rows, want 1 (untouched)", got)
	}

	// The response is the freshly recomputed metric: beta alone now, no
	// trace of alpha's row.
	if strings.Contains(rec.Body.String(), `data-assessor="alpha"`) {
		t.Fatalf("response fragment still names alpha after deleting alpha's track: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-assessor="beta"`) {
		t.Fatalf("response fragment lost beta's row, which was never deleted: %s", rec.Body.String())
	}
}

// TestProgressTrackDelete_ProjectScope proves the project-level track (empty
// "task" field) deletes through the same door and leaves the task-level
// track for the same assessor name untouched — the two scopes are disjoint.
func TestProgressTrackDelete_ProjectScope(t *testing.T) {
	env := newProgressDeleteEnv(t)
	env.addMark(t, nil, "alpha", 30)
	env.addMark(t, nil, "alpha", 70)
	env.addMark(t, &env.task.ID, "alpha", 90) // same assessor, task scope

	sess := sessionFor(t, env.mgr, progressDeleteAdminSecret)
	csrfCookie, csrfTok := env.csrfPair(t, sess)
	body := "csrf_token=" + csrfTok + "&project=BMB&assessor=alpha"
	rec := postDelete(env.w, body, sessionCookie(sess), csrfCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}

	if got := env.countMarks(t, nil, "alpha"); got != 0 {
		t.Fatalf("project-level alpha marks after delete = %d, want 0", got)
	}
	if got := env.countMarks(t, &env.task.ID, "alpha"); got != 1 {
		t.Fatalf("task-level alpha marks after a project-scope delete = %d, want 1 (untouched)", got)
	}
}

// TestProgressTrackDelete_GETCannotDelete proves a GET to the exact same
// path never reaches the handler at all (Go's ServeMux only registered
// POST), and — the assertion that actually matters — the marks survive.
func TestProgressTrackDelete_GETCannotDelete(t *testing.T) {
	env := newProgressDeleteEnv(t)
	env.addMark(t, &env.task.ID, "alpha", 50)

	sess := sessionFor(t, env.mgr, progressDeleteAdminSecret)
	req := httptest.NewRequest("GET", "/fragments/progress/delete?project=BMB&task="+env.task.Key+"&assessor=alpha", nil)
	req.AddCookie(sessionCookie(sess))
	rec := httptest.NewRecorder()
	env.w.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("GET returned 200; want it rejected (405/404), got body=%q", rec.Body.String())
	}
	if got := env.countMarks(t, &env.task.ID, "alpha"); got != 1 {
		t.Fatalf("alpha's mark did not survive a GET request: %d rows, want 1", got)
	}
}

// TestProgressTrackDelete_MissingCSRFRejected proves the session path still
// requires the CSRF double-submit — copied from handleFragmentMove's own
// protection, per the brief — and that a rejected request deletes nothing.
func TestProgressTrackDelete_MissingCSRFRejected(t *testing.T) {
	env := newProgressDeleteEnv(t)
	env.addMark(t, &env.task.ID, "alpha", 50)

	sess := sessionFor(t, env.mgr, progressDeleteAdminSecret)
	body := "project=BMB&task=" + env.task.Key + "&assessor=alpha"
	rec := postDelete(env.w, body, sessionCookie(sess)) // no CSRF cookie, no csrf_token field
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CSRF rejected); body=%q", rec.Code, rec.Body.String())
	}
	if got := env.countMarks(t, &env.task.ID, "alpha"); got != 1 {
		t.Fatalf("alpha's mark did not survive a CSRF-less request: %d rows, want 1", got)
	}
}

// TestProgressTrackDelete_BearerTokenCannotDelete is the acceptance-critical
// negative case: an AI never authenticates with a browser session, only a
// bearer token or an API key. This proves that even a WRITE-scope bearer
// token — exactly what an assessing AI carries — cannot reach this handler,
// because requireOwnerSession never even looks at Authorization. There is
// no MCP method for this deletion, and this is the other half of that
// guarantee: the web door does not quietly open for the same credential
// either.
func TestProgressTrackDelete_BearerTokenCannotDelete(t *testing.T) {
	env := newProgressDeleteEnv(t)
	env.addMark(t, &env.task.ID, "alpha", 50)

	ctx := context.Background()
	secret, err := env.mgr.MintAndStore(ctx, &domain.Token{
		Name: "ai-agent", Scopes: domain.Scopes{domain.ScopeWrite},
	})
	if err != nil {
		t.Fatalf("mint write-scope token: %v", err)
	}

	req := httptest.NewRequest("POST", "/fragments/progress/delete",
		strings.NewReader("project=BMB&task="+env.task.Key+"&assessor=alpha"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	env.w.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("bearer (write-scope) request status = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	if got := env.countMarks(t, &env.task.ID, "alpha"); got != 1 {
		t.Fatalf("alpha's mark did not survive a bearer-authenticated attempt: %d rows, want 1", got)
	}
}

// TestProgressTrackDelete_AdminScopeBearerCannotDelete closes the gap an
// independent review found in TestProgressTrackDelete_BearerTokenCannotDelete
// above: that test only proves a WRITE-scope bearer is refused, which is
// true even if the handler were mistakenly changed to gate on scope
// (requireAPIAuth(domain.ScopeAdmin)) rather than on credential TYPE
// (requireOwnerSession, session cookie only) — a write-scope bearer fails
// either way, so that mutation would have left every test in this file
// green while a bearer minted with ADMIN scope could delete through the web
// door. The actual invariant this feature promises is stronger than "no
// write-scope bearer": no bearer or API-key token, of ANY scope including
// admin, may ever reach this handler — an AI never authenticates with a
// browser session, so a session-only gate is the only thing that can honour
// that promise, and this is the case that would catch a regression back to
// scope-based gating.
func TestProgressTrackDelete_AdminScopeBearerCannotDelete(t *testing.T) {
	env := newProgressDeleteEnv(t)
	env.addMark(t, &env.task.ID, "alpha", 50)

	ctx := context.Background()
	secret, err := env.mgr.MintAndStore(ctx, &domain.Token{
		Name: "ai-agent-admin", Scopes: domain.Scopes{domain.ScopeAdmin},
	})
	if err != nil {
		t.Fatalf("mint admin-scope token: %v", err)
	}

	req := httptest.NewRequest("POST", "/fragments/progress/delete",
		strings.NewReader("project=BMB&task="+env.task.Key+"&assessor=alpha"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	env.w.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("bearer (admin-scope) request status = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	if got := env.countMarks(t, &env.task.ID, "alpha"); got != 1 {
		t.Fatalf("alpha's mark did not survive an admin-scope bearer attempt: %d rows, want 1", got)
	}
}

// TestProgressTrackDelete_WriteScopeSessionCannotDelete proves the session
// path itself is admin-gated: a signed-in session with only write scope
// (not the owner) is refused, same as an AI's bearer token, just through
// the other credential shape.
func TestProgressTrackDelete_WriteScopeSessionCannotDelete(t *testing.T) {
	env := newProgressDeleteEnv(t)
	env.addMark(t, &env.task.ID, "alpha", 50)

	ctx := context.Background()
	secret, err := env.mgr.MintAndStore(ctx, &domain.Token{
		Name: "writer", Scopes: domain.Scopes{domain.ScopeWrite},
	})
	if err != nil {
		t.Fatalf("mint write-scope token: %v", err)
	}
	sess := sessionFor(t, env.mgr, secret)
	csrfCookie, csrfTok := env.csrfPair(t, sess)
	body := "csrf_token=" + csrfTok + "&project=BMB&task=" + env.task.Key + "&assessor=alpha"
	rec := postDelete(env.w, body, sessionCookie(sess), csrfCookie)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("write-scope session status = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	if got := env.countMarks(t, &env.task.ID, "alpha"); got != 1 {
		t.Fatalf("alpha's mark did not survive a write-scope session attempt: %d rows, want 1", got)
	}
}

// TestProgressTrackDelete_UnknownAssessorIsANoOp mirrors the store's own
// contract (store.ProgressRepo.DeleteTrack on an unknown assessor removes
// nothing and errors at nobody) through the full HTTP path.
func TestProgressTrackDelete_UnknownAssessorIsANoOp(t *testing.T) {
	env := newProgressDeleteEnv(t)
	env.addMark(t, &env.task.ID, "alpha", 50)

	sess := sessionFor(t, env.mgr, progressDeleteAdminSecret)
	csrfCookie, csrfTok := env.csrfPair(t, sess)
	body := "csrf_token=" + csrfTok + "&project=BMB&task=" + env.task.Key + "&assessor=nobody"
	rec := postDelete(env.w, body, sessionCookie(sess), csrfCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a clean no-op); body=%q", rec.Code, rec.Body.String())
	}
	if got := env.countMarks(t, &env.task.ID, "alpha"); got != 1 {
		t.Fatalf("alpha's mark changed after deleting an unknown assessor: %d rows, want 1", got)
	}
}
