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
