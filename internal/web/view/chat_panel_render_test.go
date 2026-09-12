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
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "next-cursor-str", chatFixtureNow), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))

	for _, want := range []string{
		"agent-alpha", "agent-beta",
		"investigating the flaky test", "found the root cause",
		"1m ago", "just now",
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

// TestChatFeed_NewestLastAtBottom pins the feed order: the newest message is
// the LAST one rendered, like a chat window — the owner's eye rests at the
// bottom of the panel, and that is where a new thought must land. app.js
// autoscrolls the panel to its bottom (see the "ai thoughts panel" section
// of app.js), which only reads right if the newest entry is actually last
// in the DOM.
func TestChatFeed_NewestLastAtBottom(t *testing.T) {
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))

	newest := strings.Index(html, "found the root cause")
	older := strings.Index(html, "investigating the flaky test")
	if newest < 0 || older < 0 {
		t.Fatalf("messages missing from the panel (newest at %d, older at %d)", newest, older)
	}
	if newest < older {
		t.Fatal("feed renders newest first; the newest thought must be last, at the bottom")
	}
}

// TestChatPanel_EmptyProjectShowsSaneEmptiness: a project without messages
// opens the panel into a clear empty state — not an error, not a broken
// template, not a lie about messages existing.
func TestChatPanel_EmptyProjectShowsSaneEmptiness(t *testing.T) {
	m := boardWithChat(view.NewChatPanel(nil, "", chatFixtureNow), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))
	if !strings.Contains(html, "No thoughts yet") {
		t.Fatalf("empty project panel does not explain itself: %s", html)
	}
	if !strings.Contains(html, `data-chat-toggle`) {
		t.Fatal("empty project still needs the toggle button: the panel must open")
	}

	// A failed chat read (Chat == nil) renders no panel and no button —
	// the board alone, never a panel pretending the chat is empty.
	none := boardWithChat(nil, false)
	noneHTML := renderProgress(t, "page-board", view.SamplePage("Test", "board", none))
	if strings.Contains(noneHTML, "data-chat-toggle") || strings.Contains(noneHTML, `id="chat-panel"`) {
		t.Fatal("a failed chat read must not render a panel or its button")
	}
}

// TestBoardLayout_ClosedVsOpenMarkup: the closed page is distinguishable
// from the open one in markup alone — the wrapper carries is-open and the
// panel loses hidden only when the split is on. The server always renders
// closed; app.js flips it.
func TestBoardLayout_ClosedVsOpenMarkup(t *testing.T) {
	closed := renderProgress(t, "page-board",
		view.SamplePage("Test", "board", boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow), false)))
	open := renderProgress(t, "page-board",
		view.SamplePage("Test", "board", boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow), true)))

	if !strings.Contains(closed, `class="board-split" data-chat-split`) {
		t.Fatalf("closed page lost the plain split wrapper: %s", closed[:0])
	}
	if !strings.Contains(open, `class="board-split is-open" data-chat-split`) {
		t.Fatal("open page wrapper does not carry is-open")
	}
	if !strings.Contains(closed, `aria-label="AI thoughts" hidden`) {
		t.Fatal("closed page panel is not hidden")
	}
	if strings.Contains(open, `aria-label="AI thoughts" hidden`) {
		t.Fatal("open page panel is still hidden")
	}
	if !strings.Contains(closed, `id="board"`) || !strings.Contains(open, `id="board"`) {
		t.Fatal("the board region must render in both states")
	}
}

// TestChatMarkup_NoInlineStylesNoHandlers: the panel obeys the CSP like the
// rest of the page — no inline style attributes (the layout is pure CSS
// classes), no inline event handlers (the toggle is wired in app.js by
// data-attribute).
func TestChatMarkup_NoInlineStylesNoHandlers(t *testing.T) {
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))
	if strings.Contains(html, `style="`) || strings.Contains(html, `style='`) {
		t.Fatal("board page carries an inline style attribute")
	}
	handler := regexp.MustCompile(`\son[a-z]+\s*=`)
	if loc := handler.FindStringIndex(html); loc != nil {
		t.Fatalf("board page carries an inline event handler: %q", html[loc[0]:loc[1]])
	}
}

// TestChatEntriesOldestFirst_FullOrder pins the exact reordering
// ChatEntriesOldestFirst performs (not just "first differs from last", the
// way the render test above does): a ChatList page arrives newest-first and
// must come out fully reversed, oldest first, with every field intact and
// no message dropped or duplicated. Both the initial panel and the "older
// messages" endpoint depend on this exact mapping.
func TestChatEntriesOldestFirst_FullOrder(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	msgs := []domain.ChatMessage{
		{ID: "m3", Author: "agent-c", Body: "third, newest", CreatedAt: now},
		{ID: "m2", Author: "agent-b", Body: "second", CreatedAt: now.Add(-time.Minute)},
		{ID: "m1", Author: "agent-a", Body: "first, oldest", CreatedAt: now.Add(-2 * time.Minute)},
	}
	entries := view.ChatEntriesOldestFirst(msgs, now)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	wantAuthors := []string{"agent-a", "agent-b", "agent-c"}
	wantBodies := []string{"first, oldest", "second", "third, newest"}
	for i, e := range entries {
		if e.Author != wantAuthors[i] {
			t.Errorf("entry %d: author = %q, want %q (order not fully reversed)", i, e.Author, wantAuthors[i])
		}
		if e.Text != wantBodies[i] {
			t.Errorf("entry %d: text = %q, want %q", i, e.Text, wantBodies[i])
		}
	}
	// The oldest message is 2 minutes behind now.
	if entries[0].When != "2m ago" {
		t.Errorf("entries[0].When = %q, want %q", entries[0].When, "2m ago")
	}
	if entries[2].When != "just now" {
		t.Errorf("entries[2].When = %q, want %q", entries[2].When, "just now")
	}
}

// TestChatEntriesOldestFirst_Empty: an empty page maps to an empty (not
// nil-that-panics-on-range, not one-element) slice.
func TestChatEntriesOldestFirst_Empty(t *testing.T) {
	entries := view.ChatEntriesOldestFirst(nil, chatFixtureNow)
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
	m := boardWithChat(view.NewChatPanel(msgs, "", chatFixtureNow), true)
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
