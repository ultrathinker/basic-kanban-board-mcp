package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// adminPagerSvc is a controllable service stub: every test decides how many
// projects the admin page sees and what their keys are. Only BoardGet is
// called by handleAdmin, so the rest of the interface keeps the cheap
// "service unavailable" return — that catches any future handler that
// reaches for something else.
type adminPagerSvc struct {
	projects []service.BoardProject
}

func (s adminPagerSvc) BoardGet(_ context.Context, _ service.Actor, _ service.BoardGetInput) (*service.Board, error) {
	return &service.Board{Projects: s.projects}, nil
}

func (adminPagerSvc) TaskNext(context.Context, service.Actor, service.TaskNextInput) (*service.NextResult, error) {
	return nil, ErrServiceUnavailable
}
func (adminPagerSvc) TaskGet(context.Context, service.Actor, service.TaskGetInput) (*service.TaskGetResult, error) {
	return nil, ErrServiceUnavailable
}
func (adminPagerSvc) TaskCreate(context.Context, service.Actor, service.TaskCreateInput) (*service.TaskCreateResult, error) {
	return nil, ErrServiceUnavailable
}
func (adminPagerSvc) TaskUpdate(context.Context, service.Actor, service.TaskUpdateInput) (*service.TaskUpdateResult, error) {
	return nil, ErrServiceUnavailable
}
func (adminPagerSvc) TaskLink(context.Context, service.Actor, service.TaskLinkInput) (*service.TaskLinkResult, error) {
	return nil, ErrServiceUnavailable
}
func (adminPagerSvc) TaskClaim(context.Context, service.Actor, service.TaskClaimInput) (*service.TaskClaimResult, error) {
	return nil, ErrServiceUnavailable
}
func (adminPagerSvc) TaskRemove(context.Context, service.Actor, service.TaskRemoveInput) (*service.TaskRemoveResult, error) {
	return nil, ErrServiceUnavailable
}
func (adminPagerSvc) ProjectUpsert(context.Context, service.Actor, service.ProjectUpsertInput) (*service.ProjectUpsertResult, error) {
	return nil, ErrServiceUnavailable
}

// adminPagerHarness bundles the wired Web together with the freshly
// minted admin secret. Tests need both: the Web to issue requests, the
// secret to forge a session cookie against. Captured inline at
// construction because the manager never exposes the secret again — the
// only honest copy lives in the row's hashed form.
type adminPagerHarness struct {
	w      *Web
	secret string
}

// newAdminPagerWeb builds a Web with the given number of tokens (raw
// inserts into the in-memory store) and projects (served by a controllable
// service stub). Both lists are stable across calls so page 2 is the
// deterministic continuation of page 1.
//
// Token-management goes through MintAndStore, not a raw Create with a
// fake hash: the manager's Bootstrap path validates the supplied secret
// against the stored hash, so a pre-populating "admin" row whose hash
// disagrees with a static secret would refuse to start up. Bootstrapping
// first (with empty supplied) and capturing the freshly minted secret
// keeps the test self-contained — no string constant shared with anyone
// else, no cross-test contamination.
func newAdminPagerWeb(t *testing.T, tokenCount, projectCount int) adminPagerHarness {
	t.Helper()
	tpl, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	store := &authMemTokenStore{}
	mgr := auth.NewManager(store, &authMemSessionStore{}, nil, "", true, nil)
	secret, err := mgr.Bootstrap(context.Background(), "")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if secret == "" {
		// Bootstrap returns "" once tokens already exist — a development-mode
		// shortcut. It cannot happen here because the in-memory store starts
		// empty, but pin the assumption rather than discover it via a
		// confusing "session: unauthorized" later.
		t.Fatal("Bootstrap returned no secret; manager state is not what the test expected")
	}
	for i := 1; i <= tokenCount; i++ {
		name := "tok-" + strconv.Itoa(i)
		if _, err := mgr.MintAndStore(context.Background(), &domain.Token{
			ID:          uuid.NewString(),
			Name:        name,
			Scopes:      domain.Scopes{domain.ScopeWrite},
			ProjectKeys: []string{"BMB"},
		}); err != nil {
			t.Fatalf("MintAndStore(%s): %v", name, err)
		}
	}
	projs := make([]service.BoardProject, 0, projectCount)
	for i := 1; i <= projectCount; i++ {
		projs = append(projs, service.BoardProject{
			Key: "P" + strconv.Itoa(i), Name: "Project " + strconv.Itoa(i), Version: i,
		})
	}
	w := New(Deps{
		Service:   adminPagerSvc{projects: projs},
		Auth:      mgr,
		Bus:       events.New(newEmptyHistory(), 4),
		Templates: tpl,
		BaseURL:   "http://127.0.0.1:0",
	})
	return adminPagerHarness{w: w, secret: secret}
}

// adminSession signs in as the admin token and returns the cookie so a
// GET /admin does not redirect to /login.
func (h adminPagerHarness) adminSession(t *testing.T) *http.Cookie {
	t.Helper()
	sess, err := h.w.d.Auth.CreateSession(context.Background(), h.secret)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sessionCookie(sess)
}

// rowCountTokens / rowCountProjects count <tr> entries inside the tbody of
// the first / second table on the page. The admin page renders two tables
// in source order — Tokens first, Projects second — so the indices are
// stable. Returned count ignores any "No tokens." colspan row, since the
// pagination brief is about non-empty tables.
func rowCountTokens(t *testing.T, body string) int {
	t.Helper()
	return tableRows(t, body, 0)
}
func rowCountProjects(t *testing.T, body string) int {
	t.Helper()
	return tableRows(t, body, 1)
}

// tableRows finds the n-th <table> block, then counts <tr> children of its
// <tbody> that are NOT the colspan fallback row. Used to pin "page 1 has 20
// row entries, page 2 has 5".
func tableRows(t *testing.T, body string, tableIdx int) int {
	t.Helper()
	tablePositions := findAll(body, "<table")
	if tableIdx >= len(tablePositions) {
		t.Fatalf("only %d tables in body, asked for index %d", len(tablePositions), tableIdx)
	}
	rest := body[tablePositions[tableIdx]:]
	tbodyStart := strings.Index(rest, "<tbody>")
	if tbodyStart < 0 {
		t.Fatalf("no <tbody> in table %d", tableIdx)
	}
	rest = rest[tbodyStart:]
	tbodyEnd := strings.Index(rest, "</tbody>")
	if tbodyEnd < 0 {
		t.Fatalf("no </tbody> in table %d", tableIdx)
	}
	tbody := rest[len("<tbody>"):tbodyEnd]
	trs := splitOnTag(tbody, "<tr")
	count := 0
	for _, raw := range trs {
		// Skip the fallback "No tokens." colspan row — it has colspan="7"
		// and is rendered only when the table is empty.
		if strings.Contains(raw, "colspan=") {
			continue
		}
		count++
	}
	return count
}

// findAll returns every offset of needle in s. The implementation must
// keep returning ABSOLUTE positions in the original string, not relative
// positions in a shrinking suffix — the consumers all index into the
// original body, and a "position 2108 inside a 12609-byte body" silently
// slices the wrong region when there is more than one match. The body
// always has a small, bounded number of tables; the cost is irrelevant.
func findAll(s, needle string) []int {
	out := []int{}
	cursor := 0
	for {
		i := strings.Index(s[cursor:], needle)
		if i < 0 {
			return out
		}
		out = append(out, cursor+i)
		cursor += i + len(needle)
		if cursor >= len(s) {
			return out
		}
	}
}

// splitOnTag splits s into pieces every time openTag appears at offset 0
// of the remainder — close enough to "find every tr" without pulling in a
// dependency.
func splitOnTag(s, openTag string) []string {
	var out []string
	rest := s
	for {
		i := strings.Index(rest, openTag)
		if i < 0 {
			return out
		}
		out = append(out, rest[i:])
		rest = rest[i+len(openTag):]
	}
}

// hasPagerUnder counts "<nav class=\"pager\"" occurrences and tells which
// table the nth one belongs to. Order matches page-admin's source order:
// tokens-pager first, projects-pager second. The brief: show the pager
// controls only when there are more than 20 rows — when total<=20 there must
// be zero pagers of each kind.
func pagerAnchors(body string) []string {
	re := regexp.MustCompile(`<nav class="pager"[^>]*aria-label="([^"]+)"`)
	return re.FindAllString(body, -1)
}

// pagerLink reads the href rendered into a <nav class="pager"> block's
// prev/next anchors. Used to assert that the URL preserves the OTHER page
// parameter (so paging tokens doesn't reset proj_page and vice versa).
func pagerHrefs(t *testing.T, body, label string) (prev, next string) {
	t.Helper()
	re := regexp.MustCompile(`<nav class="pager"[^>]*aria-label="` + regexp.QuoteMeta(label) + `"(?s:.*?)</nav>`)
	blk := re.FindString(body)
	if blk == "" {
		t.Fatalf("no pager block with aria-label %q in body", label)
	}
	hrefRe := regexp.MustCompile(`href="([^"]+)"`)
	for _, m := range hrefRe.FindAllStringSubmatch(blk, -1) {
		switch {
		case strings.Contains(blk[:strings.Index(blk, m[0])], "rel=\"prev\""):
			prev = m[1]
		case strings.Contains(blk[:strings.Index(blk, m[0])], "rel=\"next\""):
			next = m[1]
		}
	}
	return prev, next
}

// adminBody issues a GET /admin with the given query string and returns
// the rendered HTML. status is asserted 200 — anything else is a wiring
// failure, not the thing the test wants to pin.
func adminBody(t *testing.T, h adminPagerHarness, query string) string {
	t.Helper()
	ck := h.adminSession(t)
	path := "/admin"
	if query != "" {
		path = "/admin?" + query
	}
	rw := do(h.w, "GET", path, nil, ck)
	if rw.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%q", path, rw.Code, trim(rw.Body.String(), 240))
	}
	return rw.Body.String()
}

// ---------------------------------------------------------------------------
// view.NewPager unit tests — the brief calls for pure pager math, kept
// separate from the handler so future pages can reuse it.
// ---------------------------------------------------------------------------

func TestNewPager_BoundsAndClamp(t *testing.T) {
	cases := []struct {
		name     string
		total    int
		pageSize int
		page     int
		wantPage int
		wantPC   int
		wantLoHi [2]int
	}{
		{"empty table is one page", 0, 20, 1, 1, 1, [2]int{0, 0}},
		{"single page under cap", 7, 20, 1, 1, 1, [2]int{0, 7}},
		{"two pages first", 20, 20, 1, 1, 1, [2]int{0, 20}},
		{"two pages second", 25, 20, 2, 2, 2, [2]int{20, 25}},
		{"out of range clamps down", 25, 20, 99, 2, 2, [2]int{20, 25}},
		{"zero clamps to one", 25, 20, 0, 1, 2, [2]int{0, 20}},
		{"negative clamps to one", 25, 20, -3, 1, 2, [2]int{0, 20}},
		{"exact multiple", 40, 20, 2, 2, 2, [2]int{20, 40}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, lo, hi := view.NewPager("/admin", "tok_page", tc.total, tc.pageSize, tc.page, nil)
			if p.Page != tc.wantPage || p.PageCount != tc.wantPC {
				t.Fatalf("page/pageCount = (%d,%d), want (%d,%d)", p.Page, p.PageCount, tc.wantPage, tc.wantPC)
			}
			if lo != tc.wantLoHi[0] || hi != tc.wantLoHi[1] {
				t.Fatalf("slice bounds = [%d,%d), want [%d,%d)", lo, hi, tc.wantLoHi[0], tc.wantLoHi[1])
			}
		})
	}
}

// TestNewPager_PreservesOtherParams is the URL-shape half of the brief:
// the prev/next anchors must carry the OTHER parameter, so paging one
// table does not reset the other.
func TestNewPager_PreservesOtherParams(t *testing.T) {
	keep := url.Values{}
	keep.Set("proj_page", "1")

	p, _, _ := view.NewPager("/admin", "tok_page", 25, 20, 1, keep)
	if !strings.Contains(p.NextURL, "proj_page=1") {
		t.Fatalf("NextURL lost the proj_page carry-over: %s", p.NextURL)
	}
	// Page 1: Prev is rendered as the inert <span> in the template, so
	// PrevURL's contents do not matter for the user — but it must not
	// smuggle a stale tok_page=0 into the URL.
	if strings.Contains(p.PrevURL, "tok_page=") {
		t.Fatalf("PrevURL should not carry tok_page when already on page 1: %s", p.PrevURL)
	}
	// Page 1: HasPrev is false (the template renders the inert <span>),
	// so PrevURL is allowed to be empty.
	if p.HasPrev {
		t.Fatalf("page 1 has HasPrev=true")
	}

	// Page 2 of 2 with size 20 / total 25 IS the last page: HasPrev=true
	// and HasNext=false. The user sees the inert Next span; this asserts
	// the math, not the markup.
	p2, _, _ := view.NewPager("/admin", "tok_page", 25, 20, 2, keep)
	if !p2.HasPrev || p2.HasNext {
		t.Fatalf("page 2 (last) HasPrev/HasNext = (%v,%v), want true,false", p2.HasPrev, p2.HasNext)
	}
	if !strings.Contains(p2.PrevURL, "tok_page=1") || !strings.Contains(p2.PrevURL, "proj_page=1") {
		t.Fatalf("PrevURL missing carry-over: %s", p2.PrevURL)
	}

	// Carrying on to a hypothetical page 3 (clamped back to 2). The
	// handler can also be exercised against a wider set: 41 tokens →
	// 3 pages, page 2 in the middle has both HasPrev and HasNext.
	mid, _, _ := view.NewPager("/admin", "tok_page", 41, 20, 2, keep)
	if !mid.HasPrev || !mid.HasNext {
		t.Fatalf("middle page HasPrev/HasNext = (%v,%v), want true,true", mid.HasPrev, mid.HasNext)
	}
	if !strings.Contains(mid.PrevURL, "tok_page=1") {
		t.Fatalf("PrevURL on middle page must lead back to page 1: %s", mid.PrevURL)
	}
	if !strings.Contains(mid.NextURL, "tok_page=3") || !strings.Contains(mid.NextURL, "proj_page=1") {
		t.Fatalf("NextURL on middle page must lead forward AND carry proj_page: %s", mid.NextURL)
	}

	last, _, _ := view.NewPager("/admin", "tok_page", 41, 20, 3, keep)
	if !last.HasPrev || last.HasNext {
		t.Fatalf("last page HasPrev/HasNext = (%v,%v), want true,false", last.HasPrev, last.HasNext)
	}
	if !strings.Contains(last.PrevURL, "tok_page=2") || !strings.Contains(last.PrevURL, "proj_page=1") {
		t.Fatalf("PrevURL on last page must keep both parameters: %s", last.PrevURL)
	}
}

// ---------------------------------------------------------------------------
// /admin pagination: tokens
// ---------------------------------------------------------------------------

// TestAdminPagination_Tokens25_Page1 covers the steady-state happy path:
// 25 tokens (plus the bootstrap admin row → 26 total), page 1 must render
// the first 20 rows and offer a Next link.
func TestAdminPagination_Tokens25_Page1(t *testing.T) {
	h := newAdminPagerWeb(t, 25, 0)
	body := adminBody(t, h, "")
	if got := rowCountTokens(t, body); got != 20 {
		t.Fatalf("page 1 tokens rows = %d, want 20", got)
	}
	if !strings.Contains(body, ">Next →<") {
		t.Fatalf("page 1 missing Next → link; body tail=%q", trim(body, 200))
	}
	if !strings.Contains(body, "<span aria-disabled=\"true\">← Prev</span>") {
		t.Fatalf("page 1 should render inert Prev span, not an active link")
	}
	anchors := pagerAnchors(body)
	if len(anchors) != 1 {
		t.Fatalf("expected 1 pager block on /admin (tokens only), got %d", len(anchors))
	}
}

// TestAdminPagination_Tokens25_Page2 covers the second page: 26 - 20 = 6
// remaining rows, Prev becomes a link, Next becomes the inert span (last
// page).
//
// The "last token" assertion names the alphabetically-first token that
// falls on page 2 (tok-4 in lexicographic name order: "tok-1", "tok-10"…
// "tok-19", "tok-2", "tok-20"… "tok-25", "tok-3"… "tok-9" sort into one
// roll; the first 20 entries form page 1, the next 6 — tok-4 … tok-9 —
// form page 2). The assertion pins "the row I expect on page 2 is there",
// not the integer suffix.
func TestAdminPagination_Tokens25_Page2(t *testing.T) {
	h := newAdminPagerWeb(t, 25, 0)
	body := adminBody(t, h, "tok_page=2")
	if got := rowCountTokens(t, body); got != 6 {
		t.Fatalf("page 2 tokens rows = %d, want 6 (26 total - first page's 20)", got)
	}
	if !strings.Contains(body, "tok-4") {
		t.Fatalf("page 2 must include its first row (tok-4 in lex order)")
	}
	if !strings.Contains(body, "tok-9") {
		t.Fatalf("page 2 must include its last row (tok-9 in lex order)")
	}
	if !strings.Contains(body, "<span aria-disabled=\"true\">Next →</span>") {
		t.Fatalf("last page must render inert Next; not found")
	}
	if !strings.Contains(body, "rel=\"prev\"") {
		t.Fatalf("page 2 must offer a real Prev link")
	}
}

// TestAdminPagination_Tokens25_OutOfRangeClamps: ?tok_page=99 lands on the
// last non-empty page (the brief: clamp page so that ?tok_page=99 lands
// on the last non-empty page).
func TestAdminPagination_Tokens25_OutOfRangeClamps(t *testing.T) {
	h := newAdminPagerWeb(t, 25, 0)
	body := adminBody(t, h, "tok_page=99")
	if got := rowCountTokens(t, body); got != 6 {
		t.Fatalf("clamped page rows = %d, want 6 (the last page's remainder)", got)
	}
	if !strings.Contains(body, "<span aria-disabled=\"true\">Next →</span>") {
		t.Fatalf("clamp landed on last page so Next must be inert")
	}
}

// TestAdminPagination_Tokens25_GarbageFallsToPage1 covers "garbage falls
// back to page 1": non-numeric and zero page values must never 500, never
// panic, never render the empty-table colspan row.
func TestAdminPagination_Tokens25_GarbageFallsToPage1(t *testing.T) {
	h := newAdminPagerWeb(t, 25, 0)
	for _, q := range []string{"tok_page=0", "tok_page=-3", "tok_page=abc", "tok_page="} {
		t.Run(q, func(t *testing.T) {
			body := adminBody(t, h, q)
			if got := rowCountTokens(t, body); got != 20 {
				t.Fatalf("garbage input rendered %d rows, want 20 (page 1)", got)
			}
		})
	}
}

// TestAdminPagination_LTE20_HidesControls is the brief's "≤20 → no
// pager" rule. Pairs tokenCount with the +1 the bootstrap row adds, so
// the assertion pins "PageCount==1 ⇒ no <nav class=pager>".
func TestAdminPagination_LTE20_HidesControls(t *testing.T) {
	// tokenCount + 1 (bootstrap) lands at <= 20
	cases := []struct {
		minted int
	}{
		{0}, {5}, {19},
	}
	for _, tc := range cases {
		name := "tokens=" + strconv.Itoa(tc.minted) + "(+admin=<=20)"
		t.Run(name, func(t *testing.T) {
			h := newAdminPagerWeb(t, tc.minted, 0)
			body := adminBody(t, h, "")
			if anchors := pagerAnchors(body); len(anchors) != 0 {
				t.Fatalf("got %d pager blocks, want 0 (PageCount==1)", len(anchors))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// /admin pagination: projects
// ---------------------------------------------------------------------------

func TestAdminPagination_Projects25_Page1(t *testing.T) {
	h := newAdminPagerWeb(t, 0, 25)
	body := adminBody(t, h, "")
	if got := rowCountProjects(t, body); got != 20 {
		t.Fatalf("page 1 projects rows = %d, want 20", got)
	}
	if !strings.Contains(body, "aria-label=\"Projects pages\"") {
		t.Fatalf("page 1 missing Projects pager; body tail=%q", trim(body, 200))
	}
}

func TestAdminPagination_Projects25_Page2(t *testing.T) {
	h := newAdminPagerWeb(t, 0, 25)
	body := adminBody(t, h, "proj_page=2")
	if got := rowCountProjects(t, body); got != 5 {
		t.Fatalf("page 2 projects rows = %d, want 5", got)
	}
	if !strings.Contains(body, "P25") {
		t.Fatalf("page 2 must include the last project")
	}
}

func TestAdminPagination_Projects25_OutOfRangeClamps(t *testing.T) {
	h := newAdminPagerWeb(t, 0, 25)
	body := adminBody(t, h, "proj_page=42")
	if got := rowCountProjects(t, body); got != 5 {
		t.Fatalf("clamped projects page rows = %d, want 5", got)
	}
}

// ---------------------------------------------------------------------------
// independence: paging one grid does NOT reset the other
// ---------------------------------------------------------------------------

// TestAdminPagination_IndependentParams is the independence proof: the
// parameters must not bleed into each other, and the rendered prev/next
// href must carry the OTHER parameter at its current value.
//
// 25 minted tokens + the bootstrap admin row = 26 → two pages of tokens
// (20 + 6). 25 projects → two pages of projects (20 + 5). Each side is
// paginated independently, so asking for ?tok_page=2&proj_page=1 gives
// the second slice of tokens with the first slice of projects.
func TestAdminPagination_IndependentParams(t *testing.T) {
	h := newAdminPagerWeb(t, 25, 25)
	body := adminBody(t, h, "tok_page=2&proj_page=1")

	// Tokens rendered are the second-page slice (26 - 20 = 6 rows).
	if got := rowCountTokens(t, body); got != 6 {
		t.Fatalf("tokens page 2 rows = %d, want 6; body tail=%q", got, trim(body, 200))
	}
	// Projects are still on their first page (>= 20 rows; full first page).
	if got := rowCountProjects(t, body); got != 20 {
		t.Fatalf("projects page 1 rows = %d, want 20; body tail=%q", got, trim(body, 200))
	}

	prevTok, _ := pagerHrefs(t, body, "Tokens pages")
	// On page 2 with 26 tokens and size 20, Prev is real (page 1) and Next
	// is inert (page 2 IS the last page here). The assertion is on Prev:
	// it must round-trip back to page 1 AND carry the other page parameter.
	if prevTok == "" {
		t.Fatalf("tokens Prev must be a real link on page 2 (26 tokens, pageSize 20)")
	}
	if !strings.Contains(prevTok, "tok_page=1") {
		t.Fatalf("tokens Prev must keep tok_page=1: %s", prevTok)
	}
	if !strings.Contains(prevTok, "proj_page=1") {
		t.Fatalf("tokens Prev lost the proj_page carry-over: %s", prevTok)
	}

	// Projects pager on page 1: only Next is a real link, and it must
	// carry proj_page=2 while preserving tok_page=2.
	_, nextProj := pagerHrefs(t, body, "Projects pages")
	if nextProj == "" {
		t.Fatalf("projects pager on page 1 must have a Next href")
	}
	if !strings.Contains(nextProj, "proj_page=2") {
		t.Fatalf("projects Next must advance the projects page: %s", nextProj)
	}
	if !strings.Contains(nextProj, "tok_page=2") {
		t.Fatalf("projects Next lost tok_page=2: %s", nextProj)
	}

	// Render once more, this time with both on page 2 (still legal — each
	// parameter is read independently). The pager math should agree:
	// projects page 2 = 5 (25 total - page 1's 20).
	body2 := adminBody(t, h, "tok_page=2&proj_page=2")
	if got := rowCountProjects(t, body2); got != 5 {
		t.Fatalf("proj_page=2 rows = %d, want 5; body tail=%q", got, trim(body2, 200))
	}
}

// TestAdminPagination_EmptyHiddenNoPager pins the "short session" path:
// zero tokens + zero projects, no pager, but the page still renders.
func TestAdminPagination_EmptyHiddenNoPager(t *testing.T) {
	h := newAdminPagerWeb(t, 0, 0)
	body := adminBody(t, h, "")
	if anchors := pagerAnchors(body); len(anchors) != 0 {
		t.Fatalf("empty admin page rendered %d pager blocks, want 0", len(anchors))
	}
}

// ---------------------------------------------------------------------------
// /admin still requires auth — sanity check that the new flow did not
// drop the session gate.
// ---------------------------------------------------------------------------

func TestAdminPagination_AnonymousRedirectsToLogin(t *testing.T) {
	h := newAdminPagerWeb(t, 5, 5)
	rw := do(h.w, "GET", "/admin?tok_page=2", nil)
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("anon: status=%d, want 303; body=%q", rw.Code, trim(rw.Body.String(), 200))
	}
	if loc := rw.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("anon: Location=%q, want /login…", loc)
	}
}

// ---------------------------------------------------------------------------
// httptest plumbing kept local so the tests read top-down.
// ---------------------------------------------------------------------------
