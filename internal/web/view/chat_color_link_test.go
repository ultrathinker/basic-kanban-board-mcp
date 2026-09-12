package view

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// TestAuthorColorClass_PinsTheAlgorithm hard-codes the exact class FNV-1a
// over the name's bytes, mod 8, produces for a handful of names. This is
// deliberately over-specified (not just "returns something", not just "same
// name same class"): it pins the actual hash algorithm, so a change that
// swaps in something that varies across runs (map iteration, rand, time,
// pointer address) — or one that just narrows the output to a single class
// — shows up here as a wrong value, not as a test that still trivially
// passes.
func TestAuthorColorClass_PinsTheAlgorithm(t *testing.T) {
	cases := map[string]string{
		"":            "chat-c5",
		"a":           "chat-c4",
		"agent-alpha": "chat-c7",
		"agent-beta":  "chat-c3",
		"claude":      "chat-c7",
		"codex":       "chat-c4",
		"gpt-5":       "chat-c0",
		"gemini":      "chat-c6",
	}
	for name, want := range cases {
		if got := authorColorClass(name); got != want {
			t.Errorf("authorColorClass(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestAuthorColorClass_StableAndDeterministic re-derives the class many
// times for the same names (authorColorClass is a pure function of the
// name's bytes: no map iteration, no rand, no address, no clock), which is
// exactly what "the same colour after a restart" reduces to for a stateless
// process — restarting cannot change what a pure function returns.
func TestAuthorColorClass_StableAndDeterministic(t *testing.T) {
	names := []string{"agent-alpha", "agent-beta", "", "x", strings.Repeat("z", 500)}
	for _, n := range names {
		first := authorColorClass(n)
		for i := 0; i < 50; i++ {
			if got := authorColorClass(n); got != first {
				t.Fatalf("authorColorClass(%q) changed across calls: %q then %q", n, first, got)
			}
		}
	}
}

// TestAuthorColorClass_EdgeCaseNames: an empty name, a one-character name,
// and a very long name must all resolve to one of the 8 fixed slots without
// panicking.
func TestAuthorColorClass_EdgeCaseNames(t *testing.T) {
	for _, n := range []string{"", "x", strings.Repeat("agent-name-", 60)} {
		got := authorColorClass(n)
		if !strings.HasPrefix(got, "chat-c") {
			t.Fatalf("authorColorClass(%q) = %q, not a chat-cN class", n, got)
		}
		suffix := strings.TrimPrefix(got, "chat-c")
		if len(suffix) != 1 || suffix[0] < '0' || suffix[0] > '7' {
			t.Fatalf("authorColorClass(%q) = %q, index out of the 8-slot range", n, got)
		}
	}
}

// TestAuthorColorClass_DifferentNamesSpreadAcrossSlots guards against a
// degenerate hash that maps everything to one class (which would still make
// "one name always gets the same colour" trivially true, but would defeat
// the whole point: telling authors apart at a glance).
func TestAuthorColorClass_DifferentNamesSpreadAcrossSlots(t *testing.T) {
	names := []string{"agent-alpha", "agent-beta", "claude", "codex", "gpt-5", "gemini", "reviewer-bot", "planner"}
	seen := map[string]bool{}
	for _, n := range names {
		seen[authorColorClass(n)] = true
	}
	if len(seen) < 4 {
		t.Fatalf("8 distinct author names only spread across %d colour slots: %v", len(seen), seen)
	}
}

// TestLinkifyChatText_KnownKeyBecomesLink is the direct, unit-level version
// of the acceptance criterion "a task key in the text becomes a working
// link". The href comes from taskURL applied to the matched, normalized
// token — never from splicing message text.
func TestLinkifyChatText_KnownKeyBecomesLink(t *testing.T) {
	known := map[string]struct{}{"KANB-7": {}}
	got := string(linkifyChatText("it creates KANB-7 now", known))
	want := `it creates <a href="/t/KANB-7">KANB-7</a> now`
	if got != want {
		t.Fatalf("linkifyChatText = %q, want %q", got, want)
	}
}

// TestLinkifyChatText_UnknownKeyStaysPlainText is the direct check for "a
// key that does not exist must not become a broken link": KANB-9 is
// syntactically a key but is absent from knownKeys, so it renders as plain
// escaped text with no anchor at all.
func TestLinkifyChatText_UnknownKeyStaysPlainText(t *testing.T) {
	known := map[string]struct{}{"KANB-7": {}}
	got := string(linkifyChatText("see KANB-9 for the follow-up", known))
	if strings.Contains(got, "<a") {
		t.Fatalf("linkifyChatText produced a link for an unknown key: %q", got)
	}
	if !strings.Contains(got, "KANB-9") {
		t.Fatalf("linkifyChatText dropped the unknown key's text: %q", got)
	}
}

// TestLinkifyChatText_NilKnownKeys is the "never checked" case (e.g. the
// existence lookup failed): every mention stays plain text rather than
// panicking on a nil map or guessing at a link.
func TestLinkifyChatText_NilKnownKeys(t *testing.T) {
	got := string(linkifyChatText("KANB-7 done", nil))
	if strings.Contains(got, "<a") {
		t.Fatalf("linkifyChatText linked against a nil known-keys set: %q", got)
	}
	if !strings.Contains(got, "KANB-7") {
		t.Fatalf("text lost: %q", got)
	}
}

// TestLinkifyChatText_BoundaryCases drives every boundary case the brief
// calls out explicitly: a trailing letter fuses onto the token and voids
// the match ("KANB-7X"), a leading letter changes which key the token names
// rather than being ignored ("xKANB-7" is not a mention of KANB-7), a bare
// number never matches at all, and ordinary punctuation around a real
// mention (start of sentence, parens, trailing comma/period) does not
// interfere with recognizing it.
func TestLinkifyChatText_BoundaryCases(t *testing.T) {
	known := map[string]struct{}{"KANB-7": {}}
	linkOf := func(key string) string { return `<a href="/t/` + key + `">` + key + `</a>` }

	cases := []struct {
		name     string
		text     string
		wantLink bool
	}{
		{"plain mention", "KANB-7 is done", true},
		{"start of sentence", "KANB-7 was just created.", true},
		{"in parentheses", "(see KANB-7)", true},
		{"trailing comma", "blocked by KANB-7, please review", true},
		{"trailing period", "closing KANB-7.", true},
		{"trailing letter fuses the token", "KANB-7X is unrelated", false},
		{"leading letter changes the key", "xKANB-7 is unrelated", false},
		{"bare number, no project prefix", "task 7 is unrelated", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(linkifyChatText(c.text, known))
			hasLink := strings.Contains(got, linkOf("KANB-7"))
			if hasLink != c.wantLink {
				t.Fatalf("text %q: link present = %v, want %v (rendered: %q)", c.text, hasLink, c.wantLink, got)
			}
			// Whatever happens, the visible characters of the original
			// mention must survive as text (never silently dropped).
			if c.name == "trailing letter fuses the token" && !strings.Contains(got, "KANB-7X") {
				t.Fatalf("KANB-7X text lost: %q", got)
			}
			if c.name == "leading letter changes the key" && !strings.Contains(got, "xKANB-7") {
				t.Fatalf("xKANB-7 text lost: %q", got)
			}
			if c.name == "bare number, no project prefix" && !strings.Contains(got, "task 7") {
				t.Fatalf("bare-number text lost: %q", got)
			}
		})
	}
}

// TestLinkifyChatText_ScriptInjectionIsEscapedAndVisible is the mandatory
// injection test: HTML and a <script> tag in a message body must come out
// inert (no live tag) and visible as plain text, exactly like the existing
// html/template auto-escaping this replaces for the chat body.
func TestLinkifyChatText_ScriptInjectionIsEscapedAndVisible(t *testing.T) {
	got := string(linkifyChatText(`<script>alert(1)</script> A & B "quoted"`, nil))
	if strings.Contains(got, "<script>alert(1)") {
		t.Fatalf("script tag reached the output live: %q", got)
	}
	for _, want := range []string{
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		"A &amp; B",
		"&#34;quoted&#34;",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("escaped output lost %q: got %q", want, got)
		}
	}
}

// TestLinkifyChatText_InjectionCannotEscapeTheLinkAttribute is the sharper
// version: a message that mentions a REAL key and immediately follows it
// with an attribute-breakout attempt must still produce exactly the
// key-derived anchor, with the breakout attempt landing as escaped text
// after the anchor closes — never inside the href, never splicing message
// text into the tag.
func TestLinkifyChatText_InjectionCannotEscapeTheLinkAttribute(t *testing.T) {
	known := map[string]struct{}{"KANB-7": {}}
	got := string(linkifyChatText(`KANB-7"><script>alert(1)</script>`, known))
	want := `<a href="/t/KANB-7">KANB-7</a>&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;`
	if got != want {
		t.Fatalf("linkifyChatText = %q, want %q", got, want)
	}
}

// TestLinkifyChatText_InjectionBeforeKnownKeyIsEscaped pins the branch that
// escapes text[last:start] — the text BEFORE a matched key — on the loop's
// FIRST iteration, where last==0. This is the exact branch a review canary
// found unguarded: TestLinkifyChatText_ScriptInjectionIsEscapedAndVisible
// passes knownKeys=nil, so it never enters the loop at all (early-exit path
// before any match), and TestLinkifyChatText_InjectionCannotEscapeTheLinkAttribute
// puts the key at the very start of the string, so text[last:start] is empty
// on its only iteration — the injected markup there sits in the TAIL, which
// is escaped by a different write, after the loop. Neither test exercises
// "untrusted text before a recognized key". This one does, with an exact
// full-string comparison so removing the escape call fails it.
func TestLinkifyChatText_InjectionBeforeKnownKeyIsEscaped(t *testing.T) {
	known := map[string]struct{}{"KANB-7": {}}
	got := string(linkifyChatText(`<script>alert(1)</script> see KANB-7 for details`, known))
	want := `&lt;script&gt;alert(1)&lt;/script&gt; see <a href="/t/KANB-7">KANB-7</a> for details`
	if got != want {
		t.Fatalf("linkifyChatText = %q, want %q", got, want)
	}
}

// TestLinkifyChatText_InjectionBetweenTwoKnownKeysIsEscaped pins the same
// text[last:start] escape, but on the loop's SECOND iteration (last != 0,
// start != 0), between two real links rather than before the first or after
// the last. A prior version of this function could have escaped correctly
// on iteration 1 and dropped the escape only from iteration 2 onward without
// either of the other tests noticing; this one would catch exactly that.
func TestLinkifyChatText_InjectionBetweenTwoKnownKeysIsEscaped(t *testing.T) {
	known := map[string]struct{}{"KANB-7": {}, "KANB-8": {}}
	got := string(linkifyChatText(`KANB-7 <script>alert(2)</script> KANB-8`, known))
	want := `<a href="/t/KANB-7">KANB-7</a> &lt;script&gt;alert(2)&lt;/script&gt; <a href="/t/KANB-8">KANB-8</a>`
	if got != want {
		t.Fatalf("linkifyChatText = %q, want %q", got, want)
	}
}

// TestLinkifyChatText_LongUnbrokenWordIsNotAKey defends the "one unbroken
// 500-character word" case the brief calls out: a long run of letters with
// no dash-digit shape never matches, and is not dropped or truncated by the
// escaping path.
func TestLinkifyChatText_LongUnbrokenWordIsNotAKey(t *testing.T) {
	word := strings.Repeat("x", 500)
	got := string(linkifyChatText(word, map[string]struct{}{"KANB-7": {}}))
	if got != word {
		t.Fatalf("a 500-char unbroken word without key shape was altered: got %d chars, want %d", len(got), len(word))
	}
}

// TestCandidateTaskKeys_DedupsAndNormalizes: the same key mentioned several
// times (in different letter case) across several messages contributes only
// one candidate, in canonical uppercase form.
func TestCandidateTaskKeys_DedupsAndNormalizes(t *testing.T) {
	msgs := []domain.ChatMessage{
		{Author: "a", Body: "created kanb-7 and KANB-7 again"},
		{Author: "b", Body: "still on Kanb-7"},
	}
	got := CandidateTaskKeys(msgs)
	if len(got) != 1 || got[0] != "KANB-7" {
		t.Fatalf("CandidateTaskKeys = %v, want exactly [\"KANB-7\"]", got)
	}
}

// TestCandidateTaskKeys_CapsAtMaxGetKeys: many distinct mentions across many
// messages never exceed domain.MaxGetKeys candidates — the hard limit
// service.TaskGet enforces per call, so the batched existence lookup this
// feeds can never itself become invalid by asking for too many keys.
func TestCandidateTaskKeys_CapsAtMaxGetKeys(t *testing.T) {
	var msgs []domain.ChatMessage
	for i := 0; i < domain.MaxGetKeys+25; i++ {
		msgs = append(msgs, domain.ChatMessage{Author: "a", Body: "touching KANB-" + strconv.Itoa(i)})
	}
	got := CandidateTaskKeys(msgs)
	if len(got) != domain.MaxGetKeys {
		t.Fatalf("CandidateTaskKeys returned %d candidates, want capped at %d", len(got), domain.MaxGetKeys)
	}
}

// TestCandidateTaskKeys_Empty: a page with no key-shaped mentions returns an
// empty (not nil-panicking) slice.
func TestCandidateTaskKeys_Empty(t *testing.T) {
	got := CandidateTaskKeys([]domain.ChatMessage{{Author: "a", Body: "nothing to see here"}})
	if len(got) != 0 {
		t.Fatalf("CandidateTaskKeys = %v, want empty", got)
	}
}
