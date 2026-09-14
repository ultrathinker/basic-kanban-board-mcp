package view_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// chatFixtureNow anchors the fixture timestamps; the relative ages in the
// rendered feed are computed against it.
var chatFixtureNow = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

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
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "next-cursor-str", chatFixtureNow, nil), true)
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
	// The full timestamp rides along as the hover title.
	if !strings.Contains(html, `title="`+chatFixtureNow.Format(time.RFC3339)+`"`) {
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
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil), true)
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
	m := boardWithChat(view.NewChatPanel(nil, "", chatFixtureNow, nil), true)
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
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil), true)
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
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil), true)
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
	entries := view.ChatEntriesNewestFirst(msgs, now, nil)
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
	entries := view.ChatEntriesNewestFirst(nil, chatFixtureNow, nil)
	if len(entries) != 0 {
		t.Fatalf("got %d entries for an empty page, want 0", len(entries))
	}
}

// TestChatTextEscaped: message bodies are stored verbatim by the service and
// must arrive on the page as TEXT — a script tag is escaped and inert, an
// ampersand is encoded, and any non-ASCII alphabet passes through untouched.
func TestChatTextEscaped(t *testing.T) {
	msgs := []domain.ChatMessage{
		{ID: "m1", Author: "agent-x", Body: `<script>alert(1)</script> A & B καλημέρα`, CreatedAt: chatFixtureNow},
	}
	m := boardWithChat(view.NewChatPanel(msgs, "", chatFixtureNow, nil), true)
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
