package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The expand button must be invisible while the panel is open.
//
// .board-split is a three-track grid: panel | splitter | board. The expand
// button is a FOURTH child, deliberately a sibling of the panel so it survives
// the panel collapsing (see static_css_section_toggle_test.go for that trap).
// It is therefore laid out by the same grid, and the only thing keeping it out
// of the board's track is `display: none` on the bare selector.
//
// That guard was silently lost. A later block restyling the button re-declared
// the property:
//
//	.chat-expand { display: none; }          // the guard
//	...
//	.chat-expand { ...; display: flex; ... }  // same specificity, wins
//
// Equal specificity, later wins, so the button was visible at all times. It
// then occupied the grid's third track as a 1174px grey slab and pushed the
// board out of the first row entirely — the whole board dropped below the
// Thoughts panel in a 488px column. Nothing failed: markup tests still found
// every element, every board test still passed on its own fixtures, and the
// defect was visible only to a human looking at the page.
//
// A markup test cannot see this and neither can a render test: the cascade is
// the whole defect. So this reads the stylesheet the server actually serves,
// from the embedded bytes, which also fails if the web/static mirror drifts.
// ---------------------------------------------------------------------------

// displayDecl finds every `display: <value>` inside one rule body.
var displayDecl = regexp.MustCompile(`display\s*:\s*([a-z-]+)`)

// topLevelCSS strips every at-rule block (@media, @supports, ...) so what
// remains is the cascade that applies at ANY viewport. The defect lived
// exactly here: a third `.chat-expand { display: none }` DOES exist, but it
// sits inside @media (max-width: 40rem) and so never applies on the desktop
// width the board is actually used at. A scanner that ignores nesting reads
// that narrow-screen rule as the winner and reports the stylesheet healthy
// while the button covers a third of the page.
func topLevelCSS(css string) string {
	var b strings.Builder
	depth, i := 0, 0
	for i < len(css) {
		c := css[i]
		if c == '@' && depth == 0 {
			// Skip to this at-rule's block and past it.
			j := strings.IndexByte(css[i:], '{')
			if j < 0 {
				break
			}
			i += j
			d := 0
			for i < len(css) {
				if css[i] == '{' {
					d++
				} else if css[i] == '}' {
					d--
					if d == 0 {
						i++
						break
					}
				}
				i++
			}
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// bareRule matches a rule whose selector list is exactly the given selector,
// capturing its body. Selectors that merely CONTAIN it (".x .chat-expand",
// ".chat-expand:hover") are not matched — those are allowed to show it.
func bareRule(css, selector string) []string {
	var bodies []string
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`)
	for _, m := range re.FindAllStringSubmatch(css, -1) {
		bodies = append(bodies, m[1])
	}
	return bodies
}

func TestAppCSS_ExpandButtonStaysHiddenWhileThePanelIsOpen(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	raw, err := fs.ReadFile(sub, "app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := topLevelCSS(string(raw))

	bodies := bareRule(css, ".chat-expand")
	if len(bodies) == 0 {
		t.Fatalf(".chat-expand has no bare rule at all: the button is then laid out in the board's grid track at all times")
	}

	// The LAST display declaration among the bare rules is the one that wins,
	// because they all carry the same specificity.
	last := ""
	for _, b := range bodies {
		for _, m := range displayDecl.FindAllStringSubmatch(b, -1) {
			last = m[1]
		}
	}
	if last != "none" {
		t.Errorf("the winning display for a bare .chat-expand is %q, want \"none\" outside every media query: the expand button then sits in the board's grid track while the panel is open and pushes the board onto a second row", last)
	}

	// And the collapsed state must still be able to show it.
	shown := false
	for _, b := range bareRule(css, ".board-split.is-collapsed .chat-expand") {
		if strings.Contains(b, "display") && !strings.Contains(b, "display: none") {
			shown = true
		}
	}
	if !shown {
		t.Errorf("nothing shows .chat-expand when .board-split is collapsed: hiding it by default would then make the panel unrecoverable")
	}
}
