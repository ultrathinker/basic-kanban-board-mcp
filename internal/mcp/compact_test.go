package mcp

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// update, when true, rewrites the golden fixtures instead of comparing against
// them. Without -update the goldens are gates: a renderer that drifts from
// the stored bytes fails the test.
var update = flag.Bool("update", false, "rewrite golden fixtures in testdata/")

// fixedNow is the deterministic clock tests render against.
var fixedNow = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

// mkTask is the test fixture builder. Defaults to a backlog leaf with a
// minimal but real-looking task; tests override what they assert on.
func mkTask(key string, opts ...func(*domain.TaskView)) domain.TaskView {
	tv := domain.TaskView{
		Task: domain.Task{
			ID:              key,
			Key:             key,
			ProjectID:       "p",
			ColumnID:        "c",
			Rank:            1024,
			Title:           "Sample task " + key,
			Type:            domain.TypeTask,
			Priority:        domain.PriorityNone,
			ColumnEnteredAt: fixedNow.Add(-2 * time.Hour),
			Version:         1,
			CreatedAt:       fixedNow.Add(-24 * time.Hour),
			UpdatedAt:       fixedNow,
			CreatedBy:       "test",
			UpdatedBy:       "test",
		},
		ProjectKey: "BMB",
		ColumnName: "Backlog",
		ColumnKind: domain.KindBacklog,
	}
	for _, o := range opts {
		o(&tv)
	}
	return tv
}

func withType(t domain.Type) func(*domain.TaskView) {
	return func(tv *domain.TaskView) { tv.Type = t }
}
func withPriority(p domain.Priority) func(*domain.TaskView) {
	return func(tv *domain.TaskView) { tv.Priority = p }
}
func withTitle(s string) func(*domain.TaskView) {
	return func(tv *domain.TaskView) { tv.Title = s }
}
func withEstimate(n float64) func(*domain.TaskView) {
	return func(tv *domain.TaskView) { tv.Estimate = &n }
}
func withAssignee(a string) func(*domain.TaskView) {
	return func(tv *domain.TaskView) { tv.Assignee = &a }
}
func withClaim(actor string, remaining time.Duration) func(*domain.TaskView) {
	return func(tv *domain.TaskView) {
		claimed := fixedNow.Add(-time.Minute)
		expires := fixedNow.Add(remaining)
		tv.ClaimedBy = &actor
		tv.ClaimedAt = &claimed
		tv.ClaimExpiresAt = &expires
	}
}
func withExpiredClaim(actor string) func(*domain.TaskView) {
	return func(tv *domain.TaskView) {
		claimed := fixedNow.Add(-2 * time.Hour)
		expires := fixedNow.Add(-time.Minute) // already expired
		tv.ClaimedBy = &actor
		tv.ClaimedAt = &claimed
		tv.ClaimExpiresAt = &expires
	}
}
func withSub(done, total int) func(*domain.TaskView) {
	return func(tv *domain.TaskView) {
		tv.SubDone = done
		tv.SubTotal = total
	}
}
func withBlockedBy(keys ...string) func(*domain.TaskView) {
	return func(tv *domain.TaskView) { tv.BlockedBy = append([]string(nil), keys...) }
}
func withTags(tags ...string) func(*domain.TaskView) {
	return func(tv *domain.TaskView) {
		cp := append([]string(nil), tags...)
		tv.Tags = cp
	}
}
func withVersion(v int) func(*domain.TaskView) {
	return func(tv *domain.TaskView) { tv.Version = v }
}
func withEnteredAt(t time.Time) func(*domain.TaskView) {
	return func(tv *domain.TaskView) { tv.ColumnEnteredAt = t }
}

func mkColumn(name string, kind domain.Kind, count int, tasks ...domain.TaskView) service.BoardColumn {
	wip := (*int)(nil)
	if kind == domain.KindActive && name == "Doing" {
		v := 3
		wip = &v
	}
	return service.BoardColumn{
		Name:     name,
		Kind:     kind,
		WIPLimit: wip,
		Count:    count,
		Tasks:    append([]domain.TaskView(nil), tasks...),
	}
}

// golden compares actual to the stored fixture. Without -update the goldens
// are gates: a renderer that drifts from the stored bytes fails the test.
// With -update the fixtures are rewritten so a human can inspect the diff
// before committing.
//
// On mismatch the failure names the first line that differs and prints both
// versions of that line, so a regression is actionable without diffing the
// whole file by hand.
func golden(t *testing.T, path, actual string) {
	t.Helper()
	if *update {
		if err := writeFile(path, []byte(actual)); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	want, err := readFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run with -update to create)", path, err)
	}
	if string(want) == actual {
		return
	}
	diffGolden(t, path, string(want), actual)
}

// diffGolden fails the test with the first differing line number and both
// versions of that line, then prints a focused window so the regression is
// obvious in CI output.
func diffGolden(t *testing.T, path, want, got string) {
	t.Helper()
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	max := len(wantLines)
	if len(gotLines) > max {
		max = len(gotLines)
	}
	for i := 0; i < max; i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			t.Fatalf("golden mismatch at %s line %d\nwant: %q\ngot:  %q\n\n--- want ---\n%s\n--- got ---\n%s",
				path, i+1, w, g,
				window(wantLines, i),
				window(gotLines, i))
		}
	}
	t.Fatalf("golden mismatch in %s (no line difference but totals differ): want %d bytes, got %d bytes", path, len(want), len(got))
}

func window(lines []string, i int) string {
	start := i - 2
	if start < 0 {
		start = 0
	}
	end := i + 3
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n")
}

func TestRender_BasicGolden(t *testing.T) {
	doing := mkColumn("Doing", domain.KindActive, 2,
		mkTask("BMB-14",
			withType(domain.TypeBug),
			withPriority(domain.PriorityHigh),
			withTitle("Fix WAL checkpoint race"),
			withEstimate(2),
			withAssignee("alex"),
			withClaim("claude@rog", 43*time.Minute),
			withSub(1, 3),
			withTags("sync"),
			withVersion(7),
			withEnteredAt(fixedNow.Add(-2*time.Hour)),
		),
		mkTask("BMB-17",
			withType(domain.TypeFeat),
			withPriority(domain.PriorityMedium),
			withTitle("Encrypted FTS index"),
			withEstimate(8),
			withExpiredClaim("codex@desk"),
			withBlockedBy("BMB-14", "BMB-9"),
			withVersion(3),
			withEnteredAt(fixedNow.Add(-3*24*time.Hour)),
		),
	)
	review := mkColumn("Review", domain.KindActive, 1,
		mkTask("BMB-12",
			withType(domain.TypeTask),
			withTitle("Squash migrations 41-44"),
			withEstimate(1),
			withAssignee("alex"),
			withVersion(2),
			withEnteredAt(fixedNow.Add(-24*time.Hour)),
		),
	)
	backlog := mkColumn("Backlog", domain.KindBacklog, 1,
		mkTask("BMB-18",
			withType(domain.TypeBug),
			withPriority(domain.PriorityCritical),
			withTitle("Sync loses tombstones"),
			withTags("sync"),
			withVersion(1),
		),
	)
	board := &service.Board{Projects: []service.BoardProject{{
		Key:       "BMB",
		Name:      "BeeMemoryBank",
		Version:   9,
		FocusKey:  "BMB-14",
		Columns:   []service.BoardColumn{doing, review, backlog},
		DoneTotal: 40,
	}}}
	got := Render(board, fixedNow)
	golden(t, "testdata/basic.golden", got)
}

func TestRender_NoFocusAndDoneShown(t *testing.T) {
	backlog := mkColumn("Backlog", domain.KindBacklog, 0)
	done := mkColumn("Done", domain.KindDone, 2,
		mkTask("BMB-1", withTitle("First"),
			withEnteredAt(fixedNow.Add(-2*time.Hour)),
		),
		mkTask("BMB-2", withTitle("Second"),
			withEnteredAt(fixedNow.Add(-4*time.Hour)),
		),
	)
	board := &service.Board{Projects: []service.BoardProject{{
		Key:       "BMB",
		Name:      "BeeMemoryBank",
		Version:   1,
		FocusKey:  "",
		Columns:   []service.BoardColumn{backlog, done},
		DoneTotal: 12,
		DoneShown: 2,
	}}}
	got := Render(board, fixedNow)
	golden(t, "testdata/no_focus.golden", got)
}

func TestRoundTrip(t *testing.T) {
	// The round-trip property is field-level, not byte-level. The grammar
	// carries: KEY, [priority type], title, est, @assignee, lease actor+rem,
	// sub, blocked-by, #tags, v. Age carries duration but is only present on
	// active columns; an active-without-WIP column (Review) parses as backlog,
	// so age is lost for tasks there — accepted, documented.
	want := &service.Board{Projects: []service.BoardProject{{
		Key:      "BMB",
		Name:     "BeeMemoryBank",
		Version:  9,
		FocusKey: "BMB-14",
		Columns: []service.BoardColumn{
			mkColumn("Doing", domain.KindActive, 1,
				mkTask("BMB-14",
					withType(domain.TypeBug),
					withPriority(domain.PriorityHigh),
					withTitle("Fix WAL checkpoint race"),
					withEstimate(2),
					withAssignee("alex"),
					withClaim("claude@rog", 43*time.Minute),
					withSub(1, 3),
					withTags("sync"),
					withVersion(7),
					withEnteredAt(fixedNow.Add(-2*time.Hour)),
				),
			),
			mkColumn("Review", domain.KindActive, 1,
				mkTask("BMB-12",
					withType(domain.TypeTask),
					withTitle("Squash migrations 41-44"),
					withEstimate(1),
					withAssignee("alex"),
					withVersion(2),
				),
			),
			mkColumn("Backlog", domain.KindBacklog, 1,
				mkTask("BMB-18",
					withType(domain.TypeBug),
					withPriority(domain.PriorityCritical),
					withTitle("Sync loses tombstones"),
					withTags("sync"),
					withVersion(1),
				),
			),
		},
		DoneTotal: 40,
	}}}
	rendered := Render(want, fixedNow)
	parsed, err := ParseCompactAt(rendered, fixedNow)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Project header fields.
	if got := parsed.Projects[0]; got.Key != want.Projects[0].Key ||
		got.Name != want.Projects[0].Name ||
		got.Version != want.Projects[0].Version ||
		got.FocusKey != want.Projects[0].FocusKey ||
		got.DoneTotal != want.Projects[0].DoneTotal {
		t.Fatalf("project header fields lost: got %+v want %+v", got, want.Projects[0])
	}
	// Task fields. We pull tasks out of the parsed columns in declaration order.
	var parsedTasks []domain.TaskView
	for i := range parsed.Projects[0].Columns {
		parsedTasks = append(parsedTasks, parsed.Projects[0].Columns[i].Tasks...)
	}
	var wantTasks []domain.TaskView
	for i := range want.Projects[0].Columns {
		wantTasks = append(wantTasks, want.Projects[0].Columns[i].Tasks...)
	}
	if len(parsedTasks) != len(wantTasks) {
		t.Fatalf("task count: got %d want %d", len(parsedTasks), len(wantTasks))
	}
	for i := range wantTasks {
		assertTaskGrammarRecovered(t, wantTasks[i], parsedTasks[i])
	}
}

// TestRoundTripMany asserts the same field-level property across a broad
// fixture so a regression in any field has many chances to surface.
func TestRoundTripMany(t *testing.T) {
	tags := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		tags = append(tags, "tag"+string(rune('a'+i)))
	}
	tags[0] = "sync"
	tags[5] = "release"
	cases := []struct {
		name string
		tv   domain.TaskView
		kind domain.Kind
	}{
		{
			name: "minimal-no-suffixes-but-v",
			tv:   mkTask("BMB-1", withVersion(1)),
			kind: domain.KindBacklog,
		},
		{
			name: "title-with-separator",
			tv:   mkTask("BMB-2", withTitle("Fix A · B · C"), withVersion(1)),
			kind: domain.KindBacklog,
		},
		{
			name: "title-with-newlines-collapsed",
			tv:   mkTask("BMB-3", withTitle("Fix\nbroken  parser"), withVersion(1)),
			kind: domain.KindBacklog,
		},
		{
			name: "all-suffixes",
			tv: mkTask("BMB-4",
				withType(domain.TypeBug),
				withPriority(domain.PriorityHigh),
				withTitle("Many fields"),
				withEstimate(2.5),
				withAssignee("alex"),
				withClaim("claude@rog", 12*time.Minute),
				withSub(1, 3),
				withBlockedBy("BMB-9", "BMB-1"),
				withTags(tags...),
				withVersion(8),
				withEnteredAt(fixedNow.Add(-3*time.Hour)),
			),
			kind: domain.KindActive,
		},
		{
			name: "expired-lease",
			tv:   mkTask("BMB-5", withExpiredClaim("codex@desk"), withVersion(1)),
			kind: domain.KindBacklog,
		},
		{
			name: "no-tags-no-blockers",
			tv:   mkTask("BMB-6", withVersion(3)),
			kind: domain.KindBacklog,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			board := &service.Board{Projects: []service.BoardProject{{
				Key:      "BMB",
				Name:     "BeeMemoryBank",
				FocusKey: "",
				Columns: []service.BoardColumn{
					mkColumn("Backlog", domain.KindBacklog, 1, c.tv),
				},
				DoneTotal: 0,
			}}}
			rendered := Render(board, fixedNow)
			parsed, err := ParseCompactAt(rendered, fixedNow)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var got domain.TaskView
			for _, col := range parsed.Projects[0].Columns {
				if len(col.Tasks) > 0 {
					got = col.Tasks[0]
				}
			}
			assertTaskGrammarRecovered(t, c.tv, got)
		})
	}
}

// assertTaskGrammarRecovered checks every field that the grammar carries on
// a task line. Fields the grammar does NOT carry (ClaimedAt/ClaimExpiresAt
// absolute timestamps, ColumnEnteredAt absolute timestamp, the estimate
// unit, the column Kind on the task itself) are out of scope.
func assertTaskGrammarRecovered(t *testing.T, want, got domain.TaskView) {
	t.Helper()
	if got.Key != want.Key {
		t.Errorf("Key: got %q want %q", got.Key, want.Key)
	}
	if got.Priority != want.Priority {
		t.Errorf("Priority: got %v want %v", got.Priority, want.Priority)
	}
	if got.Type != want.Type {
		t.Errorf("Type: got %v want %v", got.Type, want.Type)
	}
	// Title is normalized by the renderer (whitespace collapsed, ` · ` → ` - `).
	wantTitle := domain.NormalizeTitle(want.Title)
	wantTitle = strings.ReplaceAll(wantTitle, " · ", " - ")
	if got.Title != wantTitle {
		t.Errorf("Title: got %q want %q (after renderer normalization)", got.Title, wantTitle)
	}
	if !floatPtrApprox(got.Estimate, want.Estimate) {
		t.Errorf("Estimate: got %v want %v", got.Estimate, want.Estimate)
	}
	if !strPtrEq(got.Assignee, want.Assignee) {
		t.Errorf("Assignee: got %v want %v", got.Assignee, want.Assignee)
	}
	if !strPtrEq(got.ClaimedBy, want.ClaimedBy) {
		t.Errorf("ClaimedBy: got %v want %v", got.ClaimedBy, want.ClaimedBy)
	}
	// For non-expired leases the parser anchors ClaimExpiresAt at now+rem, so
	// the remaining duration must still be positive.
	if want.ClaimedBy != nil && want.ClaimExpiresAt != nil && want.ClaimExpiresAt.After(fixedNow) {
		if got.ClaimExpiresAt == nil || !got.ClaimExpiresAt.After(fixedNow) {
			t.Errorf("live lease must remain live after round-trip: got expiry %v", got.ClaimExpiresAt)
		}
	}
	if got.Version != want.Version {
		t.Errorf("Version: got %d want %d", got.Version, want.Version)
	}
	if !strSliceEq(got.BlockedBy, want.BlockedBy) {
		t.Errorf("BlockedBy: got %v want %v", got.BlockedBy, want.BlockedBy)
	}
	if !strSliceSortedEq(got.Tags, want.Tags) {
		t.Errorf("Tags: got %v want %v", got.Tags, want.Tags)
	}
	if got.SubDone != want.SubDone {
		t.Errorf("SubDone: got %d want %d", got.SubDone, want.SubDone)
	}
	if got.SubTotal != want.SubTotal {
		t.Errorf("SubTotal: got %d want %d", got.SubTotal, want.SubTotal)
	}
}

func floatPtrApprox(a, b *float64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func strPtrEq(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func strSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func strSliceSortedEq(a, b []string) bool {
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	return strSliceEq(ac, bc)
}

func TestParseCompact_Errors(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"no-version", "# BMB Foo · focus none · Backlog 1 · Done 0 (hidden)"},
		{"wrong-version", "compact_version=99\n"},
		{"no-focus", "# BMB Foo · Backlog 1 · Done 0 (hidden)\n"},
		{"no-done", "# BMB Foo · focus none · Backlog 1\n"},
		{"bad-task-key", "- bmb-1 [task] Foo · v1\n"},
		{"missing-v", "- BMB-1 [task] Foo\n"},
		{"unknown-column", "# BMB Foo · focus none · Backlog 1 · Done 0 (hidden)\n## Mystery\n- BMB-1 [task] Foo · v1\n"},
		{"trailing-suffix", "- BMB-1 [task] Foo · v1 · bogus\n"},
		{"bad-version", "- BMB-1 [task] Foo · vX\n"},
		{"bad-estimate", "- BMB-1 [task] Foo · est 2 · v1\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseCompact(c.in)
			if err == nil {
				t.Fatalf("expected error, got nil; input:\n%s", c.in)
			}
		})
	}
}

func TestParseCompact_MissingRequiredV(t *testing.T) {
	// Even when every other suffix is absent, v<version> is required.
	in := "# BMB Foo · focus none · Backlog 1 · Done 0 (hidden)\n## Backlog\n- BMB-1 [task] Hello world\n"
	if _, err := ParseCompact(in); err == nil {
		t.Fatalf("expected missing-v error")
	}
}

func TestBudgetFixture_StaysUnderTokenBudget(t *testing.T) {
	// 30 realistic active tasks must render under CompactTokenBudget tokens
	// (estimated as len/4). The release gate from PLAN §11.
	//
	// The gate is a regression guard, not the product claim. It measures
	// ~1064 tokens against a ceiling of 1200. The number worth publishing is
	// the ratio on this same board: ~5640 tokens as minified JSON and ~10650
	// as indented JSON, i.e. roughly 81% and 90% fewer. See PLAN §18
	// deviation 5 — the original 600 was written before anything was measured.
	//
	// The fixture is intentionally realistic, NOT tuned to fit. Titles come
	// from real backlog items (most are 30–60 characters, longer than the
	// abbreviated forms previously used here). Most tasks carry an estimate
	// and an assignee, about half carry 1–3 tags, about a third carry
	// blockers or subtask progress, and there is a mix of live and expired
	// claims. If the honest number exceeds the budget, the test fails —
	// the gate is supposed to measure, not pass by construction.
	priorities := []domain.Priority{
		domain.PriorityCritical, domain.PriorityHigh, domain.PriorityMedium,
		domain.PriorityLow, domain.PriorityNone,
	}
	types := []domain.Type{
		domain.TypeTask, domain.TypeBug, domain.TypeFeat,
		domain.TypeChore, domain.TypeDoc, domain.TypePerf, domain.TypeResearch,
	}
	assignees := []string{"alex", "claude", "codex", "robert", "kira"}
	tagPool := [][]string{
		{"sync"}, {"ui"}, {"db"}, {"infra"},
		{"sync", "release"}, {"ui", "a11y"}, {"perf"}, {"security"},
		{"docs"}, {"perf", "ui"},
	}
	titles := []string{
		"Fix WAL checkpoint race under concurrent readers",
		"Encrypted FTS index for sensitive body columns",
		"Squash migrations 41-44 into a single coherent step",
		"Sync sometimes loses tombstones after reconnect",
		"Trim ambiguous grammar in compact board rendering",
		"Validate project_upsert mode is always required",
		"Add idempotency replay golden test with payload hash",
		"SSE Last-Event-ID resync path on stale client buffer",
		"Honor X-Forwarded-* only from --trusted-proxies CIDRs",
		"Bootstrap admin token prints once under migration lock",
		"DoneLimit ordering by most recent DoneAt descending",
		"Lexicographic sort of #tags on every compact task line",
		"Strict_done blocks move into Done with unchecked items",
		"task_next peek returns work even when WIP is full",
		"Deterministic ordering under identical priority/due",
		"Append note must never bump task.Version optimistic field",
		"Symbolic @ref inside task_create blocked_by array",
		"Focus banner clears when focused task is archived",
		"Atomic claim CAS expressed as a single UPDATE statement",
		"task_remove cascade_subtasks defaults to true on archive",
		"Export JSON pretty-prints with two-space indent for diffs",
		"Doctor reports bind address and base-url plan in plain text",
		"Healthcheck exit codes zero on healthy only, nonzero otherwise",
		"agent-config prints paste-ready Claude/Codex/Cursor snippets",
		"Rate-limit LRU store bounded at 10000 entries to bound memory",
		"Markdown body sanitized with bluemonday policy on render",
		"Alpine drawer state must survive htmx swap on navigation",
		"SortableJS keyboard reorder via arrow keys without a mouse",
		"htmx SSE reconnect wrapper for transient dropped connections",
		"Tailwind v4 theme tokens for priority stripe and type badges",
	}
	if len(titles) != domain.CompactBudgetTasks {
		t.Fatalf("budget fixture needs exactly %d titles, have %d", domain.CompactBudgetTasks, len(titles))
	}

	tasks := make([]domain.TaskView, 0, domain.CompactBudgetTasks)
	for i := 0; i < domain.CompactBudgetTasks; i++ {
		tv := mkTask("BMB-"+itoa(i+1),
			withType(types[i%len(types)]),
			withPriority(priorities[i%len(priorities)]),
			withTitle(titles[i]),
			withVersion(i+1),
			withEnteredAt(fixedNow.Add(time.Duration(2+i%12)*time.Hour)),
		)
		// Most tasks carry an estimate and an assignee (≈80%). The remainder
		// are unestimated / unassigned backlog entries — real boards have them.
		if i%5 != 4 {
			est := float64(1 + i%8)
			tv.Estimate = &est
			a := assignees[i%len(assignees)]
			tv.Assignee = &a
		}
		// About half of tasks carry 1–3 tags.
		if i%2 == 0 {
			tv.Tags = tagPool[i%len(tagPool)]
		}
		// About a third carry blockers or subtask progress.
		switch i % 3 {
		case 0:
			other := ((i + 7) % domain.CompactBudgetTasks) + 1
			tv.BlockedBy = []string{"BMB-" + itoa(other)}
		case 1:
			tv.SubDone = (i / 3) % 4
			tv.SubTotal = tv.SubDone + 2
		}
		// A realistic spread of live and expired claims (≈40% of tasks).
		switch i % 5 {
		case 0, 1:
			actor := "claude@" + string(rune('a'+i%3))
			claimed := fixedNow.Add(-time.Minute)
			exp := fixedNow.Add(time.Duration(15+i*7) * time.Minute)
			tv.ClaimedBy = &actor
			tv.ClaimedAt = &claimed
			tv.ClaimExpiresAt = &exp
		case 2:
			actor := "codex@" + string(rune('a'+i%3))
			claimed := fixedNow.Add(-2 * time.Hour)
			exp := fixedNow.Add(-time.Minute) // already expired
			tv.ClaimedBy = &actor
			tv.ClaimedAt = &claimed
			tv.ClaimExpiresAt = &exp
		}
		tasks = append(tasks, tv)
	}

	wip := domain.CompactBudgetTasks
	doing := service.BoardColumn{
		Name:     "Doing",
		Kind:     domain.KindActive,
		WIPLimit: &wip,
		Count:    len(tasks),
		Tasks:    tasks,
	}
	board := &service.Board{Projects: []service.BoardProject{{
		Key:       "BMB",
		Name:      "BeeMemoryBank",
		Version:   11,
		FocusKey:  "BMB-1",
		Columns:   []service.BoardColumn{doing},
		DoneTotal: 200,
	}}}
	out := Render(board, fixedNow)
	tokens := len(out) / 4
	// The golden fixture is the gate on renderer correctness: the renderer
	// must reproduce the bytes the fixture says it produces. Drift the
	// fixture deliberately only via -update after inspecting the diff.
	golden(t, "budget.golden", out)
	// Honest measurement of the release-gate claim. This fixture is
	// intentionally realistic — NOT tuned to pass — so a failure is a real
	// signal that the grammar has grown. The fixture is fixed and
	// deterministic, so this number only moves when the renderer changes.
	// Do not raise the ceiling to make a red run green without measuring the
	// compact/JSON ratio again — the ratio is the claim, the ceiling only
	// guards it.
	if tokens > domain.CompactTokenBudget {
		t.Fatalf("CompactTokenBudget gate: %d tokens over %d (rendered %d bytes).\n\n"+
			"This is the honest measurement of 30 realistic active tasks.\n"+
			"The fixture was deliberately NOT shrunk to fit — see docs/tasks/B-review-1.md.\n"+
			"The grammar most likely grew a field. Check what changed before\n"+
			"touching this number, and re-measure the compact/JSON ratio.\n\n%s",
			tokens, domain.CompactTokenBudget, len(out), out)
	}
}

// TestRender_EstimatesUseProjectUnit is the regression test for architecture
// review finding #10's second half: the unit was configurable per project and
// never reached the renderer, so a project set to days still printed hours.
// The unit now travels on the BoardProject the renderer is handed.
func TestRender_EstimatesUseProjectUnit(t *testing.T) {
	mkBoard := func(unit string) *service.Board {
		return &service.Board{Projects: []service.BoardProject{{
			Key:          "BMB",
			Name:         "BeeMemoryBank",
			EstimateUnit: unit,
			Columns: []service.BoardColumn{mkColumn("Backlog", domain.KindBacklog, 1,
				mkTask("BMB-1", withEstimate(3), withVersion(1)),
			)},
		}}}
	}
	// An unset unit is the "h" default; "d" must actually surface.
	if got := Render(mkBoard(""), fixedNow); !strings.Contains(got, "est 3h") {
		t.Fatalf("default unit h missing: %s", got)
	}
	if got := Render(mkBoard("d"), fixedNow); !strings.Contains(got, "est 3d") {
		t.Fatalf("per-project unit d missing: %s", got)
	}
}

// TestRender_ProjectVersionInHeader pins the project version onto the compact
// header. project_upsert(mode:"update") requires if_version and compact is
// board_get's default format, so a board read in that format has to be able to
// feed the write that follows it.
func TestRender_ProjectVersionInHeader(t *testing.T) {
	board := &service.Board{Projects: []service.BoardProject{{
		Key:     "BMB",
		Name:    "BeeMemoryBank",
		Version: 42,
		Columns: []service.BoardColumn{mkColumn("Backlog", domain.KindBacklog, 0)},
	}}}
	out := Render(board, fixedNow)
	header := strings.Split(out, "\n")[1]
	if !strings.HasSuffix(header, " · v42") {
		t.Fatalf("project header %q does not end in the version segment", header)
	}
	parsed, err := ParseCompactAt(out, fixedNow)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := parsed.Projects[0].Version; got != 42 {
		t.Fatalf("parsed project version = %d, want 42", got)
	}
}

func TestRender_LeaseRemainingIsRemainingNotElapsed(t *testing.T) {
	// 2 hours claimed, 30 min remaining. We must render 30m, never 2h.
	board := &service.Board{Projects: []service.BoardProject{{
		Key:      "BMB",
		Name:     "BeeMemoryBank",
		FocusKey: "",
		Columns: []service.BoardColumn{mkColumn("Doing", domain.KindActive, 1,
			mkTask("BMB-1", withClaim("claude@rog", 30*time.Minute), withVersion(1)),
		)},
	}}}
	out := Render(board, fixedNow)
	if !strings.Contains(out, "lease claude@rog 30m") {
		t.Fatalf("lease remaining not rendered as remaining: %s", out)
	}
	if strings.Contains(out, "lease claude@rog 2h") {
		t.Fatalf("lease rendered as elapsed, not remaining: %s", out)
	}
}

func TestRender_VVersionAlwaysPresent(t *testing.T) {
	// Even a bare-minimum line keeps v<version>.
	board := &service.Board{Projects: []service.BoardProject{{
		Key:      "BMB",
		Name:     "BeeMemoryBank",
		FocusKey: "",
		Columns: []service.BoardColumn{mkColumn("Backlog", domain.KindBacklog, 1,
			mkTask("BMB-1", withVersion(5)),
		)},
	}}}
	out := Render(board, fixedNow)
	if !strings.Contains(out, "v5") {
		t.Fatalf("v<version> missing: %s", out)
	}
}

func TestRender_PriorityNoneOmitsBracket(t *testing.T) {
	board := &service.Board{Projects: []service.BoardProject{{
		Key:      "BMB",
		Name:     "BeeMemoryBank",
		FocusKey: "",
		Columns: []service.BoardColumn{mkColumn("Backlog", domain.KindBacklog, 1,
			mkTask("BMB-1", withPriority(domain.PriorityNone), withVersion(1)),
		)},
	}}}
	out := Render(board, fixedNow)
	if !strings.Contains(out, "[task]") {
		t.Fatalf("expected [task] bracket: %s", out)
	}
	if strings.Contains(out, "[none ") {
		t.Fatalf("priority=none should not appear in brackets: %s", out)
	}
}

func TestRender_SortedTags(t *testing.T) {
	board := &service.Board{Projects: []service.BoardProject{{
		Key:      "BMB",
		Name:     "BeeMemoryBank",
		FocusKey: "",
		Columns: []service.BoardColumn{mkColumn("Backlog", domain.KindBacklog, 1,
			mkTask("BMB-1",
				withTags("sync", "alpha", "zeta"),
				withVersion(1),
			),
		)},
	}}}
	out := Render(board, fixedNow)
	want := "#alpha #sync #zeta"
	if !strings.Contains(out, want) {
		t.Fatalf("tags not sorted: %s", out)
	}
}

func TestRender_TitleDottedSeparatorReplaced(t *testing.T) {
	board := &service.Board{Projects: []service.BoardProject{{
		Key:      "BMB",
		Name:     "BeeMemoryBank",
		FocusKey: "",
		Columns: []service.BoardColumn{mkColumn("Backlog", domain.KindBacklog, 1,
			mkTask("BMB-1",
				withTitle("Fix A · B"),
				withVersion(1),
			),
		)},
	}}}
	out := Render(board, fixedNow)
	if strings.Contains(strings.Split(out, "\n")[2], "Fix A · B") {
		t.Fatalf("title separator not replaced: %s", out)
	}
	if !strings.Contains(out, "Fix A - B") {
		t.Fatalf("expected dash in title: %s", out)
	}
}

func TestParseCompact_FocusMustBeTaskKeyOrNone(t *testing.T) {
	in := "# BMB Foo · focus not-a-key · Backlog 1 · Done 0 (hidden)\n"
	if _, err := ParseCompact(in); err == nil {
		t.Fatalf("focus=bogus should be rejected")
	}
}

func TestParseCompact_DoneSectionWithTasks(t *testing.T) {
	in := "compact_version=1\n# BMB Foo · focus none · Backlog 0 · Done 2 (1 shown)\n## Done\n- BMB-1 [task] Hello · v1\n"
	board, err := ParseCompact(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(board.Projects) != 1 {
		t.Fatalf("projects=%d", len(board.Projects))
	}
	p := board.Projects[0]
	if p.DoneTotal != 2 || p.DoneShown != 1 {
		t.Fatalf("done total/shown = %d/%d", p.DoneTotal, p.DoneShown)
	}
	// The Done column is materialised on demand.
	var done *service.BoardColumn
	for i := range p.Columns {
		if p.Columns[i].Name == "Done" {
			done = &p.Columns[i]
		}
	}
	if done == nil {
		t.Fatalf("Done column not materialised")
	}
	if done.Kind != domain.KindDone {
		t.Errorf("Done.Kind=%s want done", done.Kind)
	}
	if len(done.Tasks) != 1 {
		t.Errorf("Done.Tasks=%d want 1", len(done.Tasks))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(filepath.Join("testdata", filepath.Base(path)), data, 0o644)
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", filepath.Base(path)))
}
