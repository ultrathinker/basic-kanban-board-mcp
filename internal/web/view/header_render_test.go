package view_test

import (
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// KANB-19/KANB-20: the owner asked for two related changes to the project
// page header — the visible LIVE dot/word replaced by a static "Update"
// link (without losing the one polite live-region announcement screen
// readers get), and the progress metrics + Thoughts toggle moved off the
// title row onto their own centered row. These tests pin the rendered
// markup for both, the same way live_regions_test.go pins KANB-12's
// contract — not just "the page renders", but the exact structure app.js
// and a screen reader depend on.

// liveStatusSpan extracts the full opening tag of the live-status element
// so tests can inspect its attributes without caring about surrounding
// whitespace.
func liveStatusSpan(t *testing.T, html string) string {
	t.Helper()
	idx := strings.Index(html, `data-live-status`)
	if idx < 0 {
		t.Fatal("no element carrying data-live-status found")
	}
	// Walk back to the start of the tag and forward to its close.
	start := strings.LastIndex(html[:idx], "<span")
	if start < 0 {
		t.Fatal("data-live-status is not on a <span>")
	}
	end := strings.Index(html[start:], ">")
	if end < 0 {
		t.Fatal("unterminated live-status tag")
	}
	return html[start : start+end]
}

// TestBoardPage_LiveIndicatorIsGoneButAnnouncerSurvives is KANB-19's core
// contract: the visible dot is gone from the markup entirely, the
// live-status element is now sr-only (so the owner sees nothing there), but
// it is still the same role="status"/aria-live="polite" element app.js
// announces "updated" into — losing that would silently kill the page's
// only polite live region (see the "board-columns" comment in
// partials.html for why the board itself is deliberately not aria-live).
func TestBoardPage_LiveIndicatorIsGoneButAnnouncerSurvives(t *testing.T) {
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", boardModelWithProjectProgress()))

	if strings.Contains(html, `class="dot"`) {
		t.Fatal(`a visible dot element still renders — KANB-19 asked for it gone entirely`)
	}

	tag := liveStatusSpan(t, html)
	if !strings.Contains(tag, "sr-only") {
		t.Fatalf("live-status is not sr-only, so the owner would still see it: %q", tag)
	}
	if !strings.Contains(tag, `role="status"`) || !strings.Contains(tag, `aria-live="polite"`) {
		t.Fatalf("live-status lost its polite live-region contract: %q", tag)
	}
	if !strings.Contains(html, "data-live-label") {
		t.Fatal("data-live-label is missing — app.js has nowhere to write the 'updated' announcement text")
	}
}

// TestBoardPage_UpdateLinkReplacesLiveIndicator pins the new, static
// "Update" control: it must carry data-live-refresh (app.js's hook for the
// immediate refresh) and, for the no-JS case, a plain href back to this
// same board — not a bare "#" or a POST-only affordance.
func TestBoardPage_UpdateLinkReplacesLiveIndicator(t *testing.T) {
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", boardModelWithProjectProgress()))

	idx := strings.Index(html, "data-live-refresh")
	if idx < 0 {
		t.Fatal(`no element carries data-live-refresh — the static Update control is missing`)
	}
	start := strings.LastIndex(html[:idx], "<a ")
	if start < 0 {
		t.Fatal("data-live-refresh is not on an <a> — it must work as a plain link with JS off")
	}
	end := strings.Index(html[start:], "</a>")
	if end < 0 {
		t.Fatal("unterminated Update link")
	}
	link := html[start : start+end+len("</a>")]

	if !strings.Contains(link, `href="/p/BMB"`) {
		t.Fatalf("Update link's href does not point back at this board (no-JS fallback broken): %q", link)
	}
	if !strings.Contains(link, ">Update</a>") {
		t.Fatalf("Update link is missing its visible 'Update' label: %q", link)
	}
	// The link's visible text content (between its ">" and "</a>") must be
	// exactly "Update" — not "live", not "Update (live)", nothing that
	// would put the word back in front of the owner's eyes.
	visStart := strings.Index(link, ">") + 1
	visible := link[visStart:strings.Index(link, "</a>")]
	if visible != "Update" {
		t.Fatalf("Update link's visible text is %q, want exactly \"Update\"", visible)
	}
}

// TestActivityPage_LiveIndicatorGoneUpdateLinkPresent mirrors the two tests
// above for the Activity page header, which carries its own independent
// live-status + Update pair (partials.html's live-status is instantiated
// twice: board header and activity header).
func TestActivityPage_LiveIndicatorGoneUpdateLinkPresent(t *testing.T) {
	html := renderProgress(t, "page-activity", view.SamplePage("Test", "activity", view.SampleEmptyActivityModel()))

	if strings.Contains(html, `class="dot"`) {
		t.Fatal("activity page still renders a visible dot element")
	}
	tag := liveStatusSpan(t, html)
	if !strings.Contains(tag, "sr-only") {
		t.Fatalf("activity page's live-status is not sr-only: %q", tag)
	}
	if !strings.Contains(tag, `role="status"`) || !strings.Contains(tag, `aria-live="polite"`) {
		t.Fatalf("activity page's live-status lost its polite live-region contract: %q", tag)
	}

	idx := strings.Index(html, "data-live-refresh")
	if idx < 0 {
		t.Fatal("activity page has no Update control (data-live-refresh)")
	}
	start := strings.LastIndex(html[:idx], "<a ")
	end := strings.Index(html[start:], "</a>")
	link := html[start : start+end+len("</a>")]
	if !strings.Contains(link, `href="/p/BMB/activity"`) {
		t.Fatalf("activity page's Update link does not point back at the activity feed: %q", link)
	}
}

// TestBoardPage_ProgressAndThoughtsMoveToSubhead is KANB-20's structural
// contract: the project-progress group and the Thoughts toggle must have
// left .page-head for their own centered row (.page-subhead), while
// #project-progress's id, .proj-progress-group's live-region contract and
// data-chat-toggle's ability to open the panel all keep working — moving
// markup must not break what other code depends on finding it by id/attr.
func TestBoardPage_ProgressAndThoughtsMoveToSubhead(t *testing.T) {
	m := boardWithChat(view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil), true)
	// boardWithChat does not set progress metrics; attach both so this test
	// also exercises the "both present" layout, matching the real header.
	m.ManualProgress = view.NewAssessedProgress(percentPtr(42), 2, nil, "")
	m.AutoProgress = view.NewDoneShareProgress(percentPtr(50), 1, 2)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", m))

	headStart := strings.Index(html, `<header class="page-head">`)
	if headStart < 0 {
		t.Fatal("could not locate the page-head element")
	}
	// The topbar chrome above also opens/closes a <header>, so the search
	// for the CLOSING tag must start from page-head's own opening tag, not
	// from the top of the document.
	headEnd := headStart + strings.Index(html[headStart:], "</header>")
	if headEnd < headStart {
		t.Fatal("unterminated page-head element")
	}
	head := html[headStart:headEnd]

	if strings.Contains(head, `id="project-progress"`) {
		t.Fatal("#project-progress is still inside .page-head — KANB-20 asked for it moved to its own row")
	}
	if strings.Contains(head, "data-chat-toggle") {
		t.Fatal("the Thoughts toggle is still inside .page-head — KANB-20 asked for it moved alongside the progress group")
	}

	subStart := strings.Index(html, `class="page-subhead"`)
	if subStart < 0 {
		t.Fatal("no .page-subhead element found — the moved row is missing entirely")
	}
	subEnd := strings.Index(html[subStart:], "</div>")
	if subEnd < 0 {
		t.Fatal("unterminated .page-subhead element")
	}
	subhead := html[subStart : subStart+subEnd]

	if !strings.Contains(subhead, `id="project-progress"`) {
		t.Fatal("#project-progress did not land inside .page-subhead")
	}
	if !strings.Contains(subhead, "data-chat-toggle") {
		t.Fatal("the Thoughts toggle did not land inside .page-subhead")
	}
	// KANB-12's live-region contract must still resolve to the moved element:
	// #project-progress must appear exactly once in the whole document (the
	// id, not just the string), and the live-region declaration must still
	// target it.
	if strings.Count(html, `id="project-progress"`) != 1 {
		t.Fatalf("expected exactly one #project-progress element, found markup suggesting otherwise")
	}
	if !strings.Contains(html, `data-live-region="#project-progress"`) {
		t.Fatal("data-live-region=\"#project-progress\" declaration is missing after the move")
	}

	// The two progress captions must still both be present and in order,
	// inside the moved row.
	assessedIdx := strings.Index(subhead, "assessed")
	doneIdx := strings.Index(subhead, "tasks done")
	if assessedIdx < 0 || doneIdx < 0 || assessedIdx > doneIdx {
		t.Fatal("assessed/tasks done captions missing or reordered inside .page-subhead")
	}
}

// TestBoardPage_EmptySubheadRendersNothing is the degenerate case: a project
// with no progress metrics and no chat must not render an empty,
// content-free .page-subhead row — that would be a blank centered band for
// no reason, which the owner's minimalism rule does not allow.
func TestBoardPage_EmptySubheadRendersNothing(t *testing.T) {
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", view.SampleColumnlessBoardModel()))
	if strings.Contains(html, "page-subhead") {
		t.Fatal("an empty project still renders .page-subhead — it should render nothing when there is no progress metric and no chat")
	}
}
