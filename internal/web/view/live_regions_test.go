package view_test

import (
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// KANB-12: the board page must expose a live-region target for the two
// project-level progress bars (they sit in the page header, outside #board,
// so without their own wrapper a project-level progress.recorded event would
// have nowhere to refresh) and for the thoughts feed. These tests pin the
// markup app.js's refreshLiveRegions and appendNewChatEntries depend on —
// not just "the page renders", but the exact ids/attributes a canary could
// silently drop.

// boardModelWithProjectProgress builds a board model carrying both
// project-level progress metrics, the manual one built Clickable (WithTracks)
// so it also exercises the chart-toggle attribute the same as a real,
// assessed metric would.
func boardModelWithProjectProgress() view.BoardModel {
	manual := view.NewAssessedProgress(percentPtr(42), 2, nil, "").
		WithTracks("BMB", "", []view.ProgressTrack{{Assessor: "alpha", Percent: 42, Count: 1}})
	auto := view.NewDoneShareProgress(percentPtr(50), 1, 2)
	return view.BoardModel{
		Project:        view.ProjectSummary{Key: "BMB", Name: "Test"},
		Columns:        []view.ColumnView{{Name: "Backlog", Kind: domain.KindBacklog}},
		ManualProgress: manual,
		AutoProgress:   auto,
	}
}

// TestBoardPage_ProjectProgressHasOwnLiveRegion pins the #project-progress
// wrapper: both header bars must render inside one element with that id, and
// the page must declare a live region targeting it — otherwise a project-
// level progress.recorded event has no swap target at all and the "assessed"
// bar can never move without a full reload.
func TestBoardPage_ProjectProgressHasOwnLiveRegion(t *testing.T) {
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", boardModelWithProjectProgress()))

	if !strings.Contains(html, `id="project-progress"`) {
		t.Fatal(`page-board is missing id="project-progress" — the header progress bars have no stable live-region target`)
	}
	if !strings.Contains(html, `data-live-region="#project-progress"`) {
		t.Fatal(`page-board never declares data-live-region="#project-progress" — app.js's liveRegions() will never refresh the header bars`)
	}

	// Both bars must still be inside the wrapper, in the original order —
	// wrapping must not have dropped or reordered either metric.
	wrapStart := strings.Index(html, `id="project-progress"`)
	if wrapStart < 0 {
		t.Fatal("wrapper not found")
	}
	assessedIdx := strings.Index(html, "assessed")
	doneIdx := strings.Index(html, "tasks done")
	if assessedIdx < 0 || doneIdx < 0 {
		t.Fatal("one or both project-level progress labels are missing from the render")
	}
	if assessedIdx > doneIdx {
		t.Fatal("assessed/tasks done bars were reordered by the wrapper")
	}
	if assessedIdx < wrapStart {
		t.Fatal("the assessed bar rendered outside the #project-progress wrapper")
	}
}

// TestBoardPage_ChatFeedIsAlwaysPresentAsALiveRegion covers the empty-panel
// edge case KANB-12 has to handle: a project with zero chat history must
// still render an addressable #chat-feed (hidden, since there is nothing to
// show yet) so the very first live message can be appended without a reload.
// It also pins the live-region declaration and the empty-state placeholder
// wired to reappear/disappear via data-chat-empty.
func TestBoardPage_ChatFeedIsAlwaysPresentAsALiveRegion(t *testing.T) {
	empty := boardWithChat(view.NewChatPanel(nil, "", chatFixtureNow, nil), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", empty))

	if !strings.Contains(html, `id="chat-feed"`) {
		t.Fatal(`an empty chat panel must still render id="chat-feed" — app.js needs somewhere to land the first live message`)
	}
	if !strings.Contains(html, `data-live-region="#chat-feed"`) {
		t.Fatal(`page-board never declares data-live-region="#chat-feed"`)
	}
	if !strings.Contains(html, `data-chat-empty`) {
		t.Fatal(`the "No thoughts yet" placeholder must carry data-chat-empty so app.js can hide it once the first message lands`)
	}
	// The <ol> itself must be hidden while empty (nothing to show), and the
	// placeholder visible — the opposite of the non-empty case below.
	olIdx := strings.Index(html, `id="chat-feed"`)
	olTagEnd := strings.Index(html[olIdx:], ">")
	if olTagEnd < 0 {
		t.Fatal("could not find the end of the chat-feed <ol> tag")
	}
	olTag := html[olIdx : olIdx+olTagEnd]
	if !strings.Contains(olTag, "hidden") {
		t.Fatalf("empty chat-feed <ol> is not hidden: %q", olTag)
	}

	// With messages present, the relationship flips: the feed is visible and
	// the placeholder is the one carrying hidden.
	withMsgs := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil), true)
	htmlWithMsgs := renderProgress(t, "page-board", view.SamplePage("Test", "board", withMsgs))
	olIdx2 := strings.Index(htmlWithMsgs, `id="chat-feed"`)
	olTagEnd2 := strings.Index(htmlWithMsgs[olIdx2:], ">")
	olTag2 := htmlWithMsgs[olIdx2 : olIdx2+olTagEnd2]
	if strings.Contains(olTag2, "hidden") {
		t.Fatalf("non-empty chat-feed <ol> must not be hidden: %q", olTag2)
	}
	emptyPIdx := strings.Index(htmlWithMsgs, "data-chat-empty")
	// The placeholder <p> tag: find its closing '>' to inspect its attributes.
	pTagEnd := strings.Index(htmlWithMsgs[emptyPIdx:], ">")
	pTag := htmlWithMsgs[emptyPIdx : emptyPIdx+pTagEnd]
	if !strings.Contains(pTag, "hidden") {
		t.Fatalf("placeholder must be hidden once real messages exist: %q", pTag)
	}
}

// TestChatEntry_CarriesDataChatID pins the client-side dedup key: without
// data-chat-id on every rendered <li>, app.js's appendNewChatEntries cannot
// tell a genuinely new message apart from one already on screen, and a live
// refresh would either duplicate every message or append none at all.
func TestChatEntry_CarriesDataChatID(t *testing.T) {
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil), true)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))

	for _, msg := range chatFixtureMsgs {
		want := `data-chat-id="` + msg.ID + `"`
		if !strings.Contains(html, want) {
			t.Fatalf("rendered feed is missing %q", want)
		}
	}
}
