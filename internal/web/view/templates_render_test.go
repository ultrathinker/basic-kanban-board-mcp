package view_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// This file exists because the first cut of the UI shipped two templates
// that failed on EVERY render — a WIP badge that indexed an *int as if it
// were a slice, and a drawer checkbox that read a field off the wrong
// struct. Neither is subtle; both survived because nothing had ever
// executed a template against real data. So the rule this file enforces is:
//
//	every named template is executed, against a fixture rich enough to walk
//	its branches, and the render must not error.
//
// html/template reports a bad field or a bad `index` at EXECUTION time, not
// at parse time, so "it compiles" and even "it parses" prove nothing. Only
// Execute does.
//
// These tests live in internal/web/view rather than next to the templates
// because internal/web/templates is a GENERATED mirror of web/templates:
// `make check-embed` fails on any file there that web/templates does not
// also have, and templates.go (the embed directive) is the single exemption.
// A _test.go file dropped into the mirror would break the CI mirror job.

// templatesDir is the mirrored template source, one directory over. It is
// what //go:embed compiles in, so it is what the tests must execute.
const templatesDir = "../templates"

// canonicalTemplatesDir is where the templates are actually edited.
const canonicalTemplatesDir = "../../../web/templates"

// renderCase is one named template plus the data it binds to.
type renderCase struct {
	name string
	// template is the {{define}} name to execute.
	template string
	data     any
	// wants are substrings that must appear. They are deliberately about
	// behaviour (a WIP badge, a CSRF field, a version attribute), not about
	// styling — a class rename must not fail this suite, a missing hidden
	// input must.
	wants []string
	// notWants catches things that must never reach the page.
	notWants []string
	// wantEmpty marks a case whose correct output IS nothing — a guarded
	// fragment that must disappear rather than render an empty shell.
	wantEmpty bool
}

func boardPage() view.Page {
	m, _, _ := view.SampleBoardModel()
	return view.SamplePage("BeeMemoryBank", "board", m)
}

func renderCases() []renderCase {
	drawer := view.SampleDrawerModel()
	board, _, _ := view.SampleBoardModel()

	return []renderCase{
		{
			name:     "board/full",
			template: "page-board",
			data:     boardPage(),
			wants: []string{
				// The WIP badge: "Doing" has a limit of 3 and three tasks, so
				// this is both the count/limit rendering and the "full" state.
				// `index .Column.WIP 0` crashed exactly here.
				"3/3",
				`class="wip full"`,
				// The empty "Ready" column still renders its header.
				"Ready",
				"No tasks",
				// data-version is what lets a drag send if_version.
				`data-version="7"`,
				// The focus banner.
				"BMB-14",
				// Live updates are declared as a change signal, not as an
				// element that swaps raw JSON into itself.
				`data-live-source="/events/p/BMB"`,
				`data-live-region="#board"`,
				`id="board"`,
				// Export is a POST form carrying a CSRF token, not a bare link.
				`name="csrf_token"`,
				`method="post" action="/p/BMB/export"`,
				// The keyboard alternative to drag.
				"data-move-menu",
			},
			notWants: []string{
				// sse-swap on a status span put raw JSON on the page.
				"sse-swap",
				"hx-ext",
				// Inline handlers are dead under `script-src 'self'`.
				"onchange=",
				"onclick=",
				// A template that silently produced no value.
				"<no value>",
				"ZgotmplZ",
			},
		},
		{
			name:     "board/empty-project",
			template: "page-board",
			data:     view.SamplePage("Fresh project", "board", view.SampleEmptyBoardModel()),
			wants:    []string{"No tasks", "Backlog"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "board/no-columns",
			template: "page-board",
			data:     view.SamplePage("No columns yet", "board", view.SampleColumnlessBoardModel()),
			// A project with no columns must say so rather than render a void.
			wants:    []string{"no columns yet"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "drawer/full",
			template: "page-drawer",
			data:     view.SamplePage("BMB-14", "board", drawer),
			wants: []string{
				// The acceptance list is the block that crashed on
				// `{{if not $.CanEdit}}` — $ is the Page, not the model.
				"Acceptance",
				"no WAL growth over 24h",
				`type="checkbox"`,
				// Blockers, blocking, subtasks, notes and history all present.
				"Blocked by",
				"BMB-9",
				"Blocking",
				"BMB-17",
				"Subtasks",
				"Notes",
				"History",
				// Rendered markdown, not escaped source.
				"<h2",
			},
			notWants: []string{"<no value>", "ZgotmplZ", "&lt;h2"},
		},
		{
			name:     "drawer/minimal",
			template: "page-drawer",
			data:     view.SamplePage("BMB-42", "board", view.SampleMinimalDrawerModel()),
			wants:    []string{"BMB-42", "No description."},
			// With CanEdit false the checkbox must be disabled — but there
			// are no acceptance items at all here, so the whole section is
			// absent rather than rendering an empty <ul>.
			notWants: []string{"Acceptance", "Subtasks", "Notes", "History", "<no value>"},
		},
		{
			name:     "activity/full",
			template: "page-activity",
			data:     view.SamplePage("Activity · BMB", "activity", view.SampleActivityModel()),
			wants: []string{
				`id="activity-feed"`,
				`aria-live="polite"`,
				"claude@rog",
				`data-live-region="#activity-feed"`,
			},
			notWants: []string{"sse-swap", "<no value>"},
		},
		{
			name:     "activity/empty",
			template: "page-activity",
			data:     view.SamplePage("Activity · BMB", "activity", view.SampleEmptyActivityModel()),
			wants:    []string{"No activity yet"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "overview/full",
			template: "page-overview",
			data:     view.SamplePage("Overview", "overview", view.SampleOverviewModel()),
			wants:    []string{"BeeMemoryBank", "backlog", "/p/BMB"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "overview/empty",
			template: "page-overview",
			data:     view.SamplePage("Overview", "overview", view.SampleEmptyOverviewModel()),
			wants:    []string{"No projects yet"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "login/anonymous",
			template: "page-login",
			data:     view.SampleAnonymousPage("Sign in", "login", view.SampleLoginModel("", "/p/BMB")),
			wants: []string{
				`name="csrf_token"`,
				`value="csrf-fixture-token"`,
				`name="next" value="/p/BMB"`,
				"Sign in",
			},
			// Anonymous chrome: no sign-out form, no project switcher.
			notWants: []string{"/logout", "data-auto-submit", "<no value>"},
		},
		{
			name:     "login/error",
			template: "page-login",
			data:     view.SampleAnonymousPage("Sign in", "login", view.SampleLoginModel("That token was not recognized.", "")),
			wants:    []string{`role="alert"`, "That token was not recognized."},
			notWants: []string{"<no value>"},
		},
		{
			name:     "firstrun",
			template: "page-firstrun",
			data:     view.SampleAnonymousPage("Welcome", "login", view.SampleFirstRunModel("kbn_demo_0123456789abcdef")),
			wants:    []string{"kbn_demo_0123456789abcdef", "data-copy"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "agent-setup",
			template: "page-agent-setup",
			data:     view.SampleAnonymousPage("Agent setup", "agent-setup", view.SampleAgentSetup("http://127.0.0.1:8080", "admin")),
			// The Authorization header is the thing everyone fumbles; if it
			// ever stops rendering, the page has no reason to exist.
			wants:    []string{"Authorization", "Bearer", "Claude Code", "Codex CLI", "Cursor"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "admin/full",
			template: "page-admin",
			data:     view.SamplePage("Admin", "admin", view.SampleAdminModel()),
			wants: []string{
				// Every admin mutation is a POST form with a CSRF token. The
				// handlers call verifyCSRF, so a form without the hidden
				// field is a guaranteed 403 — which is how they shipped.
				`method="post" action="/admin/tokens"`,
				`action="/admin/tokens/claude@rog/rotate"`,
				`action="/admin/tokens/claude@rog/revoke"`,
				`method="post" action="/admin/export"`,
				`method="post" action="/admin/backup"`,
				`name="csrf_token"`,
			},
			notWants: []string{
				// Backup used to be a bare <a href>: a GET that writes a file.
				`<a class="btn btn-primary" href="/admin/backup"`,
				`href="/admin/export"`,
				"<no value>",
			},
		},
		{
			name:     "admin/empty",
			template: "page-admin",
			data:     view.SamplePage("Admin", "admin", view.SampleEmptyAdminModel()),
			wants:    []string{"No tokens.", "No projects."},
			notWants: []string{"<no value>"},
		},

		// --- fragments, rendered on their own the way a swap would ---------
		{
			name:     "fragment/card",
			template: "card",
			data:     map[string]any{"Card": board.Columns[2].Tasks[0], "CSRF": "t", "Columns": board.Columns},
			wants:    []string{"data-task-key=", "data-version=", "data-move-menu"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "fragment/card-without-columns",
			template: "card",
			// A fragment handler that forgets to pass the column list must
			// still render a card — just without the move menu.
			data:     map[string]any{"Card": board.Columns[2].Tasks[0], "CSRF": "t"},
			wants:    []string{"data-task-key="},
			notWants: []string{"data-move-menu", "<no value>"},
		},
		{
			name:     "fragment/column-wip-full",
			template: "column",
			data:     map[string]any{"Column": columnNamed(board, "Doing"), "CSRF": "t", "Columns": board.Columns},
			wants:    []string{"3/3", "full"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "fragment/column-empty",
			template: "column",
			data:     map[string]any{"Column": columnNamed(board, "Ready"), "CSRF": "t", "Columns": board.Columns},
			wants:    []string{"No tasks"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "fragment/column-hidden-done",
			template: "column",
			data:     map[string]any{"Column": columnNamed(board, "Done"), "CSRF": "t", "Columns": board.Columns},
			// hide_done is on in the fixture, so the done column collapses to
			// a count rather than rendering cards nobody asked for.
			wants:    []string{"hidden", "show done"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "fragment/board-columns",
			template: "board-columns",
			data:     map[string]any{"Columns": board.Columns, "CSRF": "t"},
			wants:    []string{`id="board"`, "Backlog", "Doing"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "fragment/activity-list",
			template: "activity-list",
			data:     map[string]any{"Events": view.SampleActivityModel().Events},
			wants:    []string{`id="activity-feed"`, "claude@rog"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "fragment/activity-list-empty",
			template: "activity-list",
			data:     map[string]any{"Events": nil},
			wants:    []string{"No activity yet"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "fragment/activity-row",
			template: "activity-row",
			data:     map[string]any{"Event": view.SampleActivityModel().Events[0]},
			wants:    []string{"claude@rog", "BMB-14"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "fragment/focus-banner",
			template: "focus-banner",
			data:     map[string]any{"Focus": board.Focus},
			wants:    []string{"BMB-14", "now"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "fragment/focus-banner-none",
			template: "focus-banner",
			data:     map[string]any{"Focus": nil},
			// No focus set: the banner must vanish, not render an empty band.
			notWants:  []string{"now", "<no value>"},
			wantEmpty: true,
		},
		{
			name:     "fragment/card-move-menu",
			template: "card-move-menu",
			data:     map[string]any{"Card": board.Columns[2].Tasks[0], "Columns": board.Columns, "CSRF": "t"},
			wants:    []string{`<option value="Backlog">`, `<option value="Review">`},
			// The card is in "Doing"; offering "move to Doing" is noise.
			notWants: []string{`<option value="Doing">`, "<no value>"},
		},
		{
			name:     "fragment/live-status",
			template: "live-status",
			data:     map[string]any{},
			wants:    []string{"data-live-status", "data-live-label"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "chrome/topbar-signed-in",
			template: "topbar",
			data:     boardPage(),
			wants: []string{
				// Signing out is a POST with CSRF, never a link.
				`action="/logout"`,
				`name="csrf_token"`,
				"data-theme-toggle",
				"data-auto-submit",
			},
			notWants: []string{"onchange=", "<no value>"},
		},
		{
			name:     "chrome/topbar-anonymous",
			template: "topbar",
			data:     view.SampleAnonymousPage("Sign in", "login", view.SampleLoginModel("", "")),
			wants:    []string{"Sign in"},
			notWants: []string{"/logout", "/admin", "<no value>"},
		},
		{
			name:     "chrome/project-switcher",
			template: "project-switcher",
			data:     boardPage(),
			wants:    []string{`value="BMB"`, "selected", "data-auto-submit"},
			notWants: []string{"onchange=", "<no value>"},
		},
		{
			name:     "chrome/layout-head",
			template: "layout-head",
			data:     boardPage(),
			wants:    []string{`name="csrf-token"`, "/static/app.css", "skip-link"},
			notWants: []string{"<no value>"},
		},
		{
			name:     "chrome/layout-foot",
			template: "layout-foot",
			data:     boardPage(),
			wants:    []string{"/static/app.js", "/static/vendor/sortable.min.js"},
			// Three vendored libraries are deliberately not loaded, each for
			// a reason recorded in web/static/vendor/LICENSES.md: the SSE
			// extension targets htmx 2's extension API and throws against the
			// vendored htmx 4; htmx 4 itself had no consumer once the inert
			// hx-ext markup went; and the standard Alpine build evaluates
			// x- expressions with new Function(), which our own CSP refuses.
			notWants: []string{"sse.min.js", "htmx.min.js", "alpine.min.js", "<no value>"},
		},
		{
			name:     "chrome/footer",
			template: "footer",
			data:     boardPage(),
			wants:    []string{"Agent setup"},
			notWants: []string{"<no value>"},
		},
	}
}

func columnNamed(b view.BoardModel, name string) view.ColumnView {
	for _, c := range b.Columns {
		if c.Name == name {
			return c
		}
	}
	panic("fixture has no column named " + name)
}

// TestNewParsesEmbeddedTemplates is the cheapest possible guard: New() runs
// the whole funcmap against the whole template set at parse time, so a
// helper the templates call but view.Funcs does not define (this is how
// `dict` went missing) fails right here.
func TestNewParsesEmbeddedTemplates(t *testing.T) {
	t.Parallel()
	if _, err := templates.New(); err != nil {
		t.Fatalf("templates.New() = %v", err)
	}
}

// TestRenderEveryNamedTemplate executes each named template against a
// fixture. A template that only renders under empty data is untested, so
// the cases above deliberately include a WIP-limited column that is full,
// an empty column, a collapsed done column, a drawer with acceptance items,
// a focused task, a task with blockers and every empty-state counterpart.
func TestRenderEveryNamedTemplate(t *testing.T) {
	t.Parallel()
	eng, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New(): %v", err)
	}
	for _, tc := range renderCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !eng.HasTemplate(tc.template) {
				t.Fatalf("template %q is not defined", tc.template)
			}
			var buf bytes.Buffer
			if err := eng.Render(&buf, tc.template, tc.data); err != nil {
				t.Fatalf("render %q: %v", tc.template, err)
			}
			out := buf.String()
			switch {
			case tc.wantEmpty && strings.TrimSpace(out) != "":
				t.Fatalf("render %q should have produced nothing, got:\n%s", tc.template, out)
			case !tc.wantEmpty && strings.TrimSpace(out) == "":
				t.Fatalf("render %q produced no output", tc.template)
			}
			for _, want := range tc.wants {
				if !strings.Contains(out, want) {
					t.Errorf("render %q: missing %q", tc.template, want)
				}
			}
			for _, notWant := range tc.notWants {
				if strings.Contains(out, notWant) {
					t.Errorf("render %q: unexpectedly contains %q", tc.template, notWant)
				}
			}
			if t.Failed() {
				t.Logf("rendered output:\n%s", out)
			}
		})
	}
}

var defineRe = regexp.MustCompile(`{{-?\s*define\s+"([^"]+)"`)

// TestEveryDefinedTemplateIsCovered fails when someone adds a
// {{define "..."}} without adding a render case for it. Coverage of the
// template set is the whole point of this file; a new template that nobody
// executes is exactly the hole that shipped last time.
func TestEveryDefinedTemplateIsCovered(t *testing.T) {
	t.Parallel()
	defined := definedTemplateNames(t)
	covered := map[string]bool{}
	for _, tc := range renderCases() {
		covered[tc.template] = true
	}
	var missing []string
	for name := range defined {
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("templates defined but never rendered by a test: %v\n"+
			"Add a case to renderCases() — a template nobody executes is a template nobody has tested.", missing)
	}
}

// TestFilenamesMatchSources keeps the test honest about which files it is
// reading: Filenames() is what the mirror check and uipreview rely on.
func TestFilenamesMatchSources(t *testing.T) {
	t.Parallel()
	names, err := templates.Filenames(templatesDir)
	if err != nil {
		t.Fatalf("Filenames: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("Filenames returned nothing")
	}
	for _, n := range names {
		if !strings.HasSuffix(n, ".html") {
			t.Errorf("Filenames returned a non-template: %s", n)
		}
	}
}

// TestEmbeddedMatchesCanonicalSource is the drift check with teeth. The
// canonical templates live at web/templates and internal/web/templates is a
// generated mirror (`make sync-embed`); editing the mirror is silently
// undone by the next build. CI runs check-embed, but a developer running
// `go test ./...` on Windows without make deserves the same warning.
func TestEmbeddedMatchesCanonicalSource(t *testing.T) {
	t.Parallel()
	canonicalDir := canonicalTemplatesDir
	entries, err := os.ReadDir(canonicalDir)
	if err != nil {
		t.Skipf("canonical template dir not available (installed module?): %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		canonical, err := os.ReadFile(filepath.Join(canonicalDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		mirrored, err := os.ReadFile(filepath.Join(templatesDir, e.Name()))
		if err != nil {
			t.Fatalf("mirror is missing %s — run `make sync-embed`: %v", e.Name(), err)
		}
		if !bytes.Equal(normalizeEOL(canonical), normalizeEOL(mirrored)) {
			t.Errorf("internal/web/templates/%s differs from web/templates/%s.\n"+
				"web/ is the source of truth; run `make sync-embed` and commit the mirror.", e.Name(), e.Name())
		}
	}
}

func normalizeEOL(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }

func definedTemplateNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		t.Fatalf("read template dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(templatesDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range defineRe.FindAllSubmatch(src, -1) {
			names[string(m[1])] = true
		}
	}
	if len(names) == 0 {
		t.Fatal("no {{define}} blocks found — the test is not reading the templates")
	}
	return names
}
