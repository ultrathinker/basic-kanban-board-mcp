package view_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// ---------------------------------------------------------------------------
// KANB-38 — declared scope changes stand on the items chart's time axis.
// The tick is the axis-mark vocabulary the archival tick introduced, with
// one addition: it links to the message in the Thoughts feed that declared
// the change, so the reader gets the author's own words, not the chart's
// summary of them. The note wording itself carries the card's constraint:
// a declaration explains a declaration, it is not a proof of what moved the
// neighbouring point.
// ---------------------------------------------------------------------------

// scopeFixturePoints is a small two-point curve; the span it defines is what
// the marks must land inside.
func scopeFixturePoints() []service.HistoryPoint {
	return []service.HistoryPoint{
		{At: time.Date(2026, 9, 1, 9, 0, 0, 0, time.Local), TotalTasks: 1, OpenTasks: 1},
		{At: time.Date(2026, 9, 10, 9, 0, 0, 0, time.Local), TotalTasks: 4, OpenTasks: 3},
	}
}

func scopeFixtureNote(at time.Time) view.ScopeChangeNote {
	return view.ScopeChangeNote{
		At:     at,
		Author: "lead",
		Body:   "we took on the export task",
		Href:   "/p/BMB#chat-m1",
	}
}

func TestItemsChart_ScopeTickCarriesTheDeclarationAndTheLink(t *testing.T) {
	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.Local)
	marks := view.ScopeChangeMarks([]view.ScopeChangeNote{scopeFixtureNote(at)})
	svg := string(view.RenderItemsChart(scopeFixturePoints(), 0, 0, marks...))

	for _, want := range []string{
		`data-axis-mark="scope"`,
		`<a href="/p/BMB#chat-m1">`,
		`>scope</text>`,
		// The tooltip quotes the author and their body, and keeps the card's
		// constraint in words: a declaration, not a proof of causality.
		`scope change declared here by lead: &#34;we took on the export task&#34;`,
		`not a proof of what moved the curve`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("items chart is missing %q", want)
		}
	}
	// The archival ticks have nowhere to link: without scope notes there is
	// no anchor anywhere on the axis.
	bare := string(view.RenderItemsChart(scopeFixturePoints(), 0, 0))
	if strings.Contains(bare, "<a href=") {
		t.Error("a chart without scope notes carries a link on its axis")
	}
}

func TestItemsChart_ScopeTickNoteExcerptIsRuneCut(t *testing.T) {
	note := scopeFixtureNote(time.Date(2026, 9, 5, 12, 0, 0, 0, time.Local))
	note.Body = strings.Repeat("漢", 200)
	svg := string(view.RenderItemsChart(scopeFixturePoints(), 0, 0, view.ScopeChangeMarks([]view.ScopeChangeNote{note})...))
	if !strings.Contains(svg, "not a proof") {
		t.Fatal("scope tick did not render at all")
	}
	// The excerpt must hold exactly the 80-rune cut the feed's reply quotes
	// use — the tooltip is one glyph run longer than the axis can show, and
	// a byte-based cut would produce a different rune count.
	const marker = `declared here by lead: &#34;`
	start := strings.Index(svg, marker)
	if start < 0 {
		t.Fatalf("tooltip lost its opening quote: %s", svg)
	}
	start += len(marker)
	end := strings.Index(svg[start:], `&#34;`)
	if end < 0 {
		t.Fatalf("tooltip lost its closing quote: %s", svg)
	}
	body := svg[start : start+end]
	if n := len([]rune(body)); n != 81 {
		t.Errorf("tooltip excerpt is %d runes, want 80 body runes plus the ellipsis", n)
	}
	if !strings.HasSuffix(body, "…") {
		t.Errorf("excerpt %q is not cut", body)
	}
}

func TestItemsChart_ScopeTickOffTheSpanIsDropped(t *testing.T) {
	spanStart := scopeFixturePoints()[0].At
	spanEnd := scopeFixturePoints()[1].At
	marks := view.ScopeChangeMarks([]view.ScopeChangeNote{
		scopeFixtureNote(spanStart.Add(-time.Hour)), // before the axis
		scopeFixtureNote(spanEnd.Add(time.Hour)),    // after the axis
		scopeFixtureNote(spanStart.Add(time.Hour)),  // on the axis
	})
	svg := string(view.RenderItemsChart(scopeFixturePoints(), 0, 0, marks...))
	if got := strings.Count(svg, `data-axis-mark="scope"`); got != 1 {
		t.Errorf("scope ticks drawn = %d, want exactly the one inside the span", got)
	}
}

// TestChatEntry_CarriesIdAnchorForTheScopeTickLink: the link's destination
// exists — every rendered feed entry wears an id anchor the chart's href
// points at.
func TestChatEntry_CarriesIdAnchorForTheScopeTickLink(t *testing.T) {
	chat := view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil, nil)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", boardWithChat(chat, true)))
	if !strings.Contains(html, `id="chat-`) {
		t.Error("feed entries carry no id anchor for the chart's scope links")
	}
}
