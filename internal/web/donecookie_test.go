package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// doneToggleService answers BoardGet with a fixed, per-key board and stubs
// out the other reads the board page makes (ProjectProgress, TaskProgress,
// ChatList) so handleBoard's best-effort calls do not panic on the nil
// embedded service.Service. Keeping these bare-minimum (no counters, no
// scoring) is deliberate: this file is testing the hide-done cookie, not
// re-testing the progress batching progress_board_test.go already covers.
type doneToggleService struct {
	service.Service
	boards map[string]*service.Board
}

func (s *doneToggleService) BoardGet(_ context.Context, _ service.Actor, in service.BoardGetInput) (*service.Board, error) {
	if b, ok := s.boards[in.ProjectKey]; ok {
		return b, nil
	}
	return &service.Board{}, nil
}

func (s *doneToggleService) ProjectProgress(_ context.Context, _ service.Actor, in service.ProjectProgressInput) (*service.ProjectProgressResult, error) {
	return &service.ProjectProgressResult{ProjectKey: in.ProjectKey}, nil
}

func (s *doneToggleService) TaskProgress(_ context.Context, _ service.Actor, in service.TaskProgressInput) (*service.TaskProgressResult, error) {
	return &service.TaskProgressResult{ProjectKey: in.ProjectKey}, nil
}

func (s *doneToggleService) ChatList(_ context.Context, _ service.Actor, _ service.ChatListInput) (*service.ChatListResult, error) {
	return &service.ChatListResult{}, nil
}

// doneToggleBoard builds a one-project board with a single Done-column task,
// whose title is the thing tests grep for to tell "done is shown" from
// "done is hidden" (partials.html renders a hidden Done column as a bare
// "N hidden" line with no task markup at all).
func doneToggleBoard(key, doneTaskTitle string) *service.Board {
	return &service.Board{Projects: []service.BoardProject{{
		Key:  key,
		Name: key,
		Columns: []service.BoardColumn{
			{Name: "Backlog", Kind: domain.KindBacklog, Count: 0},
			{Name: "Done", Kind: domain.KindDone, Count: 1, Tasks: []domain.TaskView{
				{Task: domain.Task{Key: key + "-1", Title: doneTaskTitle}, ColumnName: "Done"},
			}},
		},
	}}}
}

// newDoneToggleWeb wires a Web against doneToggleService for the given
// boards, reusing newTestManager's fixed clock/bootstrap secret so
// sessionFor keeps working unchanged.
func newDoneToggleWeb(t *testing.T, mgr *auth.Manager, boards map[string]*service.Board) (*Web, *auth.Manager) {
	t.Helper()
	tpl, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	if mgr == nil {
		mgr = newTestManager(t)
	}
	w := New(Deps{
		Service:   &doneToggleService{boards: boards},
		Auth:      mgr,
		Bus:       events.New(newEmptyHistory(), 4),
		Templates: tpl,
		BaseURL:   "http://127.0.0.1:0",
	})
	return w, mgr
}

// doneShownCookie picks the kanban_done_shown Set-Cookie out of a response,
// or nil if the handler did not set one.
func doneShownCookie(rw interface{ Result() *http.Response }) *http.Cookie {
	for _, c := range rw.Result().Cookies() {
		if c.Name == doneCookieName {
			return c
		}
	}
	return nil
}

const testAdminSecret = "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"

// ---------------------------------------------------------------------------
// Acceptance criterion 1: click Show done, navigate away, come back with no
// parameters -- done tasks are still shown.
// ---------------------------------------------------------------------------

func TestHideDone_CookiePersistsAcrossRequests(t *testing.T) {
	boards := map[string]*service.Board{"BMB": doneToggleBoard("BMB", "Finished Widget")}
	w, mgr := newDoneToggleWeb(t, nil, boards)
	sess := sessionFor(t, mgr, testAdminSecret)
	ck := sessionCookie(sess)

	rw1 := do(w, "GET", "/p/BMB?hide_done=0", nil, ck)
	if rw1.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB?hide_done=0 = %d, want 200; body=%q", rw1.Code, trim(rw1.Body.String(), 300))
	}
	if !strings.Contains(rw1.Body.String(), "Finished Widget") {
		t.Fatal("explicit hide_done=0 did not render the done task")
	}
	shown := doneShownCookie(rw1)
	if shown == nil {
		t.Fatal("no kanban_done_shown cookie was set after an explicit ?hide_done=0")
	}

	// A later, unrelated request: no query parameter at all, only the
	// cookie the previous response left behind.
	rw2 := do(w, "GET", "/p/BMB", nil, ck, shown)
	if rw2.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB (revisit) = %d, want 200; body=%q", rw2.Code, trim(rw2.Body.String(), 300))
	}
	if !strings.Contains(rw2.Body.String(), "Finished Widget") {
		t.Fatal("a plain revisit with no ?hide_done forgot the remembered show-done state")
	}
	// Criterion 4: no redirect, no second request needed to reach that state.
	if rw2.Header().Get("Location") != "" {
		t.Fatal("board render should not redirect to reach the remembered state")
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 2: per-project -- switching it in one project does
// not change another.
// ---------------------------------------------------------------------------

func TestHideDone_PerProjectIndependence(t *testing.T) {
	boards := map[string]*service.Board{
		"BMB": doneToggleBoard("BMB", "BMB done task"),
		"OTH": doneToggleBoard("OTH", "OTH done task"),
	}
	w, mgr := newDoneToggleWeb(t, nil, boards)
	sess := sessionFor(t, mgr, testAdminSecret)
	ck := sessionCookie(sess)

	rw1 := do(w, "GET", "/p/BMB?hide_done=0", nil, ck)
	shown := doneShownCookie(rw1)
	if shown == nil {
		t.Fatal("no cookie set after toggling show-done on BMB")
	}

	rwOth := do(w, "GET", "/p/OTH", nil, ck, shown)
	if rwOth.Code != http.StatusOK {
		t.Fatalf("GET /p/OTH = %d, want 200; body=%q", rwOth.Code, trim(rwOth.Body.String(), 300))
	}
	if strings.Contains(rwOth.Body.String(), "OTH done task") {
		t.Fatal("toggling show-done on BMB leaked into a different project (OTH)")
	}

	rwBmb := do(w, "GET", "/p/BMB", nil, ck, shown)
	if !strings.Contains(rwBmb.Body.String(), "BMB done task") {
		t.Fatal("BMB's own remembered show-done state was lost by the OTH request")
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 3: an explicit ?hide_done overrides the remembered
// value and becomes the new remembered value.
// ---------------------------------------------------------------------------

func TestHideDone_QueryOverridesAndPersists(t *testing.T) {
	boards := map[string]*service.Board{"BMB": doneToggleBoard("BMB", "Finished Widget")}
	w, mgr := newDoneToggleWeb(t, nil, boards)
	sess := sessionFor(t, mgr, testAdminSecret)
	ck := sessionCookie(sess)

	// Remember "show done" for BMB first.
	rw1 := do(w, "GET", "/p/BMB?hide_done=0", nil, ck)
	shown := doneShownCookie(rw1)
	if shown == nil {
		t.Fatal("no cookie set after ?hide_done=0")
	}

	// An explicit hide, arriving with the "show" cookie still attached,
	// must override it for this response...
	rw2 := do(w, "GET", "/p/BMB?hide_done=1", nil, ck, shown)
	if rw2.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB?hide_done=1 = %d, want 200; body=%q", rw2.Code, trim(rw2.Body.String(), 300))
	}
	if strings.Contains(rw2.Body.String(), "Finished Widget") {
		t.Fatal("explicit ?hide_done=1 did not override the remembered show-done state")
	}
	updated := doneShownCookie(rw2)
	if updated == nil {
		t.Fatal("explicit ?hide_done=1 did not rewrite the cookie")
	}
	if updated.Value == shown.Value {
		t.Fatalf("cookie value unchanged after an explicit override: %q", updated.Value)
	}

	// ...and that override must now BE the remembered value: a later plain
	// revisit with no query parameter stays hidden.
	rw3 := do(w, "GET", "/p/BMB", nil, ck, updated)
	if strings.Contains(rw3.Body.String(), "Finished Widget") {
		t.Fatal("the explicit hide_done=1 override was not persisted as the new remembered value")
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 6: a corrupt or foreign cookie does not break the
// page and falls back to the default (done hidden).
// ---------------------------------------------------------------------------

func TestHideDone_CorruptCookieFallsBackToDefault(t *testing.T) {
	boards := map[string]*service.Board{"BMB": doneToggleBoard("BMB", "Finished Widget")}
	w, mgr := newDoneToggleWeb(t, nil, boards)
	sess := sessionFor(t, mgr, testAdminSecret)
	ck := sessionCookie(sess)

	cases := []struct {
		name  string
		value string
	}{
		{"punctuation and empty fields", "!!!,,not-a-key,b-mb"},
		{"foreign opaque blob", "some-other-apps-opaque-blob=="},
		{"grossly oversized value", strings.Repeat("z", 10000)},
		{"a formerly-valid key that no longer exists", "ZZZZZZZZ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rw := do(w, "GET", "/p/BMB", nil, ck, &http.Cookie{Name: doneCookieName, Value: tc.value})
			if rw.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rw.Code, trim(rw.Body.String(), 300))
			}
			if strings.Contains(rw.Body.String(), "Finished Widget") {
				t.Fatalf("a garbage cookie (%q) did not fall back to the default (done hidden)", tc.value)
			}
		})
	}
}

// TestParseDoneShownCookie_Tolerant exercises the parser directly for the
// same "survive garbage" contract at the unit level: nothing here should
// ever panic, and only well-formed project keys should survive.
func TestParseDoneShownCookie_Tolerant(t *testing.T) {
	got := parseDoneShownCookie("BMB,,!!!,ab,toolongkey9,bm-b,xy")
	want := map[string]bool{"BMB": true, "AB": true, "XY": true}
	if len(got) != len(want) {
		t.Fatalf("parseDoneShownCookie = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("parseDoneShownCookie = %v, missing %q", got, k)
		}
	}
}

func TestParseDoneShownCookie_EmptyAndOversized(t *testing.T) {
	if got := parseDoneShownCookie(""); len(got) != 0 {
		t.Fatalf("empty cookie value produced %v, want empty set", got)
	}
	// A value far past the truncation bound must not panic and must not
	// spuriously validate.
	got := parseDoneShownCookie(strings.Repeat("q", 100000))
	if len(got) != 0 {
		t.Fatalf("oversized garbage produced %v, want empty set", got)
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 7: HttpOnly + SameSite=Lax always; Secure follows the
// session cookie's own rule (auth.CookiePolicy.Secure), not a second one.
// ---------------------------------------------------------------------------

func TestHideDone_CookieAttributes(t *testing.T) {
	boards := map[string]*service.Board{"BMB": doneToggleBoard("BMB", "Finished Widget")}
	w, mgr := newDoneToggleWeb(t, nil, boards)
	sess := sessionFor(t, mgr, testAdminSecret)
	ck := sessionCookie(sess)

	rw := do(w, "GET", "/p/BMB?hide_done=0", nil, ck)
	got := doneShownCookie(rw)
	if got == nil {
		t.Fatal("no kanban_done_shown cookie set")
	}
	if !got.HttpOnly {
		t.Fatal("kanban_done_shown must be HttpOnly")
	}
	if got.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax", got.SameSite)
	}
	if got.Path != "/" {
		t.Fatalf("Path = %q, want /", got.Path)
	}
	if got.MaxAge <= 0 {
		t.Fatalf("MaxAge = %d, want a long positive lifetime", got.MaxAge)
	}
}

// TestHideDone_SecureMatchesSessionCookiePolicy proves Secure is not a
// second, independently-invented rule: on the very same response, our
// cookie and the CSRF cookie (which explicitly reuses the session cookie's
// auth.CookiePolicy.Secure) must agree, both when the deployment is
// plain-http/insecure and when it is forced https.
func TestHideDone_SecureMatchesSessionCookiePolicy(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }

	t.Run("insecure http deployment", func(t *testing.T) {
		mgr := auth.NewManager(&authMemTokenStore{}, &authMemSessionStore{}, nil, "", true, now)
		if _, err := mgr.Bootstrap(context.Background(), testAdminSecret); err != nil {
			t.Fatalf("Bootstrap: %v", err)
		}
		boards := map[string]*service.Board{"BMB": doneToggleBoard("BMB", "Finished Widget")}
		w, _ := newDoneToggleWeb(t, mgr, boards)
		sess := sessionFor(t, mgr, testAdminSecret)
		rw := do(w, "GET", "/p/BMB?hide_done=0", nil, sessionCookie(sess))

		done := doneShownCookie(rw)
		if done == nil {
			t.Fatal("no kanban_done_shown cookie set")
		}
		csrf := cookieNamed(rw, auth.CSRFCookieName)
		if csrf == nil {
			t.Fatal("no kanban_csrf cookie set (test setup problem, not the feature under test)")
		}
		if done.Secure != csrf.Secure {
			t.Fatalf("kanban_done_shown.Secure = %v, kanban_csrf.Secure = %v -- must agree", done.Secure, csrf.Secure)
		}
		if done.Secure {
			t.Fatal("plain-http, --insecure-http deployment must not set Secure")
		}
	})

	t.Run("forced https deployment", func(t *testing.T) {
		mgr := auth.NewManager(&authMemTokenStore{}, &authMemSessionStore{}, nil, "https://kanban.example", false, now)
		if _, err := mgr.Bootstrap(context.Background(), testAdminSecret); err != nil {
			t.Fatalf("Bootstrap: %v", err)
		}
		boards := map[string]*service.Board{"BMB": doneToggleBoard("BMB", "Finished Widget")}
		w, _ := newDoneToggleWeb(t, mgr, boards)
		sess := sessionFor(t, mgr, testAdminSecret)
		rw := do(w, "GET", "/p/BMB?hide_done=0", nil, sessionCookie(sess))

		done := doneShownCookie(rw)
		if done == nil {
			t.Fatal("no kanban_done_shown cookie set")
		}
		csrf := cookieNamed(rw, auth.CSRFCookieName)
		if csrf == nil {
			t.Fatal("no kanban_csrf cookie set (test setup problem, not the feature under test)")
		}
		if done.Secure != csrf.Secure {
			t.Fatalf("kanban_done_shown.Secure = %v, kanban_csrf.Secure = %v -- must agree", done.Secure, csrf.Secure)
		}
		if !done.Secure {
			t.Fatal("an https BaseURL deployment must set Secure")
		}
	})
}

func cookieNamed(rw interface{ Result() *http.Response }, name string) *http.Cookie {
	for _, c := range rw.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Acceptance criterion 8: after an SSE-driven re-read the state is not
// lost. app.js re-fetches window.location.href (or reloads it) on a
// resync/notify frame -- i.e. the very same URL, so the remembered value
// keeps applying as long as the browser still carries the cookie and the
// URL still carries no ?hide_done. This is exercised here as a second,
// independent request against the same URL with only the cookie attached
// (no server-side session/connection state to simulate SSE with).
// ---------------------------------------------------------------------------

func TestHideDone_SurvivesRepeatedSameURLFetch(t *testing.T) {
	boards := map[string]*service.Board{"BMB": doneToggleBoard("BMB", "Finished Widget")}
	w, mgr := newDoneToggleWeb(t, nil, boards)
	sess := sessionFor(t, mgr, testAdminSecret)
	ck := sessionCookie(sess)

	rw1 := do(w, "GET", "/p/BMB?hide_done=0", nil, ck)
	shown := doneShownCookie(rw1)
	if shown == nil {
		t.Fatal("no cookie set after ?hide_done=0")
	}

	// Simulate the SSE-triggered re-fetch of the same URL app.js performs:
	// repeated GETs of the exact same path, no query string, cookie intact.
	for i := 0; i < 3; i++ {
		rw := do(w, "GET", "/p/BMB", nil, ck, shown)
		if !strings.Contains(rw.Body.String(), "Finished Widget") {
			t.Fatalf("re-fetch #%d lost the remembered show-done state", i+1)
		}
	}
}
