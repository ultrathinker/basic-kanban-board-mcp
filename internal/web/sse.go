package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
)

// handleEvents serves the SSE stream for either the whole install (no
// project in the path/query) or one project ("/events/p/{key}" or
// "/events?project=KEY"). Browsers authenticate with the session cookie;
// token holders exchange a one-time ticket via POST /events/ticket rather
// than ever putting a bearer token in a URL (PLAN §8).
//
// Note on the fragment protocol: PLAN explicitly leaves "the SSE fragment
// protocol" to implementation. This handler implements the transport layer
// precisely (Last-Event-ID replay, the resync sentinel on a gap, per-project
// fan-out, non-blocking publish) and emits each domain.Event under its own
// type name plus the "board-update"/"activity-feed" aliases the board and
// activity page markup declare via `sse-swap`. The payload is the
// JSON-encoded event, not a pre-rendered HTML fragment: wiring a real
// swappable fragment requires an `hx-target` on the actual board columns /
// activity list in web/templates (currently `sse-swap` sits on a small
// status span), which is outside this package's ownership. See the task
// report for the follow-up.
func (w *Web) handleEvents(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireAPIAuth(rw, r, domain.ScopeRead)
	if !ok {
		return
	}

	key := r.PathValue("key")
	if key == "" {
		key = strings.TrimSpace(r.URL.Query().Get("project"))
	}
	var projectID string
	if key != "" {
		pk, err := domain.ValidateProjectKey(key)
		if err != nil {
			apiError(rw, err)
			return
		}
		if !tok.MayAccessProject(pk) {
			apiError(rw, domain.Forbidden("this token cannot access project "+pk, "Use a token whose project_keys include "+pk+"."))
			return
		}
		id, found, err := w.d.ProjectLookup.ProjectIDForKey(r.Context(), pk)
		if err != nil {
			apiError(rw, err)
			return
		}
		if !found {
			apiError(rw, domain.NotFound("project", pk))
			return
		}
		projectID = id
	}

	flusher, canFlush := rw.(http.Flusher)
	if !canFlush {
		apiError(rw, domain.Forbidden("streaming unsupported by this connection", "Use an HTTP/1.1+ client that supports chunked responses."))
		return
	}

	sub, err := w.d.Bus.Subscribe(projectID, lastEventID(r))
	if err != nil {
		apiError(rw, err)
		return
	}
	defer sub.Cancel()

	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("Connection", "keep-alive")
	rw.Header().Set("X-Accel-Buffering", "no") // PLAN §9: every reverse-proxy example needs this
	rw.WriteHeader(http.StatusOK)

	writeSSERaw(rw, "event: connected\ndata: {}\n\n")
	flusher.Flush()

	ctxDone := r.Context().Done()
	for {
		select {
		case e, ok := <-sub.Events:
			if !ok {
				return
			}
			writeSSEEvent(rw, e)
			flusher.Flush()
		case <-ctxDone:
			return
		case <-w.d.Shutdown:
			writeSSERaw(rw, "event: shutdown\ndata: {}\n\n")
			flusher.Flush()
			return
		}
	}
}

// handleEventsTicket mints a 60s one-time SSE ticket for a token holder
// (PLAN §8: "never a bearer token in a query string"). Browsers do not need
// this — they use the session cookie directly.
func (w *Web) handleEventsTicket(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireAPIAuth(rw, r, domain.ScopeRead)
	if !ok {
		return
	}
	ticket, err := w.d.Auth.Tickets.Issue(tok.ID)
	if err != nil {
		apiError(rw, err)
		return
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(map[string]any{
		"ok": true,
		"data": map[string]any{
			"ticket":     ticket,
			"expires_in": int(domain.TicketTTL.Seconds()),
		},
	})
}

func lastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("lastEventId")
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func writeSSEEvent(rw http.ResponseWriter, e domain.Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		payload = []byte(`{}`)
	}
	var names []string
	if e.Type == events.ResyncSentinel {
		names = []string{"resync"}
	} else {
		names = []string{string(e.Type), "board-update", "activity-feed"}
	}
	var b strings.Builder
	if e.ID > 0 {
		b.WriteString("id: ")
		b.WriteString(strconv.FormatInt(e.ID, 10))
		b.WriteString("\n")
	}
	for _, n := range names {
		b.WriteString("event: ")
		b.WriteString(n)
		b.WriteString("\n")
	}
	for _, line := range strings.Split(string(payload), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	writeSSERaw(rw, b.String())
}

func writeSSERaw(rw http.ResponseWriter, s string) {
	_, _ = rw.Write([]byte(s))
}

// snapshotEvents drains a project's replay (via a throwaway subscription) to
// build the activity page's initial server-rendered list. events.Bus has no
// synchronous "give me the latest N" query outside Subscribe's async replay
// stream, so this uses a short idle-timeout heuristic: once ~150ms passes
// with nothing new arriving, the replay is assumed complete. See the task
// report — a direct HistoryLoader.Latest-backed method on Bus would be
// cleaner and is a reasonable follow-up for whoever owns internal/events.
func (w *Web) snapshotEvents(projectID string, limit int) []domain.Event {
	sub, err := w.d.Bus.Subscribe(projectID, 0)
	if err != nil {
		return nil
	}
	defer sub.Cancel()

	var out []domain.Event
	idle := time.NewTimer(150 * time.Millisecond)
	defer idle.Stop()
	overall := time.NewTimer(1500 * time.Millisecond)
	defer overall.Stop()
	for {
		select {
		case e, ok := <-sub.Events:
			if !ok {
				return capNewestFirst(out, limit)
			}
			if e.Type != events.ResyncSentinel {
				out = append(out, e)
			}
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(150 * time.Millisecond)
		case <-idle.C:
			return capNewestFirst(out, limit)
		case <-overall.C:
			return capNewestFirst(out, limit)
		}
	}
}

func capNewestFirst(evs []domain.Event, limit int) []domain.Event {
	sortEventsNewestFirst(evs)
	if limit > 0 && len(evs) > limit {
		evs = evs[:limit]
	}
	return evs
}
