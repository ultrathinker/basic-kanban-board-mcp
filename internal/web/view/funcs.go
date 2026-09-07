package view

import (
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// Funcs returns the template helper map the templates use. It is kept here,
// next to the view-models, so every helper has its consumer right above it.
//
// The helpers are deliberately conservative: anything that could plausibly
// leak user input into the DOM goes through html/template's escaping. The
// markdown helper returns template.HTML but ONLY after running the result
// through bluemonday with the default "bluemonday UGC" policy — the same
// pipeline PLAN §4 commits to.
func Funcs() template.FuncMap {
	return template.FuncMap{
		// formatting
		"priorityClass":  priorityCSS,
		"priorityName":   priorityLabel,
		"priorityBadge":  priorityBadge,
		"wipFull":        wipFull,
		"duration":       formatDuration,
		"age":            formatAge,
		"relTime":        relTime,
		"relTimeFuture":  relTimeFuture,
		"formatEstimate": func(n float64, unit string) string { return formatEstimate(n, unit) },
		"truncate":       truncate,
		"joinTags":       joinTags,
		"commaKeys":      commaKeys,
		"upper":          strings.ToUpper,
		"dict": func(values ...any) (map[string]any, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]any, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				k, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %v is not a string", values[i])
				}
				m[k] = values[i+1]
			}
			return m, nil
		},
		"int": func(v any) int {
			switch n := v.(type) {
			case int:
				return n
			case *int:
				if n == nil {
					return 0
				}
				return *n
			default:
				return 0
			}
		},

		// navigation / URLs
		"boardURL":    boardURL,
		"taskURL":     taskURL,
		"drawerURL":   taskURL, // drawer is the task page
		"activityURL": activityURL,
		"overviewURL": func() string { return "/" },
		"adminURL":    func() string { return "/admin" },
		"agentURL":    func() string { return "/agent-setup" },
		"loginURL": func(r string) string {
			if r == "" {
				return "/login"
			}
			return "/login?next=" + url.QueryEscape(r)
		},

		// content rendering
		"markdown":        markdownSafe,
		"markdownSummary": markdownSummary,

		// collections
		"hasAny":      hasAny,
		"moveTargets": moveTargets,
		"add":         func(a, b int) int { return a + b },
		"sub":         func(a, b int) int { return a - b },
		"div": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"mul": func(a, b int) int { return a * b },
		"now": func() time.Time { return time.Now().UTC() },
	}
}

func boardURL(key string) string {
	if key == "" {
		return "/"
	}
	return "/p/" + url.PathEscape(strings.ToUpper(key))
}

func taskURL(key string) string {
	return "/t/" + url.PathEscape(strings.ToUpper(key))
}

func activityURL(key string) string {
	if key == "" {
		return "/"
	}
	return "/p/" + url.PathEscape(strings.ToUpper(key)) + "/activity"
}

// moveTargets returns the column names a card may be moved to: every column
// except the one it is already in. It takes `any` because the board template
// passes the value straight through a dict, where a missing key arrives as
// nil — a card fragment rendered without the column list must degrade to "no
// move menu", not blow up mid-render.
func moveTargets(columns any, current string) []string {
	cols, ok := columns.([]ColumnView)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if c.Name == current {
			continue
		}
		out = append(out, c.Name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// hasAny reports whether v contains any element (for empty-collection tests).
func hasAny(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case []TaskCard:
		return len(x) > 0
	case []ColumnView:
		return len(x) > 0
	case []ActivityEvent:
		return len(x) > 0
	case []DrawerNote:
		return len(x) > 0
	case []DrawerHistoryEntry:
		return len(x) > 0
	case []AcceptanceView:
		return len(x) > 0
	case []AdminToken:
		return len(x) > 0
	case []AdminProject:
		return len(x) > 0
	case []ProjectSummary:
		return len(x) > 0
	case []OverviewProject:
		return len(x) > 0
	case []TaskRef:
		return len(x) > 0
	case []AgentSnippet:
		return len(x) > 0
	case []string:
		return len(x) > 0
	}
	return false
}

// ---------------------------------------------------------------------------
// Markdown rendering. We keep one goldmark+bluemonday instance for the whole
// process so the first render isn't slow. PLAN §4 commits to this pipeline;
// the security review's "sanitized markdown" rule is satisfied here because
// bluemonday runs AFTER goldmark and the policy is strict by default.
// ---------------------------------------------------------------------------

var (
	mdRenderer = goldmark.New(
		goldmark.WithExtensions(extension.GFM, extension.Footnote),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(html.WithHardWraps(), html.WithUnsafe()),
	)
	mdSanitizer = bluemonday.UGCPolicy()
)

func markdownSafe(s string) template.HTML {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	var buf strings.Builder
	if err := mdRenderer.Convert([]byte(s), &buf); err != nil {
		// Render the input as escaped text rather than drop it; the page must
		// always degrade safely and never break the layout because a single
		// task has malformed markdown.
		return template.HTML(template.HTMLEscapeString(s))
	}
	clean := mdSanitizer.SanitizeBytes([]byte(buf.String()))
	return template.HTML(clean)
}

// markdownSummary produces a one-line, plain-text preview from the body, for
// the card. It strips markdown syntax crudely — the real rendering for the
// drawer uses markdownSafe.
func markdownSummary(s string, n int) string {
	if s == "" {
		return ""
	}
	// Drop fenced code blocks first.
	var out strings.Builder
	inCodeBlock := false
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "```") || strings.HasPrefix(l, "~~~") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}
		if l == "" {
			if out.Len() > 0 {
				out.WriteString(" ")
			}
			continue
		}
		out.WriteString(l)
		out.WriteString(" ")
	}
	collapsed := strings.Join(strings.Fields(out.String()), " ")
	return truncate(collapsed, n)
}

// fmtEstimateKey is a small helper used by the templates and exposed for
// tests via Funcs().
func fmtEstimateKey(n float64, unit string) string {
	if unit == "" {
		unit = "h"
	}
	return fmt.Sprintf("%g%s", n, unit)
}

var _ = fmtEstimateKey // keep the helper reachable from tests if needed
