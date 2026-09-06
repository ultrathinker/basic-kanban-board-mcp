// Package templates loads, parses and serves the html/template files used
// by the web UI. The package is small on purpose — every page template is
// a thin wrapper around the shared layout, and the only meaningful logic
// lives in the Funcmap (see internal/web/view.Funcs).
//
// The template sources live in this directory (next to the Go file) so
// //go:embed can compile them into the binary. The same files are mirrored
// at web/templates/ at the repository root for editor discoverability and
// for the uipreview command, which prefers on-disk files so a designer can
// edit a template and refresh the browser without rebuilding the Go binary.
// Production builds always use the embedded copies — `go build` is enough.
package templates

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

//go:embed *.html
var embeddedFS embed.FS

// MirroredDir is the on-disk location the uipreview command looks at first.
// It exists so designers can edit templates at the repository root without
// going into the internal Go tree.
const MirroredDir = "web/templates"

// Engine bundles the parsed templates and the helper funcmap.
type Engine struct {
	tpl *template.Template
}

// New constructs an Engine from the embedded FS. Safe to call concurrently.
func New() (*Engine, error) {
	return newFromFS(embeddedFS)
}

// NewFromDir constructs an Engine from templates living at dir. Used by the
// uipreview command when iterating on the UI without rebuilding the binary.
func NewFromDir(dir string) (*Engine, error) {
	return newFromFS(os.DirFS(dir))
}

func newFromFS(fsys fs.FS) (*Engine, error) {
	t := template.New("").Funcs(view.Funcs())
	matches, err := fs.Glob(fsys, "*.html")
	if err != nil {
		return nil, fmt.Errorf("templates: glob: %w", err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("templates: no .html files found")
	}
	for _, name := range matches {
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("templates: read %s: %w", name, err)
		}
		if _, err := t.New(name).Parse(string(b)); err != nil {
			return nil, fmt.Errorf("templates: parse %s: %w", name, err)
		}
	}
	return &Engine{tpl: t}, nil
}

// Render writes the named template (e.g. "page-board") into w with data.
func (e *Engine) Render(w io.Writer, name string, data any) error {
	return e.tpl.ExecuteTemplate(w, name, data)
}

// HasTemplate reports whether the engine has parsed the named template.
func (e *Engine) HasTemplate(name string) bool {
	return e.tpl.Lookup(name) != nil
}

// MustParseTemplates is a convenience wrapper used by tests: it loads the
// embedded templates and panics on failure. It exists so callers do not
// have to thread an error through every test.
func MustParseTemplates() *template.Template {
	e, err := New()
	if err != nil {
		panic(err)
	}
	return e.tpl
}

// Filenames returns the names of the .html files the engine would parse,
// in lexical order. Useful for tests.
func Filenames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".html") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}
