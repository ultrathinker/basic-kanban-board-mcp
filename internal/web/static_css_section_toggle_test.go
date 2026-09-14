package web

import (
	"io/fs"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// A panel section must never be hidden as a whole: its own "show" button lives
// in its header.
//
// The panel-level version of this trap was already closed — the expand button
// is rendered as a sibling of the panel so it survives the panel collapsing.
// One level further in, the two sections repeated it exactly: the rule read
//
//	.chat-panel.charts-hidden .panel-section-charts { display: none }
//
// and .panel-section-charts contains both the body AND the header holding
// data-charts-toggle. Hiding the charts therefore removed the only control
// that could bring them back, and because the choice is persisted per project
// the dead end survived a reload. Recovering it meant clearing localStorage by
// hand.
//
// A markup test cannot see this: the button is already outside the section
// BODY, which is where a reader of the template would expect the hiding to
// apply. The defect lives entirely in which element the stylesheet points at,
// so that is what this test reads — from the embedded bytes the server serves,
// so a drifted web/static mirror fails too.
// ---------------------------------------------------------------------------

// sectionSubjects are the elements that carry a section's header, and so must
// never be the subject of a display:none rule.
var sectionSubjects = []string{
	".panel-section-charts",
	".panel-section-thoughts",
	".panel-section",
}

func TestAppCSS_SectionsAreHiddenByTheirBodyNotAsAWhole(t *testing.T) {
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

		d := displayDeclRe.FindStringSubmatch(body)
		if d == nil || strings.TrimSpace(d[2]) != "none" {
			continue
		}

		for _, sel := range strings.Split(selectorList, ",") {
			sel = strings.TrimSpace(sel)
			if sel == "" || strings.HasPrefix(sel, "@") {
				continue
			}
			parts := strings.Fields(sel)
			subject := parts[len(parts)-1]

			for _, bad := range sectionSubjects {
				if subject != bad {
					continue
				}
				t.Errorf("selector %q hides a whole panel section with display:none.\n"+
					"That section's header carries the toggle that shows it again, so hiding the "+
					"section removes the only way back and the choice is persisted, which makes the "+
					"dead end survive a reload.\n"+
					"Hide %s .panel-section-body instead, and leave the header standing.",
					sel, bad)
			}
		}
	}
}
