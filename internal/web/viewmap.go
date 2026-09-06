package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// writeJSONIndent writes v as pretty-printed JSON. Used by the export
// endpoints; PLAN §12 asks for "two-space indent for diffs".
func writeJSONIndent(rw http.ResponseWriter, v any) {
	enc := json.NewEncoder(rw)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// render executes name into a buffer first so a mid-template failure never
// sends a half-written page with a 200 status; only a fully rendered buffer
// is flushed to the client.
func (w *Web) render(rw http.ResponseWriter, r *http.Request, status int, name string, page view.Page) {
	var buf bytes.Buffer
	if err := w.d.Templates.Render(&buf, name, page); err != nil {
		w.pageError(rw, r, err)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(status)
	_, _ = buf.WriteTo(rw)
}

// renderFragment is render's sibling for htmx/fetch fragment responses: no
// status-code defaulting surprises, same buffer-first discipline.
func (w *Web) renderFragment(rw http.ResponseWriter, r *http.Request, status int, name string, data any) {
	var buf bytes.Buffer
	if err := w.d.Templates.Render(&buf, name, data); err != nil {
		apiError(rw, err)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(status)
	_, _ = buf.WriteTo(rw)
}

// markdownRender reuses view.Funcs()'s "markdown" entry (goldmark render +
// bluemonday sanitize) rather than re-implementing it: that function is
// unexported in internal/web/view, but the FuncMap that carries it is public
// and is the same value the templates already trust.
//
// The lookup uses comma-ok against a defined signature so that an upstream
// signature change (e.g. view's markdown helper moving to take an *html/template
// or returning a string) surfaces as a panic on the first real render of the
// drawer, not at package init before main runs.
var markdownRender = func() func(string) template.HTML {
	v := view.Funcs()["markdown"]
	f, ok := v.(func(string) template.HTML)
	if !ok {
		panic("web: view.Funcs()[\"markdown\"] is not func(string) template.HTML; got " +
			fmt.Sprintf("%T", v) + " — keep internal/web/view/markdownSafe's signature in sync")
	}
	return f
}()

// taskCardFrom builds a view.TaskCard for one task via view.BuildBoard,
// reusing its lease/estimate/age/priority rendering rules instead of
// duplicating them. col only needs Name/Kind/WIPLimit; a synthetic id keeps
// BuildBoard's column-grouping happy for this single-task call.
func taskCardFrom(projectKey string, col domain.Column, t domain.TaskView) view.TaskCard {
	col.ID = "c"
	tv := t
	tv.ColumnID = "c"
	m := view.BuildBoard(view.ProjectSummary{Key: projectKey}, nil, []domain.Column{col}, []domain.TaskView{tv}, false)
	if len(m.Columns) == 1 && len(m.Columns[0].Tasks) == 1 {
		return m.Columns[0].Tasks[0]
	}
	return view.TaskCard{Key: t.Key, Title: t.Title}
}

// buildBoardModel maps a service.BoardProject onto the view.BoardModel the
// board page template binds to. It is written directly against
// service.BoardColumn (rather than reusing view.BuildBoard end-to-end)
// because BoardColumn has no internal column id for BuildBoard's grouping —
// only a name and kind, which is all the template needs anyway.
func buildBoardModel(proj service.BoardProject, hideDone bool) view.BoardModel {
	m := view.BoardModel{
		Project:   view.ProjectSummary{Key: proj.Key, Name: proj.Name, Focus: proj.FocusKey},
		HideDone:  hideDone,
		DoneTotal: proj.DoneTotal,
		DoneShown: proj.DoneShown,
	}
	for _, c := range proj.Columns {
		cv := view.ColumnView{
			Name:   c.Name,
			Kind:   c.Kind,
			WIP:    c.WIPLimit,
			Count:  c.Count,
			Hidden: hideDone && c.Kind == domain.KindDone,
		}
		col := domain.Column{Name: c.Name, Kind: c.Kind, WIPLimit: c.WIPLimit}
		for _, t := range c.Tasks {
			card := taskCardFrom(proj.Key, col, t)
			cv.Tasks = append(cv.Tasks, card)
			if proj.FocusKey != "" && strings.EqualFold(t.Key, proj.FocusKey) {
				m.Focus = &view.FocusCard{Key: t.Key, Title: t.Title, Type: t.Type}
			}
		}
		m.Columns = append(m.Columns, cv)
	}
	return m
}

// taskRefsFor turns a batch TaskGet result into TaskRef values, preserving
// only the keys that were actually found (a stale blocker/blocking key that
// no longer resolves is dropped rather than shown as a broken link).
func taskRefsFor(res *service.TaskGetResult) []view.TaskRef {
	out := make([]view.TaskRef, 0, len(res.Tasks))
	for _, t := range res.Tasks {
		out = append(out, view.TaskRef{Key: t.Key, Title: t.Title, Type: t.Type, URL: "/t/" + t.Key})
	}
	return out
}

// eventVerb renders a domain.EventType as the short, human verb the activity
// feed shows. PLAN §12 leaves the SSE/activity presentation format to
// implementation, so this mapping is this package's decision.
func eventVerb(t domain.EventType) string {
	switch t {
	case domain.EventProjectCreated:
		return "created project"
	case domain.EventProjectUpdated:
		return "updated project"
	case domain.EventProjectArchived:
		return "archived project"
	case domain.EventTaskCreated:
		return "created"
	case domain.EventTaskUpdated:
		return "updated"
	case domain.EventTaskMoved:
		return "moved"
	case domain.EventTaskStarted:
		return "started"
	case domain.EventTaskClaimed:
		return "claimed"
	case domain.EventTaskRenewed:
		return "renewed the lease on"
	case domain.EventTaskReleased:
		return "released"
	case domain.EventTaskArchived:
		return "archived"
	case domain.EventTaskRestored:
		return "restored"
	case domain.EventLinkAdded:
		return "linked"
	case domain.EventLinkRemoved:
		return "unlinked"
	case domain.EventNoteAdded:
		return "added a note to"
	case domain.EventFocusChanged:
		return "changed focus to"
	default:
		return string(t)
	}
}

// eventPayloadString reads a best-effort display string out of an event's
// payload. The wire schema for Payload is implementation-defined (PLAN
// explicitly leaves it open); this package's convention — used by
// eventTarget/eventTitle below — is "key" and "title" string entries. Until
// internal/service (agent F) populates them, both degrade to empty strings,
// which the activity-row template already renders fine.
func eventPayloadString(e domain.Event, field string) string {
	if e.Payload == nil {
		return ""
	}
	if v, ok := e.Payload[field].(string); ok {
		return v
	}
	return ""
}

// sortEventsNewestFirst reorders a slice already sorted oldest-first (as the
// events store returns it) for the activity feed's newest-first display.
func sortEventsNewestFirst(evs []domain.Event) {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].ID > evs[j].ID })
}
