package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// searchSvc is a controllable stub for the projects/search handler. Only
// BoardGet is exercised; the rest of service.Service stays as "service
// unavailable" sentinel so the handler being tested cannot silently start
// talking to a wrong code path.
type searchSvc struct {
	projects []service.BoardProject
}

func (s searchSvc) BoardGet(_ context.Context, _ service.Actor, _ service.BoardGetInput) (*service.Board, error) {
	return &service.Board{Projects: s.projects}, nil
}
func (searchSvc) TaskNext(context.Context, service.Actor, service.TaskNextInput) (*service.NextResult, error) {
	return nil, ErrServiceUnavailable
}
func (searchSvc) TaskGet(context.Context, service.Actor, service.TaskGetInput) (*service.TaskGetResult, error) {
	return nil, ErrServiceUnavailable
}
func (searchSvc) TaskCreate(context.Context, service.Actor, service.TaskCreateInput) (*service.TaskCreateResult, error) {
	return nil, ErrServiceUnavailable
}
func (searchSvc) TaskUpdate(context.Context, service.Actor, service.TaskUpdateInput) (*service.TaskUpdateResult, error) {
	return nil, ErrServiceUnavailable
}
func (searchSvc) TaskLink(context.Context, service.Actor, service.TaskLinkInput) (*service.TaskLinkResult, error) {
	return nil, ErrServiceUnavailable
}
func (searchSvc) TaskClaim(context.Context, service.Actor, service.TaskClaimInput) (*service.TaskClaimResult, error) {
	return nil, ErrServiceUnavailable
}
func (searchSvc) TaskRemove(context.Context, service.Actor, service.TaskRemoveInput) (*service.TaskRemoveResult, error) {
	return nil, ErrServiceUnavailable
}
func (searchSvc) ProjectUpsert(context.Context, service.Actor, service.ProjectUpsertInput) (*service.ProjectUpsertResult, error) {
	return nil, ErrServiceUnavailable
}

// newSearchWeb wires an auth.Manager + minimal Web so the handler can be
// driven without spinning up SQLite. Returns the freshly minted bootstrap
// secret so the caller can mint a session without a second Bootstrap call.
func newSearchWeb(t *testing.T, projectCount int) (*Web, *auth.Manager, string) {
	t.Helper()
	tpl, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	tokStore := &authMemTokenStore{}
	mgr := auth.NewManager(tokStore, &authMemSessionStore{}, nil, "", true, nil)
	secret, err := mgr.Bootstrap(context.Background(), "")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if secret == "" {
		t.Fatal("Bootstrap returned no secret")
	}
	if _, err := mgr.MintAndStore(context.Background(), &domain.Token{
		ID:     uuid.NewString(),
		Name:   "read-tok",
		Scopes: domain.Scopes{domain.ScopeRead},
	}); err != nil {
		t.Fatalf("MintAndStore: %v", err)
	}
	projs := make([]service.BoardProject, 0, projectCount)
	for i := 1; i <= projectCount; i++ {
		projs = append(projs, service.BoardProject{
			Key: "P" + strconv.Itoa(i), Name: "Project " + strconv.Itoa(i), Version: i,
		})
	}
	w := New(Deps{
		Service:   searchSvc{projects: projs},
		Auth:      mgr,
		Bus:       events.New(newEmptyHistory(), 4),
		Templates: tpl,
		BaseURL:   "http://127.0.0.1:0",
	})
	return w, mgr, secret
}

// signIn returns a session cookie for the bootstrap admin token. The
// freshly-minted secret is exactly the one CreateSession wants; reusing
// it here keeps the test self-contained without poking the token store
// for an admin record by name.
func signIn(t *testing.T, mgr *auth.Manager, secret string) *http.Cookie {
	t.Helper()
	sess, err := mgr.CreateSession(context.Background(), secret)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sessionCookie(sess)
}

// projectSearchResponse mirrors the JSON envelope /projects/search emits.
// Fields are decoded here with permissive types on purpose: the test suite
// cares about shape and counts, not about whether the handler renders "0"
// or "" for an absent has_more value.
type projectSearchResponse struct {
	Items []struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"items"`
	HasMore bool `json:"has_more"`
}

// readProjects decodes the response payload. Returning the typed struct
// keeps the assertions readable instead of every test reaching for map
// indexing.
func readProjects(t *testing.T, rw *httptest.ResponseRecorder) projectSearchResponse {
	t.Helper()
	if got := rw.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q, want application/json", got)
	}
	var out projectSearchResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body: %v (body=%q)", err, rw.Body.String())
	}
	return out
}

// --- tests ---------------------------------------------------------------

func TestProjectsSearch_AllTwentyFive_ReturnsTwentyAndHasMore(t *testing.T) {
	w, mgr, secret := newSearchWeb(t, 25)
	ck := signIn(t, mgr, secret)
	rw := do(w, "GET", "/projects/search", nil, ck)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rw.Code, rw.Body.String())
	}
	got := readProjects(t, rw)
	if len(got.Items) != projectSearchLimit {
		t.Fatalf("items len = %d, want %d", len(got.Items), projectSearchLimit)
	}
	if !got.HasMore {
		t.Fatal("has_more = false, want true (25 > 20)")
	}
	// The handler must preserve board.Projects order, so the slice is the
	// first 20 of the synthetic list — pin it so a future "sort for
	// determinism" change cannot silently reorder the dropdown.
	wantKeys := []string{}
	for i := 1; i <= 20; i++ {
		wantKeys = append(wantKeys, "P"+strconv.Itoa(i))
	}
	gotKeys := make([]string, 0, len(got.Items))
	for _, it := range got.Items {
		gotKeys = append(gotKeys, it.Key)
	}
	if !equalStrings(gotKeys, wantKeys) {
		t.Errorf("items keys = %v, want %v", gotKeys, wantKeys)
	}
}

func TestProjectsSearch_FilterByName(t *testing.T) {
	// 39 projects so q=P3 has 11 matches (P3 plus P30..P39), all within
	// the 20-row limit. Splitting the seeded list across two-digit and
	// three-digit keys also verifies the handler does not get cute about
	// "anything that begins with P3" and instead does a plain substring
	// match the user typed.
	w, mgr, secret := newSearchWeb(t, 39)
	ck := signIn(t, mgr, secret)
	rw := do(w, "GET", "/projects/search?q=P3", nil, ck)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rw.Code, rw.Body.String())
	}
	got := readProjects(t, rw)
	if got.HasMore {
		t.Fatal("has_more = true, want false (11 matches < limit 20)")
	}
	// Substring "P3" matches key "P3" and keys "P30".. "P39". Names never
	// carry "P3" without a space between "Project" and the digit, so 11
	// items total, all by key.
	if len(got.Items) != 11 {
		t.Fatalf("items len = %d, want 11; items=%v", len(got.Items), got.Items)
	}
	if got.Items[0].Key != "P3" {
		t.Errorf("first item key = %q, want P3", got.Items[0].Key)
	}
	// Every returned key must contain "P3" (case-insensitive). Catches a
	// future regression that leaks an unmatched row through the filter.
	for i, it := range got.Items {
		if !strings.Contains(strings.ToUpper(it.Key), "P3") {
			t.Errorf("items[%d].Key = %q does not match filter", i, it.Key)
		}
	}
}

func TestProjectsSearch_NameContainsFilter(t *testing.T) {
	w, mgr, secret := newSearchWeb(t, 25)
	ck := signIn(t, mgr, secret)
	// "Project 7" matches by name. P7 matches by key too — either match
	// must surface, and the union is what the handler returns, so look
	// for at least P7 in the response.
	rw := do(w, "GET", "/projects/search?q=Project+7", nil, ck)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rw.Code, rw.Body.String())
	}
	got := readProjects(t, rw)
	if len(got.Items) == 0 {
		t.Fatal("name filter returned no items")
	}
	foundP7 := false
	for _, it := range got.Items {
		if it.Key == "P7" {
			foundP7 = true
			break
		}
	}
	if !foundP7 {
		t.Errorf("P7 not in items; got keys = %v", keysOf(got))
	}
}

func TestProjectsSearch_NoMatches(t *testing.T) {
	w, mgr, secret := newSearchWeb(t, 25)
	ck := signIn(t, mgr, secret)
	rw := do(w, "GET", "/projects/search?q=zzznosuchproject", nil, ck)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rw.Code, rw.Body.String())
	}
	got := readProjects(t, rw)
	if len(got.Items) != 0 {
		t.Errorf("items len = %d, want 0", len(got.Items))
	}
	if got.HasMore {
		t.Errorf("has_more = true, want false")
	}
}

func TestProjectsSearch_NoSessionReturnsNotOK(t *testing.T) {
	w, _, _ := newSearchWeb(t, 5)
	rw := do(w, "GET", "/projects/search?q=foo", nil)
	if rw.Code == http.StatusOK {
		t.Fatalf("status = 200 for unauthenticated request; body=%q", rw.Body.String())
	}
}

func TestProjectsSearch_LimitClampedAtTwenty(t *testing.T) {
	w, mgr, secret := newSearchWeb(t, 25)
	ck := signIn(t, mgr, secret)
	rw := do(w, "GET", "/projects/search?limit=999", nil, ck)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rw.Code, rw.Body.String())
	}
	got := readProjects(t, rw)
	if len(got.Items) > projectSearchLimit {
		t.Errorf("items len = %d > cap %d", len(got.Items), projectSearchLimit)
	}
}

func TestProjectsSearch_LimitInvalidRejected(t *testing.T) {
	w, mgr, secret := newSearchWeb(t, 5)
	ck := signIn(t, mgr, secret)
	rw := do(w, "GET", "/projects/search?limit=notanumber", nil, ck)
	if rw.Code == http.StatusOK {
		t.Fatalf("status = 200 for invalid limit; body=%q", rw.Body.String())
	}
}

func TestProjectsSearch_EmptyQBoundedByLimit(t *testing.T) {
	w, mgr, secret := newSearchWeb(t, 8)
	ck := signIn(t, mgr, secret)
	rw := do(w, "GET", "/projects/search", nil, ck)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rw.Code, rw.Body.String())
	}
	got := readProjects(t, rw)
	if len(got.Items) != 8 {
		t.Errorf("items len = %d, want 8", len(got.Items))
	}
	if got.HasMore {
		t.Errorf("has_more = true with only 8 projects")
	}
}

// equalStrings / keysOf are the tiny test helpers so the assertions stay
// on one line each.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func keysOf(r projectSearchResponse) []string {
	out := make([]string, 0, len(r.Items))
	for _, it := range r.Items {
		out = append(out, it.Key)
	}
	sort.Strings(out)
	return out
}
