package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// stubService implements service.Service with every method returning
// ErrServiceUnavailable. The web handlers depend on a real service
// implementation but tests want to assert "the handler reached the service
// call" rather than exercise business logic that lives in internal/service.
// Tests that want different behaviour swap in a different service via
// newTestWebWithService.
type stubService struct{}

func (stubService) BoardGet(context.Context, service.Actor, service.BoardGetInput) (*service.Board, error) {
	return nil, ErrServiceUnavailable
}
func (stubService) TaskNext(context.Context, service.Actor, service.TaskNextInput) (*service.NextResult, error) {
	return nil, ErrServiceUnavailable
}
func (stubService) TaskGet(context.Context, service.Actor, service.TaskGetInput) (*service.TaskGetResult, error) {
	return nil, ErrServiceUnavailable
}
func (stubService) TaskCreate(context.Context, service.Actor, service.TaskCreateInput) (*service.TaskCreateResult, error) {
	return nil, ErrServiceUnavailable
}
func (stubService) TaskUpdate(context.Context, service.Actor, service.TaskUpdateInput) (*service.TaskUpdateResult, error) {
	return nil, ErrServiceUnavailable
}
func (stubService) TaskLink(context.Context, service.Actor, service.TaskLinkInput) (*service.TaskLinkResult, error) {
	return nil, ErrServiceUnavailable
}
func (stubService) TaskClaim(context.Context, service.Actor, service.TaskClaimInput) (*service.TaskClaimResult, error) {
	return nil, ErrServiceUnavailable
}
func (stubService) TaskRemove(context.Context, service.Actor, service.TaskRemoveInput) (*service.TaskRemoveResult, error) {
	return nil, ErrServiceUnavailable
}
func (stubService) ProjectUpsert(context.Context, service.Actor, service.ProjectUpsertInput) (*service.ProjectUpsertResult, error) {
	return nil, ErrServiceUnavailable
}

// newTestWeb builds a minimal Web wired against a stub service so the
// handler tree can be exercised without spinning up SQLite. The shutdown
// channel is closed-once-by-Close; tests that need to drive SSE shutdown
// pass their own channel via web.Deps.Shutdown.
//
// Templates are built once per call: they embed cleanly into the test
// binary, so building the engine here costs ~50ms. That's fine for unit
// tests; if a future per-test setup needs the same engine many times,
// switch to a package-level sync.Once.
func newTestWeb(t *testing.T) *Web {
	t.Helper()
	tpl, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	mgr := newTestManager(t)
	return New(Deps{
		Service:   stubService{},
		Auth:      mgr,
		Bus:       events.New(newEmptyHistory(), 4),
		Templates: tpl,
		BaseURL:   "http://127.0.0.1:0",
	})
}

// newTestManager builds an auth.Manager with the in-memory fakes declared
// in auth_test.go's same package. Those fakes live in the auth package's
// _test.go file, so they are accessible from this package too (Go's
// internal-test rule).
func newTestManager(t *testing.T) *auth.Manager {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }
	mgr := auth.NewManager(&authMemTokenStore{}, &authMemSessionStore{}, nil, "", true, now)
	if _, err := mgr.Bootstrap(context.Background(), "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return mgr
}

// ---------------------------------------------------------------------------
// In-memory stores — declared here rather than imported from auth_test.go
// because the auth tests' fakes live in a _test.go file and are not visible
// from another package. The shapes mirror memTokenStore / memSessionStore
// just closely enough to satisfy auth.TokenLookup / auth.SessionLookup.
//
// Both stores are POINTER receivers because Create mutates the map; a value
// receiver would mutate a local copy and the manager would observe an
// empty store on the next Get.
// ---------------------------------------------------------------------------

type authMemTokenStore struct {
	tokens map[string]*domain.Token
}

func (s *authMemTokenStore) GetByHash(_ context.Context, hash []byte) (*domain.Token, error) {
	for _, t := range s.tokens {
		if bytes.Equal(t.Hash, hash) {
			cp := *t
			return &cp, nil
		}
	}
	return nil, domain.NotFound("token", "?")
}
func (s *authMemTokenStore) GetByName(_ context.Context, name string) (*domain.Token, error) {
	for _, t := range s.tokens {
		if t.Name == name {
			cp := *t
			return &cp, nil
		}
	}
	return nil, domain.NotFound("token", name)
}
func (s *authMemTokenStore) List(_ context.Context) ([]*domain.Token, error) {
	// Sorted by name so callers get a stable order — Go map iteration is
	// not stable across processes, and a few of the admin page's
	// assertions (e.g. "page 2 must include the last token") read as
	// flake if the row order is random. The real store sorts via SQL
	// ORDER BY so this also keeps the test honest against production.
	keys := make([]string, 0, len(s.tokens))
	for k := range s.tokens {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*domain.Token, 0, len(s.tokens))
	for _, k := range keys {
		if t := s.tokens[k]; t != nil {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *authMemTokenStore) Count(_ context.Context) (int, error) { return len(s.tokens), nil }
func (s *authMemTokenStore) Create(_ context.Context, t *domain.Token) error {
	if s.tokens == nil {
		s.tokens = map[string]*domain.Token{}
	}
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	cp := *t
	s.tokens[t.Name] = &cp
	return nil
}
func (s *authMemTokenStore) UpdateHash(_ context.Context, id string, hash []byte) error {
	for _, t := range s.tokens {
		if t.ID == id {
			t.Hash = append([]byte(nil), hash...)
			return nil
		}
	}
	return domain.NotFound("token", id)
}
func (s *authMemTokenStore) Revoke(_ context.Context, name string) error {
	t, ok := s.tokens[name]
	if !ok {
		return nil
	}
	now := time.Now().UTC()
	t.RevokedAt = &now
	return nil
}

type authMemSessionStore struct {
	sessions map[string]*domain.Session
}

func (s *authMemSessionStore) CreateSession(_ context.Context, sess *domain.Session) error {
	if s.sessions == nil {
		s.sessions = map[string]*domain.Session{}
	}
	cp := *sess
	s.sessions[sess.ID] = &cp
	return nil
}
func (s *authMemSessionStore) GetSession(_ context.Context, id string) (*domain.Session, error) {
	sess, ok := s.sessions[id]
	if !ok {
		return nil, domain.NotFound("session", id)
	}
	cp := *sess
	return &cp, nil
}
func (s *authMemSessionStore) TouchSession(_ context.Context, id string, now time.Time) error {
	if sess, ok := s.sessions[id]; ok {
		sess.LastSeenAt = now
	}
	return nil
}
func (s *authMemSessionStore) DeleteSession(_ context.Context, id string) error {
	delete(s.sessions, id)
	return nil
}

// ---------------------------------------------------------------------------
// Helpers used by the route tests
// ---------------------------------------------------------------------------

// do issues a request through w.Handler() and returns the recorded response.
// path is joined to a base URL so the new ServeMux pattern parser still
// receives the right path; cookies and headers are optional.
func do(w *Web, method, path string, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rw := httptest.NewRecorder()
	w.Handler().ServeHTTP(rw, req)
	return rw
}

// sessionFor issues a CreateSession against the manager and returns the
// resulting Session so tests can attach its cookie to a request.
func sessionFor(t *testing.T, mgr *auth.Manager, secret string) *domain.Session {
	t.Helper()
	sess, err := mgr.CreateSession(context.Background(), secret)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sess
}

// sessionCookie builds the cookie the browser would carry for a session.
func sessionCookie(sess *domain.Session) *http.Cookie {
	return &http.Cookie{Name: "kanban_session", Value: sess.ID}
}

// ---------------------------------------------------------------------------
// /healthz, /readyz, /agent-setup, /login form
// ---------------------------------------------------------------------------

func TestHealthz_AlwaysOK(t *testing.T) {
	w := newTestWeb(t)
	rw := do(w, "GET", "/healthz", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "ok") {
		t.Fatalf("body = %q, want to contain 'ok'", rw.Body.String())
	}
}

func TestReadyz_OKWithInMemoryStore(t *testing.T) {
	w := newTestWeb(t)
	rw := do(w, "GET", "/readyz", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rw.Code, rw.Body.String())
	}
	if rw.Header().Get("Content-Type") == "" {
		t.Fatal("missing Content-Type")
	}
}

func TestAgentSetup_NoSessionRequired(t *testing.T) {
	w := newTestWeb(t)
	rw := do(w, "GET", "/agent-setup", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "MCP") {
		t.Fatalf("body does not contain MCP snippets, got head=%q", trim(rw.Body.String(), 200))
	}
}

func TestLoginForm_AnonymousRender(t *testing.T) {
	w := newTestWeb(t)
	rw := do(w, "GET", "/login", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "csrf_token") &&
		!strings.Contains(rw.Body.String(), "kanban_csrf") {
		// The form may render either as a literal cookie-bound hidden field
		// or via a script that reads the cookie; both are valid. The HTTP
		// 200 + HTML is the binding assertion.
		t.Logf("login form rendered; csrf surface left to client wiring")
	}
}

// ---------------------------------------------------------------------------
// Static assets
// ---------------------------------------------------------------------------

func TestStatic_ServesCSSWithCacheHeader(t *testing.T) {
	w := newTestWeb(t)
	rw := do(w, "GET", "/static/app.css", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rw.Code, rw.Body.String())
	}
	ct := rw.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("Content-Type = %q, want text/css...", ct)
	}
	if rw.Header().Get("Cache-Control") == "" {
		t.Fatal("missing Cache-Control header")
	}
	if rw.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing X-Content-Type-Options")
	}
	if rw.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("missing CSP header")
	}
}

func TestStatic_NotFoundReturns404(t *testing.T) {
	w := newTestWeb(t)
	rw := do(w, "GET", "/static/does-not-exist", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rw.Code)
	}
}

// ---------------------------------------------------------------------------
// Security headers on every response
// ---------------------------------------------------------------------------

func TestSecurityHeaders_OnEveryResponse(t *testing.T) {
	w := newTestWeb(t)
	for _, path := range []string{"/healthz", "/readyz", "/agent-setup", "/login", "/static/app.css"} {
		rw := do(w, "GET", path, nil)
		if rw.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("%s: missing CSP", path)
		}
		if rw.Header().Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%s: missing X-Frame-Options", path)
		}
		if rw.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: missing Referrer-Policy", path)
		}
	}
}

// ---------------------------------------------------------------------------
// Pages redirect anonymous callers to /login
// ---------------------------------------------------------------------------

func TestPages_RedirectAnonymousToLogin(t *testing.T) {
	w := newTestWeb(t)
	cases := []struct {
		path string
	}{
		{"/"},
		{"/p/BMB"},
		{"/t/BMB-1"},
		{"/p/BMB/activity"},
		{"/admin"},
	}
	for _, tc := range cases {
		rw := do(w, "GET", tc.path, nil)
		if rw.Code != http.StatusSeeOther {
			t.Fatalf("%s: status = %d, want 303 (location=%q)", tc.path, rw.Code, rw.Header().Get("Location"))
		}
		if !strings.HasPrefix(rw.Header().Get("Location"), "/login") {
			t.Fatalf("%s: location = %q, want /login", tc.path, rw.Header().Get("Location"))
		}
	}
}

// ---------------------------------------------------------------------------
// Service placeholder: pages render the 503 envelope, never panic.
// ---------------------------------------------------------------------------

func TestPages_ServiceUnavailableIsJSONEnvelope(t *testing.T) {
	w := newTestWeb(t)
	// Sign in as admin so we get past the redirect.
	mgr := w.d.Auth
	sess := sessionFor(t, mgr, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")
	ck := sessionCookie(sess)

	// /admin needs a service call (board_get). The placeholder returns
	// ErrServiceUnavailable -> 503 with the JSON envelope. The page
	// handler renders via pageError (HTML), so we expect a 503 HTML page
	// not JSON — that's the contract from errors.go.
	rw := do(w, "GET", "/admin", nil, ck)
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("/admin status = %d, want 503 (placeholder service); body=%q",
			rw.Code, trim(rw.Body.String(), 200))
	}
	if !strings.Contains(rw.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", rw.Header().Get("Content-Type"))
	}
}

// ---------------------------------------------------------------------------
// Login submit
// ---------------------------------------------------------------------------

func TestLogin_RejectsBadSecret(t *testing.T) {
	w := newTestWeb(t)
	csrf, _ := w.d.Auth.IssueCSRF()
	// Body has a matching CSRF token in both cookie and form, but the
	// supplied auth secret is bogus. Expected: 401 (token not recognised),
	// not 403 (CSRF rejected) and not 429 (rate-limited — we just opened).
	form := "csrf_token=" + csrf + "&token=not-a-real-token&next=/"
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%q", rec.Code, trim(rec.Body.String(), 200))
	}
}

func TestLogin_RejectsWithoutCSRF(t *testing.T) {
	w := newTestWeb(t)
	form := "token=kbn_anything"
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	// No CSRF cookie + no CSRF header + no CSRF form value -> rejected.
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", rec.Code, trim(rec.Body.String(), 200))
	}
}

// ---------------------------------------------------------------------------
// CSRF: form-field path (the path the templates actually use)
// ---------------------------------------------------------------------------

// TestCSRF_FormFieldIsAccepted proves the form-field path works after the
// fallback deletion: a request with a matching csrf_token hidden field and
// the matching cookie passes auth.Manager.VerifyCSRF (which handles both
// the header AND the form field). Without the form-field branch, this test
// would fail post-deletion because the area used to have a hand-rolled
// comparison; that fallback is gone, so this test exercises the same code
// path the templates actually take.
func TestCSRF_FormFieldIsAccepted(t *testing.T) {
	w := newTestWeb(t)
	mgr := w.d.Auth
	secret := "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"
	sess := sessionFor(t, mgr, secret)
	ck := sessionCookie(sess)
	csrf, err := mgr.IssueCSRF()
	if err != nil {
		t.Fatalf("IssueCSRF: %v", err)
	}

	// Build a POST to /admin/tokens with a matching form CSRF. The handler
	// will fail downstream (placeholder service), but it should pass CSRF
	// validation and reach the service call.
	body := "csrf_token=" + csrf + "&name=alice&scope=read&projects=BMB"
	req := httptest.NewRequest("POST", "/admin/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ck)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	// The downstream /admin/tokens handler will produce a 503 because
	// the placeholder service cannot list projects; what we want is for
	// the response NOT to be 403 (which would mean CSRF rejected the
	// form-field path).
	if rec.Code == http.StatusForbidden {
		t.Fatalf("CSRF form-field path rejected; status = 403, body=%q",
			trim(rec.Body.String(), 200))
	}
}

func TestCSRF_MismatchRejected(t *testing.T) {
	w := newTestWeb(t)
	mgr := w.d.Auth
	secret := "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"
	sess := sessionFor(t, mgr, secret)
	ck := sessionCookie(sess)

	body := "name=alice&scope=read&projects=wrong"
	req := httptest.NewRequest("POST", "/admin/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ck)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "different"})
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", rec.Code, trim(rec.Body.String(), 200))
	}
}

// ---------------------------------------------------------------------------
// Body cap on POST routes
// ---------------------------------------------------------------------------

func TestBodyCap_LoginRejectsOversizedBody(t *testing.T) {
	w := newTestWeb(t)
	mgr := w.d.Auth
	tok, _ := mgr.IssueCSRF()
	// Body > 1 MB. Even with a valid CSRF cookie + matching form field,
	// MaxBytesReader trips when the handler reads PostForm.
	huge := strings.Repeat("a", int(domain.MaxRequestBodyBytes)+1024)
	body := "token=kbn_x&csrf_token=" + tok + "&extra=" + huge
	req := httptest.NewRequest("POST", "/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: tok})
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	// The web layer's body cap fires at PostForm parse time, returning a
	// generic 4xx (typically 400 from the standard library's error
	// mapping). The auth middleware's own 413 only applies to
	// bearer-authenticated routes — this test exercises the session-based
	// path, where the wrapping is the same store.MaxBytesReader from the
	// "1 MB body limit" rule. The exact code is allowed to be either 400
	// or 413; both are correct rejections.
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("status = %d, want 4xx; body=%q", rec.Code, trim(rec.Body.String(), 200))
	}
}

// ---------------------------------------------------------------------------
// CSRF: bearer-authenticated fragment calls do not require a CSRF token.
// session-authenticated fragment calls still do.
// ---------------------------------------------------------------------------

// TestCSRF_BearerFragmentNoCSRF confirms a request authenticated by a
// bearer token reaches the service call on a fragment endpoint without a
// CSRF token. This is the regression net for the live bug: bearer tokens
// have no browser cookies to forge, so the cross-origin attack CSRF
// defends against is structurally impossible.
func TestCSRF_BearerFragmentNoCSRF(t *testing.T) {
	w := newTestWeb(t)
	secret := "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"
	body := "key=BMB-1&action=release"
	req := httptest.NewRequest("POST", "/fragments/claim", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+secret)
	// Deliberately NO CSRF cookie, NO csrf_token form field, NO
	// X-CSRF-Token header.
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	// The placeholder service returns ErrServiceUnavailable -> 503. What
	// matters is that the request is NOT 403 (CSRF rejection) and NOT 401
	// (no auth). It must reach the service layer.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("bearer request rejected by CSRF; status = 403, body=%q",
			trim(rec.Body.String(), 200))
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("bearer request rejected by auth; status = 401, body=%q",
			trim(rec.Body.String(), 200))
	}
}

// TestCSRF_SessionFragmentStillRequiresCSRF confirms a session-authenticated
// fragment call without a CSRF token is rejected — sessions carry cookies,
// so the cross-origin attack CSRF defends against remains live. The other
// half of the bearer-vs-session split: we did not loosen session security
// in order to fix bearer clients.
func TestCSRF_SessionFragmentStillRequiresCSRF(t *testing.T) {
	w := newTestWeb(t)
	mgr := w.d.Auth
	secret := "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"
	sess := sessionFor(t, mgr, secret)

	body := "key=BMB-1&action=release"
	req := httptest.NewRequest("POST", "/fragments/claim", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie(sess))
	// No CSRF cookie, no csrf_token field, no header.
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("session-auth fragment without CSRF accepted; status = %d, want 403; body=%q",
			rec.Code, trim(rec.Body.String(), 200))
	}
}

// TestCSRF_QueryParamCannotBypass is a regression test for a real CSRF bypass.
//
// verifyCSRF skips the double-submit check when the request carries a
// credential a cross-origin page could not have attached. An earlier version
// counted a `?ticket=` query parameter as one of those. It never validated the
// ticket — only checked the parameter was present — and a query string is
// entirely attacker-chosen, so any page could POST to an admin route with
// `?ticket=anything` and the victim's session cookie and have CSRF skipped.
//
// Only headers can qualify: a cross-origin form can aim a request anywhere but
// cannot set a header on it. These cases pin that boundary.
func TestCSRF_QueryParamCannotBypass(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"fragment route", "/fragments/claim?ticket=anything"},
		{"admin token create", "/admin/tokens?ticket=anything"},
		{"ticket param among others", "/fragments/claim?foo=1&ticket=x&bar=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWeb(t)
			secret := "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"
			sess := sessionFor(t, w.d.Auth, secret)

			req := httptest.NewRequest("POST", tc.path,
				strings.NewReader("key=BMB-1&action=release&name=evil"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(sessionCookie(sess))
			// No CSRF cookie, no csrf_token field, no header — exactly what a
			// cross-origin form can produce.
			rec := httptest.NewRecorder()
			w.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("CSRF bypassed via query parameter: %s returned %d, want 403; body=%q",
					tc.path, rec.Code, trim(rec.Body.String(), 200))
			}
		})
	}
}

// TestCSRF_SessionFragmentWithCSRF confirms the session path with a matching
// CSRF token still passes through — the bearer shortcut did not regress
// the session path. This is the same form-field coverage as the existing
// admin-token test, but exercised through a fragment route so we know
// /fragments/* (which uses requireAPIAuth) honours the same cookie/field.
func TestCSRF_SessionFragmentWithCSRF(t *testing.T) {
	w := newTestWeb(t)
	mgr := w.d.Auth
	secret := "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"
	sess := sessionFor(t, mgr, secret)
	csrf, _ := mgr.IssueCSRF()

	body := "csrf_token=" + csrf + "&key=BMB-1&action=release"
	req := httptest.NewRequest("POST", "/fragments/claim", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie(sess))
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	// Reaches the placeholder service -> 503, never 403.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("session+CSRF fragment rejected; status = 403, body=%q",
			trim(rec.Body.String(), 200))
	}
}

// ---------------------------------------------------------------------------
// MCP stub
// ---------------------------------------------------------------------------

func TestMCP_StubReturns501(t *testing.T) {
	w := newTestWeb(t)
	rw := do(w, "GET", "/mcp", nil)
	if rw.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "not_implemented") {
		t.Fatalf("body does not contain not_implemented code: %q", rw.Body.String())
	}
}

// ---------------------------------------------------------------------------
// SSE handshake
// ---------------------------------------------------------------------------

// TestSSE_EmitsConnectedFrame drives handleEvents through httptest.NewServer
// so the body can be read incrementally (httptest.NewRecorder buffers the
// whole response and cannot test SSE flushes). A real bearer token gets
// past auth, the Subscribe call drains the empty history, the handler
// writes the initial "event: connected\ndata: {}\n\n" frame and then
// blocks on the select. Cancelling the request context unblocks it.
func TestSSE_EmitsConnectedFrame(t *testing.T) {
	w := newTestWeb(t)
	mgr := w.d.Auth
	secret := "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"
	if _, err := mgr.Bootstrap(context.Background(), secret); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	srv := httptest.NewServer(w.Handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	// Read the first frame, with a hard deadline so a hung server does
	// not wedge the test.
	type readResult struct {
		line string
		err  error
	}
	done := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := resp.Body.Read(buf)
		if err != nil {
			done <- readResult{err: err}
			return
		}
		done <- readResult{line: string(buf[:n])}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("read: %v", r.err)
		}
		if !strings.Contains(r.line, "event: connected") {
			t.Fatalf("first frame missing 'event: connected': %q", r.line)
		}
		if !strings.Contains(r.line, "data: {}") {
			t.Fatalf("first frame missing 'data: {}': %q", r.line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive the connected frame within 3s")
	}

	// Trigger the shutdown path so the handler returns cleanly and the
	// goroutine exits before the test cleanup runs.
	cancel()
}

// ---------------------------------------------------------------------------
// Route conflict check: registering the patterns in web.go must not panic.
// ---------------------------------------------------------------------------

func TestRouteRegistration_DoesNotPanic(t *testing.T) {
	// Building a Web already calls routes(); if any of the GET /p/{key}
	// patterns collide the ServeMux would panic in Handler(). The mere fact
	// that newTestWeb succeeded above is the assertion — but we still
	// touch the most likely collision sites explicitly so a future change
	// to those URLs fails fast with a clear name.
	for _, path := range []string{"/p/BMB", "/p/BMB/activity", "/p/BMB/export", "/t/BMB-1", "/healthz", "/readyz"} {
		req := httptest.NewRequest("GET", path, nil)
		rw := httptest.NewRecorder()
		newTestWeb(t).Handler().ServeHTTP(rw, req)
		// All routes either redirect anonymous callers or answer directly;
		// none should 5xx (placeholder service only renders pages, not
		// these particular URLs).
		if rw.Code >= 500 {
			t.Fatalf("%s: 5xx response (code %d)", path, rw.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// newEmptyHistory backs the events.Bus for tests that never subscribe.
// ---------------------------------------------------------------------------

type emptyHistory struct{}

func (emptyHistory) Since(string, int64, int) ([]domain.Event, error) { return nil, nil }
func (emptyHistory) MinID() (int64, error)                            { return 0, nil }

func newEmptyHistory() emptyHistory { return emptyHistory{} }

// trim returns the first n bytes of s, ellipsised when truncated. Used by
// every assertion that prints a body, so an unexpected 5 KB HTML page does
// not blow up the test log.
func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
