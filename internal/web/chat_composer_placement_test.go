package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The composer sits at the TOP of the thoughts section, starts hidden, and is
// revealed by the header's "write" button.
//
// It used to be the last thing in the section, after the whole feed. The panel
// has no scroll region of its own (owner's call), so on a long feed writing a
// message meant scrolling past every message on the page to reach the box —
// and the feed renders newest first, so the box was furthest from the content
// a reader had just been looking at.
//
// Three properties, none of which a "the composer renders" test would catch:
//
//  1. it is above the feed in document order;
//  2. it starts hidden, so it costs no vertical space until asked for;
//  3. it is hidden with the hidden ATTRIBUTE, never removed and never emptied
//     — app.js owns draft survival, and a composer that is torn out of the DOM
//     when toggled away takes the half-written message with it.
// ---------------------------------------------------------------------------

func TestPages_ComposerSitsAboveTheFeedAndStartsHidden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("templates", "pages.html"))
	if err != nil {
		t.Fatalf("read pages.html: %v", err)
	}
	tpl := string(raw)

	composerAt := strings.Index(tpl, `data-chat-compose>`)
	if composerAt < 0 {
		composerAt = strings.Index(tpl, "data-chat-compose ")
	}
	feedAt := strings.Index(tpl, `id="chat-feed"`)
	if composerAt < 0 || feedAt < 0 {
		t.Fatalf("markers missing: composer=%d feed=%d", composerAt, feedAt)
	}
	if composerAt > feedAt {
		t.Errorf("the composer (%d) renders after the feed (%d): on a long feed that puts the box a whole page of scrolling away", composerAt, feedAt)
	}

	form := tpl[composerAt:]
	if end := strings.Index(form, ">"); end >= 0 {
		form = form[:end+1]
	}
	if !strings.Contains(form, "hidden") {
		t.Errorf("the composer does not render hidden, so it takes space before anyone asks to write:\n%s", form)
	}

	if !strings.Contains(tpl, "data-chat-compose-toggle") {
		t.Fatal(`no "write" toggle in the thoughts header: a composer that starts hidden with nothing to reveal it cannot be used at all`)
	}
	// The toggle must be rendered only where a composer exists, or a
	// read-only session gets a button for a form it does not have.
	toggleAt := strings.Index(tpl, "data-chat-compose-toggle")
	if !strings.Contains(tpl[:toggleAt], "{{if .Chat.Compose}}") && !strings.Contains(tpl[maxInt(0, toggleAt-220):toggleAt], "{{if .Chat.Compose}}") {
		t.Errorf("the write toggle is not guarded by {{if .Chat.Compose}}: a read-only session would get a button for a form it never receives")
	}
}

func TestAppJS_WriteToggleHidesWithoutDestroyingTheDraft(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	raw, err := fs.ReadFile(sub, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	js := string(raw)

	if !strings.Contains(js, "data-chat-compose-toggle") {
		t.Fatal("app.js does not wire [data-chat-compose-toggle]: the write button renders and does nothing")
	}
	if !strings.Contains(js, "compose.hidden = !composeShown") {
		t.Errorf("the composer is not shown/hidden through the hidden attribute; anything that removes or empties the form loses a half-written draft")
	}
	// Whatever else the toggle does, it must not tear the form out.
	for _, destructive := range []string{
		"compose.remove()",
		"compose.innerHTML = ''",
	} {
		if strings.Contains(js, destructive) {
			t.Errorf("app.js contains %q: toggling the composer away would destroy the draft", destructive)
		}
	}
	// The state has to be persisted alongside the other panel fields, or the
	// choice is forgotten on every refresh the board makes on its own.
	if !strings.Contains(js, "compose: 'hidden'") {
		t.Errorf("the panel's default state carries no compose field: the composer's visibility is then not persisted with the rest of the panel")
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
