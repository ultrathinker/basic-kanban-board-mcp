package web

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// adminPageSize is the row cap for each table on /admin. Chosen small
// enough that even an accidental flood of tokens or projects (the auth
// list has no built-in cap) renders quickly, big enough that an admin
// rarely pages in practice. Pager math (ceil division, clamping, bounds)
// lives in view.NewPager so it can be unit-tested without a request.
const adminPageSize = 20

// handleAdmin is "GET /admin": tokens (via auth.Manager, which owns token
// storage) and projects (via board_get's summary view — the frozen
// service.Service interface has no dedicated project-listing call, and
// board_get already returns every accessible project with its columns).
//
// Each grid is server-paginated: ?tok_page=N for tokens, ?proj_page=N for
// projects. The two parameters are independent — paging one does not
// reset the other, and the prev/next links echo the OTHER parameter at
// its current value (the brief: paging tokens must not reset the
// projects page). Garbage or out-of-range page numbers clamp to [1, pageCount]
// so a hand-edited link never 500s.
func (w *Web) handleAdmin(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireSessionPage(rw, r, domain.ScopeAdmin)
	if !ok {
		return
	}
	toks, err := w.d.Auth.Tokens.List(r.Context())
	if err != nil {
		w.pageError(rw, r, err)
		return
	}
	allTokens := make([]view.AdminToken, 0, len(toks))
	for _, t := range toks {
		allTokens = append(allTokens, view.AdminToken{
			Name: t.Name, Scope: formatScopes(t.Scopes), ProjectKeys: formatProjectKeys(t.ProjectKeys),
			CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339), LastUsed: formatLastUsed(t.LastUsedAt),
			Active: t.Active(),
		})
	}
	board, err := w.d.Service.BoardGet(r.Context(), actorFor(tok), service.BoardGetInput{View: service.ViewSummary})
	if err != nil {
		w.pageError(rw, r, err)
		return
	}
	allProjects := make([]view.AdminProject, 0, len(board.Projects))
	for _, p := range board.Projects {
		var activeTasks int
		for _, c := range p.Columns {
			if c.Kind == domain.KindActive {
				activeTasks += c.Count
			}
		}
		allProjects = append(allProjects, view.AdminProject{
			Key: p.Key, Name: p.Name, Columns: len(p.Columns), ActiveTasks: activeTasks,
			Version: p.Version,
		})
	}

	q := r.URL.Query()
	tokPager, tokLo, tokHi := view.NewPager(
		"/admin", "tok_page",
		len(allTokens), adminPageSize, parsePageParam(q.Get("tok_page")),
		preserveOther(q, "tok_page"),
	)
	projPager, projLo, projHi := view.NewPager(
		"/admin", "proj_page",
		len(allProjects), adminPageSize, parsePageParam(q.Get("proj_page")),
		preserveOther(q, "proj_page"),
	)

	model := view.AdminModel{
		ExportURL:     "/admin/export",
		BackupURL:     "/admin/backup",
		TokensPager:   tokPager,
		ProjectsPager: projPager,
	}
	if tokHi > tokLo {
		model.Tokens = allTokens[tokLo:tokHi]
	}
	if projHi > projLo {
		model.Projects = allProjects[projLo:projHi]
	}

	page := w.newPage(r.Context(), rw, r, tok, "admin")
	page.Model = model
	w.render(rw, r, http.StatusOK, "page-admin", page)
}

// parsePageParam reads ?tok_page= / ?proj_page= and clamps the value so a
// hand-edited or typed-in "0", "-3", "abc", "99" never crashes the page
// and never lands outside [1, pageCount] after the pager math. Returns 1
// for missing / empty / unparseable input.
func parsePageParam(raw string) int {
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 1
	}
	if n < 1 {
		return 1
	}
	return n
}

// preserveOther returns every query parameter EXCEPT skip, so the pager
// links carry the OTHER page parameter at its current value. Passing nil
// is safe (url.Values(nil).Get just returns ""), so the result is always
// a usable url.Values the pager can encode back into the URL.
func preserveOther(q url.Values, skip string) url.Values {
	out := url.Values{}
	for k, vs := range q {
		if k == skip {
			continue
		}
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	return out
}

// handleAdminTokenCreate is "POST /admin/tokens".
func (w *Web) handleAdminTokenCreate(rw http.ResponseWriter, r *http.Request) {
	if _, ok := w.requireSessionPage(rw, r, domain.ScopeAdmin); !ok {
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		w.pageError(rw, r, err)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	scope := strings.TrimSpace(r.PostFormValue("scope"))
	if name == "" || strings.ContainsAny(name, " \t\r\n") {
		w.pageError(rw, r, domain.Invalid("name", "token name must be non-empty and contain no whitespace", "Use a short handle like claude@rog."))
		return
	}
	sc := domain.Scope(scope)
	if !sc.Valid() {
		w.pageError(rw, r, domain.Invalid("scope", "scope must be read, write or admin", "Pick one of the three scopes."))
		return
	}
	var projectKeys []string
	if raw := strings.TrimSpace(r.PostFormValue("projects")); raw != "" {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			pk, err := domain.ValidateProjectKey(p)
			if err != nil {
				w.pageError(rw, r, err)
				return
			}
			projectKeys = append(projectKeys, pk)
		}
	}
	newTok := &domain.Token{
		ID:          uuid.NewString(),
		Name:        name,
		Scopes:      domain.Scopes{sc},
		ProjectKeys: projectKeys,
	}
	if _, err := w.d.Auth.MintAndStore(r.Context(), newTok); err != nil {
		w.pageError(rw, r, err)
		return
	}
	http.Redirect(rw, r, "/admin", http.StatusSeeOther)
}

// handleAdminTokenRotate is "POST /admin/tokens/{name}/rotate".
func (w *Web) handleAdminTokenRotate(rw http.ResponseWriter, r *http.Request) {
	if _, ok := w.requireSessionPage(rw, r, domain.ScopeAdmin); !ok {
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		w.pageError(rw, r, err)
		return
	}
	name := r.PathValue("name")
	if _, err := w.d.Auth.Rotate(r.Context(), name); err != nil {
		w.pageError(rw, r, err)
		return
	}
	http.Redirect(rw, r, "/admin", http.StatusSeeOther)
}

// handleAdminTokenRevoke is "POST /admin/tokens/{name}/revoke".
func (w *Web) handleAdminTokenRevoke(rw http.ResponseWriter, r *http.Request) {
	if _, ok := w.requireSessionPage(rw, r, domain.ScopeAdmin); !ok {
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		w.pageError(rw, r, err)
		return
	}
	name := r.PathValue("name")
	if err := w.d.Auth.Tokens.Revoke(r.Context(), name); err != nil {
		w.pageError(rw, r, err)
		return
	}
	http.Redirect(rw, r, "/admin", http.StatusSeeOther)
}

// handleAdminExport is "GET /admin/export": every accessible project, full
// detail, as one JSON document.
func (w *Web) handleAdminExport(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireSessionPage(rw, r, domain.ScopeAdmin)
	if !ok {
		return
	}
	if err := w.verifyCSRF(r); err != nil {
		w.pageError(rw, r, err)
		return
	}
	board, err := w.d.Service.BoardGet(r.Context(), actorFor(tok), service.BoardGetInput{
		View: service.ViewTasks, DoneLimit: domain.MaxDoneLimit,
		Include: service.Includes{service.IncludeBody, service.IncludeAcceptance, service.IncludeLinks, service.IncludeMetadata},
	})
	if err != nil {
		apiError(rw, err)
		return
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.Header().Set("Content-Disposition", `attachment; filename="kanban-export.json"`)
	writeJSONIndent(rw, board)
}

// handleAdminBackup is "POST /admin/backup": a VACUUM INTO copy of the
// database under BackupDir, then back to /admin. It is a POST behind the
// CSRF gate, never a GET: the route writes to disk and runs a VACUUM on
// every call, and a GET would let any foreign page spray both at the server
// via an <img src="/admin/backup"> tag (independent review #10).
func (w *Web) handleAdminBackup(rw http.ResponseWriter, r *http.Request) {
	if _, ok := w.requireSessionPage(rw, r, domain.ScopeAdmin); !ok {
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		w.pageError(rw, r, err)
		return
	}
	// Nothing at startup creates the backups directory (the composition root
	// creates the data dir only), and VACUUM INTO refuses to open a file in a
	// missing directory — so a clean install's very first backup click used
	// to 500. Creating it here, right before the backup, keeps that first
	// click working without giving this package a startup side effect.
	dir := w.d.BackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		w.pageError(rw, r, fmt.Errorf("web: create backup directory %s: %w", dir, err))
		return
	}
	dest := filepath.Join(dir, time.Now().UTC().Format("20060102-150405")+".db")
	if err := w.d.Backup(r.Context(), dest); err != nil {
		w.pageError(rw, r, err)
		return
	}
	http.Redirect(rw, r, "/admin", http.StatusSeeOther)
}

func formatScopes(ss domain.Scopes) string {
	if len(ss) == 0 {
		return "-"
	}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, string(s))
	}
	return strings.Join(out, ",")
}

func formatProjectKeys(keys []string) string {
	if len(keys) == 0 {
		return "*"
	}
	return strings.Join(keys, ",")
}

func formatLastUsed(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
