# Vendored third-party assets

Everything in `web/static/vendor/` is redistributed under its original licence.
This directory is the source of truth; `internal/web/static/vendor/` is a
generated mirror kept in step by `make sync-embed` and checked by
`make check-embed`, because Go's `//go:embed` cannot traverse `..`.

Every file here is loaded by the application. Nothing is vendored "just in
case" — an embedded file that no page loads is dead weight inside a binary
whose whole pitch is that it is small.

## SortableJS v1.15.7 — `sortable.min.js`

- Licence: MIT
- Copyright (c) 2019 All contributors to SortableJS
- Source: <https://github.com/SortableJS/Sortable>
- Loaded by: `web/templates/layout.html`
- Used for: drag-and-drop reordering of cards between board columns. The
  keyboard move menu in `app.js` does the same job without it, so the board
  stays fully operable if this fails to load.

## Removed 2026-09-06 — htmx, its SSE extension, and Alpine.js

They were vendored during the first build and are gone now. Recorded here
because "why is this not here?" is a fair question and the answer took a
while to establish:

- **htmx v4.0.0** — v4 is a rewrite with no extension mechanism: no
  `htmx.defineExtension`, no `hx-ext`. Once the inert `hx-ext="sse"` markup
  was removed it had no consumer at all. If you want htmx back, pin **2.x** —
  that is the line the SSE extension was written against.
- **htmx SSE extension v2.2.2** — written against htmx 2's extension API. It
  called `htmx.defineExtension` and threw a TypeError on every page load
  under htmx 4, so live updates never worked. Replaced by a native
  `EventSource` in `app.js`, which treats an event as a change signal and
  re-fetches the page rather than swapping server-sent fragments.
- **Alpine.js v3.17.1** — cannot run here at all. Its standard build evaluates
  expressions with `new Function()`, which the `script-src 'self'` policy in
  `internal/web/web.go` refuses, and that CSP is mandated by PLAN §8. Its one
  use, a copy-to-clipboard button, is plain JavaScript now.

Together they were ~95 KB of JavaScript embedded in the binary that no page
executed. See PLAN §18 deviations 7 and 11.
