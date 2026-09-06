package web

import (
	"embed"
	"io/fs"
	"net/http"
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
		// Vendored, versioned assets never change under a live server, so a
		// long max-age is safe; we do not fingerprint filenames, so keep the
		// window short enough that a redeploy with edited CSS/JS is picked up
		// within a day rather than "long forever".
		rw.Header().Set("Cache-Control", "public, max-age=86400")
		fileServer.ServeHTTP(rw, r)
	}))
}
