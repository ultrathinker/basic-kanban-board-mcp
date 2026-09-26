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

// chatInitialLimit is how many of the most recent chat messages the board
// page's thoughts panel opens with (KANB-24: "10 last, the rest behind show
// more"). It is also the page size app.js requests from /chat/older on every
// "show more" click, so the panel always grows by the same round number the
// owner asked for, whichever direction the messages entered the page.
const chatInitialLimit = 10

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

// chatAcceptanceDoneKeys resolves, for every accepted command on one page,
// which of the acceptance's task keys currently sit in a done column
// (KANB-48: the aggregate command state comes from the linked tasks, never
// from a parallel status machine). One batched TaskGet for the whole page —
// the same shape as knownChatTaskKeys — best-effort like every other chat
// read: a failed lookup yields an empty map, which renders the keys without
// their done state rather than inventing one.
func chatAcceptanceDoneKeys(ctx context.Context, svc service.Service, a service.Actor, chat *service.ChatListResult) map[string]bool {
	done := make(map[string]bool)
	if chat == nil {
		return done
	}
	var keys []string
	seen := make(map[string]struct{})
	for _, meta := range chat.Meta {
		if meta.Acceptance == nil {
			continue
		}
		for _, k := range meta.Acceptance.TaskKeys {
			up := strings.ToUpper(k)
			if _, dup := seen[up]; dup {
				continue
			}
			seen[up] = struct{}{}
			keys = append(keys, k)
		}
	}
	for len(keys) > 0 {
		chunk := keys
		if len(chunk) > domain.MaxGetKeys {
			chunk = chunk[:domain.MaxGetKeys]
		}
		keys = keys[len(chunk):]
		res, err := svc.TaskGet(ctx, a, service.TaskGetInput{Keys: chunk})
		if err != nil {
			return done
		}
		for _, t := range res.Tasks {
			done[strings.ToUpper(t.Key)] = t.ColumnKind == domain.KindDone
		}
	}
	return done
}

// scopeChangeFeedPage and scopeChangeFeedMaxPages bound the forward walk of
// the whole feed that the chart's scope marks need: the marks must cover the
// journal's entire span, not just the panel's freshest page, and ChatFeed is
// the one read that starts at the beginning of history. One hundred pages of
// one hundred messages is far past any feed this board has carried; at the
// cap the walk stops AND says so — scopeChangeTruncatedNote names the limit
// in the items panels, because old marks dropping out must never be
// indistinguishable from "nothing was declared back then".
const (
	scopeChangeFeedPage     = 100
	scopeChangeFeedMaxPages = 100
)

// scopeChangeTruncatedNote is the sentence the items panels print when the
// scope_change walk hit its page cap: the limit is named where the reader
// sees the marks, because a silent cap reads as "nothing was declared".
func scopeChangeTruncatedNote() string {
	return fmt.Sprintf(
		"scope-change marks cover only the newest %d feed messages (the walk caps at %d pages of %d); older declarations are not drawn",
		scopeChangeFeedPage*scopeChangeFeedMaxPages, scopeChangeFeedMaxPages, scopeChangeFeedPage)
}

// scopeChangeNotes walks the project's whole feed and collects the
// scope_change declarations for the items chart's axis (KANB-38). Each note
// carries the message's own instant, author and body — the chart's tooltip
// quotes the declaration, and the href leads to the entry in the Thoughts
// feed, whose entries render with the matching id anchor. Best-effort like
// every other chat read feeding a page (see knownChatTaskKeys): a failed
// read costs the marks, not the chart.
//
// The second return reports the walk hitting its page cap
// (scopeChangeFeedMaxPages): the oldest declarations lie beyond it and will
// not be drawn, and the caller is expected to name that limit where the
// reader sees the marks (scopeChangeTruncatedNote) instead of leaving the
// omissions to read as an empty axis.
func scopeChangeNotes(ctx context.Context, svc service.Service, a service.Actor, projectKey string) ([]view.ScopeChangeNote, bool) {
	after := ""
	var notes []view.ScopeChangeNote
	for page := 0; page < scopeChangeFeedMaxPages; page++ {
		res, err := svc.ChatFeed(ctx, a, service.ChatFeedInput{
			ProjectKey: projectKey,
			After:      after,
			Limit:      scopeChangeFeedPage,
		})
		if err != nil {
			return notes, false
		}
		for _, m := range res.Messages {
			if m.Message.Kind != domain.MessageScopeChange {
				continue
			}
			notes = append(notes, view.ScopeChangeNote{
				At:     m.Message.CreatedAt,
				Author: m.Message.Author,
				Body:   m.Message.Body,
				Href:   "/p/" + projectKey + "#chat-" + m.Message.ID,
			})
		}
		if !res.HasMore {
			return notes, false
		}
		after = res.NextCursor
	}
	return notes, true
}

// chatEntryMetaView maps the service's resolved display data onto the view's
// own meta type — the view package renders what it is handed and reaches
// nowhere for data, so the field-by-field translation lives here, next to
// the read that produced it. done is chatAcceptanceDoneKeys's result.
func chatEntryMetaView(chat *service.ChatListResult, done map[string]bool) map[string]view.ChatEntryMeta {
	if chat == nil || len(chat.Meta) == 0 {
		return nil
	}
	out := make(map[string]view.ChatEntryMeta, len(chat.Meta))
	for id, m := range chat.Meta {
		vm := view.ChatEntryMeta{
			RecipientName: m.RecipientName,
			ExecutorName:  m.ExecutorName,
		}
		if m.Parent != nil {
			vm.ParentAuthor = m.Parent.Author
			vm.ParentBody = m.Parent.Body
		}
		if m.Acceptance != nil {
			vm.Accepted = true
			vm.Tasks = make([]view.ChatEntryTask, 0, len(m.Acceptance.TaskKeys))
			for _, k := range m.Acceptance.TaskKeys {
				vm.Tasks = append(vm.Tasks, view.ChatEntryTask{Key: k, Done: done[strings.ToUpper(k)]})
			}
		}
		out[id] = vm
	}
	return out
}

// chatParticipants maps the board read's participants onto the composer's
// recipient options: id and display name only, the same shape
// service.Participant already carries.
func chatParticipants(p service.BoardProject) []view.ChatRecipient {
	out := make([]view.ChatRecipient, 0, len(p.Participants))
	for _, t := range p.Participants {
		out = append(out, view.ChatRecipient{ID: t.TokenID, Label: t.Name})
	}
	return out
}

// coordinatorName is the appointed coordinator's display name, "" when none.
func coordinatorName(p service.BoardProject) string {
	if p.Coordinator == nil {
		return ""
	}
	return p.Coordinator.Name
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

// overviewTally is one project's cards split by the kind of the column they
// sit in — the four numbers the overview page reports.
//
// It exists as its own type so the split can be tested directly against
// domain.AllKinds (TestOverview_CountsEveryColumnKind). The bug it replaces
// was a three-armed switch with no default inside the handler: when KANB-52
// added the waiting kind, cards parked in a waiting column stopped being
// counted anywhere on the page — not as backlog, not as active, not as done,
// and not in the roll-up — and no test could see it, because the split was
// not reachable from one. The guard test is the default branch this switch
// cannot usefully have: a page render is the wrong place to fail loudly, so
// the loudness lives in the build instead.
type overviewTally struct {
	Backlog int
	Active  int
	Done    int
	Waiting int
}

// sum is every card the tally accounted for. A kind with no bucket below
// contributes nothing to it, which is exactly what the guard test detects.
func (t overviewTally) sum() int { return t.Backlog + t.Active + t.Done + t.Waiting }

func tallyOverview(cols []service.BoardColumn) overviewTally {
	var t overviewTally
	for _, c := range cols {
		switch c.Kind {
		case domain.KindBacklog:
			t.Backlog += c.Count
		case domain.KindActive:
			t.Active += c.Count
		case domain.KindDone:
			t.Done += c.Count
		case domain.KindWaiting:
			t.Waiting += c.Count
		}
	}
	return t
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
		t := tallyOverview(p.Columns)
		model.Projects = append(model.Projects, view.OverviewProject{
			Key: p.Key, Name: p.Name, Focus: p.FocusKey,
			Active: t.Active, Backlog: t.Backlog, Done: t.Done, Waiting: t.Waiting,
			URL: "/p/" + p.Key,
		})
		model.Total.Active += t.Active
		model.Total.Backlog += t.Backlog
		model.Total.Done += t.Done
		model.Total.Waiting += t.Waiting
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
	//
	// KANB-24: the panel opens showing only the 10 most recent messages, not
	// every message the project has ever accumulated. That cap is enforced
	// HERE, server-side, by asking ChatList for exactly 10 — not by fetching
	// everything and slicing in the template or in app.js. ChatList already
	// requests one extra row internally to detect "is there more" (see
	// service.ChatList), so this single call also gives Chat.NextCursor,
	// which is what makes the panel's "show more" control appear at all.
	if chat, err := w.d.Service.ChatList(r.Context(), actorFor(tok), service.ChatListInput{
		ProjectKey: key,
		Limit:      chatInitialLimit,
	}); err == nil {
		known := knownChatTaskKeys(r.Context(), w.d.Service, actorFor(tok), chat.Messages)
		done := chatAcceptanceDoneKeys(r.Context(), w.d.Service, actorFor(tok), chat)
		model.Chat = view.NewChatPanel(chat.Messages, chat.Cursor, w.d.Now(), known, chatEntryMetaView(chat, done))
		// The composer renders only for a session that may actually post;
		// NewChatComposer returns nil otherwise and the panel stays
		// read-only. The participant list comes from the board read the
		// page already made — no second service call.
		model.Chat.Compose = view.NewChatComposer(tok.Scopes.Has(domain.ScopeWrite),
			coordinatorName(board.Projects[0]), chatParticipants(board.Projects[0]))
	}

	model.Charts = w.projectChartFragment(r.Context(), actorFor(tok), board.Projects[0].Key)

	page := w.newPage(r.Context(), rw, r, tok, "board")
	page.Title = board.Projects[0].Name + " · basic-kanban-board-mcp"
	page.Model = model
	w.render(rw, r, http.StatusOK, "page-board", page)
}

// handleChatOlder is "GET /p/{key}/chat/older?before=<cursor>[&limit=N]": one
// older page of the thoughts panel's chat feed. It backs two client
// behaviours that both reuse this same route rather than each growing their
// own (KANB-23/KANB-24): the panel's "show more" control appends the next
// chatInitialLimit-sized page below the feed's current last entry, and (were
// it still wired) the old scroll-triggered pagination this replaced worked
// the same way.
//
// It uses requireAPIAuth, not requireSessionPage: this is a JS-driven
// fragment endpoint like /projects/search and the /fragments/* handlers, not
// a page — a failed or unauthenticated call must get a non-2xx status for
// app.js to catch, not a redirect to /login.
//
// The response body is only the "chat-entries" fragment (bare <li> markup,
// newest first within the page — the same order and the same shared
// "chat-entry" template the initial panel render uses, via
// view.ChatEntriesNewestFirst), so app.js can insert it verbatim with
// insertAdjacentHTML: an <ol> only tolerates <li> children there, so the
// next cursor cannot also ride in the body. It travels in the
// X-Chat-Next-Cursor response header instead (empty when history is
// exhausted, which is also the signal for app.js to stop asking).
//
// "before" is required and is service.ChatListInput's own cursor encoding
// (domain.ChatCursor.String()); the caller (app.js) always has one because
// the initial board render seeds it from ChatPanel.NextCursor, and this
// handler's own response reseeds it for the next call.
//
// "limit" is optional and, when present, must be a positive integer — app.js
// always passes chatInitialLimit (10) explicitly so a "show more" click grows
// the panel by the same round number the initial render used. Omitting it
// keeps this endpoint's pre-KANB-24 default (50 via service.ChatList) for any
// other caller, so this is an additive change, not a behaviour break.
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
	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n <= 0 {
			apiError(rw, domain.Invalid("limit", "limit must be a positive integer", "Omit it, or pass a positive integer."))
			return
		}
		limit = n
	}

	result, err := w.d.Service.ChatList(r.Context(), actorFor(tok), service.ChatListInput{
		ProjectKey: key,
		Cursor:     before,
		Limit:      limit,
	})
	if err != nil {
		apiError(rw, err)
		return
	}
	known := knownChatTaskKeys(r.Context(), w.d.Service, actorFor(tok), result.Messages)
	done := chatAcceptanceDoneKeys(r.Context(), w.d.Service, actorFor(tok), result)
	entries := view.ChatEntriesNewestFirst(result.Messages, w.d.Now(), known, chatEntryMetaView(result, done))
	rw.Header().Set("X-Chat-Next-Cursor", result.Cursor)
	w.renderFragment(rw, r, http.StatusOK, "chat-entries", entries)
}

// handleChatPost is "POST /p/{key}/chat": the thoughts panel's composer
// (KANB-48). body is required and stored verbatim; kind, recipient,
// reply_to and idempotency_key are the protocol fields passed through to
// service.ChatAdd, which owns every rule about them (valid kinds, recipient
// resolution, the reply target, and retry deduplication scoped to the
// sender's token).
//
// The idempotency key deserves its own sentence, because the card makes it
// the UI's obligation: the composer generates one per send attempt and
// keeps it until the send succeeds, so a second click — or a retried
// request after a network error — carries the SAME key and the service
// returns the first message instead of creating a twin command. The handler
// does not invent a key when the field is empty: a caller that omits it
// said what it wanted, and silently adding one would trade an explicit
// behaviour for an unspecified one.
//
// The response is a small JSON body, not rendered markup: the feed renders
// entries through exactly one path (the shared chat-entry template, reached
// by the initial page render and by the live refresh the SSE event this
// very post triggers), so a successful send refetches like any other
// arrival and can never draw its message differently from how every other
// message is drawn. requireAPIAuth + verifyCSRF: the same JS-driven
// fragment mutation shape the /fragments/* handlers use.
func (w *Web) handleChatPost(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireAPIAuth(rw, r, domain.ScopeWrite)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		apiError(rw, err)
		return
	}
	key, err := domain.ValidateProjectKey(r.PathValue("key"))
	if err != nil {
		apiError(rw, err)
		return
	}
	m, err := w.d.Service.ChatAdd(r.Context(), actorFor(tok), service.ChatAddInput{
		ProjectKey:     key,
		Body:           r.PostFormValue("body"),
		Kind:           r.PostFormValue("kind"),
		Recipient:      r.PostFormValue("recipient"),
		ReplyTo:        r.PostFormValue("reply_to"),
		IdempotencyKey: r.PostFormValue("idempotency_key"),
	})
	if err != nil {
		apiError(rw, err)
		return
	}
	writeJSONIndent(rw, map[string]any{"ok": true, "id": m.ID})
}

// projectChartFragment builds the panel's chart set for a project scope. It
// exists because the charts are now rendered WITH the board page instead of
// fetched when a progress bar is clicked (owner, 14.09.2026: the bar is no
// longer a control over the charts). The fragment handler still uses the same
// pieces for the per-task scope and for the live refresh, so the projections
// live in exactly one place and the page and the fragment cannot disagree.
//
// A failed read costs the charts, not the page: the board renders without
// them rather than not at all, the same best-effort rule the chat read
// already follows.
func (w *Web) projectChartFragment(ctx context.Context, a service.Actor, key string) *view.ProgressChartFragment {
	result, err := w.d.Service.ProgressHistory(ctx, a, service.ProgressHistoryInput{
		ProjectKey:    key,
		IncludeReplay: true,
	})
	if err != nil {
		return nil
	}
	notes, truncated := scopeChangeNotes(ctx, w.d.Service, a, key)
	items := view.NewItemsChartView(result.Replay, view.DefaultChartWidth, view.DefaultChartHeight,
		view.ScopeChangeMarks(notes)...)
	if items != nil && truncated {
		items.Note = scopeChangeTruncatedNote()
	}
	frag := &view.ProgressChartFragment{
		Progress:  view.NewProgressChartView(result.Marks, view.DefaultChartWidth, view.DefaultChartHeight),
		Forecast:  view.NewForecastChartView(result.Marks, view.DefaultChartWidth, view.DefaultChartHeight),
		Items:     items,
		Readiness: view.NewReadinessChartView(result.Replay, view.DefaultChartWidth, view.DefaultChartHeight),
	}
	if frag.Empty() {
		return nil
	}
	return frag
}

// handleProgressChart is "GET /p/{key}/progress/chart?task=<key>": the
// progress-history chart for one metric, fetched the first time its bar is
// clicked open rather than rendered for every bar on the page — see
// REPORT.md for the measured page-weight argument behind that choice. task
// is optional; empty selects the project's manual scope, set selects one
// task's summary scope — the same (project, task) convention
// service.ProgressHistoryInput and the delete-track control already use.
//
// Like handleChatOlder and handleProgressTrackDelete, this is a JS-driven
// fragment endpoint: requireAPIAuth, not requireSessionPage, so a failed or
// unauthenticated call gets a non-2xx status for app.js to catch instead of
// a redirect to /login. Read-only (ScopeRead), unlike the owner-only delete:
// any caller who can already see the board's progress bars may open the
// chart behind one.
//
// The response body is only the "progress-chart-fragment" fragment (see
// partials.html): a bare wrapper around chart.go's own SVG, or nothing at
// all when the scope's history has vanished since the bar was rendered
// (e.g. a track delete raced the click) — app.js treats an empty body as a
// no-op, the same way it already does for handleChatOlder's exhausted page.
func (w *Web) handleProgressChart(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireAPIAuth(rw, r, domain.ScopeRead)
	if !ok {
		return
	}
	key, err := domain.ValidateProjectKey(r.PathValue("key"))
	if err != nil {
		apiError(rw, err)
		return
	}
	taskKey := strings.TrimSpace(r.URL.Query().Get("task"))
	// detail names ONE enlarged chart for the modal: "progress" or "items".
	// Same data and same projections as the inline panel, with the axis
	// ticks, midpoint tick and legend the 600x200 panel has no room for.
	//
	// One chart per request, never both: the modal is sized to fill the
	// viewport so the reader never has to scroll a chart, and two of them
	// stacked in that space is exactly the scrolling this avoids. Which one
	// opens is decided by which panel was clicked.
	detail := strings.TrimSpace(r.URL.Query().Get("detail"))

	result, err := w.d.Service.ProgressHistory(r.Context(), actorFor(tok), service.ProgressHistoryInput{
		ProjectKey: key,
		TaskKey:    taskKey,
		// The item-count panel is a project-level reading — how much work
		// the board holds and how much of it is still open — so the journal
		// replay behind it is asked for only on the project scope. The
		// service ignores the flag when a task is named, but not asking
		// keeps the replay off the per-task path entirely.
		IncludeReplay: taskKey == "",
	})
	if err != nil {
		apiError(rw, err)
		return
	}
	// KANB-38: the scope_change declarations ride the items chart's time
	// axis. They come from the feed, not the journal, so this is one extra
	// read on the project scope only — the per-task charts have no items
	// panel and ask for no replay either. scopeTruncated is the walk saying
	// it hit its page cap: the oldest declarations are beyond it, and the
	// items panels below must name that limit where the reader sees the
	// marks instead of leaving them to read as an empty axis.
	var scopeMarks []view.ChartAxisMark
	scopeTruncated := false
	if taskKey == "" {
		notes, truncated := scopeChangeNotes(r.Context(), w.d.Service, actorFor(tok), key)
		scopeMarks = view.ScopeChangeMarks(notes)
		scopeTruncated = truncated
	}
	switch detail {
	case "":
		// The inline panels, handled below.
	case "progress":
		w.renderChartDetail(rw, r, view.NewProgressChartDetail(result.Marks))
		return
	case "forecast":
		// KANB-36: the forecast chart is its own panel. NewForecastChartDetail
		// returns nil when no mark in history carried an ETA, exactly like the
		// percent and items charts' nil semantics — a modal frame around
		// "nothing here" would be a worse lie than no modal.
		w.renderChartDetail(rw, r, view.NewForecastChartDetail(result.Marks))
		return
	case "items":
		chart := view.NewItemsChartDetail(result.Replay, scopeMarks...)
		if chart != nil && scopeTruncated {
			chart.Note = scopeChangeTruncatedNote()
		}
		w.renderChartDetail(rw, r, chart)
		return
	default:
		apiError(rw, domain.Invalid("detail",
			fmt.Sprintf("unknown chart %q", detail),
			`Ask for "progress", "forecast" or "items", or omit detail for the inline panels.`))
		return
	}
	items := view.NewItemsChartView(result.Replay, view.DefaultChartWidth, view.DefaultChartHeight, scopeMarks...)
	if items != nil && scopeTruncated {
		items.Note = scopeChangeTruncatedNote()
	}
	frag := &view.ProgressChartFragment{
		Progress: view.NewProgressChartView(result.Marks, view.DefaultChartWidth, view.DefaultChartHeight),
		// KANB-36: the forecast chart is its own panel, fed from the same
		// marks the percent chart reads. NewForecastChartView returns nil
		// when nobody in this history ever gave a forecast, the same
		// "render nothing" rule the percent chart already follows for
		// missing data.
		Forecast: view.NewForecastChartView(result.Marks, view.DefaultChartWidth, view.DefaultChartHeight),
		Items:    items,
		// KANB-53: the historical readiness curve, from the same replay, so
		// its caption (service's own sentence about point-in-time estimates)
		// always has the curve beside it. Nil for a task scope (no replay is
		// asked for) and when nothing was ever estimated — a frame around
		// nothing, with the warning but no picture, would be worse than
		// neither.
		Readiness: view.NewReadinessChartView(result.Replay, view.DefaultChartWidth, view.DefaultChartHeight),
	}
	if frag.Empty() {
		// Nothing to draw at all: render the empty body app.js already
		// treats as "leave the slot closed", rather than an empty frame.
		w.renderFragment(rw, r, http.StatusOK, "progress-chart-fragment", nil)
		return
	}
	w.renderFragment(rw, r, http.StatusOK, "progress-chart-fragment", frag)
}

// renderChartDetail writes one enlarged chart, or an empty body when there
// is nothing to chart — app.js prints its own "no history yet" rather than
// opening a frame around nothing.
func (w *Web) renderChartDetail(rw http.ResponseWriter, r *http.Request, chart *view.ChartDetailView) {
	if chart == nil {
		w.renderFragment(rw, r, http.StatusOK, "chart-detail-fragment", nil)
		return
	}
	w.renderFragment(rw, r, http.StatusOK, "chart-detail-fragment", chart)
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
			ID: e.ID, Actor: e.Actor, Verb: activityVerb(e),
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

// handleProjectExport is "/p/{key}/export": the full backup document of one
// project, built by Service.Export — the same call `kanban export --project
// KEY` makes, so the download is a document `kanban import` accepts and
// restores to the same project (KANB-70).
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
	doc, err := w.d.Service.Export(r.Context(), actorFor(tok), service.ExportInput{ProjectKey: key})
	if err != nil {
		apiError(rw, err)
		return
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.Header().Set("Content-Disposition", `attachment; filename="`+strings.ToLower(key)+`-export.json"`)
	writeJSONIndent(rw, doc)
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
