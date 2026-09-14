package view_test

import (
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// ---------------------------------------------------------------------------
// KANB-33: the expand button must be a SIBLING of the panel, never nested
// inside it.
//
// TestBoardLayout_PanelControlsRenderOnThePanel already proves the button
// EXISTS. That is not enough, and the gap is not cosmetic: the panel is
// collapsed by `.board-split.is-collapsed .chat-panel { display: none }`, so a
// button nested inside the panel disappears together with the thing it is
// supposed to bring back. The user collapses the panel once and there is
// nothing left on screen to expand it with — the state is unrecoverable
// without clearing localStorage by hand.
//
// The card forbids this in as many words -- it says the button must not be
// hidden away inside the panel it reveals -- and so does the comment in
// pages.html. Moving the button inside the panel used to pass every test in
// the suite.
// ---------------------------------------------------------------------------

func TestBoardLayout_ExpandButtonIsASiblingOfThePanel(t *testing.T) {
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))

	expand := strings.Index(html, "data-panel-expand")
	if expand < 0 {
		t.Fatal("the expand button is missing entirely (KANB-33 item 3)")
	}

	start, end := elementSpan(t, html, `id="chat-panel"`, "aside")
	if expand > start && expand < end {
		t.Fatalf("the expand button is nested INSIDE the panel (offset %d, panel spans %d..%d).\n"+
			"The panel is hidden with display:none when collapsed, so a nested button vanishes with it "+
			"and the panel can never be expanded again. Render it as a sibling of the panel, at the "+
			"panel's own edge.", expand, start, end)
	}
}

// elementSpan returns the byte range of the element that carries marker, from
// the "<" of its opening tag to just past its closing tag. tag is the element
// name so nesting of the same element can be counted correctly.
func elementSpan(t *testing.T, html, marker, tag string) (int, int) {
	t.Helper()

	at := strings.Index(html, marker)
	if at < 0 {
		t.Fatalf("marker %q not found in the rendered page", marker)
	}
	start := strings.LastIndex(html[:at], "<"+tag)
	if start < 0 {
		t.Fatalf("no opening <%s for marker %q", tag, marker)
	}

	open, close := "<"+tag, "</"+tag+">"
	depth := 0
	i := start
	for i < len(html) {
		nextOpen := strings.Index(html[i:], open)
		nextClose := strings.Index(html[i:], close)
		if nextClose < 0 {
			t.Fatalf("unclosed <%s in the rendered page", tag)
		}
		if nextOpen >= 0 && nextOpen < nextClose {
			depth++
			i += nextOpen + len(open)
			continue
		}
		depth--
		i += nextClose + len(close)
		if depth == 0 {
			return start, i
		}
	}
	t.Fatalf("could not find the end of <%s", tag)
	return 0, 0
}
