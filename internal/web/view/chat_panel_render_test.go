package view_test

import (
	gohtml "html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// chatFixtureNow anchors the fixture timestamps; the relative ages in the
// rendered feed are computed against it.
//
// time.Local, not time.UTC. A UTC fixture hid a real defect: the panel header
// printed "last 22:00" beside a post the feed stamped 00:00, because the
// header printed CreatedAt raw while the feed put it through formatChartTime,
// which converts. On a machine running UTC the two agree by accident and the
// bug is invisible. Anchoring the fixture to the reader's own clock is what
// makes the disagreement show up as a failure.
var chatFixtureNow = time.Date(2026, 9, 12, 12, 0, 0, 0, time.Local)

// chatFixtureMsgs is a ChatList page in the service's own order: newest
// first. Two authors so author rendering cannot pass by accident.
var chatFixtureMsgs = []domain.ChatMessage{
	{ID: "m2", Author: "agent-beta", Body: "found the root cause", CreatedAt: chatFixtureNow},
	{ID: "m1", Author: "agent-alpha", Body: "investigating the flaky test", CreatedAt: chatFixtureNow.Add(-time.Minute)},
}

// boardWithChat builds a board page model with the given chat panel state.
func boardWithChat(chat *view.ChatPanel, open bool) view.BoardModel {
	return view.BoardModel{
		Project:  view.ProjectSummary{Key: "BMB", Name: "Test"},
		Columns:  []view.ColumnView{{Name: "Backlog", Kind: domain.KindBacklog}},
		Chat:     chat,
		ChatOpen: open,
	}
}

// TestChatPanel_RendersMessagesWithAuthorsAndTimes: the panel carries every
// message with its author and its time — the two things the owner needs to
// tell one agent's thought from another's.
func TestChatPanel_RendersMessagesWithAuthorsAndTimes(t *testing.T) {
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "next-cursor-str", chatFixtureNow, nil, nil), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))

	for _, want := range []string{
		"agent-alpha", "agent-beta",
		"investigating the flaky test", "found the root cause",
		// KANB-25 item E: absolute HH:MM, not a relative age — both fixture
		// messages fall on the same calendar day as chatFixtureNow.
		"11:59", "12:00",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("panel is missing %q", want)
		}
	}
	// The full timestamp rides along as the hover title. Unescaped first:
	// html/template writes the "+" of a zone offset as &#43; inside an
	// attribute, so a fixture on the reader's own clock would otherwise fail
	// this on the escaping rather than on the timestamp.
	if !strings.Contains(gohtml.UnescapeString(html), `title="`+chatFixtureNow.Format(time.RFC3339)+`"`) {
		t.Fatal("panel does not carry the full timestamp for the hover title")
	}
	// The keyset cursor for older messages is preserved for the next task,
	// not swallowed.
	if !strings.Contains(html, `data-chat-next-cursor="next-cursor-str"`) {
		t.Fatal("panel lost the keyset cursor for older messages")
	}
}

// TestChatFeed_NewestFirstAtTop pins the feed order after KANB-23 reversed
// it: the newest message is the FIRST one rendered — the owner asked for the
// most current thought at the top, not the bottom. This REPLACES the old
// TestChatFeed_NewestLastAtBottom, which pinned the opposite (and now
// obsolete) order; it is a deliberate rewrite, not a weakened test — see
// REPORT.md.
func TestChatFeed_NewestFirstAtTop(t *testing.T) {
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil, nil), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))

	newest := strings.Index(html, "found the root cause")
	older := strings.Index(html, "investigating the flaky test")
	if newest < 0 || older < 0 {
		t.Fatalf("messages missing from the panel (newest at %d, older at %d)", newest, older)
	}
	if newest > older {
		t.Fatal("feed renders oldest first; the newest thought must be first, at the top")
	}
}

// TestChatPanel_EmptyProjectShowsSaneEmptiness: a project without messages
// opens the panel into a clear empty state — not an error, not a broken
// template, not a lie about messages existing.
//
// KANB-33: the panel is visible by default, so the previous assertion
// (the page must still carry the toggle button to open it) is gone: the
// panel itself renders. What still matters is that the empty state is
// explicit and that a failed chat read does NOT render a placeholder.
func TestChatPanel_EmptyProjectShowsSaneEmptiness(t *testing.T) {
	m := boardWithChat(view.NewChatPanel(nil, "", chatFixtureNow, nil, nil), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))
	if !strings.Contains(html, "No thoughts yet") {
		t.Fatalf("empty project panel does not explain itself: %s", html)
	}

	// A failed chat read (Chat == nil) renders no thoughts content — never
	// a panel pretending the chat is empty. The thoughts section is the
	// one zone that depends on Chat; the panel itself (with its charts
	// section) still renders, because the chart does not depend on the
	// chat read.
	none := boardWithChat(nil, false)
	noneHTML := renderProgress(t, "page-board", view.SamplePage("Test", "board", none))
	if strings.Contains(noneHTML, "No thoughts yet") || strings.Contains(noneHTML, `id="chat-feed"`) {
		t.Error("a failed chat read rendered the feed or its empty-state text, which would claim there are no messages")
	}
	if !strings.Contains(noneHTML, "data-progress-chart-slot") {
		t.Error("the chart slot went missing with the chat: the chart does not depend on the chat read")
	}
	// KANB-33: the panel always renders its chrome (charts section +
	// collapse control) so the user has somewhere to land an explicit
	// state, even with no chat.
	if !strings.Contains(noneHTML, "data-panel-collapse") {
		t.Error("a failed chat read lost the panel's own collapse control")
	}
}

// TestBoardLayout_PanelControlsRenderOnThePanel is KANB-33's structural
// contract: the panel renders its own controls (charts toggle, thoughts
// toggle, collapse panel) and they live on the panel — NOT in the project
// header — so the user's toggles never require a separate row to find.
// app.js wires the three buttons by data-attribute and persists the state
// per project in localStorage; the markup must carry the hooks.
func TestBoardLayout_PanelControlsRenderOnThePanel(t *testing.T) {
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil, nil), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))

	if !strings.Contains(html, `id="chat-panel"`) {
		t.Fatal("the panel must render (KANB-33: visible by default)")
	}
	if !strings.Contains(html, "data-charts-toggle") {
		t.Fatal("the panel is missing its charts toggle")
	}
	if !strings.Contains(html, "data-thoughts-toggle") {
		t.Fatal("the panel is missing its thoughts toggle")
	}
	if !strings.Contains(html, "data-panel-collapse") {
		t.Fatal("the panel is missing its collapse control")
	}
	// The expand button is always in the DOM (it lives outside the panel
	// so it can survive the panel collapsing); app.css hides it until the
	// panel is collapsed.
	if !strings.Contains(html, "data-panel-expand") {
		t.Fatal("the panel is missing its expand button")
	}
	// The board region renders alongside the panel by default.
	if !strings.Contains(html, `id="board"`) {
		t.Fatal("the board region must render")
	}
}

// TestChatMarkup_NoInlineStylesNoHandlers: the panel obeys the CSP like the
// rest of the page — no inline style attributes (the layout is pure CSS
// classes), no inline event handlers (the toggle is wired in app.js by
// data-attribute).
func TestChatMarkup_NoInlineStylesNoHandlers(t *testing.T) {
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil, nil), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))
	if strings.Contains(html, `style="`) || strings.Contains(html, `style='`) {
		t.Fatal("board page carries an inline style attribute")
	}
	handler := regexp.MustCompile(`\son[a-z]+\s*=`)
	if loc := handler.FindStringIndex(html); loc != nil {
		t.Fatalf("board page carries an inline event handler: %q", html[loc[0]:loc[1]])
	}
}

// TestChatEntriesNewestFirst_FullOrder pins the exact mapping
// ChatEntriesNewestFirst performs (not just "first differs from last", the
// way the render test above does): since KANB-23, a ChatList page arrives
// newest-first and stays newest-first — no reordering — with every field
// intact and no message dropped or duplicated. Both the initial panel and
// the "older messages" endpoint depend on this exact mapping. This REPLACES
// the old TestChatEntriesOldestFirst_FullOrder, which pinned the previous
// reversal; see REPORT.md.
func TestChatEntriesNewestFirst_FullOrder(t *testing.T) {
	// time.Local, not time.UTC: the panel prints the clock the READER goes
	// by (formatChartTime converts), so a fixture pinned in UTC would assert
	// a different string on every machine whose zone is not UTC. Building
	// the fixture in the same zone the renderer prints in is what makes the
	// expected "12:00" a property of the code rather than of the runner.
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.Local)
	msgs := []domain.ChatMessage{
		{ID: "m3", Author: "agent-c", Body: "third, newest", CreatedAt: now},
		{ID: "m2", Author: "agent-b", Body: "second", CreatedAt: now.Add(-time.Minute)},
		{ID: "m1", Author: "agent-a", Body: "first, oldest", CreatedAt: now.Add(-2 * time.Minute)},
	}
	entries := view.ChatEntriesNewestFirst(msgs, now, nil, nil)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	wantAuthors := []string{"agent-c", "agent-b", "agent-a"}
	wantBodies := []string{"third, newest", "second", "first, oldest"}
	for i, e := range entries {
		if e.Author != wantAuthors[i] {
			t.Errorf("entry %d: author = %q, want %q (order must match the service's own newest-first order)", i, e.Author, wantAuthors[i])
		}
		if e.Text != wantBodies[i] {
			t.Errorf("entry %d: text = %q, want %q", i, e.Text, wantBodies[i])
		}
	}
	// KANB-25 item E: When is now the absolute HH:MM, not a relative age.
	// entries[0] (agent-c) was posted exactly at now; entries[2] (agent-a)
	// two minutes earlier — both still the same calendar day as now.
	if entries[0].When != "12:00" {
		t.Errorf("entries[0].When = %q, want %q", entries[0].When, "12:00")
	}
	if entries[2].When != "11:58" {
		t.Errorf("entries[2].When = %q, want %q", entries[2].When, "11:58")
	}
}

// TestChatEntriesNewestFirst_Empty: an empty page maps to an empty (not
// nil-that-panics-on-range, not one-element) slice.
func TestChatEntriesNewestFirst_Empty(t *testing.T) {
	entries := view.ChatEntriesNewestFirst(nil, chatFixtureNow, nil, nil)
	if len(entries) != 0 {
		t.Fatalf("got %d entries for an empty page, want 0", len(entries))
	}
}

// TestChatPanel_LatestAndSilence (KANB-39): the panel carries the time
// the most recent post landed and the human-readable gap to "now", so a
// long pause is visible without an invented "online" indicator. The
// panel header prints both.
//
// Newest-first ordering is preserved by ChatEntriesNewestFirst, so
// entries[0] is always the latest post and its CreatedAt is what the
// panel header calls "last activity".
func TestChatPanel_LatestAndSilence(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 30, 0, 0, time.Local)
	older := time.Date(2026, 9, 12, 12, 0, 0, 0, time.Local)  // 30m earlier
	oldest := time.Date(2026, 9, 12, 11, 0, 0, 0, time.Local) // 90m earlier
	msgs := []domain.ChatMessage{
		{ID: "m3", Author: "agent-c", Body: "newest", CreatedAt: now},
		{ID: "m2", Author: "agent-b", Body: "middle", CreatedAt: older},
		{ID: "m1", Author: "agent-a", Body: "oldest", CreatedAt: oldest},
	}
	// The panel's silence is computed against `now`, not against the
	// newest entry's time — it asks "how long since the last post",
	// so the panel needs the page renderer's "now", passed here.
	panel := view.NewChatPanel(msgs, "", now, nil, nil)
	if panel.LatestAt.IsZero() {
		t.Fatal("LatestAt is zero — the panel did not capture the latest entry's CreatedAt")
	}
	if !panel.LatestAt.Equal(now) {
		t.Fatalf("LatestAt = %v, want %v (the newest entry's CreatedAt)", panel.LatestAt, now)
	}
	if panel.Silence != "just now" {
		t.Fatalf("Silence = %q, want \"just now\" for a 0s gap", panel.Silence)
	}
}

// TestChatPanel_SilenceFormatsGapsHumanly pins the human-readable form
// the panel header uses: "just now" under a minute, "Nm" under an hour,
// "Nh" under a day, "Nd" beyond. Same vocabulary the activity feed uses,
// so the two cannot drift apart.
func TestChatPanel_SilenceFormatsGapsHumanly(t *testing.T) {
	cases := []struct {
		gap  time.Duration
		want string
	}{
		{0, "just now"},
		{30 * time.Second, "just now"},
		{1 * time.Minute, "1m ago"},
		{30 * time.Minute, "30m ago"},
		{1 * time.Hour, "1h ago"},
		{2 * time.Hour, "2h ago"},
		{24 * time.Hour, "1d ago"},
		{5 * 24 * time.Hour, "5d ago"},
	}
	for _, c := range cases {
		now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.Local)
		msgs := []domain.ChatMessage{
			{ID: "m1", Author: "x", Body: "single post", CreatedAt: now.Add(-c.gap)},
		}
		panel := view.NewChatPanel(msgs, "", now, nil, nil)
		if panel.Silence != c.want {
			t.Errorf("gap %v: Silence = %q, want %q", c.gap, panel.Silence, c.want)
		}
	}
}

// TestChatPanel_EmptyHasNoLatestOrSilence: an empty page does NOT report
// a last activity time. Rendering "last at HH:MM · 0s ago" for a panel
// with zero entries would be a worse lie than no line at all — there is
// no last activity to report, so the template's `{{if .Chat.LatestAt}}`
// guard skips the whole element.
func TestChatPanel_EmptyHasNoLatestOrSilence(t *testing.T) {
	panel := view.NewChatPanel(nil, "", chatFixtureNow, nil, nil)
	if !panel.LatestAt.IsZero() {
		t.Fatalf("LatestAt = %v, want zero for an empty panel", panel.LatestAt)
	}
	if panel.Silence != "" {
		t.Fatalf("Silence = %q, want empty for an empty panel", panel.Silence)
	}
}

// TestBoardPage_ThoughtsHeaderShowsLastAndSilence (KANB-39): the board
// page renders "last HH:MM · silence" inside the thoughts panel header
// when there are messages, and renders NEITHER (not "0s ago") when
// there are none. A "online" dot is forbidden by the brief.
func TestBoardPage_ThoughtsHeaderShowsLastAndSilence(t *testing.T) {
	withMsgs := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil, nil), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", withMsgs))

	if !strings.Contains(html, "chat-panel-silence") {
		t.Fatal("the thoughts panel header is missing its silence line (KANB-39)")
	}
	if !strings.Contains(html, "last ") {
		t.Fatal("the silence line is missing the 'last' prefix")
	}
	// The full RFC3339 of the most recent message lands in the
	// hover title — same rule the existing chat-entry uses, so the
	// two cannot show different times for the same instant.
	if !strings.Contains(gohtml.UnescapeString(html), chatFixtureMsgs[0].CreatedAt.Format(time.RFC3339)) {
		t.Fatal("the silence line lost the RFC3339 hover title")
	}
	// KANB-39: NO online indicator — neither a CSS class the owner
	// could mistake for a status dot, nor a data-attribute the board
	// might later wire to one. The literal word "online" CAN appear
	// inside the agent-setup prompt text ("...never shows an online
	// dot...") — that is the prompt explaining WHY there is no such
	// indicator, not the indicator itself. The check below looks for
	// the shapes the indicator would actually take.
	for _, marker := range []string{
		"online-dot", "agent-active", "agent-active-dot",
		"is-online", "agent-live", "is-live-agent",
		"data-online", "data-agent-active",
	} {
		if strings.Contains(html, marker) {
			t.Fatalf("page carries a forbidden %q online-indicator shape", marker)
		}
	}

	empty := boardWithChat(view.NewChatPanel(nil, "", chatFixtureNow, nil, nil), true)
	emptyHTML := renderProgress(t, "page-board", view.SamplePage("Test", "board", empty))
	// The chat-panel-silence class only appears when LatestAt is non-zero.
	// The page also has the "online" word in the standing prompt (the
	// prompt explains why there is no online indicator), so checking
	// for the literal "online" would always trip — look for the class
	// name that the silence line carries.
	if strings.Contains(emptyHTML, "chat-panel-silence") {
		t.Fatal("an empty panel still rendered the silence line (KANB-39: no last activity to report)")
	}
}

// TestChatTextEscaped: message bodies are stored verbatim by the service and
// must arrive on the page as TEXT — a script tag is escaped and inert, an
// ampersand is encoded, and any non-ASCII alphabet passes through untouched.
func TestChatTextEscaped(t *testing.T) {
	msgs := []domain.ChatMessage{
		{ID: "m1", Author: "agent-x", Body: `<script>alert(1)</script> A & B καλημέρα`, CreatedAt: chatFixtureNow},
	}
	m := boardWithChat(view.NewChatPanel(msgs, "", chatFixtureNow, nil, nil), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))

	if strings.Contains(html, "<script>alert(1)") {
		t.Fatal("message body rendered unescaped: a script tag reached the page live")
	}
	for _, want := range []string{
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		"A &amp; B",
		"καλημέρα",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("escaped body lost %q", want)
		}
	}
}

// TestChatPanel_HeaderAndFeedAgreeOnTheClock (KANB-39 item 4): the "last
// HH:MM" in the panel header and the timestamp on the entry it describes must
// name the same reading.
//
// They did not. The feed's timestamp goes through formatChartTime, which
// converts to the reader's zone; the header printed CreatedAt raw, and chat
// messages are stored in UTC. The result was "last 22:00" sitting directly
// above a post stamped 00:00 — two clocks on one line, with nothing saying
// which was which.
//
// The fixture builds its instant in a zone that is deliberately NOT the test
// machine's, so the assertion means the same thing on a laptop in Warsaw and
// on a UTC build agent. A fixture on UTC is exactly what hid this.
func TestChatPanel_HeaderAndFeedAgreeOnTheClock(t *testing.T) {
	// A fixed offset far from any plausible local zone, so raw-vs-converted
	// can never coincide by luck.
	odd := time.FixedZone("ODD", 7*3600+1800) // +07:30
	posted := time.Date(2026, 9, 12, 23, 40, 0, 0, odd)
	now := posted.Add(90 * time.Minute)

	msgs := []domain.ChatMessage{
		{ID: "m1", Author: "agent-alpha", Body: "the only post", CreatedAt: posted},
	}
	panel := view.NewChatPanel(msgs, "", now, nil, nil)

	want := posted.Local().Format("15:04")
	if got := panel.LatestAt.Format("15:04"); got != want {
		t.Errorf("the panel header names %q as the last activity, the reader's clock says %q.\n"+
			"The feed converts the same instant (formatChartTime does), so the header must too — "+
			"otherwise the two sit side by side showing different times for one post.", got, want)
	}

	m := boardWithChat(panel, true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))
	if !strings.Contains(html, "last "+want) {
		t.Errorf("the rendered header does not carry %q; the page shows a different clock than the feed", "last "+want)
	}
}
