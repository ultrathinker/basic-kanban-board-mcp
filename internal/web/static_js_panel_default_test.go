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
