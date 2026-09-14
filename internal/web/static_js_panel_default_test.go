package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// KANB-33, acceptance item 1: a brand-new project in a fresh browser shows
// the charts and the thoughts feed straight away.
//
// This is the whole reason the card exists — it reverses the long-standing
// "panel closed by default" design. It is also decided in one place that no
// Go test could see: defaultPanelState() in app.js. Inverting that literal to
// {charts:'hidden', thoughts:'hidden', collapsed:true} in BOTH mirrors used to
// leave the entire suite green, which means the card's headline promise was
// shipped unguarded and a later edit could quietly take it back.
//
// Go cannot execute the JS, but it can read the decision. This test parses the
// same embedded bytes the server serves — the same technique
// static_css_dialog_test.go uses for the stylesheet — so it also fails if the
// web/static mirror drifts and only the embedded copy is edited.
// ---------------------------------------------------------------------------

// defaultPanelStateRe captures the body of the defaultPanelState() function:
// everything between its opening brace and the first closing brace.
var defaultPanelStateRe = regexp.MustCompile(`function\s+defaultPanelState\s*\([^)]*\)\s*\{([^}]*\}[^}]*)\}`)

// panelFieldRe reads one field of the returned object literal, tolerating
// either quote style and any spacing, so reformatting app.js cannot turn this
// test red on its own.
func panelFieldRe(name string) *regexp.Regexp {
	return regexp.MustCompile(name + `\s*:\s*['"]?([A-Za-z]+)['"]?`)
}

func TestAppJS_PanelIsVisibleByDefault(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	raw, err := fs.ReadFile(sub, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	js := string(raw)

	m := defaultPanelStateRe.FindStringSubmatch(js)
	if m == nil {
		t.Fatal("defaultPanelState() not found in app.js.\n" +
			"KANB-33 item 1 (a new project shows charts and thoughts at once) is decided by that " +
			"function; if it was renamed or inlined, move this guard with it rather than dropping it.")
	}
	body := m[1]

	for _, want := range []struct {
		field string
		value string
		why   string
	}{
		{"charts", "shown", "the charts section must be open for a brand-new project"},
		{"thoughts", "shown", "the thoughts feed must be open for a brand-new project"},
		{"collapsed", "false", "the panel itself must not start collapsed"},
	} {
		fm := panelFieldRe(want.field).FindStringSubmatch(body)
		if fm == nil {
			t.Errorf("defaultPanelState() no longer sets %q at all (KANB-33 item 1: %s).\nbody was:%s",
				want.field, want.why, body)
			continue
		}
		if got := strings.TrimSpace(fm[1]); got != want.value {
			t.Errorf("defaultPanelState() has %s: %q, want %q.\n"+
				"KANB-33 item 1: %s. The card reverses the old closed-by-default design; "+
				"a new project with no stored preference must show both sections.",
				want.field, got, want.value, want.why)
		}
	}
}

// ---------------------------------------------------------------------------
// KANB-33 item 5: clicking the progress bar still opens the chart.
//
// The panel and the charts section can each be hidden on their own, and the
// chart is rendered INTO that section. toggleProgressChart already re-expanded
// a collapsed panel, for exactly the stated reason that "a chart rendered into
// a hidden panel would look like a click that did nothing" — but it did not
// check the section. With the charts section hidden the click was fully
// processed: the bar took aria-expanded="true" and the slot was filled, and
// nothing appeared on screen, because the element holding it was display:none.
//
// Go cannot click the bar, so the guard reads the handler instead: both ways
// of hiding the chart must be undone before it is fetched.
// ---------------------------------------------------------------------------

func TestAppJS_OpeningAChartRevealsWhateverIsHidingIt(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	raw, err := fs.ReadFile(sub, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	js := string(raw)

	const marker = "function toggleProgressChart("
	at := strings.Index(js, marker)
	if at < 0 {
		t.Fatal("toggleProgressChart not found in app.js; if it was renamed, move this guard with it")
	}
	// The handler ends where the next top-level function begins.
	rest := js[at+len(marker):]
	end := strings.Index(rest, "\n  function ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]

	for _, want := range []struct{ needle, why string }{
		{"state.collapsed", "a collapsed panel must be re-expanded before the chart is fetched"},
		{"state.charts", "a hidden charts SECTION must be revealed too, or the chart lands in display:none"},
	} {
		if !strings.Contains(body, want.needle) {
			t.Errorf("toggleProgressChart never looks at %s.\nKANB-33 item 5: %s.\n"+
				"Without it the click is processed in full — the bar takes aria-expanded=\"true\" and the "+
				"slot is filled — and the reader sees nothing happen at all.", want.needle, want.why)
		}
	}
}
