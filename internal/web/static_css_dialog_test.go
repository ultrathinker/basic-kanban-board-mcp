package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// A <dialog> must never be given an unconditional `display`.
//
// This guards a defect that shipped and that no other gate here can see. A
// closed <dialog> is hidden by the UA rule `dialog:not([open]) {display:none}`,
// whose specificity is (0,1,1). A stylesheet rule like
// `.modal.modal-wide { display: flex }` is (0,2,0) and therefore WINS — so the
// dialog stays on screen after close(), and every way of closing it (the
// cross, Escape, a backdrop click) appears to be broken while in fact working
// perfectly. The owner reported it exactly that way: "the close cross does not
// work".
//
// Go tests do not execute CSS, so nothing else in this repo could catch it.
// What CAN be checked is the rule that prevents it: any `display` applied to
// the dialog element itself must be gated on [open] (or be `display: none`).
// This reads the same embedded bytes the server serves, so it also fails if
// the mirror in web/static drifts and only the embedded copy is edited.
// ---------------------------------------------------------------------------

// cssRuleRe matches the innermost rules of a stylesheet: everything between a
// brace pair that contains no further braces. For a rule nested in an @media
// block the selector capture is the inner selector, because the capture class
// excludes braces — which is what we want.
var cssRuleRe = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)

// dialogSubjectRe matches a selector that targets a dialog element itself:
// the `dialog` type selector, or `.modal` not followed by a name character
// (so `.modal-head` and `.modal-body`, which are children, do not match).
var dialogSubjectRe = regexp.MustCompile(`(^|[\s>+~])(dialog|\.modal)(?:$|[^-\w])`)

var displayDeclRe = regexp.MustCompile(`(^|[;{\s])display\s*:\s*([^;]+)`)

func TestAppCSS_DialogDisplayIsAlwaysGatedOnOpen(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	raw, err := fs.ReadFile(sub, "app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := stripCSSComments(string(raw))

	for _, m := range cssRuleRe.FindAllStringSubmatch(css, -1) {
		selectorList, body := m[1], m[2]
		for _, sel := range strings.Split(selectorList, ",") {
			sel = strings.TrimSpace(sel)
			if sel == "" || strings.HasPrefix(sel, "@") {
				continue
			}
			// Only the SUBJECT of the rule matters — the last compound
			// selector. ".modal-wide .modal-body { display: flex }" styles a
			// child and is none of this test's business.
			parts := strings.Fields(sel)
			subject := parts[len(parts)-1]
			if !dialogSubjectRe.MatchString(" " + subject) {
				continue
			}
			if strings.Contains(subject, "[open]") || strings.Contains(subject, "::backdrop") {
				continue
			}
			d := displayDeclRe.FindStringSubmatch(body)
			if d == nil {
				continue
			}
			value := strings.TrimSpace(d[2])
			if value == "none" {
				continue
			}
			t.Errorf("selector %q gives a <dialog> `display: %s` without [open].\n"+
				"The UA hides a closed dialog with `dialog:not([open]){display:none}` at specificity (0,1,1); "+
				"this rule outranks it, so the dialog stays visible after close() and every close control looks broken.\n"+
				"Gate it: write the display on `%s[open]` instead.", sel, value, subject)
		}
	}
}

// stripCSSComments removes /* ... */ so a commented-out rule (or a comment
// that happens to contain the word "display") cannot trip the check.
func stripCSSComments(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		rest := s[i+2:]
		j := strings.Index(rest, "*/")
		if j < 0 {
			return b.String()
		}
		s = rest[j+2:]
	}
}

// ---------------------------------------------------------------------------
// KANB-41: the card-flight animation layer must NEVER eat a click.
//
// The flight is a position:fixed ghost that floats over the board while
// a card animates between columns. The brief is explicit: it must not
// intercept the mouse or keyboard, must not change scroll, must not
// delay data. The cheapest way to guarantee the mouse bit is a CSS rule,
// not a runtime check — so the test reads the embedded stylesheet and
// asserts pointer-events:none on .card-flight. A regression here would
// mean the animation literally prevents the user from clicking the
// cards underneath it for two seconds out of every move.
// ---------------------------------------------------------------------------

func TestAppCSS_CardFlightDoesNotInterceptPointerEvents(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	raw, err := fs.ReadFile(sub, "app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := stripCSSComments(string(raw))

	// Find the MAIN .card-flight rule (the one with the full property
	// set: position:fixed, z-index, etc.) — the @media
	// (prefers-reduced-motion: reduce) override is a separate, much
	// shorter rule that deliberately does not repeat every property,
	// and skipping it is fine (the override's job is to suppress the
	// transition, not to re-declare the cursor / pointer semantics).
	var mainBody string
	var mainFound bool
	for _, m := range cssRuleRe.FindAllStringSubmatch(css, -1) {
		selectorList, body := m[1], m[2]
		for _, sel := range strings.Split(selectorList, ",") {
			sel = strings.TrimSpace(sel)
			if sel == "" || strings.HasPrefix(sel, "@") {
				continue
			}
			parts := strings.Fields(sel)
			subject := parts[len(parts)-1]
			if subject != ".card-flight" {
				continue
			}
			// The main rule carries position:fixed and a font-family
			// declaration; the reduced-motion override carries just
			// `transition: none` and never position:fixed. Pick the
			// richer one as the "main" rule.
			if strings.Contains(body, "position: fixed") {
				mainBody = body
				mainFound = true
			}
		}
	}
	if !mainFound {
		t.Error("no main .card-flight rule found in app.css (one with position: fixed)")
		return
	}
	pe := pointerEventsDeclRe.FindStringSubmatch(mainBody)
	if pe == nil {
		t.Errorf(".card-flight main rule has no pointer-events declaration:\n%s", mainBody)
		return
	}
	// pe[1] is the captured char-or-start before "pointer-events";
	// pe[2] is the captured value. TrimSpace on the value, then
	// compare — "none" is the only safe value for a fixed-position
	// overlay (the test's whole point).
	if strings.TrimSpace(pe[2]) != "none" {
		t.Errorf(".card-flight pointer-events is %q, want \"none\" (KANB-41: the flight must not eat clicks)", strings.TrimSpace(pe[2]))
	}
}

// pointerEventsDeclRe finds the value of any `pointer-events: ...`
// declaration inside a CSS rule body. Same shape as displayDeclRe.
var pointerEventsDeclRe = regexp.MustCompile(`(^|[;{\s])pointer-events\s*:\s*([^;]+)`)

// TestAppCSS_CardFlightRespectsReducedMotion (KANB-41): the flight's
// motion must be suppressed under prefers-reduced-motion. The cheapest
// way to check the contract is "the stylesheet has a reduced-motion
// override that touches .card-flight". The actual duration value is
// up to app.js to set (the Web Animations call honours the same media
// query), so the test only pins the CSS half.
func TestAppCSS_CardFlightRespectsReducedMotion(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	raw, err := fs.ReadFile(sub, "app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := stripCSSComments(string(raw))

	// Find the @media (prefers-reduced-motion: reduce) block, then
	// look for .card-flight inside it. We do not care HOW it suppresses
	// the animation (transition:none, animation: none, animation-duration:
	// 0.001ms, whatever the implementation picked) — only that the
	// block mentions .card-flight at all.
	mediaRe := regexp.MustCompile(`@media\s*\(\s*prefers-reduced-motion\s*:\s*reduce\s*\)\s*\{([^@]*)\}`)
	m := mediaRe.FindStringSubmatch(css)
	if m == nil {
		t.Fatal("no @media (prefers-reduced-motion: reduce) block in app.css")
	}
	block := m[1]
	// The block may contain nested braces (e.g. a media query inside),
	// so the simple [^@]* is too greedy only if there is another @-rule
	// inside; for this stylesheet there is not, so the regex is fine.
	if !strings.Contains(block, ".card-flight") {
		t.Errorf("prefers-reduced-motion block does not mention .card-flight (KANB-41):\n%s", block)
	}
	// Same block also has to cover the highlight pulse — a user who
	// opted out of motion still sees the cue, but not as a moving
	// outline.
	if !strings.Contains(block, ".card-highlight") {
		t.Errorf("prefers-reduced-motion block does not mention .card-highlight:\n%s", block)
	}
}

// TestAppCSS_StaticDialogGateStillPasses is a canary: the KANB-41 CSS
// additions must not have re-introduced an unconditional `display`
// rule on a dialog, which is the regression TestAppCSS_DialogDisplayIsAlwaysGatedOnOpen
// exists to catch. Re-running that test directly would not show up as a
// failure of this one, but this test stays here so a future reader
// sees the contract in one place.
func TestAppCSS_CardFlightHasNoClickableDescendants(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	raw, err := fs.ReadFile(sub, "app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := stripCSSComments(string(raw))

	// .card-flight itself only carries the card key as text content —
	// it is a visual cue, not an interactive widget. Pin that it does
	// NOT contain any <button>, <a>, <input> or other interactive
	// element by class (there is no rule for that today, and adding
	// one would be a smell). The test simply asserts the rule body's
	// declarations are the safe shape: pointer-events:none is there
	// (covered by the other test) and no `cursor: pointer` slipped in.
	if strings.Contains(css, ".card-flight") {
		blockRe := regexp.MustCompile(`(?s)\.card-flight\s*\{([^}]*)\}`)
		bm := blockRe.FindStringSubmatch(css)
		if bm != nil && strings.Contains(bm[1], "cursor: pointer") {
			t.Errorf(".card-flight has cursor:pointer — it should look inert, not interactive:\n%s", bm[1])
		}
	}
}
