package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// formatEstimateLabel renders an estimate number + unit as the compact
// "2h"/"30m" string the drawer pill shows. It mirrors internal/web/view's
// own formatEstimate logic for the integer case (view.formatEstimate is
// unexported, so we cannot import it; this stays in sync by convention).
func formatEstimateLabel(n float64, unit string) string {
	if n == float64(int(n)) {
		return fmt.Sprintf("%d%s", int(n), unit)
	}
	return fmt.Sprintf("%.1f%s", n, unit)
}

// handleOverview is "/": all accessible projects, counts, focus per project.
// "?p=KEY" (the project-switcher's plain <select> GET form) redirects to the
// board instead of rendering here, so the switcher works with JavaScript off.
func (w *Web) handleOverview(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireSessionPage(rw, r, domain.ScopeRead)
	if !ok {
		return
	}
	if p := strings.TrimSpace(r.URL.Query().Get("p")); p != "" {
		key, err := domain.ValidateProjectKey(p)
		if err != nil {
			w.pageError(rw, r, err)
			return
		}
		http.Redirect(rw, r, "/p/"+key, http.StatusSeeOther)
		return
	}

	board, err := w.d.Service.BoardGet(r.Context(), actorFor(tok), service.BoardGetInput{View: service.ViewSummary})
	if err != nil {
		w.pageError(rw, r, err)
		return
	}

	model := view.OverviewModel{}
	for _, p := range board.Projects {
		var active, backlog, done int
		for _, c := range p.Columns {
			switch c.Kind {
			case domain.KindActive:
				active += c.Count
			case domain.KindBacklog:
				backlog += c.Count
			case domain.KindDone:
				done += c.Count
			}
		}
		model.Projects = append(model.Projects, view.OverviewProject{
			Key: p.Key, Name: p.Name, Focus: p.FocusKey,
			Active: active, Backlog: backlog, Done: done,
			URL: "/p/" + p.Key,
		})
		model.Total.Active += active
		model.Total.Backlog += backlog
		model.Total.Done += done
	}
	model.Total.Projects = len(board.Projects)

	page := w.newPage(r.Context(), rw, r, tok, "overview")
	page.Model = model
	w.render(rw, r, http.StatusOK, "page-overview", page)
}

// handleBoard is "/p/{key}".
func (w *Web) handleBoard(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireSessionPage(rw, r, domain.ScopeRead)
	if !ok {
		return
	}
	key, err := domain.ValidateProjectKey(r.PathValue("key"))
	if err != nil {
		w.pageError(rw, r, err)
		return
	}
	hideDone := r.URL.Query().Get("hide_done") != "0"
	doneLimit := 0
	if !hideDone {
		doneLimit = domain.MaxDoneLimit
	}

	board, err := w.d.Service.BoardGet(r.Context(), actorFor(tok), service.BoardGetInput{
		ProjectKey: key, View: service.ViewTasks, DoneLimit: doneLimit,
	})
	if err != nil {
		w.pageError(rw, r, err)
		return
	}
	if len(board.Projects) == 0 {
		w.pageError(rw, r, domain.NotFound("project", key))
		return
	}
	model := buildBoardModel(board.Projects[0], hideDone)

	page := w.newPage(r.Context(), rw, r, tok, "board")
	page.Title = board.Projects[0].Name + " · basic-kanban-board-mcp"
	page.Model = model
	w.render(rw, r, http.StatusOK, "page-board", page)
}

// handleDrawer is "/t/{key}": the full task detail view.
func (w *Web) handleDrawer(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireSessionPage(rw, r, domain.ScopeRead)
	if !ok {
		return
	}
	key, err := domain.NormalizeTaskKey(r.PathValue("key"))
	if err != nil {
		w.pageError(rw, r, err)
		return
	}
	res, err := w.d.Service.TaskGet(r.Context(), actorFor(tok), service.TaskGetInput{
		Keys:    []string{key},
		Include: service.Includes{service.IncludeBody, service.IncludeAcceptance, service.IncludeLinks, service.IncludeNotes, service.IncludeMetadata},
	})
	if err != nil {
		w.pageError(rw, r, err)
		return
	}
	if len(res.NotFound) > 0 || len(res.Tasks) == 0 {
		w.pageError(rw, r, domain.NotFound("task", key))
		return
	}
	t := res.Tasks[0]

	refKeys := append(append([]string{}, t.BlockedBy...), t.Blocks...)
	var blockers, blocking []view.TaskRef
	if len(refKeys) > 0 {
		refRes, err := w.d.Service.TaskGet(r.Context(), actorFor(tok), service.TaskGetInput{Keys: refKeys})
		if err == nil {
			byKey := map[string]view.TaskRef{}
			for _, ref := range taskRefsFor(refRes) {
				byKey[strings.ToUpper(ref.Key)] = ref
			}
			for _, k := range t.BlockedBy {
				if ref, ok := byKey[strings.ToUpper(k)]; ok {
					blockers = append(blockers, ref)
				}
			}
			for _, k := range t.Blocks {
				if ref, ok := byKey[strings.ToUpper(k)]; ok {
					blocking = append(blocking, ref)
				}
			}
		}
	}

	canEdit := tok.Scopes.Has(domain.ScopeWrite)
	model := view.DrawerModel{
		Project: view.ProjectSummary{Key: t.ProjectKey},
		Task: view.DrawerTask{
			Key: t.Key, Title: t.Title, Body: t.Body, Type: t.Type, Priority: t.Priority,
			Tags: t.Tags, Assignee: derefStr(t.Assignee), Version: t.Version,
			CreatedAt: t.CreatedAt.Format(time.RFC3339), CreatedBy: t.CreatedBy,
			UpdatedAt: t.UpdatedAt.Format(time.RFC3339), UpdatedBy: t.UpdatedBy,
			Outcome: t.Outcome, Conclusion: t.Conclusion,
		},
		Blockers:     blockers,
		Blocking:     blocking,
		MarkdownHTML: markdownRender(t.Body),
		CanEdit:      canEdit,
		IsAdmin:      tok.Scopes.Has(domain.ScopeAdmin),
	}
	if t.DueAt != nil {
		model.Task.Due = t.DueAt.Format(time.RFC3339)
	}
	if t.Estimate != nil {
		model.Task.Estimate = &view.EstimateView{N: *t.Estimate, Unit: "h", Label: formatEstimateLabel(*t.Estimate, "h")}
	}
	for i, a := range t.Acceptance {
		model.Acceptance = append(model.Acceptance, view.AcceptanceView{Index: i, Text: a.Text, Done: a.Done})
	}
	for _, n := range t.Notes {
		model.Notes = append(model.Notes, view.DrawerNote{ID: n.ID, Author: n.Author, Body: n.Body, CreatedAt: n.CreatedAt.Format(time.RFC3339)})
	}

	page := w.newPage(r.Context(), rw, r, tok, "board")
	page.Title = t.Key + " · basic-kanban-board-mcp"
	page.Model = model
	w.render(rw, r, http.StatusOK, "page-drawer", page)
}

// handleActivity is "/p/{key}/activity": an initial snapshot rendered
// server-side, followed by the live SSE feed the page connects to itself.
func (w *Web) handleActivity(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireSessionPage(rw, r, domain.ScopeRead)
	if !ok {
		return
	}
	key, err := domain.ValidateProjectKey(r.PathValue("key"))
	if err != nil {
		w.pageError(rw, r, err)
		return
	}
	if !tok.MayAccessProject(key) {
		w.pageError(rw, r, domain.Forbidden("this token cannot access project "+key, "Use a token whose project_keys include "+key+"."))
		return
	}
	projectID, found, err := w.d.ProjectLookup.ProjectIDForKey(r.Context(), key)
	if err != nil {
		w.pageError(rw, r, err)
		return
	}
	if !found {
		w.pageError(rw, r, domain.NotFound("project", key))
		return
	}

	// A failed history read must not render as a confident empty feed.
	events, err := w.snapshotEvents(projectID, 50)
	if err != nil {
		w.pageError(rw, r, err)
		return
	}
	model := view.ActivityModel{Project: view.ProjectSummary{Key: key}}
	now := w.d.Now()
	for _, e := range events {
		ae := view.ActivityEvent{
			ID: e.ID, Actor: e.Actor, Verb: eventVerb(e.Type),
			Target: eventPayloadString(e, "key"), TargetTitle: eventPayloadString(e, "title"),
			TS: e.TS,
		}
		ae.RelTime = relTimeFor(ae.TS, now)
		model.Events = append(model.Events, ae)
	}

	page := w.newPage(r.Context(), rw, r, tok, "activity")
	page.Title = "Activity · " + key
	page.Model = model
	w.render(rw, r, http.StatusOK, "page-activity", page)
}

// handleProjectExport is "/p/{key}/export": a full JSON dump of one
// project's board, built from board_get with every include on. It is not
// the compact grammar (that is the MCP surface's job) — just a convenience
// download for the "Export" link on the board page.
func (w *Web) handleProjectExport(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireSessionPage(rw, r, domain.ScopeRead)
	if !ok {
		return
	}
	// Reached as a POST form so it can carry a CSRF token; an anchor cannot.
	if err := w.verifyCSRF(r); err != nil {
		w.pageError(rw, r, err)
		return
	}
	key, err := domain.ValidateProjectKey(r.PathValue("key"))
	if err != nil {
		apiError(rw, err)
		return
	}
	board, err := w.d.Service.BoardGet(r.Context(), actorFor(tok), service.BoardGetInput{
		ProjectKey: key, View: service.ViewTasks, DoneLimit: domain.MaxDoneLimit,
		Include: service.Includes{service.IncludeBody, service.IncludeAcceptance, service.IncludeLinks, service.IncludeMetadata},
	})
	if err != nil {
		apiError(rw, err)
		return
	}
	if len(board.Projects) == 0 {
		apiError(rw, domain.NotFound("project", key))
		return
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.Header().Set("Content-Disposition", `attachment; filename="`+strings.ToLower(key)+`-export.json"`)
	writeJSONIndent(rw, board.Projects[0])
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func relTimeFor(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	}
}
