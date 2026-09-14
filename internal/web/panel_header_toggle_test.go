package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The panel's own control must exist OUTSIDE the panel, in the header row.
//
// Before this, the only whole-panel control was the "collapse" button in the
// panel's footer. The panel is routinely taller than the viewport — on the
// board this test was written against it measured 2288px — so the one action
// that hides the panel was reachable only by scrolling past everything it
// hides. The controls a reader did see were the per-section "hide" buttons in
// the section headers, which look like the panel-level control and are not:
// clicking them collapses a section and leaves the panel exactly where it was.
//
// Two properties are load-bearing and neither is visible to a render test that
// only asks "does a toggle exist":
//
//  1. the toggle is in the page header, not inside .board-split — a control
//     inside the thing it hides cannot bring it back (the same trap already
//     closed once for the sections, see static_css_section_toggle_test.go);
//  2. its label is driven by state in app.js, so it reads "Show panel" while
//     collapsed. A fixed label would leave the collapsed board offering a
//     button that says "Hide panel" over a panel that is already hidden.
// ---------------------------------------------------------------------------

func TestPages_PanelToggleLivesInTheHeaderNotInThePanel(t *testing.T) {
	// The templates package keeps its embed unexported, so this reads the
	// same file from the internal mirror the embed compiles in. A drifted
	// web/templates copy is caught separately by the mirror test.
	raw, err := os.ReadFile(filepath.Join("templates", "pages.html"))
	if err != nil {
		t.Fatalf("read pages.html: %v", err)
	}
	tpl := string(raw)

	if !strings.Contains(tpl, "data-panel-toggle") {
		t.Fatalf("no data-panel-toggle in pages.html: the only whole-panel control is then the footer one, at the bottom of a panel taller than the screen")
	}

	// The toggle must appear BEFORE the split opens, i.e. inside the page
	// header. Anything inside .board-split disappears with the panel.
	toggleAt := strings.Index(tpl, "data-panel-toggle")
	splitAt := strings.Index(tpl, "data-chat-split")
	if splitAt < 0 {
		t.Fatalf("data-chat-split not found; this test no longer knows where the panel region starts")
	}
	if toggleAt > splitAt {
		t.Errorf("data-panel-toggle is inside or after the board split (toggle at %d, split at %d): a control that collapses with the panel cannot reopen it", toggleAt, splitAt)
	}

	// It must carry aria-controls pointing at the panel, so the relationship
	// is announced rather than merely visual.
	head := tpl[:splitAt]
	btn := regexp.MustCompile(`<button[^>]*data-panel-toggle[^>]*>`).FindString(head)
	if btn == "" {
		t.Fatalf("data-panel-toggle is not on a <button> in the header")
	}
	if !strings.Contains(btn, `aria-controls="chat-panel"`) {
		t.Errorf("the header toggle does not declare aria-controls=\"chat-panel\":\n%s", btn)
	}
	if !strings.Contains(btn, "aria-expanded") {
		t.Errorf("the header toggle has no aria-expanded, so its state is invisible to a screen reader:\n%s", btn)
	}
}

func TestAppJS_PanelToggleLabelFollowsTheState(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	raw, err := fs.ReadFile(sub, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	js := string(raw)

	for _, want := range []string{"Show panel", "Hide panel"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js never sets the label %q: the header toggle then keeps one fixed label in both states", want)
		}
	}
	if !strings.Contains(js, "data-panel-toggle") {
		t.Fatalf("app.js does not wire [data-panel-toggle] at all: the button renders and does nothing")
	}
	// The click handler must flip the current state, not force one direction:
	// a button that only collapses is the footer button again.
	if !strings.Contains(js, "!currentPanelState().collapsed") {
		t.Errorf("the header toggle does not flip the current collapsed state; it must work in both directions from outside the panel")
	}
}

func TestAppCSS_SectionHeaderWrapsInsteadOfSheddingItsButton(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	raw, err := fs.ReadFile(sub, "app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := topLevelCSS(string(raw))

	bodies := bareRule(css, ".panel-section-head")
	if len(bodies) == 0 {
		t.Fatalf(".panel-section-head has no rule at all")
	}
	wraps := false
	for _, b := range bodies {
		if regexp.MustCompile(`flex-wrap\s*:\s*wrap`).MatchString(b) {
			wraps = true
		}
	}
	if !wraps {
		t.Errorf(".panel-section-head does not set flex-wrap: wrap. The splitter has no minimum width by design, so a nowrap header pushes its toggle past the panel edge and lets the silence text run underneath as soon as the panel is dragged narrow")
	}
}
