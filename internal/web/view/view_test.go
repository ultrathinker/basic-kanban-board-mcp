package view

import (
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// priorityCSS / priorityLabel
// ---------------------------------------------------------------------------

func TestPriorityCSS(t *testing.T) {
	t.Parallel()
	cases := []struct {
		p    domain.Priority
		want string
	}{
		{domain.PriorityNone, "prio-none"},
		{domain.PriorityLow, "prio-low"},
		{domain.PriorityMedium, "prio-medium"},
		{domain.PriorityHigh, "prio-high"},
		{domain.PriorityCritical, "prio-critical"},
	}
	for _, tc := range cases {
		if got := priorityCSS(tc.p); got != tc.want {
			t.Errorf("priorityCSS(%d) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

func TestPriorityLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		p    domain.Priority
		want string
	}{
		{domain.PriorityNone, "none"},
		{domain.PriorityLow, "low"},
		{domain.PriorityMedium, "medium"},
		{domain.PriorityHigh, "high"},
		{domain.PriorityCritical, "critical"},
	}
	for _, tc := range cases {
		if got := priorityLabel(tc.p); got != tc.want {
			t.Errorf("priorityLabel(%d) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// duration / age / relTime
// ---------------------------------------------------------------------------

func TestFormatDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "expired"},
		{-1 * time.Minute, "expired"},
		{30 * time.Second, "expired"},
		{1 * time.Minute, "1m"},
		{12 * time.Minute, "12m"},
		{59 * time.Minute, "59m"},
		{1 * time.Hour, "1h"},
		{3 * time.Hour, "3h"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{2 * 24 * time.Hour, "2d"},
		{30 * 24 * time.Hour, "30d"},
	}
	for _, tc := range cases {
		if got := formatDuration(tc.in); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRelTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Time{}, ""},
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-3 * time.Minute), "3m ago"},
		{now.Add(-2 * time.Hour), "2h ago"},
		{now.Add(-25 * time.Hour), "1d ago"},
		{now.Add(-3 * 24 * time.Hour), "3d ago"},
		{now.Add(1 * time.Hour), "just now"}, // future times degrade gracefully
	}
	for _, tc := range cases {
		if got := relTime(tc.in, now); got != tc.want {
			t.Errorf("relTime(%v, now) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRelTimeFuture(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Time{}, ""},
		{now.Add(-1 * time.Hour), "overdue"},
		{now.Add(20 * time.Minute), "in 20m"},
		{now.Add(2 * time.Hour), "in 2h"},
		{now.Add(48 * time.Hour), "in 2d"},
	}
	for _, tc := range cases {
		if got := relTimeFuture(tc.in, now); got != tc.want {
			t.Errorf("relTimeFuture(%v, now) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// formatEstimate / truncate / joinTags / commaKeys
// ---------------------------------------------------------------------------

func TestFormatEstimate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    float64
		unit string
		want string
	}{
		{0, "h", "0h"},
		{1, "h", "1h"},
		{2, "h", "2h"},
		{1.5, "h", "1.5h"},
		{2.25, "h", "2.2h"}, // %g rounds
		{30, "m", "30m"},
	}
	for _, tc := range cases {
		if got := formatEstimate(tc.n, tc.unit); got != tc.want {
			t.Errorf("formatEstimate(%v, %q) = %q, want %q", tc.n, tc.unit, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 5, ""},
		{"hi", 5, "hi"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hell…"},
		{"abc", 1, "a"},
		{"abc", 0, ""},
		{"  spaces  collapse  ", 50, "spaces  collapse"},
	}
	for _, tc := range cases {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestJoinTags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"sync"}, "#sync"},
		{[]string{"sync", "ui"}, "#sync #ui"},
	}
	for _, tc := range cases {
		if got := joinTags(tc.in); got != tc.want {
			t.Errorf("joinTags(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCommaKeys(t *testing.T) {
	t.Parallel()
	if got := commaKeys([]string{"BMB-14", "BMB-9"}); got != "BMB-14,BMB-9" {
		t.Errorf("commaKeys = %q", got)
	}
	if got := commaKeys(nil); got != "" {
		t.Errorf("commaKeys(nil) = %q", got)
	}
}

// ---------------------------------------------------------------------------
// URL helpers
// ---------------------------------------------------------------------------

func TestBoardURL(t *testing.T) {
	t.Parallel()
	if got := boardURL(""); got != "/" {
		t.Errorf("boardURL empty = %q, want /", got)
	}
	if got := boardURL("BMB"); got != "/p/BMB" {
		t.Errorf("boardURL BMB = %q, want /p/BMB", got)
	}
	if got := boardURL("bmb"); got != "/p/BMB" {
		t.Errorf("boardURL lower-case = %q, want /p/BMB", got)
	}
}

func TestTaskURL(t *testing.T) {
	t.Parallel()
	if got := taskURL("BMB-14"); got != "/t/BMB-14" {
		t.Errorf("taskURL BMB-14 = %q, want /t/BMB-14", got)
	}
	if got := taskURL("bmb-14"); got != "/t/BMB-14" {
		t.Errorf("taskURL bmb-14 = %q, want /t/BMB-14", got)
	}
}

// ---------------------------------------------------------------------------
// Markdown rendering — bluemonday must drop <script>.
// ---------------------------------------------------------------------------

func TestMarkdownSafeStripsScript(t *testing.T) {
	t.Parallel()
	in := "Hello\n\n<script>alert(1)</script>\n\nEnd"
	out := string(markdownSafe(in))
	if strings.Contains(strings.ToLower(out), "<script") {
		t.Errorf("markdownSafe let script through: %s", out)
	}
	if !strings.Contains(out, "Hello") || !strings.Contains(out, "End") {
		t.Errorf("markdownSafe dropped benign text: %s", out)
	}
}

func TestMarkdownSafeStripsOnclick(t *testing.T) {
	t.Parallel()
	in := "<a href=\"javascript:alert(1)\" onclick=\"alert(1)\">click</a>"
	out := string(markdownSafe(in))
	if strings.Contains(strings.ToLower(out), "onclick") {
		t.Errorf("markdownSafe let onclick through: %s", out)
	}
	if strings.Contains(strings.ToLower(out), "javascript:") {
		t.Errorf("markdownSafe let javascript: href through: %s", out)
	}
}

func TestMarkdownSafeHandlesEmpty(t *testing.T) {
	t.Parallel()
	if got := markdownSafe(""); got != "" {
		t.Errorf("markdownSafe empty = %q, want empty", got)
	}
	if got := markdownSafe("   \n\n  "); got != "" {
		t.Errorf("markdownSafe whitespace = %q, want empty", got)
	}
}

func TestMarkdownSafeRendersHeading(t *testing.T) {
	t.Parallel()
	out := string(markdownSafe("## Hello\n\nworld"))
	if !strings.Contains(out, "<h2") {
		t.Errorf("expected <h2, got %q", out)
	}
	if !strings.Contains(out, "world") {
		t.Errorf("expected world, got %q", out)
	}
}

func TestMarkdownSafeDegradesOnError(t *testing.T) {
	t.Parallel()
	// Null byte in input is malformed — render should still return a string,
	// not crash. The output is HTML-escaped in that case.
	out := string(markdownSafe("\x00broken"))
	if out == "" {
		t.Errorf("expected non-empty fallback")
	}
}

// ---------------------------------------------------------------------------
// hasAny
// ---------------------------------------------------------------------------

func TestHasAny(t *testing.T) {
	t.Parallel()
	if hasAny(nil) {
		t.Errorf("hasAny(nil) = true")
	}
	if hasAny([]string{}) {
		t.Errorf("hasAny([]) = true")
	}
	if !hasAny([]string{"x"}) {
		t.Errorf("hasAny([x]) = false")
	}
	if !hasAny([]TaskCard{{Key: "BMB-1"}}) {
		t.Errorf("hasAny([TaskCard]) = false")
	}
}

// ---------------------------------------------------------------------------
// BuildBoard — a small, table-driven end-to-end check.
// ---------------------------------------------------------------------------

func TestBuildBoardAssignsColumnsAndHidesDone(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	columns := []domain.Column{
		{ID: "c-b", Name: "Backlog", Kind: domain.KindBacklog},
		{ID: "c-d", Name: "Doing", Kind: domain.KindActive, WIPLimit: intPtr(3)},
		{ID: "c-o", Name: "Done", Kind: domain.KindDone},
	}
	t1 := domain.TaskView{ProjectKey: "BMB"}
	t1.ID, t1.Key, t1.Title, t1.ColumnEnteredAt, t1.Version = "t1", "BMB-1", "x", now, 1
	t1.ColumnID, t1.ColumnName, t1.ColumnKind = "c-b", "Backlog", domain.KindBacklog

	t2 := domain.TaskView{ProjectKey: "BMB"}
	t2.ID, t2.Key, t2.Title, t2.ColumnEnteredAt, t2.Version = "t2", "BMB-2", "y", now, 2
	t2.ColumnID, t2.ColumnName, t2.ColumnKind = "c-d", "Doing", domain.KindActive

	archived := now
	t3 := domain.TaskView{ProjectKey: "BMB"}
	t3.ID, t3.Key, t3.Title, t3.ColumnEnteredAt, t3.Version = "t3", "BMB-3", "z", now, 3
	t3.ArchivedAt = &archived
	t3.ColumnID, t3.ColumnName, t3.ColumnKind = "c-d", "Doing", domain.KindActive

	t4 := domain.TaskView{ProjectKey: "BMB"}
	t4.ID, t4.Key, t4.Title, t4.ColumnEnteredAt, t4.Version = "t4", "BMB-4", "d", now, 4
	t4.ColumnID, t4.ColumnName, t4.ColumnKind = "c-o", "Done", domain.KindDone

	tasks := []domain.TaskView{t1, t2, t3, t4}
	m := BuildBoard(ProjectSummary{Key: "BMB"}, nil, columns, tasks, true)
	if got := len(m.Columns); got != 3 {
		t.Fatalf("Columns len = %d, want 3", got)
	}
	for _, c := range m.Columns {
		switch c.Name {
		case "Backlog":
			if len(c.Tasks) != 1 {
				t.Errorf("Backlog tasks = %d, want 1", len(c.Tasks))
			}
		case "Doing":
			if len(c.Tasks) != 1 {
				t.Errorf("Doing tasks = %d, want 1 (archived should be skipped)", len(c.Tasks))
			}
		case "Done":
			if !c.Hidden {
				t.Errorf("Done column should be hidden when hideDone=true")
			}
		}
	}
	if m.DoneTotal != 1 {
		t.Errorf("DoneTotal = %d, want 1", m.DoneTotal)
	}
}

func TestBuildBoardClaimStates(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	columns := []domain.Column{{ID: "c", Name: "Doing", Kind: domain.KindActive}}
	ca := now.Add(-30 * time.Minute)
	ce := now.Add(30 * time.Minute) // 30 min in the future of the real clock
	expiredAt := now.Add(-1 * time.Minute)
	actor := "claude@rog"

	a := domain.TaskView{ProjectKey: "BMB"}
	a.ID, a.Key, a.Title, a.ColumnEnteredAt, a.Version = "t1", "BMB-1", "live lease", now, 1
	a.ColumnID, a.ColumnName, a.ColumnKind = "c", "Doing", domain.KindActive
	a.ClaimedBy, a.ClaimedAt, a.ClaimExpiresAt = &actor, &ca, &ce

	b := domain.TaskView{ProjectKey: "BMB"}
	b.ID, b.Key, b.Title, b.ColumnEnteredAt, b.Version = "t2", "BMB-2", "expired lease", now, 1
	b.ColumnID, b.ColumnName, b.ColumnKind = "c", "Doing", domain.KindActive
	b.ClaimedBy, b.ClaimedAt, b.ClaimExpiresAt = &actor, &ca, &expiredAt

	tasks := []domain.TaskView{a, b}
	m := BuildBoard(ProjectSummary{Key: "BMB"}, nil, columns, tasks, false)
	if len(m.Columns[0].Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(m.Columns[0].Tasks))
	}
	if m.Columns[0].Tasks[0].Lease == nil || m.Columns[0].Tasks[0].Lease.State != "live" {
		t.Errorf("BMB-1 lease = %+v", m.Columns[0].Tasks[0].Lease)
	}
	if m.Columns[0].Tasks[1].Lease == nil || m.Columns[0].Tasks[1].Lease.State != "expired" {
		t.Errorf("BMB-2 lease = %+v", m.Columns[0].Tasks[1].Lease)
	}
}

// ---------------------------------------------------------------------------
// Sample fixtures exist and render into a non-zero BoardModel.
// ---------------------------------------------------------------------------

func TestSampleBoardModel(t *testing.T) {
	t.Parallel()
	m, cols, tasks := SampleBoardModel()
	if m.Project.Key != "BMB" {
		t.Errorf("project key = %q", m.Project.Key)
	}
	if len(m.Columns) != len(cols) {
		t.Errorf("column count mismatch: %d vs %d", len(m.Columns), len(cols))
	}
	if m.Focus == nil || m.Focus.Key != "BMB-14" {
		t.Errorf("focus = %+v", m.Focus)
	}
	if len(tasks) == 0 {
		t.Errorf("expected fixture tasks")
	}
}

func TestSampleOverviewModel(t *testing.T) {
	t.Parallel()
	m := SampleOverviewModel()
	if m.Total.Projects != 3 {
		t.Errorf("Total.Projects = %d", m.Total.Projects)
	}
	if len(m.Projects) != 3 {
		t.Errorf("len(Projects) = %d", len(m.Projects))
	}
}

func TestSampleAdminModel(t *testing.T) {
	t.Parallel()
	m := SampleAdminModel()
	if len(m.Tokens) == 0 {
		t.Errorf("expected tokens")
	}
	if m.ExportURL == "" {
		t.Errorf("ExportURL empty")
	}
}

func TestSampleAgentSetup(t *testing.T) {
	t.Parallel()
	m := SampleAgentSetup("http://example.test", "admin")
	if m.BaseURL != "http://example.test" {
		t.Errorf("BaseURL = %q", m.BaseURL)
	}
	if len(m.Snippets) != 4 {
		t.Errorf("expected 4 snippets, got %d", len(m.Snippets))
	}
	for _, s := range m.Snippets {
		if !strings.Contains(s.Config, "Authorization") {
			t.Errorf("snippet %q missing Authorization header", s.ID)
		}
	}
}

func TestSampleDrawerModelHasRenderedHTML(t *testing.T) {
	t.Parallel()
	d := SampleDrawerModel()
	if d.Task.Key != "BMB-14" {
		t.Errorf("key = %q", d.Task.Key)
	}
	if !strings.Contains(string(d.MarkdownHTML), "<h2") {
		t.Errorf("markdown HTML missing <h2>: %q", d.MarkdownHTML)
	}
	if len(d.Acceptance) == 0 {
		t.Errorf("expected acceptance items")
	}
	if len(d.Blockers) == 0 {
		t.Errorf("expected blockers")
	}
}

// ---------------------------------------------------------------------------
// Funcs returns a usable FuncMap (non-nil, with a known helper).
// ---------------------------------------------------------------------------

func TestFuncs(t *testing.T) {
	t.Parallel()
	f := Funcs()
	if f == nil {
		t.Fatal("Funcs() = nil")
	}
	for _, name := range []string{"priorityClass", "priorityName", "duration", "age", "relTime", "markdown", "truncate", "joinTags"} {
		if _, ok := f[name]; !ok {
			t.Errorf("Funcs missing %q", name)
		}
	}
}
