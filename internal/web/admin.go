package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// handleAdmin is "GET /admin": tokens (via auth.Manager, which owns token
// storage) and projects (via board_get's summary view — the frozen
// service.Service interface has no dedicated project-listing call, and
// board_get already returns every accessible project with its columns).
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
	model := view.AdminModel{ExportURL: "/admin/export", BackupURL: "/admin/backup"}
	for _, t := range toks {
		model.Tokens = append(model.Tokens, view.AdminToken{
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
	for _, p := range board.Projects {
		var activeTasks int
		for _, c := range p.Columns {
			if c.Kind == domain.KindActive {
				activeTasks += c.Count
			}
		}
		// Version is not reported by board_get (service.BoardProject has no
		// Version field) — the frozen service.Service interface has no call
		// that returns a project's version outside project_upsert's own
		// result. Left at 0; see the task report's deviation note.
		model.Projects = append(model.Projects, view.AdminProject{
			Key: p.Key, Name: p.Name, Columns: len(p.Columns), ActiveTasks: activeTasks, Version: 0,
		})
	}

	page := w.newPage(r.Context(), rw, r, tok, "admin")
	page.Model = model
	w.render(rw, r, http.StatusOK, "page-admin", page)
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
