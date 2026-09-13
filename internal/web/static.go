package web

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

// staticFS embeds a mirror of the repository's web/static tree. Go's
// //go:embed cannot traverse ".." out of this package's directory, so the
// mirror lives here (the same pattern internal/web/templates already uses
// for the .html files); web/static at the repository root remains the
// human-editable canonical copy the UI agent maintains. Keeping the two in
// sync is a manual step today — see the deviation note in the task report.
//
//go:embed static
var staticFS embed.FS

// staticHandler serves the embedded assets under /static/ with long cache
// headers (PLAN §9: no CDN, everything embedded) and lets http.FileServer
// derive Content-Type from the file extension.
func (w *Web) staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// staticFS is embedded at build time from a directory that exists in
		// this package; a failure here means the build itself is broken.
		panic("web: static assets not embedded: " + err.Error())
	}
	fileServer := http.FileServerFS(sub)
	return http.StripPrefix("/static/", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// Revalidate rather than cache blind. This used to be
		// "max-age=86400" on the reasoning that a redeploy would be picked up
		// "within a day rather than forever" — but the assets are NOT
		// fingerprinted, and the owner watches this board while it is being
		// worked on. A day is forever at that pace: every stylesheet change
		// reached him only if he thought to hard-reload, and several rounds of
		// "you did not do what I asked" turned out to be his browser showing
		// yesterday's CSS.
		//
		// "no-cache" does not mean "do not store" — it means "store it, but
		// ask before reusing it". With the ETag below, an unchanged file costs
		// one conditional request answered by a bodiless 304, and a changed
		// one arrives immediately without anyone having to know about
		// Ctrl+F5. The assets are embedded, so their bytes cannot change
		// under a running server and the hash is computed once.
		if tag := staticETag(sub, r.URL.Path); tag != "" {
			rw.Header().Set("ETag", tag)
		}
		rw.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(rw, r)
	}))
}

var (
	staticETagOnce sync.Once
	staticETags    map[string]string
)

// staticETag returns a strong ETag for one embedded asset: the base64 of a
// SHA-256 over its bytes, computed once for the whole tree on first use.
// An unknown path returns "" and the response simply carries no ETag —
// http.FileServerFS will answer it with a 404 anyway.
func staticETag(sub fs.FS, urlPath string) string {
	staticETagOnce.Do(func() {
		staticETags = make(map[string]string)
		_ = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, readErr := fs.ReadFile(sub, p)
			if readErr != nil {
				return nil
			}
			sum := sha256.Sum256(b)
			staticETags[p] = `"` + base64.RawURLEncoding.EncodeToString(sum[:]) + `"`
			return nil
		})
	})
	return staticETags[path.Clean(strings.TrimPrefix(urlPath, "/"))]
}
