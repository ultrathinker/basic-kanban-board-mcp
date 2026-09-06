package view_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The stylesheet is not Go, so nothing else in the repo can hold it to the
// visual direction the maintainer picked. These tests do: they are the
// executable form of "monochrome Swiss minimalism — greyscale only,
// hairlines, no elevation".
//
// They live in internal/web/view, not beside the assets, because
// internal/web/{static,templates} are GENERATED mirrors of web/: any extra
// file in either one fails `make check-embed` and the CI mirror job.

const (
	canonicalCSS = "../../../web/static/app.css"
	canonicalJS  = "../../../web/static/app.js"
)

func readAsset(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("asset not available (installed module?): %v", err)
	}
	return string(b)
}

// stripComments removes /* ... */ blocks so the prose in the file header
// (which legitimately talks about red chips and colour) is not mistaken for
// a declaration.
var commentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)

func stripComments(css string) string { return commentRe.ReplaceAllString(css, " ") }

var hexRe = regexp.MustCompile(`#([0-9a-fA-F]{3}|[0-9a-fA-F]{4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})\b`)

// TestStylesheetIsGreyscale is the load-bearing one. A kanban board without
// colour only works if the discipline actually holds: one saturated hex
// slipped into a "temporary" badge and the whole argument for encoding
// priority in weight and rules falls apart.
func TestStylesheetIsGreyscale(t *testing.T) {
	t.Parallel()
	css := stripComments(readAsset(t, canonicalCSS))

	for _, m := range hexRe.FindAllStringSubmatch(css, -1) {
		r, g, b, ok := expandHex(m[1])
		if !ok {
			continue
		}
		if r != g || g != b {
			t.Errorf("non-greyscale colour %s (r=%d g=%d b=%d): the palette is paper, ink and greys only", m[0], r, g, b)
		}
	}

	// Functional colour notations can smuggle a hue past the hex check.
	for _, banned := range []string{"hsl(", "hsla(", "lch(", "oklch(", "lab(", "oklab(", "color-mix(", "gradient("} {
		if strings.Contains(css, banned) {
			t.Errorf("stylesheet uses %s — the palette is greyscale hex only, and gradients are out", banned)
		}
	}
	// rgb()/rgba() are allowed only when all three channels match.
	rgbRe := regexp.MustCompile(`rgba?\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)`)
	for _, m := range rgbRe.FindAllStringSubmatch(css, -1) {
		if m[1] != m[2] || m[2] != m[3] {
			t.Errorf("non-greyscale rgb() %s", m[0])
		}
	}
	// Named colours with a hue.
	namedRe := regexp.MustCompile(`:\s*(red|green|blue|orange|yellow|purple|pink|teal|cyan|magenta|gold|crimson|tomato|salmon|navy|olive|maroon|lime|aqua|fuchsia|silver)\b`)
	for _, m := range namedRe.FindAllString(css, -1) {
		t.Errorf("named colour%s — greyscale only", m)
	}
}

// TestStylesheetHasNoElevation enforces "structure over decoration":
// hairline rules instead of cards with shadows, and near-zero radius.
func TestStylesheetHasNoElevation(t *testing.T) {
	t.Parallel()
	css := stripComments(readAsset(t, canonicalCSS))

	if strings.Contains(css, "box-shadow") {
		t.Error("box-shadow found: columns and cards are separated by 1px rules, never by elevation")
	}
	if strings.Contains(css, "text-shadow") {
		t.Error("text-shadow found")
	}

	radiusRe := regexp.MustCompile(`border(-[a-z]+)*-radius\s*:\s*([^;}]+)`)
	for _, m := range radiusRe.FindAllStringSubmatch(css, -1) {
		v := strings.TrimSpace(m[2])
		if v != "0" && v != "0px" && v != "0%" {
			t.Errorf("non-zero border-radius %q: the direction is square corners", v)
		}
	}
}

// TestStylesheetShipsNoExternalRequests keeps the promise that a board works
// on a machine with no internet: no CDN, no webfont fetch, no @import.
func TestStylesheetShipsNoExternalRequests(t *testing.T) {
	t.Parallel()
	css := stripComments(readAsset(t, canonicalCSS))
	if strings.Contains(css, "@import") {
		t.Error("@import found: every asset must be served from this binary")
	}
	urlRe := regexp.MustCompile(`url\(\s*['"]?([^)'"]+)`)
	for _, m := range urlRe.FindAllStringSubmatch(css, -1) {
		ref := strings.TrimSpace(m[1])
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "//") {
			t.Errorf("external url(%s): no runtime network requests are allowed", ref)
		}
	}
}

var (
	varUseRe = regexp.MustCompile(`var\(\s*(--[a-zA-Z0-9-]+)`)
	varDefRe = regexp.MustCompile(`(--[a-zA-Z0-9-]+)\s*:`)
)

// TestThemeTokensAreReachable catches the failure mode that silently voided
// the entire light palette before: the tokens were declared inside an
// `@theme { … }` block, which is a Tailwind-compiler at-rule. A browser
// drops an unknown at-rule and everything inside it, and because the dark
// overrides sat in ordinary `:root` blocks, the bug was invisible in dark
// mode. Tokens must be declared where a browser will actually read them.
func TestThemeTokensAreReachable(t *testing.T) {
	t.Parallel()
	css := stripComments(readAsset(t, canonicalCSS))

	if strings.Contains(css, "@theme") {
		t.Error("@theme block found: browsers drop unknown at-rules wholesale, taking every token in them. Declare tokens on :root.")
	}

	defined := map[string]bool{}
	for _, m := range varDefRe.FindAllStringSubmatch(css, -1) {
		defined[m[1]] = true
	}
	// Locally scoped custom properties set on a component (the priority rule
	// sets these on the card) are defined by their own rules, so they are
	// already in `defined`; anything left undefined is a typo.
	var missing []string
	for _, m := range varUseRe.FindAllStringSubmatch(css, -1) {
		if !defined[m[1]] {
			missing = append(missing, m[1])
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("var() references with no definition: %v", dedupe(missing))
	}
}

// TestDarkThemeIsCompleteInBothDirections enforces the theming rule from
// AGENTS.md / PLAN §9: never define a colour only inside a dark-mode block,
// and let the manual toggle win over the OS preference in both directions.
func TestDarkThemeIsCompleteInBothDirections(t *testing.T) {
	t.Parallel()
	css := stripComments(readAsset(t, canonicalCSS))

	if !strings.Contains(css, `:root:not([data-theme="light"])`) {
		t.Error(`the prefers-color-scheme block must be guarded with :root:not([data-theme="light"]) so an explicit light choice wins`)
	}
	if !strings.Contains(css, `:root[data-theme="dark"]`) {
		t.Error(`missing :root[data-theme="dark"] — the manual toggle must win over the OS preference`)
	}

	rootTokens := tokensInBlock(t, css, ":root {")
	mediaTokens := tokensInBlock(t, css, `:root:not([data-theme="light"]) {`)
	manualTokens := tokensInBlock(t, css, `:root[data-theme="dark"] {`)

	if len(rootTokens) == 0 {
		t.Fatal("no tokens found on bare :root")
	}
	for name := range mediaTokens {
		if !rootTokens[name] {
			t.Errorf("%s is defined only in the dark-mode media block; every colour needs a light definition on :root", name)
		}
	}
	// The two dark blocks must agree, or the toggle and the OS preference
	// would produce two different dark themes.
	for name := range mediaTokens {
		if !manualTokens[name] {
			t.Errorf(`%s is overridden under prefers-color-scheme but not under [data-theme="dark"]`, name)
		}
	}
	for name := range manualTokens {
		if !mediaTokens[name] {
			t.Errorf(`%s is overridden under [data-theme="dark"] but not under prefers-color-scheme`, name)
		}
	}
}

// TestClientScriptHasNoInlineEvalOrCDN mirrors the CSP the server sends:
// `script-src 'self'` with no 'unsafe-inline' and no 'unsafe-eval'.
func TestClientScriptHasNoInlineEvalOrCDN(t *testing.T) {
	t.Parallel()
	js := readAsset(t, canonicalJS)
	for _, banned := range []string{"eval(", "new Function(", "https://", "http://"} {
		if strings.Contains(js, banned) {
			t.Errorf("app.js contains %q — the CSP forbids eval and there are no runtime network requests", banned)
		}
	}
}

// TestTemplatesHaveNoInlineHandlers is the other half of the same rule: an
// onclick=""/onchange="" attribute is silently dead under the CSP, which is
// exactly how the project switcher stopped submitting.
func TestTemplatesHaveNoInlineHandlers(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		t.Fatalf("read template dir: %v", err)
	}
	handlerRe := regexp.MustCompile(`(?i)\son[a-z]+\s*=\s*["']`)
	// Template comments talk about the rule; they do not break it.
	tplCommentRe := regexp.MustCompile(`(?s){{-?/\*.*?\*/-?}}`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(templatesDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		src := tplCommentRe.ReplaceAll(raw, []byte(" "))
		for _, m := range handlerRe.FindAllString(string(src), -1) {
			t.Errorf("%s has an inline event handler%q — wire it from app.js by data- attribute instead", e.Name(), strings.TrimSpace(m))
		}
		// The standard Alpine build evaluates x- expressions with
		// new Function(), which `script-src 'self'` refuses, so an x-
		// directive in a template is markup that cannot run.
		if bytes.Contains(src, []byte(" x-")) {
			t.Errorf("%s uses an Alpine x- directive, but Alpine is not loaded (see web/static/vendor/LICENSES.md)", e.Name())
		}
	}
}

// TestStaticMirrorMatchesCanonicalSource is the static-asset twin of the
// template mirror check: web/static is the source of truth, internal/web/static
// is generated by `make sync-embed`, and editing the mirror is silently undone.
func TestStaticMirrorMatchesCanonicalSource(t *testing.T) {
	t.Parallel()
	canonicalDir := filepath.Join("..", "..", "..", "web", "static")
	mirrorDir := filepath.Join("..", "static")
	if _, err := os.Stat(canonicalDir); err != nil {
		t.Skipf("canonical static dir not available: %v", err)
	}
	err := filepath.Walk(canonicalDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(canonicalDir, path)
		if relErr != nil {
			return relErr
		}
		canonical, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		mirrored, readErr := os.ReadFile(filepath.Join(mirrorDir, rel))
		if readErr != nil {
			t.Errorf("mirror is missing %s — run `make sync-embed`", rel)
			return nil
		}
		if !bytes.Equal(canonical, mirrored) {
			t.Errorf("internal/web/static/%s differs from web/static/%s; web/ is the source of truth, run `make sync-embed`", rel, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", canonicalDir, err)
	}
	// And the reverse: nothing lingering in the mirror that web/ has dropped.
	_ = filepath.Walk(mirrorDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(mirrorDir, path)
		if relErr != nil {
			return relErr
		}
		if _, statErr := os.Stat(filepath.Join(canonicalDir, rel)); statErr != nil {
			t.Errorf("internal/web/static/%s has no counterpart in web/static — run `make sync-embed` to prune it", rel)
		}
		return nil
	})
}

// tokensInBlock returns the custom properties declared in the block that
// starts at the given selector text.
func tokensInBlock(t *testing.T, css, selector string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	idx := strings.Index(css, selector)
	if idx < 0 {
		return out
	}
	rest := css[idx+len(selector):]
	end := strings.Index(rest, "}")
	if end < 0 {
		t.Fatalf("unterminated block for selector %q", selector)
	}
	for _, m := range varDefRe.FindAllStringSubmatch(rest[:end], -1) {
		out[m[1]] = true
	}
	return out
}

func expandHex(h string) (r, g, b int, ok bool) {
	parse := func(s string) int { v, _ := strconv.ParseInt(s, 16, 32); return int(v) }
	switch len(h) {
	case 3, 4: // #rgb / #rgba
		return parse(strings.Repeat(string(h[0]), 2)),
			parse(strings.Repeat(string(h[1]), 2)),
			parse(strings.Repeat(string(h[2]), 2)), true
	case 6, 8: // #rrggbb / #rrggbbaa
		return parse(h[0:2]), parse(h[2:4]), parse(h[4:6]), true
	}
	return 0, 0, 0, false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
