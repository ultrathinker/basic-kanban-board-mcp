package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// knownChatTaskKeys resolves which of the task-key-shaped tokens mentioned in
// one page of chat messages are real tasks, in exactly one batched call to
// the service — never one call per message. This is the "which keys exist"
// knowledge the chat feed needs to turn a mention into a link without ever
// turning a made-up or stale key into a broken one (KANB-11).
//
// It is best-effort like the chat read it feeds: a failed lookup (or no
// candidates at all) yields an empty set, which is always safe — it just
// means no key in this page links, never that a bad link appears.
// service.TaskGet reports missing/inaccessible keys as NotFound rather than
// erroring the whole call, so a mix of real and made-up keys in one page
// still resolves the real ones.
func knownChatTaskKeys(ctx context.Context, svc service.Service, a service.Actor, msgs []domain.ChatMessage) map[string]struct{} {
	known := make(map[string]struct{})
	candidates := view.CandidateTaskKeys(msgs)
	if len(candidates) == 0 {
		return known
	}
	res, err := svc.TaskGet(ctx, a, service.TaskGetInput{Keys: candidates})
	if err != nil {
		return known
	}
	for _, t := range res.Tasks {
		known[strings.ToUpper(t.Key)] = struct{}{}
	}
	return known
}

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
	// KANB-14: query > cookie > default. See donecookie.go for the
	// precedence rule and the per-project cookie encoding.
	hideDone := w.resolveHideDone(rw, r, key)
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
	// Progress bars come from two batch reads (one for the project header,
	// chunked ones for the cards) — never a query per card.
	attachProgress(r.Context(), w.d.Service, actorFor(tok), key, &model)
	// The thoughts panel reads one ChatList page (newest first) — a single
	// call for the whole feed, best-effort like the reads above: a failed
	// chat read costs the panel, not the board.
	if chat, err := w.d.Service.ChatList(r.Context(), actorFor(tok), service.ChatListInput{
		ProjectKey: key,
	}); err == nil {
		known := knownChatTaskKeys(r.Context(), w.d.Service, actorFor(tok), chat.Messages)
		model.Chat = view.NewChatPanel(chat.Messages, chat.Cursor, w.d.Now(), known)
	}

	page := w.newPage(r.Context(), rw, r, tok, "board")
	page.Title = board.Projects[0].Name + " · basic-kanban-board-mcp"
	page.Model = model
	w.render(rw, r, http.StatusOK, "page-board", page)
}

// handleChatOlder is "GET /p/{key}/chat/older?before=<cursor>": one older
// page of the thoughts panel's chat feed, for the panel's upward pagination
// (scrolling to the top loads more history).
//
// It uses requireAPIAuth, not requireSessionPage: this is a JS-driven
// fragment endpoint like /projects/search and the /fragments/* handlers, not
// a page — a failed or unauthenticated call must get a non-2xx status for
// app.js to catch, not a redirect to /login.
//
// The response body is only the "chat-entries" fragment (bare <li> markup,
// oldest first — the same order and the same shared "chat-entry" template
// the initial panel render uses, via view.ChatEntriesOldestFirst), so
// app.js can insert it verbatim above the panel's current first entry with
// insertAdjacentHTML: an <ol> only tolerates <li> children in that
// position, so the next cursor cannot also ride in the body. It travels in
// the X-Chat-Next-Cursor response header instead (empty when history is
// exhausted, which is also the signal for app.js to stop asking).
//
// "before" is required and is service.ChatListInput's own cursor encoding
// (domain.ChatCursor.String()); the caller (app.js) always has one because
// the initial board render seeds it from ChatPanel.NextCursor, and this
// handler's own response reseeds it for the next call.
func (w *Web) handleChatOlder(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireAPIAuth(rw, r, domain.ScopeRead)
	if !ok {
		return
	}
	key, err := domain.ValidateProjectKey(r.PathValue("key"))
	if err != nil {
		apiError(rw, err)
		return
	}
	before := strings.TrimSpace(r.URL.Query().Get("before"))
	if before == "" {
		apiError(rw, domain.Invalid("before", "before is required", "Pass the panel's next-cursor value."))
		return
	}

	result, err := w.d.Service.ChatList(r.Context(), actorFor(tok), service.ChatListInput{
		ProjectKey: key,
		Cursor:     before,
	})
	if err != nil {
		apiError(rw, err)
		return
	}
	known := knownChatTaskKeys(r.Context(), w.d.Service, actorFor(tok), result.Messages)
	entries := view.ChatEntriesOldestFirst(result.Messages, w.d.Now(), known)
	rw.Header().Set("X-Chat-Next-Cursor", result.Cursor)
	w.renderFragment(rw, r, http.StatusOK, "chat-entries", entries)
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
		unit := t.EstimateUnit
		if unit == "" {
			unit = "h"
		}
		model.Task.Estimate = &view.EstimateView{N: *t.Estimate, Unit: unit, Label: formatEstimateLabel(*t.Estimate, unit)}
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

// handleProjectCreate is "POST /projects": the admin-only "New project" form
// (the overview section and the topbar dialog both submit here). Creating a
// project is an admin operation — project_upsert requires the admin scope — so
// this requires an admin session, not merely a write one.
func (w *Web) handleProjectCreate(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireSessionPage(rw, r, domain.ScopeAdmin)
	if !ok {
		return
	}
	if err := w.verifyCSRF(r); err != nil {
		w.pageError(rw, r, err)
		return
	}
	key, err := domain.ValidateProjectKey(r.FormValue("key"))
	if err != nil {
		w.pageError(rw, r, err)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		w.pageError(rw, r, domain.Invalid("name", "name is required to create a project", "Enter a display name."))
		return
	}
	if _, err := w.d.Service.ProjectUpsert(r.Context(), actorFor(tok), service.ProjectUpsertInput{
		Mode: service.UpsertCreate,
		Key:  key,
		Name: name,
	}); err != nil {
		w.pageError(rw, r, err)
		return
	}
	http.Redirect(rw, r, "/p/"+key, http.StatusSeeOther)
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
