# basic-kanban-board-mcp — Super Plan v1

> Status: DRAFT for external review (Codex, Gemini, GLM). Author: Claude (Fable 5.1) with the
> project owner. Date: 2026-09-06. Language of all code, docs, UI, commits: English.

---

## 0. The one-paragraph pitch

**A minimal, self-hosted kanban board built for AI coding agents.** One static Go binary, one
SQLite file, one port. A native MCP server over remote streamable-HTTP with mandatory bearer
auth, so every agent on every machine (Claude Code, Codex, Cursor, Kilo, …) writes to **one**
shared board, and a human watches the cards move live in a browser. **Nine MCP tools instead of
forty to a hundred and seventy** — every write is a batch, every read is compact, and the board
answers the question agents actually ask: *"what should I work on next, and is it safe to take
it?"* Everything else is a plugin.

Tagline candidates (pick one at launch):
- *The kanban board your AI agents were missing.*
- *One board. Every agent. Nine tools.*
- *Self-hosted kanban with a native MCP server. Single binary. No bloat.*

---

## 1. Goals, in priority order

1. **Adoption.** Be *the* default answer when someone asks "self-hosted kanban board with MCP for
   my coding agents". Thousands of users. Every design decision is judged first by "does this
   make it easier to install, understand, recommend?"
2. **Do what does not exist.** Research of ~20 projects (see §13) found nobody ships the
   combination: shared remote MCP endpoint + mandatory auth + batch writes + compact board read +
   dependency-aware "next task" + atomic claim + optimistic concurrency. We ship exactly that.
3. **Be the most minimal version.** Small core, small tool surface, small binary, zero external
   services, zero JS build. Anything that is not core goes to plugins or is a non-goal.
4. **Most popular technology.** Go — the language of the self-hosted niche (Gitea, Caddy,
   Syncthing, Miniflux, Vikunja, Scrumboy, beads). Single static binary, `FROM scratch` image,
   readable by any contributor.
5. **Dogfooding.** The owner uses it daily across three machines with several agent CLIs. It must
   be strictly better than the hand-made HTML status page it replaces (§10.3).

### Non-goals for v1 (explicitly out; candidates for plugins)

Users/teams/roles beyond token scopes · time tracking · sprints/iterations · calendar/Gantt ·
file attachments · mobile apps · OAuth/SSO login · email · multi-tenant SaaS · AI features inside
the board (the agents *are* the AI) · Postgres/MySQL backends (SQLite only in v1).

---

## 2. Name, license, identity

- **Repository / Docker image:** `basic-kanban-board-mcp` (verified free on GitHub and npm,
  2026-09-06). Full name carries the search intent and reads as a standard, not a toy.
- **Binary / CLI command:** `kanban`. Go module: `github.com/ultrathinker/basic-kanban-board-mcp`.
- **Image:** `ghcr.io/ultrathinker/basic-kanban-board-mcp` (+ Docker Hub mirror later).
- **License: MIT.** Adoption is goal #1; AGPL is blocked by corporate legal in exactly the
  scenario we target (a developer installs it at work to manage their agents). All the serious
  native-MCP competitors chose AGPL — MIT is a differentiator here, not a default.
- **Hard rule for any future paid layer:** fail loud (HTTP 402 with a message), never a silent
  404. (Vikunja gates features behind a license and returns 404 — that cost us a day.)
- Repo description (SEO, the name does not have to carry it): *"Minimal self-hosted kanban board
  with a native MCP server for AI coding agents. Single Go binary, SQLite, 9 tools, remote
  streamable-HTTP, bearer auth. MIT."*

---

## 3. Users and core scenarios

| Persona | Scenario | What must be true |
|---|---|---|
| **Solo dev, several machines, several agents** (the owner) | Claude Code on the laptop creates 12 tasks; Codex on the desktop picks the next unblocked one; the human watches the board on a phone browser | One remote endpoint, per-agent tokens, `task_next` + atomic claim, live UI |
| **Agent** (the real primary user of the API) | Session starts → one cheap call to see the board → one call to get a safe next task → work → update status + note → done | `board_get` ≤ ~600 tokens, `task_next(claim:true)`, batch `task_update`, human-readable keys |
| **Homelab / self-hoster** | `docker run` one-liner, reverse proxy in front, backup = copy one file | Single binary, single port, `FROM scratch`, `--trusted-proxies`, SQLite `VACUUM INTO` backup |
| **Team lead evaluating** | Reads README in 60 seconds, sees the 9-tools comparison table, runs `--demo` | README quality, demo seed, GIF |

---

## 4. Architecture

```
                 ┌──────────────────────────────────────────────┐
  agents (MCP) ─▶│ /mcp        streamable-HTTP  (go-sdk)         │
  agents (RO)  ─▶│ /mcp/readonly  same, read tools only          │
  scripts/CI   ─▶│ /api/v1     REST (same service layer)         │──▶ service layer ──▶ SQLite (WAL)
  browser      ─▶│ /           html/template + htmx              │        │
  browser      ─▶│ /events     SSE (live board)                   │◀───────┘ events bus
  ops          ─▶│ /healthz /readyz /metrics(optional)           │        │
                 └──────────────────────────────────────────────┘        ▼
                                                                   webhooks (HMAC)
```

**One process, one port, one file.**

- **Language/runtime:** Go 1.25 (latest stable at implementation time), `CGO_ENABLED=0`.
- **HTTP:** standard library `net/http` with Go 1.22+ pattern routing. No web framework.
- **SQLite:** `modernc.org/sqlite` (pure Go, no CGO → trivial cross-compilation, `FROM scratch`).
  WAL mode, `busy_timeout`, single writer via a serialized write queue in the service layer.
- **MCP:** official `github.com/modelcontextprotocol/go-sdk`, streamable-HTTP transport,
  stateless-friendly (session id optional). stdio transport also compiled in (`kanban mcp
  --stdio`) for clients that cannot do HTTP yet — it just proxies to the local service layer.
- **Web UI:** server-rendered `html/template` + **htmx** (+ `sse` extension) + **SortableJS**
  for drag-and-drop. All assets vendored and embedded via `embed.FS`. **No npm, no bundler, no
  JS build step in the release pipeline.** Zero CDN calls (privacy, offline homelabs).
- **Events bus:** in-process fan-out of domain events → SSE clients, webhooks dispatcher, activity
  log. Append-only `events` table is the source for the UI activity feed and for audit.
- **Config:** flags + `KANBAN_*` env vars. Zero-config default works: `kanban serve` →
  `http://127.0.0.1:8080`, data in `./data/kanban.db`.
- **Migrations:** embedded SQL files, forward-only, applied at startup, tracked by (version, name).

### Repository layout

```
cmd/kanban/                 main: serve | token | export | import | backup | mcp --stdio | demo
internal/domain/            entities, validation, transitions, task_next algorithm (pure Go, no I/O)
internal/store/             sqlite repo, migrations (embed), tx helpers
internal/service/           use-cases; the ONLY layer MCP/REST/UI call
internal/mcp/               tool registration, schemas, compact formatter, token-budget tests
internal/api/               REST handlers (thin adapters over service)
internal/web/               html handlers, templates, static (embed), SSE
internal/events/            bus, webhooks dispatcher (HMAC, retry w/ backoff)
internal/auth/              tokens (hash+scopes), sessions, middleware, trusted proxies
internal/config/            flags/env, validation (e.g. refuse --auth off on non-loopback)
web/templates/  web/static/ htmx.min.js, sse.js, sortable.min.js, app.css, app.js (tiny)
docs/                       PLAN.md, ARCHITECTURE.md, MCP-TOOLS.md, AGENT-SETUP.md, reviews/
.github/workflows/          ci.yml (build/test/lint/vuln), release.yml (goreleaser + ghcr)
```

---

## 5. Data model

All timestamps UTC RFC3339. Internal ids are UUIDv7 (time-sortable); **every task also has a
human key `PROJ-N`** and every tool accepts either form.

### Project
| field | type | notes |
|---|---|---|
| id | uuid | |
| key | string | 2–8 uppercase `[A-Z][A-Z0-9]*`, unique, immutable (task keys derive from it) |
| name, description | string | |
| next_task_seq | int | monotonically increasing counter for `PROJ-N` |
| focus_task_id | uuid? | the "now" banner — one per project |
| estimate_unit | string | default `"h"`; free label (`"pt"`, `"d"`) |
| enforce_dependencies | bool | default true: cannot enter an *active* column while blockers are open |
| strict_done | bool | default false: entering a *done* column requires all acceptance criteria checked |
| definition_of_done | []string | reusable checklist template, copied into new tasks' acceptance when `apply_dod` (Backlog.md pattern) |
| claim_ttl_seconds | int | default 3600 |
| archived_at | time? | |

### Column
| field | type | notes |
|---|---|---|
| id, project_id | | |
| name | string | |
| position | int | |
| kind | enum | `backlog` \| `active` \| `done` — drives WIP checks, "done" semantics, hide-done UI |
| wip_limit | int? | only meaningful for `active`; enforced on move unless `force` |

Default columns on `project_upsert` without explicit columns: `Backlog(backlog)`,
`Doing(active, wip 3)`, `Review(active)`, `Done(done)`.

### Task
| field | type | notes |
|---|---|---|
| id | uuid | |
| key | string | `PROJ-N`, immutable |
| project_id, column_id | | |
| parent_id | uuid? | subtask; max depth 2 (task → subtask) in v1 |
| position | float | order within column (fractional insert, periodic renumber) |
| title | string | ≤ 200 chars |
| body | string | markdown, ≤ 64 KB |
| type | enum | `task` \| `bug` \| `feature` \| `chore` \| `doc` \| `perf` \| `research` (project may extend list later) |
| priority | int 0–4 | 0 none · 1 low · 2 medium · 3 high · 4 critical |
| estimate | number? | in project's `estimate_unit` |
| tags | []string | free-form, lowercase, ≤ 20 |
| assignee | string? | free text: human or agent name (no user model) |
| claimed_by, claimed_at, claim_expires_at | | lease; see §7 |
| acceptance | []{text, done} | per-task acceptance criteria |
| due_at, start_at | time? | |
| started_at, done_at | time? | set automatically on first entry into `active` / `done` column |
| version | int | optimistic concurrency, +1 on every write |
| metadata | JSON object | schema-free extension valve (plugins, importers); ≤ 16 KB |
| created_at, updated_at, created_by, updated_by | | actor = token name |
| archived_at | time? | soft delete; `task_remove(mode:"delete")` hard-deletes |

Derived (never stored): `blocked_by` (open tasks that `blocks` this one), `subtasks_done/total`,
`is_ready`.

### Link (typed dependency)
`from_id`, `to_id`, `type ∈ {blocks, relates, duplicates}`, `created_at`, `created_by`.
`blocks` is directional (A blocks B ⇒ B is blocked until A is done). Parent/child is **not** a
link (it is `parent_id`). Cycles in `blocks` are rejected.

### Note
`id`, `task_id`, `author` (token name), `body` (markdown ≤ 16 KB), `created_at`. The agent's
working log ("tried X, failed because Y"). Immutable in v1.

### Event (append-only)
`id`, `ts`, `actor`, `type`, `project_id`, `task_id?`, `payload JSON`. Types:
`project.created|updated`, `task.created|updated|moved|claimed|released|removed|restored`,
`link.added|removed`, `note.added`. Feeds SSE, webhooks, activity view. Retention: keep all;
`kanban compact-events --older-than 90d` optional.

### Token
`id`, `name` (unique, e.g. `claude@rog`, `codex@desktop`, `alex`), `hash` (SHA-256 of a
32-byte random secret; secret shown once), `scopes ⊆ {read, write, admin}`,
`project_keys []string` (empty = all), `created_at`, `last_used_at`, `revoked_at`.
The token **name is the actor identity** everywhere (events, claims, notes, `created_by`).

### Webhook
`id`, `project_id?` (null = all), `url`, `secret` (HMAC-SHA256 in `X-Kanban-Signature`),
`event_types []string`, `active`, `failures`, `last_status`.

---

## 6. MCP tool surface — nine tools

Principles: **batch by default** (every write takes an array), **compact by default** (reads
return the minimum an agent needs; `fields` widens), **strict schemas**
(`additionalProperties:false` everywhere — catches agent typos loudly), **keys not UUIDs** in
every response, **one envelope**.

Every response is text content with this envelope (JSON unless `format:"compact"`):
```json
{ "ok": true, "op": "task_update", "data": …, "meta": { "count": 3, "warnings": [] } }
{ "ok": false, "op": "task_update", "error": { "code": "conflict", "message": "…", "current": { …task… } } }
```
Error codes: `not_found`, `validation`, `conflict`, `blocked`, `wip_exceeded`, `claimed`,
`forbidden`, `cycle`.

### 6.1 `board_get` — the cheap session-start read
```
params: project?: key | null (null = all projects, summary per project)
        include_done?: false | true | N (last N done)
        filter?: { column?, type?, priority_min?, tags?, assignee?, claimed?, blocked?, q?, updated_since? }
        fields?: ["body","acceptance","notes","links","metadata"]  (default: none of these)
        format?: "compact" (default) | "json"
```
`compact` output (target **≤ 600 tokens for ~30 active tasks**; golden test enforces
`len/4 ≤ 600`):
```
# BMB BeeMemoryBank · focus BMB-14 · doing 2/3 · review 1 · backlog 12 · done 40 (hidden)
## Doing
- BMB-14 P3 bug  Fix WAL checkpoint race  est 2h  @claude@rog (claimed 12m)  sub 1/3  #sync
- BMB-17 P2 feat Encrypted FTS index      est 8h  @codex@desk               blocked-by BMB-14
## Review
- BMB-12 P2 task Squash migrations 41-44   est 1h  @alex
## Backlog
- BMB-18 P3 bug  …
```
One line per task, fixed column order: `key priority type title est assignee(claim) sub blocked tags`.
Missing fields are omitted, never padded.

### 6.2 `task_next` — what should I work on now (and take it)
```
params: project?: key
        actor?: string (defaults to token name)
        limit?: 1..10 (default 3)
        claim?: false | true   → atomically claims data[0] for the caller
        include?: ["body","acceptance","notes","links"] (default: body + acceptance)
```
Algorithm (`internal/domain/next.go`, pure function, unit-tested):
1. Candidates: tasks in `backlog` columns of the project(s) the token may access, not archived,
   `parent` not blocked, not claimed by someone else with a live lease.
2. Exclude tasks with any open `blocks` predecessor (`enforce_dependencies`).
3. Exclude if the first `active` column is at its WIP limit (return `meta.wip_full = true`).
4. Order: priority desc → due_at asc (nulls last) → position → created_at.
5. Return `limit` tasks with body+acceptance (this is the one place verbose is right).
6. `claim:true`: claim `data[0]` in the same transaction; if a race loses, retry with the next
   candidate (up to `limit`). `meta.claimed = "BMB-18"`.
Also returns `meta.blocked_summary`: `{count, top:[{key, blocked_by:[…]}]}` so the agent can see
*why* the rest is not ready.

### 6.3 `task_get`
`params: keys: [string] (1..50), include?: [...]` → full tasks incl. notes, links, subtasks.

### 6.4 `task_create`
```
params: tasks: [ { project: key, title, body?, type?, priority?, estimate?, tags?, assignee?,
                   parent?: key, column?: name (default first backlog), acceptance?: [string],
                   apply_dod?: bool, blocks?: [key], blocked_by?: [key], due_at?, metadata?,
                   claim?: bool } ]  (1..100)
```
Returns created tasks (compact) with keys. Atomic: all or nothing. Temporary references inside
the batch: `"$0"`, `"$1"` for `parent`/`blocks` to build a tree in one call.

### 6.5 `task_update`
```
params: patches: [ { key, if_version?, title?, body?, type?, priority?, estimate?, tags?
                     (replace) | tags_add? | tags_remove?, assignee?, column?: name,
                     position?: "top"|"bottom"|{after:key}, parent?, acceptance?: [{text,done}]
                     | acceptance_check?: [index], note?: string, focus?: bool,
                     due_at?, start_at?, metadata_merge?, force?: bool, reason?: string } ] (1..100)
```
- `column` triggers transition validation: dependencies (`blocked`), WIP (`wip_exceeded`),
  `strict_done` (`validation`). `force:true` bypasses with mandatory `reason`, logged in event.
- `if_version` mismatch → `conflict` with `current` task in the error so the agent can merge
  without a second read (nobody else does this).
- `note` appends a Note by the caller. `focus:true` sets project focus (clears previous).
- Per-item results: batch is **not** atomic by default (`atomic:true` param available); each
  item reports `ok` or `error`.

### 6.6 `task_link`
`params: add?: [{from, to, type}], remove?: [{from, to, type}]` → cycles rejected, returns
resulting `blocked_by` for touched tasks.

### 6.7 `task_claim`
`params: key, action: "claim" | "release" | "renew", actor?, ttl_seconds?, force?` → atomic
(`UPDATE … WHERE claimed_by IS NULL OR claim_expires_at < now`). `force:true` (admin scope or
same actor) steals with an event.

### 6.8 `task_remove`
`params: keys: [string], mode: "archive" (default) | "delete", cascade_subtasks?: true`.

### 6.9 `project_upsert`
`params: key, name?, description?, columns?: [{name, kind, wip_limit?, position?}], settings?:
{estimate_unit, enforce_dependencies, strict_done, definition_of_done, claim_ttl_seconds}`.
Renaming/removing a column that has tasks requires `move_tasks_to: name`.

### Read-only endpoint
`/mcp/readonly` serves only `board_get`, `task_get`, `task_next(claim=false forced)` regardless
of token scope. Cheap blast-radius reduction (Linear does this).

### What is deliberately *not* a tool
Search (→ `board_get.filter.q`), comments list (→ `task_get`), stats (→ `board_get` header),
sprints, time logging, users, attachments, "plan/analyze/reflect/verify" chains.

---

## 7. Concurrency model (the part nobody else got right)

- **Optimistic concurrency:** `version` on every task; `if_version` on `task_update`; conflict →
  `409`-style error carrying the current state.
- **Claims are leases:** `claimed_by + claim_expires_at`; default TTL from project (1 h). Any
  `task_update` by the claimer renews. Expired leases are ignored by `task_next` and can be
  taken by anyone. UI shows "claimed by codex@desk, 43 min left".
- **Single writer:** all mutations go through one serialized queue → SQLite never hits
  `SQLITE_BUSY` under agent bursts; reads are concurrent (WAL).
- **Idempotency:** `task_create` accepts an optional `idempotency_key` per item (24 h window) so
  an agent retrying after a network error does not duplicate tasks.
- **Dependency-gated transitions** (agent-board pattern) enforced at write time, not by the
  client.

---

## 8. Auth & security

- **Bearer tokens, mandatory by default.** `KANBAN_AUTH=off` is accepted **only** when the
  listen address is loopback; otherwise the server refuses to start with a clear message. We never
  block remote access — we require a token for it. (The inverse of Veritas Kanban's mistake.)
- **Bootstrap:** first `kanban serve` with an empty token table creates an `admin` token and
  prints it **once** to stdout/log (also honors `KANBAN_ADMIN_TOKEN` to set it explicitly). The
  first-run web page tells you where to find it; it is never displayed unauthenticated.
- **Scopes:** `read` · `write` (implies read) · `admin` (tokens, webhooks, project deletion,
  force). Optional per-project restriction.
- **Web login:** paste a token → server-side session (table `sessions`, HttpOnly, SameSite=Lax,
  Secure when behind TLS or `--base-url https://…`). CSRF token on all UI forms. Logout revokes
  the session, not the token.
- **Proxy handling:** `X-Forwarded-*` honored **only** from `KANBAN_TRUSTED_PROXIES` CIDRs, and
  only for logging/rate-limit keys and URL generation — **never for auth decisions**.
- Rate limit per token (default 600 req/min) and per IP on `/login`.
- Tokens stored hashed; shown once; `kanban token rotate`.
- Strict JSON schemas, body size limits (1 MB), markdown rendered with a sanitizer (bluemonday)
  in the UI, security headers, no inline scripts (CSP), CORS disabled by default (opt-in list).
- Dependencies kept minimal and audited in CI (`govulncheck`).

---

## 9. REST API, events, webhooks, plugins

- **REST `/api/v1`** mirrors the nine tools 1:1 (same request/response JSON) — for scripts, CI,
  and future UI features. Documented via a static OpenAPI file generated from the same schemas.
- **SSE `/events?project=KEY`** streams the event feed (auth via session or token). The UI uses
  it for live updates; agents may too.
- **Webhooks:** per project or global, HMAC-signed, event-type filter, 5 retries with backoff,
  auto-disable after 20 consecutive failures (visible in UI).
- **Plugins (v1.1, contract fixed now):** *a plugin is an external MCP server.* Config lists
  upstream streamable-HTTP MCP servers; their tools are exposed through our `/mcp` under
  `<ns>__<tool>` with our auth in front. Core stays small; time tracking, sprints, AI summaries,
  GitHub sync, etc. live outside. Also supported from v1: outbound webhooks + REST as the
  integration surface for anything.

---

## 10. Web UI

### 10.1 Pages
- **Board** (`/p/KEY`): columns with WIP indicators (`2/3`), cards: key · title · priority chip
  · type badge · estimate · tags · assignee/claim badge with remaining lease · blocked indicator ·
  subtask progress. Drag between columns (transition errors shown as toasts). "Hide done"
  toggle (default on). **Focus banner** at the top ("Now: BMB-14 …"). Project switcher.
- **Task drawer** (`/t/BMB-14`): body (rendered markdown), acceptance checklist (clickable),
  links (blocks/blocked-by/relates), subtasks, notes timeline, history (events), edit form.
- **Activity** (`/p/KEY/activity`): live event feed — *watch the agents work*.
- **Overview** (`/`): all projects, counts, focus per project.
- **Admin** (`/admin`): tokens (create/revoke/rotate), webhooks, project settings, export/backup.
- **Login** (`/login`), first-run page, `/agent-setup` page with copy-paste snippets for Claude
  Code / Codex / Cursor / generic MCP config pointing at *this* instance's URL.

### 10.2 Live updates
htmx `sse` extension subscribed to `/events`; server pushes targeted fragments (`task-card
BMB-14`) — real push, not polling. Reconnect with `Last-Event-ID` replay from the events table.

### 10.3 Visual language (ported from the owner's HTML status page)
CSS custom properties, light/dark via `prefers-color-scheme` + manual toggle; priority tiers as
colored left stripes; type badges (`bug`/`perf`/`feat`/`task`/`doc`); estimate pill; collapsible
done section; focus banner; `tabular-nums` for numbers. Keep it quiet and dense — this is a
dashboard people glance at, not a landing page.

### 10.4 Accessibility & perf
Keyboard-operable drag alternative (move menu), focus states, `prefers-reduced-motion`,
first paint < 100 ms locally, no external requests.

---

## 11. CLI & operations

```
kanban serve [--addr :8080] [--data ./data] [--base-url https://kanban.example.com]
             [--auth required|off] [--trusted-proxies 10.0.0.0/8,172.16.0.0/12] [--demo]
kanban token create --name claude@rog --scope write [--project BMB] [--ttl 0]
kanban token list | revoke <name> | rotate <name>
kanban export [--project BMB] > board.json      # full JSON (tasks, links, notes, events optional)
kanban import board.json                          # idempotent by key
kanban backup ./backups/                          # sqlite VACUUM INTO, timestamped
kanban migrate [--dry-run]
kanban mcp --stdio [--url http://127.0.0.1:8080 --token …]   # stdio shim for old clients
kanban version
```
Env: `KANBAN_ADDR, KANBAN_DATA, KANBAN_BASE_URL, KANBAN_AUTH, KANBAN_ADMIN_TOKEN,
KANBAN_TRUSTED_PROXIES, KANBAN_LOG_LEVEL, KANBAN_LOG_FORMAT (text|json), KANBAN_CLAIM_TTL`.

**Docker:** `FROM scratch`, non-root, `/data` volume, healthcheck on `/healthz`,
`docker run -d --name kanban -p 8080:8080 -v kanban-data:/data ghcr.io/ultrathinker/basic-kanban-board-mcp`.
**Releases:** GoReleaser → linux/darwin/windows × amd64/arm64, checksums, SBOM, ghcr multi-arch,
Homebrew tap (v1.1). **Reverse proxy docs:** Caddy (2 lines), nginx, Apache, Traefik — all with
the trusted-proxies note.

---

## 12. Quality, targets, CI

**Targets (measured, in CI where possible):** binary < 20 MB · image < 20 MB · idle RSS < 40 MB ·
cold start < 100 ms · `board_get` p95 < 15 ms with 1 000 tasks · **compact `board_get` ≤ 600
tokens for 30 active tasks** (golden test, `len(text)/4`) · `task_next(claim)` is one round-trip.

**Tests:** domain (transitions, next-task, cycles, leases) table-driven · store (migrations
up from every prior version, concurrency: 50 goroutines claiming one task → exactly one wins) ·
MCP integration via go-sdk client against in-process server, every tool, every error code ·
UI smoke (httptest, templates render, htmx fragments) · fuzz JSON schema inputs.

**CI:** `go vet`, `staticcheck`, `govulncheck`, `gofmt`, race tests, `-cover` gate 80% on
`internal/domain` + `internal/service`. Release on tag `v*`. Conventional commits, semver,
CHANGELOG generated.

**Docs shipped at launch:** README (60-second path, GIF, comparison table), `docs/MCP-TOOLS.md`
(every tool, every param, examples), `docs/AGENT-SETUP.md` (per-client one-liners + a
recommended `CLAUDE.md`/`AGENTS.md` snippet: *"At session start call `board_get`; take work
only via `task_next(claim:true)`; log progress with `task_update.note`"*), `docs/DEPLOY.md`,
`docs/SECURITY.md`, `docs/PLUGINS.md` (contract), `llms.txt` at repo root so assistants can
summarize the project correctly.

---

## 13. Competitive landscape (why this shape) — condensed from three research passes

| Project | Stars | Tools | Transport / auth | Fatal gap for our use case |
|---|---|---|---|---|
| Veritas Kanban | 0.8k | 42 | stdio only | password sessions hard-locked to loopback |
| Vibe Kanban | 28k | 34 | stdio only | company shut down 2026-04, no shared board |
| Scrumboy | 0.4k | 50 | HTTP + OAuth2.1 | no batching, 50 tools, tiny community |
| Kan | 5.6k | 46 | stdio wrapper | Postgres required, no priority/estimate/deps, 2 s polling "realtime" |
| kandev | 0.8k | 55–65 | HTTP, auth optional | UUID-only, "just VPN it" |
| Leantime | 11.5k | 55 | HTTP | MCP route only with a paid license (404 otherwise) |
| Plane | 59k | 30 | HTTP (official) | 13 containers, AGPL |
| Vikunja | 5.3k | 3rd-party | stdio | no optimistic concurrency, silent 404 license gate |
| Kanboard | 9.9k | 3rd-party (26–171) | mostly stdio | raw passthrough bridges, polling UI |
| Planka | 12.5k | 3rd-party 27 (104 ops) | stdio/SSE | fair-code license, no priority |
| claude-task-master | 28k | 36 | stdio | files in repo, no UI, no multi-agent safety |
| Backlog.md | 6k | ~20 | stdio | files in repo, merge conflicts under concurrent agents |
| beads | ~20k | 15 | undocumented | Dolt dependency, no UI; **but** `bd ready` + atomic claim are the best ideas in the field |

**Unclaimed territory we occupy:** remote HTTP + mandatory auth + batch writes + compact read
+ `next` with atomic claim + optimistic concurrency + single binary + MIT. No single project has
more than two of these.

**Stolen with attribution:** human keys (Linear/Scrumboy/Vibe), typed links (Vibe/beads),
`ready`/claim (beads), acceptance vs Definition-of-Done (Backlog.md), dependency-gated moves
(agent-board), WIP limits (Vikunja/kandev), `fields` param & toolset thinking (GitHub MCP),
read-only endpoint (Linear), `action`-consolidation proof (planka-mcp), batch envelope
(vikunja-mcp), metadata valve (Kanboard).

---

## 14. Roadmap

| Milestone | Scope | Exit criterion |
|---|---|---|
| **M0 Skeleton** | repo, CI, `kanban serve` with `/healthz`, SQLite + migrations, config, token bootstrap | `docker run` prints admin token, health green |
| **M1 Core + MCP** | domain model, store, service, all 9 tools, compact formatter, concurrency tests, `/mcp` + `/mcp/readonly` + stdio shim | Claude Code creates/moves/claims tasks end-to-end; token-budget test passes |
| **M2 Web UI** | board, drawer, activity, overview, login, admin, SSE live updates, drag-and-drop, theme | owner replaces the HTML page in daily use |
| **M3 Integrations** | REST `/api/v1`, webhooks, export/import/backup, `--demo`, agent-setup page | three machines, three agents, one board — for a week |
| **M4 Launch** | README + GIF, docs set, GoReleaser, ghcr, MCP registry listings, Show HN / r/selfhosted / r/ClaudeAI posts | v1.0.0 tag |
| **v1.1** | MCP proxy plugins, Homebrew tap, Postgres? (only if demanded), i18n of UI | |

Estimated effort: M0–M1 ≈ 1 week, M2 ≈ 1 week, M3–M4 ≈ 1 week of focused agent-driven work.

---

## 15. Open questions for reviewers

1. **Default `board_get` format:** compact text (fewest tokens, human-readable in logs) vs JSON
   (unambiguous). Current choice: compact default, JSON opt-in. Agree?
2. **Nine tools:** should `task_link` and `task_claim` fold into `task_update` (→ 7 tools) or
   stay separate for discoverability (an agent scanning the list should *see* "claim")?
3. **`task_update` non-atomic by default** with per-item results vs all-or-nothing. Which is
   safer for agents in practice?
4. **Subtask depth:** hard cap at 2 levels, or allow arbitrary depth?
5. **Priority 0–4 int** vs named enum in the API. Ints sort trivially; names read better.
6. **Focus per project** (what the human watches) — enough, or also per actor?
7. **Claim TTL default 1 h** — too long/short for typical agent sessions?
8. **stdio shim in v1** — worth the surface, or ship HTTP-only and point people to `mcp-remote`?
9. **MIT vs AGPL** — we chose MIT for adoption; any argument strong enough to flip it?
10. **What is missing** that would make an agent's life materially better and is still "basic"?
11. **What should be cut** to make v1 smaller without losing the differentiators?
12. **Name:** `basic-kanban-board-mcp` — does "basic" cap perception, or does it read as
    "the standard baseline"? Tagline preference?

---

## 16. Decision log

- 2026-09-06 · Go over .NET/Rust — adoption is the primary goal; the niche speaks Go.
- 2026-09-06 · MIT over AGPL — corporate adoption friction outweighs SaaS-protection.
- 2026-09-06 · Server over files-in-repo — three machines, live view, concurrent agents.
- 2026-09-06 · 9 tools (was 6) — dependencies, `next`, claim are core, not plugins.
- 2026-09-06 · No JS build — htmx + server templates; vendored assets; zero CDN.
- 2026-09-06 · SQLite only in v1 — single file is the deployment story.
