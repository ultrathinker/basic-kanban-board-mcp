package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// csrfReissueService answers BoardGet with a fixed one-project board and
// stubs the other best-effort reads handleBoard makes (ProjectProgress,
// TaskProgress, ChatList), the same minimal shape doneToggleService uses in
// donecookie_test.go. TaskClaim is stubbed to return ErrServiceUnavailable
// explicitly (not left to an embedded nil service.Service, which would
// panic) so a POST /fragments/claim can reach — and only needs to reach —
// the CSRF check this file is exercising; the 503 that follows is expected
// and irrelevant to what these tests assert.
type csrfReissueService struct {
	service.Service
}

func (s *csrfReissueService) BoardGet(_ context.Context, _ service.Actor, in service.BoardGetInput) (*service.Board, error) {
	return &service.Board{Projects: []service.BoardProject{{
		Key:  "BMB",
		Name: "BMB",
		Columns: []service.BoardColumn{
			{Name: "Backlog", Kind: domain.KindBacklog, Count: 0},
		},
	}}}, nil
}

func (s *csrfReissueService) ProjectProgress(_ context.Context, _ service.Actor, in service.ProjectProgressInput) (*service.ProjectProgressResult, error) {
	return &service.ProjectProgressResult{ProjectKey: in.ProjectKey}, nil
}

func (s *csrfReissueService) TaskProgress(_ context.Context, _ service.Actor, in service.TaskProgressInput) (*service.TaskProgressResult, error) {
	return &service.TaskProgressResult{ProjectKey: in.ProjectKey}, nil
}

func (s *csrfReissueService) ChatList(_ context.Context, _ service.Actor, _ service.ChatListInput) (*service.ChatListResult, error) {
	return &service.ChatListResult{}, nil
}

func (s *csrfReissueService) TaskClaim(_ context.Context, _ service.Actor, _ service.TaskClaimInput) (*service.TaskClaimResult, error) {
	return nil, ErrServiceUnavailable
}

// newCSRFReissueWeb builds a Web wired against csrfReissueService, so
// GET /p/BMB renders a full page (meta tag + cookie) instead of an error
// page (which newPage never runs for — see errors.go pageError).
func newCSRFReissueWeb(t *testing.T) *Web {
	t.Helper()
	w := newTestWeb(t)
	w.d.Service = &csrfReissueService{}
	return w
}

var metaCSRFRe = regexp.MustCompile(`<meta name="csrf-token" content="([^"]*)">`)

// metaCSRFToken extracts the value of <meta name="csrf-token" content="…">
// from a rendered page body — this is exactly what app.js's csrfToken()
// reads at request time.
func metaCSRFToken(t *testing.T, body string) string {
	t.Helper()
	m := metaCSRFRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no <meta name=%q> found in body: %s", "csrf-token", trim(body, 300))
	}
	return m[1]
}

// setCSRFCookie extracts the kanban_csrf cookie value from a response's
// Set-Cookie headers, if the response set one.
func setCSRFCookie(rw *http.Response) (string, bool) {
	for _, c := range rw.Cookies() {
		if c.Name == auth.CSRFCookieName {
			return c.Value, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// KANB-16: newPage must stop rotating the CSRF token on every render.
// ---------------------------------------------------------------------------

// TestCSRF_ResponseIsSelfConsistent pins that within a SINGLE response the
// rendered <meta> and the Set-Cookie value always agree — that half already
// worked before this fix and must keep working.
func TestCSRF_ResponseIsSelfConsistent(t *testing.T) {
	w := newCSRFReissueWeb(t)
	mgr := w.d.Auth
	sess := sessionFor(t, mgr, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")

	rec := do(w, "GET", "/p/BMB", nil, sessionCookie(sess))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	meta := metaCSRFToken(t, rec.Body.String())
	cookie, ok := setCSRFCookie(rec.Result())
	if !ok {
		t.Fatal("response did not set the kanban_csrf cookie")
	}
	if meta != cookie {
		t.Fatalf("single-response mismatch: meta=%q cookie=%q", meta, cookie)
	}
}

// TestCSRF_PageRereadKeepsPOSTWorking is the main regression test for
// KANB-16.
//
// The board page re-reads itself over GET after every SSE "something
// changed" signal (live updates without a full reload); the live update
// only swaps specific DOM regions, so the page's <meta name="csrf-token">
// is never touched by it and keeps whatever value the FIRST render put
// there. This test reproduces exactly that sequence:
//
//  1. GET /p/BMB -> remember the token from its <meta> (T1) and the cookie
//     the response set (cookie A).
//  2. GET /p/BMB again, carrying cookie A (this is the self-reread) ->
//     note whatever CSRF cookie this second response leaves the browser
//     holding (cookie B, which may or may not differ from A depending on
//     whether the fix reused it).
//  3. POST /fragments/claim with csrf_token=T1 (the value still sitting in
//     the stale, un-refreshed <meta>) and the CURRENT cookie (B if the
//     second response set one, else A).
//
// Before the fix, step 1 and step 2 each mint a brand new token, so T1 !=
// cookie B and the POST is rejected with 403. After the fix the token is
// derived from the session (auth.CSRFTokenForSession), so every render in
// this sequence keeps handing out the same value without the client having
// any say in what it is: T1 == cookie B, and the POST must not be rejected
// as a CSRF failure.
func TestCSRF_PageRereadKeepsPOSTWorking(t *testing.T) {
	w := newCSRFReissueWeb(t)
	mgr := w.d.Auth
	sess := sessionFor(t, mgr, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")
	sc := sessionCookie(sess)

	// Step 1: first render of the board page.
	rec1 := do(w, "GET", "/p/BMB", nil, sc)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first GET /p/BMB = %d, want 200\n%s", rec1.Code, rec1.Body.String())
	}
	t1 := metaCSRFToken(t, rec1.Body.String())
	cookieA, ok := setCSRFCookie(rec1.Result())
	if !ok {
		t.Fatal("first response did not set the kanban_csrf cookie")
	}

	// Step 2: the page re-reads itself (SSE-triggered GET), carrying
	// whatever CSRF cookie the browser currently holds (cookie A).
	rec2 := do(w, "GET", "/p/BMB", nil, sc, &http.Cookie{Name: auth.CSRFCookieName, Value: cookieA})
	if rec2.Code != http.StatusOK {
		t.Fatalf("second GET /p/BMB = %d, want 200\n%s", rec2.Code, rec2.Body.String())
	}
	current := cookieA
	if cookieB, ok := setCSRFCookie(rec2.Result()); ok {
		current = cookieB
	}

	// Step 3: a state-changing POST using T1 (the token the DOM's <meta>
	// still carries, unchanged since step 1) and the browser's current
	// cookie jar value. It must not be rejected as a CSRF failure.
	req := httptest.NewRequest("POST", "/fragments/claim",
		strings.NewReader("key=BMB-1&action=release"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", t1)
	req.AddCookie(sc)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: current})
	rec3 := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec3, req)

	if rec3.Code == http.StatusForbidden {
		t.Fatalf("page reread broke the very next POST: T1=%q current-cookie=%q status=403, body=%q",
			t1, current, trim(rec3.Body.String(), 300))
	}
}

// TestCSRF_ReissuedWhenCookieMissing confirms the first-visit path still
// works: with no incoming CSRF cookie at all, newPage must still mint a
// fresh, well-formed token and set it.
func TestCSRF_ReissuedWhenCookieMissing(t *testing.T) {
	w := newCSRFReissueWeb(t)
	sess := sessionFor(t, w.d.Auth, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")

	rec := do(w, "GET", "/p/BMB", nil, sessionCookie(sess))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	meta := metaCSRFToken(t, rec.Body.String())
	if meta == "" {
		t.Fatal("no CSRF cookie in the request, but no token was minted")
	}
	if !auth.ValidCSRFToken(meta) {
		t.Fatalf("minted token is not well-formed: %q", meta)
	}
	cookie, ok := setCSRFCookie(rec.Result())
	if !ok || cookie != meta {
		t.Fatalf("cookie (%q, ok=%v) does not match minted meta token %q", cookie, ok, meta)
	}
}

// TestCSRF_ReissuedWhenCookieMalformed confirms a garbage incoming cookie
// value is NOT reused verbatim: newPage must fall back to minting a fresh,
// well-formed token rather than embedding whatever string showed up in the
// cookie.
func TestCSRF_ReissuedWhenCookieMalformed(t *testing.T) {
	w := newCSRFReissueWeb(t)
	sess := sessionFor(t, w.d.Auth, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")

	rec := do(w, "GET", "/p/BMB", nil,
		sessionCookie(sess),
		&http.Cookie{Name: auth.CSRFCookieName, Value: "not-a-real-token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	meta := metaCSRFToken(t, rec.Body.String())
	if meta == "not-a-real-token" {
		t.Fatal("malformed incoming cookie value was reused verbatim instead of being replaced")
	}
	if !auth.ValidCSRFToken(meta) {
		t.Fatalf("replacement token is not well-formed: %q", meta)
	}
}

// ---------------------------------------------------------------------------
// The token must be the SERVER's choice, not the client's.
//
// The first version of the reread fix kept the token stable by reusing
// whatever value the request's CSRF cookie carried, validated only for shape.
// That made the value in <meta> — and in the cookie the server then set —
// something the client could pick. The two tests below pin the property that
// closes it, from both ends: the page never echoes a planted value, and a
// self-consistent planted pair does not authorise a write.
// ---------------------------------------------------------------------------

// plantedCSRFToken is well-formed — 24 bytes, base64url — and therefore
// passes auth.ValidCSRFToken. It is exactly what an attacker who can write a
// cookie for this site would plant: shape is all they need to fake.
const plantedCSRFToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestCSRF_WellFormedButUnknownCookieIsNotEchoedIntoThePage(t *testing.T) {
	w := newCSRFReissueWeb(t)
	sess := sessionFor(t, w.d.Auth, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")

	if !auth.ValidCSRFToken(plantedCSRFToken) {
		t.Fatalf("fixture is wrong: %q must be well-formed for this test to mean anything", plantedCSRFToken)
	}

	rec := do(w, "GET", "/p/BMB", nil,
		sessionCookie(sess),
		&http.Cookie{Name: auth.CSRFCookieName, Value: plantedCSRFToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /p/BMB = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	meta := metaCSRFToken(t, rec.Body.String())
	if meta == plantedCSRFToken {
		t.Fatal("the page echoed a client-supplied CSRF token: shape was checked, authenticity was not")
	}
	if want := w.d.Auth.CSRFTokenForSession(sess.ID); meta != want {
		t.Fatalf("meta token = %q, want the session-derived %q", meta, want)
	}
	cookie, ok := setCSRFCookie(rec.Result())
	if !ok || cookie == plantedCSRFToken {
		t.Fatalf("the server re-set the planted value as its own cookie (%q, ok=%v)", cookie, ok)
	}
}

// TestCSRF_PlantedCookieAndMatchingFieldIsRefused is the attack itself, end
// to end: a cross-origin POST carrying the victim's session cookie plus a
// CSRF cookie and form field the attacker chose and therefore knows. Under a
// pure cookie-vs-field comparison this pair agrees with itself and passes.
// It must not.
func TestCSRF_PlantedCookieAndMatchingFieldIsRefused(t *testing.T) {
	w := newCSRFReissueWeb(t)
	sess := sessionFor(t, w.d.Auth, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")

	req := httptest.NewRequest("POST", "/fragments/claim",
		strings.NewReader("csrf_token="+plantedCSRFToken+"&key=BMB-1&action=release"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie(sess))
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: plantedCSRFToken})
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a self-consistent planted CSRF pair was accepted: status = %d, want 403; body=%q",
			rec.Code, trim(rec.Body.String(), 300))
	}
}

// TestCSRF_DerivedTokenIsStableAndSessionScoped pins the two properties the
// derivation buys, which no shape check could give: the same session always
// yields the same token (that is what survives the SSE reread), and two
// different sessions never share one (that is what stops a token learned in
// one browser from working in another).
func TestCSRF_DerivedTokenIsStableAndSessionScoped(t *testing.T) {
	w := newCSRFReissueWeb(t)
	mgr := w.d.Auth
	a := sessionFor(t, mgr, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")
	b := sessionFor(t, mgr, "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx")

	if a.ID == b.ID {
		t.Fatal("fixture is wrong: the two sessions must be distinct")
	}
	if got, again := mgr.CSRFTokenForSession(a.ID), mgr.CSRFTokenForSession(a.ID); got != again {
		t.Fatalf("token for one session is not stable: %q then %q", got, again)
	}
	if mgr.CSRFTokenForSession(a.ID) == mgr.CSRFTokenForSession(b.ID) {
		t.Fatal("two different sessions derived the same CSRF token")
	}
	if tok := mgr.CSRFTokenForSession(a.ID); !auth.ValidCSRFToken(tok) {
		t.Fatalf("derived token is not shaped like a minted one: %q", tok)
	}
}
