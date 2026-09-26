package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// webExportDoc is the JSON shape the web Export button serves. It matches
// cmd/kanban's exportDocument byte-for-byte (same field names, same JSON
// tags) so an export downloaded from the web and one produced by
// `kanban export --out ...` go through the same import path. The web
// handler can't use cmd/kanban's struct directly because that would cycle
// the package import graph, so the shape is kept in sync by convention
// rather than by code.
//
// The progress and chat slices carry project / task keys (not internal
// UUIDs) so an import against a fresh board can remap them — same reason
// the CLI side uses keys.
type webExportDoc struct {
	Projects      []service.BoardProject  `json:"projects"`
	ProgressMarks []webExportProgressMark `json:"progress_marks"`
	ChatMessages  []webExportChatMessage  `json:"chat_messages"`

	// Truncated names every project whose done tasks did not all fit, the
	// same field cmd/kanban's exportDocument carries and for the same
	// reason: the board read behind this document caps done tasks at
	// domain.MaxDoneLimit, and KANB-29 exists against loss nobody notices.
	// The web path used to be the silent one -- it produced a document
	// missing done tasks and said nothing at all -- which is the exact
	// failure the card was filed about, just on the other surface.
	Truncated []webExportTruncationNotice `json:"truncated,omitempty"`
}

// webExportTruncationNotice is cmd/kanban's exportTruncationNotice, field
// for field: one project whose done tasks exceeded the export's capacity.
// Total is every done task the project has (BoardProject.DoneTotal);
// Included is how many reached this document (BoardProject.DoneShown).
type webExportTruncationNotice struct {
	Project  string `json:"project"`
	Included int    `json:"included"`
	Total    int    `json:"total"`
}

type webExportProgressMark struct {
	ID        string     `json:"id"`
	Project   string     `json:"project"`
	Task      string     `json:"task,omitempty"`
	Assessor  string     `json:"assessor"`
	Percent   int        `json:"percent"`
	ETA       *time.Time `json:"eta,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type webExportChatMessage struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// buildWebExportDoc mirrors cmd/kanban/export.go's exportBoard through the
// public service.Service surface, so the web Export button and `kanban
// export --out file.json` produce byte-for-byte the same document. Chat is
// paged through in full below (100 is the page size, not a cap), and the
// progress read passes Limit=0 to ProgressHistory, which the service
// interprets as "the whole scope" (no Tail cap), so a long progress track
// travels in full here too.
func buildWebExportDoc(ctx context.Context, svc service.Service, actor service.Actor, projectKey string) (*webExportDoc, error) {
	board, err := svc.BoardGet(ctx, actor, service.BoardGetInput{
		ProjectKey: projectKey,
		View:       service.ViewTasks,
		DoneLimit:  domain.MaxDoneLimit,
		Include: service.Includes{
			service.IncludeBody,
			service.IncludeAcceptance,
			service.IncludeLinks,
			service.IncludeMetadata,
		},
	})
	if err != nil {
		return nil, err
	}
	if projectKey != "" && len(board.Projects) == 0 {
		return nil, domain.NotFound("project", projectKey)
	}

	out := &webExportDoc{Projects: board.Projects}
	for _, bp := range board.Projects {
		// Symmetric with runExport: name the gap in the document AND say it
		// out loud. The CLI writes its warning to stderr, which is where the
		// person who typed the command is looking; the browser gives the
		// caller no such channel, so the equivalent here is the server log
		// the operator reads plus the "truncated" block that travels inside
		// the downloaded file itself.
		if bp.DoneTotal > bp.DoneShown {
			out.Truncated = append(out.Truncated, webExportTruncationNotice{
				Project:  bp.Key,
				Included: bp.DoneShown,
				Total:    bp.DoneTotal,
			})
			log.Printf("web: export: project %s exports only %d of %d done tasks (see \"truncated\" in the document; the rest are still on the board, just not in this export)", bp.Key, bp.DoneShown, bp.DoneTotal)
		}

		projMarks, err := svc.ProgressHistory(ctx, actor, service.ProgressHistoryInput{
			ProjectKey: bp.Key,
		})
		if err != nil {
			return nil, err
		}
		for _, m := range projMarks.Marks {
			out.ProgressMarks = append(out.ProgressMarks, webExportProgressMark{
				ID: m.ID, Project: bp.Key, Assessor: m.Assessor, Percent: m.Percent, ETA: m.ETA, CreatedAt: m.CreatedAt,
			})
		}
		for _, col := range bp.Columns {
			for _, tv := range col.Tasks {
				taskMarks, err := svc.ProgressHistory(ctx, actor, service.ProgressHistoryInput{
					ProjectKey: bp.Key,
					TaskKey:    tv.Key,
				})
				if err != nil {
					return nil, err
				}
				for _, m := range taskMarks.Marks {
					out.ProgressMarks = append(out.ProgressMarks, webExportProgressMark{
						ID: m.ID, Project: bp.Key, Task: tv.Key, Assessor: m.Assessor, Percent: m.Percent, ETA: m.ETA, CreatedAt: m.CreatedAt,
					})
				}
			}
		}

		// ChatList pages backwards 100 at a time. The LastSeen / Cursor
		// field of each result is the cursor for the older page; an empty
		// Cursor terminates the loop. 100 is the PAGE SIZE, not a cap: the
		// loop keeps walking until the cursor runs out, so the document
		// carries the whole feed. KANB-29 criterion 4 says "the same full
		// document" as the CLI, and a board loses its OLDEST thinking first
		// — the half nobody notices is gone — so stopping after one page
		// would be exactly the silent loss this card exists to prevent.
		// TestProjectExportRoute_CarriesTheWholeDocument seeds more than one
		// page and fails if this loop ever stops early.
		cursor := ""
		for {
			list, err := svc.ChatList(ctx, actor, service.ChatListInput{
				ProjectKey: bp.Key,
				Limit:      100,
				Cursor:     cursor,
			})
			if err != nil {
				return nil, err
			}
			for _, m := range list.Messages {
				out.ChatMessages = append(out.ChatMessages, webExportChatMessage{
					ID: m.ID, Project: bp.Key, Author: m.Author, Body: m.Body, CreatedAt: m.CreatedAt,
				})
			}
			if list.Cursor == "" || len(list.Messages) == 0 {
				break
			}
			cursor = list.Cursor
		}
	}
	return out, nil
}

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
	setNoStore(rw)
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
	setNoStore(rw)
	rw.WriteHeader(status)
	_, _ = buf.WriteTo(rw)
}

// setNoStore marks a dynamic response as never reusable from a cache.
//
// The static handler deliberately says "no-cache" and carries an ETag: those
// bytes cannot change under a running server, so revalidation is the cheap
// and correct answer there. Pages and fragments are the opposite — they are
// the board, they change on every write, and they carry no validator at all.
// With no directive an intermediary (the reverse proxy PLAN section 9
// documents) or a browser applying heuristic freshness is entitled to reuse
// them, and that would silently disable live updates rather than break them
// loudly: refreshLiveRegions fetches window.location.href, the very URL the
// browser already navigated to, so a cached 200 would swap every live region
// for the markup it already had while the status still announced "updated".
// A frozen board that reports success is the same failure the day-old
// stylesheet was, one layer up.
func setNoStore(rw http.ResponseWriter) {
	rw.Header().Set("Cache-Control", "no-store")
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
// duplicating them. col only needs Name/Kind; a synthetic id keeps
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
			Count:  c.Count,
			Hidden: hideDone && c.Kind == domain.KindDone,
		}
		col := domain.Column{Name: c.Name, Kind: c.Kind}
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

// attachProgress fills the board model's progress bars from the service's
// batch reads: ONE ProjectProgress call plus ceil(cards/MaxGetKeys)
// TaskProgress calls — never a query per card, or a board of sixty cards
// would pay sixty reads to draw sixty badges. A failed progress read is
// best-effort, like the drawer's blocker enrichment: the page renders
// without bars rather than not at all, and a missing bar reads truthfully
// as "no data" instead of lying about a percentage.
func attachProgress(ctx context.Context, svc service.Service, a service.Actor, projectKey string, m *view.BoardModel) {
	if pp, err := svc.ProjectProgress(ctx, a, service.ProjectProgressInput{ProjectKey: projectKey}); err == nil && pp != nil {
		m.ManualProgress = view.NewAssessedProgress(pp.Manual, pp.ManualAssessors, pp.ManualForecastETA, pp.ManualForecastBy).
			WithTracks(projectKey, "", tracksFromService(pp.ManualTracks))
		// The automatic bar is not built from marks — nothing a track-delete
		// could remove, and no forecast to carry either — so it stays a
		// plain NewDoneShareProgress with no Tracks/scope/Forecast attached.
		m.AutoProgress = view.NewDoneShareProgress(pp.Auto, pp.DoneTasks, pp.TotalTasks)
	}

	// Progress is fetched only for cards that are actually rendered: hidden
	// done columns draw no card, so their keys buy nothing.
	var keys []string
	for _, c := range m.Columns {
		if c.Hidden {
			continue
		}
		for _, t := range c.Tasks {
			keys = append(keys, t.Key)
		}
	}
	byKey := make(map[string]service.TaskProgressItem, len(keys))
	for len(keys) > 0 {
		chunk := keys
		if len(chunk) > domain.MaxGetKeys {
			chunk = chunk[:domain.MaxGetKeys]
		}
		keys = keys[len(chunk):]
		tp, err := svc.TaskProgress(ctx, a, service.TaskProgressInput{ProjectKey: projectKey, Keys: chunk})
		if err != nil {
			return
		}
		for _, item := range tp.Items {
			byKey[strings.ToUpper(item.Key)] = item
		}
	}
	for ci := range m.Columns {
		if m.Columns[ci].Hidden {
			continue
		}
		for ti := range m.Columns[ci].Tasks {
			card := &m.Columns[ci].Tasks[ti]
			if item, ok := byKey[strings.ToUpper(card.Key)]; ok {
				card.Progress = view.NewAssessedProgress(item.Percent, item.Assessors, item.ForecastETA, item.ForecastBy).
					WithTracks(projectKey, card.Key, tracksFromService(item.Tracks))
			}
		}
	}
}

// tracksFromService maps the service's per-assessor breakdown onto the view
// package's own type. A thin, deliberate copy rather than a shared type: the
// view package's exported types must stay free of service's own shapes, so a
// template only ever binds to plain render data — not because view may never
// import service at all (view/chart.go legitimately does, to reuse
// service.RoundMeanHalfUp rather than duplicate that rounding rule; see its
// own comment). This is the one place the two TRACK shapes are kept in sync
// by hand.
func tracksFromService(in []service.AssessorTrack) []view.ProgressTrack {
	if len(in) == 0 {
		return nil
	}
	out := make([]view.ProgressTrack, 0, len(in))
	for _, t := range in {
		out = append(out, view.ProgressTrack{Assessor: t.Assessor, Percent: t.Percent, Count: t.Count})
	}
	return out
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
	case domain.EventProgressRecorded:
		return "recorded progress on"
	case domain.EventProgressTrackDeleted:
		return "removed a progress track from"
	case domain.EventChatPosted:
		return "posted a thought in"
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
