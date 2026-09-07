package web

import (
	"encoding/json"
	"fmt"
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
// Note on the wire protocol: PLAN explicitly leaves "the SSE fragment
// protocol" to implementation. This handler implements the transport layer
// precisely (Last-Event-ID replay, the resync sentinel on a gap, per-project
// fan-out, non-blocking publish) and dispatches every domain.Event under the
// single canonical name sseChangeEvent. One name per frame is not a stylistic
// choice: the W3C parser overwrites the event-type buffer for every "event:"
// field inside a frame, so several names in one frame collapse to the last
// one — exactly the bug independent review #6 found here. The concrete domain
// type rides inside the JSON payload ("Type" field) for clients that want
// it; web/static/app.js treats any event as a change signal and re-fetches
// the page's live regions, which is the only contract the shipped client
// needs.
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

// sseChangeEvent is the one event name every domain event is dispatched
// under. web/static/app.js registers listeners for "board-update" and
// "activity-feed" that both mean "re-fetch the live regions", so this name
// keeps the shipped client working unchanged; the extra alias listener on
// the client is harmless redundancy.
const sseChangeEvent = "board-update"

// sseResyncEvent is the resync sentinel's wire name. app.js reloads the whole
// page on it, because a gap the replay buffer cannot cover means the page's
// state is untrustworthy.
const sseResyncEvent = "resync"

func writeSSEEvent(rw http.ResponseWriter, e domain.Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		payload = []byte(`{}`)
	}
	name := sseChangeEvent
	if e.Type == events.ResyncSentinel {
		name = sseResyncEvent
	}
	var b strings.Builder
	if e.ID > 0 {
		b.WriteString("id: ")
		b.WriteString(strconv.FormatInt(e.ID, 10))
		b.WriteString("\n")
	}
	b.WriteString("event: ")
	b.WriteString(name)
	b.WriteString("\n")
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

// snapshotEvents builds the activity page's initial server-rendered list.
//
// With Deps.History wired this is a direct, synchronous read of committed
// events — no subscription, no waiting. Since's limit is oldest-first ("up
// to limit from the start of the table"), which is the wrong end for a feed,
// so the read is unbounded and capped newest-first afterwards; that is the
// same volume the replay path below ever delivered.
//
// The bus-drain path is the fallback for a History that is not wired (yet).
// It is what the hardcoded ~150ms idle window was papering over: events.Bus
// delivers its replay asynchronously and signals nothing when the replay is
// done, so the drain has to guess from silence (review #13). Removing the
// wait without changing the data path would not fix that — it would make the
// initial feed intermittently empty whenever the replay goroutine had not
// delivered yet. The fallback stays so an unwired History degrades to the
// pre-seam behaviour instead of breaking the page.
func (w *Web) snapshotEvents(projectID string, limit int) ([]domain.Event, error) {
	if w.d.History != nil {
		// Bound the read in the query — the newest `limit` events — rather than
		// pulling the whole append-only table and truncating in memory.
		evs, err := w.d.History.Latest(projectID, limit)
		if err != nil {
			return nil, fmt.Errorf("web: read activity history for %q: %w", projectID, err)
		}
		return capNewestFirst(evs, limit), nil
	}

	sub, err := w.d.Bus.Subscribe(projectID, 0)
	if err != nil {
		return nil, fmt.Errorf("web: subscribe for activity snapshot on %q: %w", projectID, err)
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
				return capNewestFirst(out, limit), nil
			}
			if e.Type != events.ResyncSentinel {
				out = append(out, e)
			}
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(150 * time.Millisecond)
		case <-idle.C:
			return capNewestFirst(out, limit), nil
		case <-overall.C:
			return capNewestFirst(out, limit), nil
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
