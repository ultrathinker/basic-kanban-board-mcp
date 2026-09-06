# Vendored JavaScript libraries

All assets in `web/static/vendor/` are redistributed under their original licenses
to keep the board CSP-safe (no CDN, no runtime fetch) and to make a plain
`go build` sufficient — see `docs/PLAN.md` §4 and §9.

**Only `sortable.min.js` is loaded by the templates today.** The other three
are kept here — vendored, licensed and ready — but are not referenced from
`layout.html`, each for a specific reason recorded below. Restoring any of
them is one `<script>` tag. Shipping a library that cannot run is worse than
not shipping it: it costs every page load and fails silently or loudly for
no benefit.

## htmx v4.0.0 — `htmx.min.js` (NOT LOADED)

- Copyright (c) 2024–2026 Big Sky Software
- License: BSD 2-Clause License
- Source: <https://htmx.org/>
- Local file: `htmx.min.js`
- **No consumer.** htmx 4 is a rewrite: it has no `hx-get`/`hx-post`
  (they are `hx-action` + `hx-method`), no `hx-ext`, no extension API and a
  different event vocabulary (`htmx:before:viewTransition`, not
  `htmx:afterSwap`). After removing the inert `hx-ext="sse"` markup, no
  template used a single `hx-*` attribute, so this was 37 KB of no-op on
  every page. If htmx is wanted back, pin **2.x** — that is the version the
  SSE extension below and the `hx-get`/`hx-target` idiom in PLAN §9 assume.

## htmx Server-Sent Events Extension v2.2.2 — `sse.min.js` (NOT LOADED)

- Copyright (c) 2024 Big Sky Software
- License: BSD 2-Clause License
- Source: <https://htmx.org/extensions/sse/>
- Local file: `sse.min.js`
- **Not referenced by any template.** This extension is written against
  htmx 2's extension API: it calls `htmx.defineExtension` and
  `htmx.createEventSource`, and it is activated by `hx-ext="sse"`. The htmx
  vendored here is v4, which has none of those — so loading the file threw a
  `TypeError` on every page and `hx-ext="sse"` was inert. Live updates are
  handled by a native `EventSource` in `app.js` instead; see the LIVE
  UPDATES note at the top of that file. The file is kept so the decision can
  be reversed cheaply (pin htmx 2.x, restore the `<script>` tag in
  `layout.html`) rather than re-vendored from scratch.

## Alpine.js v3.17.1 — `alpine.min.js` (NOT LOADED)

- Copyright (c) 2019–2026 Caleb Porzio and contributors
- License: MIT
- Source: <https://alpinejs.dev>
- Bundles `@vue/reactivity` v3.5.41 (c) 2018-present Yuxi (Evan) You and
  contributors (MIT) — used under the hood by Alpine's `Alpine.reactive`
  helpers; that dependency is not separately vendored.
- Local file: `alpine.min.js`
- **Incompatible with our own CSP.** The standard Alpine build evaluates
  every `x-` expression with `new Function(...)`, which `script-src 'self'`
  (PLAN §8, no `'unsafe-eval'`) refuses. Every directive on the page threw.
  The one place Alpine was used — the copy button on `/agent-setup` — is
  handled by `app.js` instead. If Alpine is wanted back, vendor the
  CSP-friendly build (`@alpinejs/csp`), which trades inline expressions for
  `Alpine.data()` registrations; loosening the CSP is not the trade to make.

## SortableJS v1.15.7 — `sortable.min.js`

- Copyright (c) SortableJS contributors
- License: MIT
- Source: <https://github.com/SortableJS/Sortable>
- Local file: `sortable.min.js`

## `../app.css`

- Hand-authored in this repository and served verbatim. It contains no
  third-party CSS and is **not** Tailwind output: the templates use semantic
  class names, not utility classes, so there is nothing for Tailwind to
  scan. `make css` does not currently produce this file — see the task
  report.
