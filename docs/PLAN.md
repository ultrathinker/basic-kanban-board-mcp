# basic-kanban-board-mcp — Super Plan v2 (implementation-ready)

> Status: **v2, ready for implementation.** v1 (2026-09-06) was reviewed independently by Codex
> (GPT-5.6, xhigh), Gemini 3.8 Flash (high) and GLM-5.3 (max); their full reviews are in
> `docs/reviews/`. v2 integrates them — §17 lists every change and how disagreements were
> resolved. Language of all code, docs, UI, commits: English.

## How to use this plan (governance)

- **This document is the spec of record**, not a contract carved in stone. It was written before
  a line of code existed; implementation *will* surface facts the plan did not know (SDK
  behaviour, SQLite quirks, client incompatibilities, wrong assumptions about agents).
- **Deviate when the facts demand it — but never silently.** Any implementation that departs
  from what a section here says must, in the same change:
  1. add a row to **§18 Deviations log** (date · section · planned · actual · the new fact that
     forced it · impact on other sections);
  2. edit the affected section so the plan again describes reality, marking the edit
     `(changed YYYY-MM-DD, see §18 #N)`.
  A deviation without a logged reason is a bug, not a decision.
- **What the plan deliberately leaves to implementation** (derivable, would only rot here):
  the nine tools' concrete JSON Schemas and the compact-grammar regex (`docs/MCP-TOOLS.md`,
  produced in M1 *before* any UI work, with golden tests), migration DDL (derived from §5),
  the exact `instructions` text, the export JSON format, the SSE fragment protocol, and the
  repository's own `AGENTS.md` for implementing agents (M0).
- **What must not change without the owner's explicit decision:** the goals order (§1),
  name/license (§2), nine tools as the public surface, MIT, "MCP + UI + CLI only" for v1.0.
- The **decision log (§16)** records *why* choices were made; the **deviations log (§18)**
  records *where reality overrode them*. Keep both current; they are what makes the plan
  trustworthy six months from now.

---

## 0. The pitch

**A minimal, self-hosted kanban board built for AI coding agents.** One static Go binary, one
SQLite file, one port. A native MCP server over remote streamable-HTTP with mandatory bearer
auth, so every agent on every machine (Claude Code, Codex, Cursor, Kilo, …) writes to **one**
shared board, and a human watches the cards move live in a browser. **Nine MCP tools** — every
task write is a batch, every read is compact, and the board answers the question agents
actually ask: *"what should I work on next, and is it safe to take it?"*

**Tagline (decided, 3/3 reviewers): "One board. Every agent. Nine tools."**
Sub-line for search: *self-hosted kanban board for coding agents.*

---

## 1. Goals, in priority order

1. **Adoption.** Be *the* default answer to "self-hosted kanban board with MCP for my coding
   agents". Every decision is judged first by: does it make the product easier to install,
   understand, recommend?
2. **Do what does not exist.** Nobody ships the combination: shared remote MCP endpoint +
   mandatory auth + batch writes + compact board read + dependency-aware `next` with atomic
   claim/start + optimistic concurrency + single binary + MIT (§13).
3. **Most minimal version.** Small core, nine tools, one binary, zero external services, zero
   JS build. **v1 public surface is MCP + the web UI + the CLI. Nothing else.**
4. **Most popular technology.** Go — the language of the self-hosted niche.
5. **Dogfooding.** The owner uses it daily across three machines with several agent CLIs.

### Non-goals for v1
Users/teams/roles beyond token scopes · time tracking · sprints · calendar · attachments ·
mobile · OAuth/SSO · email · multi-tenant SaaS · AI features inside the board · Postgres/MySQL ·
**webhooks, public REST API, plugin proxying (all moved to v1.1 — §14).**

---

## 2. Name, license, identity

- **Repository / image slug:** `basic-kanban-board-mcp` (verified free on GitHub + npm,
  2026-09-06). The owner's explicit choice: a stable, descriptive, authoritative name that wins
  the search box. Reviewers (2/3) warned "basic" can read as "toy"; we answer that with
  positioning, not renaming: README header *"basic-kanban-board-mcp — the baseline kanban board
  for coding agents"*, tagline directly under it, GIF above the fold. Revisit at v1.1 with data.
- **Command:** `kanban`. Module: `github.com/ultrathinker/basic-kanban-board-mcp`.
  Image: `ghcr.io/ultrathinker/basic-kanban-board-mcp`.
- **License: MIT** (3/3). Corporate adoption friction of AGPL outweighs SaaS-protection.
- **Hard rule:** any future paid layer fails loud (402 + message), never silent 404.
- **"Native MCP" means:** the `/mcp` endpoint ships, tested, ungated, in the default binary. Say
  exactly that in the README.
- GitHub topics: `mcp`, `mcp-server`, `kanban`, `ai-agents`, `coding-agents`, `self-hosted`,
  `claude-code`, `codex`, `go`, `sqlite`.

---

## 3. Users and core scenarios

| Persona | Scenario | Must be true |
|---|---|---|
| **Solo dev, several machines, several agents** (owner) | Claude Code creates 12 tasks; Codex on another box starts the next unblocked one; the human watches on a phone | one remote endpoint, per-agent tokens, `task_next(action:start)`, live UI |
| **Agent** (primary API user) | session start → `board_get` (~1 000 tokens for 30 tasks, ~90% less than a JSON dump) → `task_next(start)` → work → `task_update{note, if_version}` → done | compact read, one-round-trip start, batch writes, human keys, conflict carries current state |
| **Homelab self-hoster** | `docker run` one-liner, Caddy in front, backup = one file | single binary, single port, distroless image, `--trusted-proxies`, `kanban backup` |
| **Team lead evaluating** | README in 60 s, comparison table, `--demo` | GIF of the activity feed while three agents work — *that* is the money shot |

---

## 4. Architecture

```
  agents (MCP)  ─▶ /mcp            streamable-HTTP (official go-sdk), bearer token
  agents (RO)   ─▶ /mcp/readonly   read tools only; mutations rejected loudly
  browser       ─▶ /               html/template + htmx (internal fragment endpoints, not a public API)
  browser       ─▶ /events         SSE via session cookie (agents: one-time ticket)
  ops           ─▶ /healthz /readyz
                        │
                  service layer  ──▶ SQLite (WAL) ──▶ events table ──▶ SSE fan-out
```

**One process, one port, one file.**

- **Go 1.27**, `CGO_ENABLED=0`, stdlib `net/http` (1.22+ routing). No framework.
  *(changed 2026-09-06, see §18 #3)*
- **SQLite via `modernc.org/sqlite`** (pure Go). Discipline (§7): two `*sql.DB` handles on the
  same file — writer `SetMaxOpenConns(1)`, reader `SetMaxOpenConns(NumCPU*2)`; pragmas
  per-connection via DSN (`_pragma=journal_mode(WAL)`, `busy_timeout(5000)`,
  `foreign_keys(1)`, `synchronous(NORMAL)`); `BEGIN IMMEDIATE` for writes; periodic
  `wal_checkpoint(TRUNCATE)`; migrations under an exclusive startup lock; **one writer process
  per file** (documented; the stdio bridge is an HTTP client whenever `--url` is set).
- **MCP:** `github.com/modelcontextprotocol/go-sdk` **v1.7.0** (stable, past 1.0 — *changed
  2026-09-06, see §18 #1*), pinned exactly, protocol conformance tests. Every tool declares `outputSchema`; every result carries **both** text
  content (compact where defined) **and** `structuredContent`; failures set `isError`. Server
  `instructions` (returned at `initialize`) state the operating policy: project keys, actor
  identity = token name, lease TTL, the start-work convention, compact grammar version.
- **Web UI** *(changed 2026-09-06, see §18 #2)*: server-rendered `html/template` +
  **Tailwind CSS v4.3.3** (standalone CLI binary — **no Node, no npm, no node_modules**) +
  **htmx v4.0.0** (+ `sse` ext) + **Alpine.js v3.17.1** (small local interactivity) +
  **SortableJS v1.15.7** (drag). All JS vendored and embedded via `embed.FS`; the generated
  `app.css` is **committed**, so a plain `go build` never needs the Tailwind binary — only
  `make css` / CI regenerates it. Exact versions + license notices, CSP-compatible init.
  Markdown: `goldmark` render + `bluemonday` sanitize on render.
- **Events bus:** in-process fan-out after commit → SSE, activity feed. Append-only `events`.
- **Config:** flags + `KANBAN_*` env. Zero-config default: `kanban serve` →
  `http://127.0.0.1:8080`, data `./data/kanban.db`, auth required, bootstrap token printed once.

### Repository layout
```
cmd/kanban/            serve | token | export | import | backup | doctor | healthcheck | agent-config | mcp --stdio | demo | version
internal/domain/       entities, validation, transitions, next algorithm, compact grammar (pure Go)
internal/store/        sqlite repos, migrations (embed), tx helpers, writer/reader pools
internal/service/      use-cases — the ONLY layer MCP/UI/CLI call
internal/mcp/          tool registration, schemas, instructions, compact formatter, golden tests
internal/web/          handlers, templates, static (embed), SSE, sessions
internal/events/       bus
internal/auth/         tokens, sessions, middleware, trusted proxies, rate limit
internal/config/       flags/env, startup validation (auth/TLS/bind rules)
web/templates/ web/static/   htmx, sse ext, sortable, app.css, app.js (tiny reconnect wrapper)
docs/                  PLAN.md, MCP-TOOLS.md (grammar + every tool), AGENT-SETUP.md, DEPLOY.md, SECURITY.md, reviews/
.github/workflows/     ci.yml, release.yml
llms.txt
```

---

## 5. Data model

Timestamps UTC RFC3339, generated by the **database clock**. Internal ids UUIDv7; every task
has a human key `PROJ-N`; every tool accepts keys **case-insensitively** and returns canonical
uppercase. **Column names are unique per project.**

### Project
| field | notes |
|---|---|
| id, key | key `[A-Z][A-Z0-9]{1,7}`, unique, immutable |
| name, description | |
| version | int, +1 on every project/column config change |
| next_task_seq | counter for `PROJ-N` |
| focus_task_id | the "Now" banner, one per project |
| estimate_unit | default `"h"` |
| enforce_dependencies | default true |
| strict_done | default false: entering a `done` column requires all acceptance items checked |
| claim_ttl_seconds | default 3600, clamp [60, 86400] |
| archived_at | |

### Column
`id, project_id, name (unique per project), position (int), kind ∈ {backlog, active, done},
wip_limit (int?, active only)`. Defaults: `Backlog/backlog`, `Doing/active wip 3`,
`Review/active`, `Done/done`.

### Task
| field | notes |
|---|---|
| id, key, project_id, column_id | |
| parent_id | subtask; **depth cap 2** (task → subtask); a parent may not be blocked by its own subtree |
| rank | **sparse integer** (step 1024), transactional renumber when gaps exhaust; stable tie-break on key |
| title | ≤ 200 chars, single line |
| body | markdown ≤ 64 KB |
| type | `task \| bug \| feat \| chore \| doc \| perf \| research` — **one spelling everywhere** |
| priority | stored int 0–4; **API and compact use names** `none \| low \| medium \| high \| critical` |
| estimate | number?, in project unit |
| tags | []string, lowercase, no spaces, ≤ 20 |
| assignee | string?, free text (human or agent) |
| claimed_by, claimed_at, claim_expires_at | lease (§7); **does not bump `version`** |
| acceptance | []{text, done} ≤ 50 |
| due_at | time? (feeds `task_next` ordering) |
| column_entered_at | for `age` in compact |
| started_at, done_at | set on first entry into `active` / `done` |
| version | int; bumps on content/structure writes (§7) |
| metadata | JSON object ≤ 16 KB, schema-free extension valve |
| created_at/by, updated_at/by | actor = token name |
| archived_at | soft delete (restore possible) |

Derived: `blocked_by` (open blockers), `sub_done/sub_total`, `ready`, `lease_remaining`.

### Link
`blocker_id, blocked_id, type = "blocks"` (only value in v1; column reserved), `created_at/by`.
Rejected: self-links, duplicates (idempotent), cycles **including parent chains**.

### Note
`id, task_id, author, body ≤ 16 KB, created_at`. Append-only. Does not bump task `version`.

### Event (append-only)
`id, ts, actor, type, project_id, task_id?, payload`. Types: `project.created|updated|archived`,
`task.created|updated|moved|started|claimed|renewed|released|archived|restored`, `link.added|removed`,
`note.added`, `focus.changed`.

### Token
`id, name (unique; the actor identity), hash (SHA-256 of a 32-byte random secret — unsalted is
acceptable ONLY because the secret is 256 random bits; documented invariant), scopes ⊆ {read,
write, admin}, project_keys [] (empty = all), created_at, last_used_at, revoked_at`.

### Idempotency record
`token_id, key, request_hash, response, expires_at (24 h)`; unique `(token_id, key)`.

### Session (web)
`id, token_id, created_at, last_seen_at, expires_at` — idle 7 d, absolute 30 d.

---

## 6. MCP tool surface — nine tools

Principles: **task writes are batches** (`task_create`, `task_update`, `task_remove`); **reads
are compact by default**, `include` widens (one param name everywhere); **strict schemas**
(`additionalProperties:false`, every default/maximum/union stated in the published JSON
Schema); **keys not UUIDs**; **actor = token name, never a parameter**; **structuredContent on
every result**.

Envelope (`structuredContent`, mirrored as text unless the tool defines compact text):
```json
{ "ok": true,  "op": "task_update", "data": …, "meta": { "count": 3, "warnings": [] } }
{ "ok": false, "op": "task_update", "error": { "code": "conflict", "message": "…",
    "remediation": "Re-read BMB-14 (current state attached) and retry with if_version=8.",
    "current": { …task… } } }
```
Domain errors are tool results with `ok:false` + `isError:true`; transport/protocol errors are
JSON-RPC errors. Codes: `not_found, validation, conflict, blocked, wip_exceeded, claimed,
forbidden, cycle, rate_limited, payload_too_large, idempotency_mismatch`. Every error carries a
human `remediation` string.

### 6.1 `board_get`
```
project?: key                       (omitted = all accessible projects)
view?: "tasks" | "summary"          (default "tasks" for one project, "summary" for all)
done_limit?: int                    (default 0; max 200; most recently done first)
filter?: { columns?: [name] (OR), types?: [type] (OR), priority_min?: name, tags?: [tag] (OR),
           assignee?: string, claimed?: "any"|"mine"|"unclaimed"|"other", blocked?: bool,
           q?: string (title/body substring), updated_since?: time }
include?: ["body","acceptance","notes","links","metadata"]   (default none)
format?: "compact" | "json"         (default "compact"; structuredContent always JSON)
```
**Compact grammar v1** (versioned; `docs/MCP-TOOLS.md` is the contract; a strict regex must
round-trip every line in a golden test; gate **≤ 1 200 tokens for 30 active
tasks**):
```
compact_version=1
# BMB BeeMemoryBank · focus BMB-14 · Doing 2/3 · Review 1 · Backlog 12 · Done 40 (hidden)
## Doing
- BMB-14 [high bug] Fix WAL checkpoint race · est 2h · @alex · lease claude@rog 43m · sub 1/3 · #sync · v7 · age 2h
- BMB-17 [medium feat] Encrypted FTS index · est 8h · lease codex@desk expired · blocked-by BMB-14,BMB-9 · v3 · age 3d
## Review
- BMB-12 [task] Squash migrations 41-44 · est 1h · @alex · v2 · age 1d
## Backlog
- BMB-18 [critical bug] Sync loses tombstones · #sync · v1
```
Rules: header = key, name, `focus <KEY>|none`, one `<Column> <count>[/<wip>]` per column in
board order, `Done <total> (<n> shown|hidden)`. Task line: `- KEY [priority type]` (priority
omitted when `none`), title with whitespace collapsed, newlines removed, ` · ` replaced by
` - `; then fixed-order labeled suffixes separated by ` · `, each omitted when absent:
`est <n><unit>` · `@<assignee>` · `lease <actor> <remaining>|expired` · `sub <done>/<total>` ·
`blocked-by <KEY>,<KEY>` (open blockers only, sorted) · `#tag #tag` (sorted) · `v<version>` ·
`age <duration>` (time since column entry; active columns only). Durations `12m|3h|2d`.
`v<version>` is always present so read-modify-write needs no `task_get`.

### 6.2 `task_next`
```
project?: key
action?: "peek" | "claim" | "start"   (default "peek")
limit?: 1..10 (default 3)
include?: ["body","acceptance","notes","links"]  (default body[≤2 KB, truncated with "… +N chars"] + acceptance[≤10])
```
Algorithm (pure `internal/domain/next.go`, table-tested): candidates = unarchived tasks in
`backlog` columns of accessible projects, **runnable leaves** (no task with incomplete
subtasks), parent not blocked, not claimed by another actor with a live lease, no open `blocks`
predecessor. Order: priority desc → `due_at` asc (nulls last) → rank → created_at.
- `peek`: returns up to `limit` ready tasks **even when WIP is full** (`meta.wip_full=true`).
- `claim`: atomically leases `data[0]` (retry down the list on a lost race); **does not move**.
- `start`: in one transaction — WIP check on the first `active` column, claim, move there, set
  `started_at`; fails loud with `wip_exceeded`.
Always returns `meta.reasons = { blocked_dependency, wip_full, claimed_by_other,
parent_incomplete, not_leaf }` counts and `meta.blocked_top: [{key, blocked_by}]` (≤ 5) so the
agent knows *why* the rest is not ready. On `/mcp/readonly`, `claim|start` → `forbidden`.

### 6.3 `task_get`
`keys: [key] (1..50)`, `include?` (default: body, acceptance, links, subtask summary, version,
lease; `notes` opt-in, last 20 with `notes_before` cursor). Preserves request order; missing keys
appear as per-key `not_found` entries, never dropped.

### 6.4 `task_create` — all-or-nothing
```
tasks: [ { project: key, title, body?, type?, priority?, estimate?, tags?, assignee?,
           column?: name (default first backlog), parent?: key | "@ref", blocked_by?: [key | "@ref"],
           acceptance?: [string], due_at?, metadata?, ref?: string, idempotency_key?: string } ] (1..100)
```
Symbolic refs only (`ref:"schema"` → `"@schema"`), no positional `$0`. Refs may point at any
item in the batch; the whole batch is validated as a DAG and committed atomically. Idempotency:
`(token, idempotency_key)` unique for 24 h; replay returns the **original** response; same key
with a different request hash → `idempotency_mismatch`. Returns compact lines + keys.

### 6.5 `task_update` — per-item results by default
```
patches: [ { key, if_version?, title?, body?, type?, priority?, estimate?|null, tags? | tags_add? | tags_remove?,
             assignee?|null, column?: name, rank?: "top"|"bottom", parent?: key|null,
             acceptance?: [{text,done}] | acceptance_check?: [index] | acceptance_add?: [string],
             note?: string, focus?: bool, due_at?|null, metadata_merge?: object (null value deletes key),
             force?: bool, reason?: string } ] (1..100)
atomic?: bool (default false)
```
- **`if_version` is required** for any replacement-style field (title, body, type, priority,
  estimate, tags, assignee, column, rank, parent, acceptance*, due_at, metadata_merge). A patch
  containing only commutative ops (`note`, `tags_add`, `tags_remove`) may omit it.
- `column` runs transition validation inside the transaction: dependencies (`blocked`), WIP
  (`wip_exceeded`), `strict_done` (`validation`). `force:true` requires **admin scope** and a
  `reason`, both logged in the event.
- Conflict → `conflict` error with `current` task attached.
- `tags` (replace) is mutually exclusive with `tags_add/tags_remove`; `acceptance` with
  `acceptance_check/acceptance_add`. `focus:false` clears focus only if this task holds it.
- Per-item `ok|error`; `atomic:true` makes the batch all-or-nothing.

### 6.6 `task_link`
`add?: [{blocker, blocked}]`, `remove?: [{blocker, blocked}]` (type implicit `blocks`; a `type`
field is reserved). Atomic. Rejects self, cycle (incl. parent chains). Bumps `version` of both
endpoints (readiness changed). Returns resulting `blocked_by` + versions for touched tasks.

### 6.7 `task_claim` — singular CAS on one row
`key, action: "claim" | "renew" | "release", ttl_seconds? (clamped), force?: bool (admin only —
steals; same actor never needs force)`. SQL:
`UPDATE tasks SET claimed_by=:me, claimed_at=:now, claim_expires_at=:exp WHERE key=:key AND
(claimed_by IS NULL OR claim_expires_at < :now OR claimed_by = :me)`; `RowsAffected()==1`
decides. Returns `claimed_by, claim_expires_at, lease_remaining_seconds`. An expired former
owner cannot `renew` after another actor won.

### 6.8 `task_remove` — archive only
`items: [{key, if_version?}] (1..100), cascade_subtasks?: bool (default true), restore?: bool`.
Archiving clears claim and focus, keeps links (hidden while archived). **Hard delete is not
available over MCP** — `kanban task purge` (admin CLI) only.

### 6.9 `project_upsert`
`mode: "create" | "update"` (required — a key typo must not fork the board), `key`, `name?`,
`description?`, `if_version?` (required for update), `columns?: [{name, kind, wip_limit?}]`
(full desired list, in order), `remove_columns?: [{name, move_tasks_to}]` (required for every
existing column absent from `columns`), `settings?: {estimate_unit, enforce_dependencies,
strict_done, claim_ttl_seconds}`, `archived?: bool`. Returns the final layout + version.

### Not tools, on purpose
Search (`board_get.filter.q`), stats (`board_get` header), comments listing (`task_get`),
sprints, time logging, users, attachments, plan/analyze/reflect/verify chains, hard delete.
**"Every write is a batch" is stated precisely as: every *task* write (create/update/remove) is
a batch;** `task_claim`, `task_link`, `project_upsert` are single-intent operations.

---

## 7. Concurrency model

- **Versions.** `version` bumps on: title, body, type, priority, estimate, tags, assignee,
  column, rank, parent, acceptance, due_at, metadata, archive/restore, link add/remove (both
  endpoints). **Does not bump on:** notes (append-only), lease claim/renew/release, focus
  (project state → project `version`). Rationale: a background lease renewal by agent A must
  never fail agent B's `if_version` edit — spurious conflicts teach agents to drop `if_version`.
- **Required `if_version`** for replacement writes (§6.5). Conflicts carry `current`.
- **Leases.** `claimed_by + claim_expires_at`, compared with the DB clock. Renewed by the
  claimer's own `task_update`/`task_claim renew`; never by another actor's edit. Compact shows
  remaining time / `expired`; `task_next` ignores expired leases.
- **Single writer.** One-connection writer pool, `BEGIN IMMEDIATE`, bounded queue, request
  context cancellation honored before execution, events published **after commit**. All checks
  (WIP, dependencies, cycles, sequence, focus, versions, claims, idempotency) happen inside the
  transaction — no pure-Go precheck followed by a write.
- **Ranks.** Sparse integers with transactional renumbering; deterministic order via `(rank, key)`.
- **Idempotency** persisted in the same transaction as the mutation; 24 h sweeper.
- **SSE.** `Last-Event-ID` replay from the events table; if the id is older than retained →
  send `event: resync` so the client reloads instead of silently missing state.

---

## 8. Auth & security

- **Bearer tokens, mandatory by default.** `--auth off` is accepted **only** when the resolved
  listen address is loopback (`127.0.0.1`, `::1`); `0.0.0.0`, `::`, empty host or any other
  interface with auth off → refuse to start with a clear message.
- **Default bind for the bare binary is `127.0.0.1:8080`** (café-Wi-Fi footgun). The Docker
  image entrypoint binds `0.0.0.0:8080` with auth required.
- **TLS acknowledgement.** A non-loopback listener whose `--base-url` is not `https://` requires
  the explicit flag `--insecure-http`; otherwise refuse to start. Reason: mandatory bearer auth
  does not protect a bearer sent in cleartext. Docs show the two-minute Caddy path.
- **Bootstrap.** Empty token table → create `admin` token: use `KANBAN_ADMIN_TOKEN` (canonical
  for Docker, may be a `_FILE` path) or auto-generate and print **once** with a prominent "this
  token is now in your logs — rotate it: `kanban token rotate admin`" line. Concurrent startups
  cannot double-print (created under the migration lock). Never shown unauthenticated in the UI.
- **Scopes:** `read` · `write` (implies read) · `admin` (tokens, project archive, force, steal).
  Optional per-project restriction.
- **Actor identity** = token name. No `actor` parameter exists on any tool.
- **Web sessions:** paste token → `sessions` row, HttpOnly, SameSite=Lax, `Secure` when base-url
  is https or the request arrived via a trusted proxy with `X-Forwarded-Proto: https`. CSRF token
  on every UI form. Idle 7 d / absolute 30 d.
- **SSE for agents:** never a bearer in a query string. `POST /events/ticket` (token-authed)
  returns a 60 s one-time ticket usable as `?ticket=`. Browsers use the session cookie.
- **Proxies:** `X-Forwarded-*` honored **only** from `--trusted-proxies` CIDRs and only for
  logging, rate-limit keys, URL generation and cookie `Secure` — **never for auth decisions**.
  CI test: a spoofed `X-Forwarded-For` from an untrusted IP changes nothing.
- Constant-time token-hash comparison; authorization headers and bootstrap secrets redacted from
  logs; `/readyz` reveals no database details anonymously.
- Rate limits: 600 req/min per token (a 100-item batch = 1 request), 20/min per IP on `/login`;
  bounded LRU store.
- Strict schemas, 1 MB body limit, sanitized markdown, CSP with no inline scripts, CORS off by
  default (opt-in allow-list), `govulncheck` in CI.

---

## 9. Web UI

- **Board** `/p/KEY`: columns with WIP `2/3`; cards: key · title · priority stripe · type badge ·
  estimate · tags · assignee · lease badge with remaining time (server-computed) · blocked
  indicator · subtask progress · age. Drag between columns (transition errors as toasts).
  "Hide done" default on. **Focus banner**. Project switcher.
- **Task drawer** `/t/BMB-14`: rendered body, acceptance checklist (clickable), blockers /
  blocking, subtasks, notes timeline, history, edit form.
- **Activity** `/p/KEY/activity`: live feed — *watch the agents work*.
- **Overview** `/`: projects, counts, focus per project.
- **Admin** `/admin`: tokens (create/revoke/rotate), projects/columns, export/backup. (The only
  admin path in a shell-less container besides env vars.)
- **Login**, first-run page, **`/agent-setup`**: copy-paste snippets for Claude Code / Codex /
  Cursor / generic, each including the `Authorization` header — the part everyone fumbles.
- **Live updates:** htmx `sse` on `/events`, targeted fragment swaps; small reconnect wrapper in
  `app.js`; every reverse-proxy example includes `proxy_buffering off` / `X-Accel-Buffering: no`.
- **Visual language** *(changed 2026-09-06, see §18 #2)*: Tailwind v4 with a small theme layer
  (`@theme` tokens) carrying the owner's HTML-status-page language — light/dark via
  `prefers-color-scheme` + manual toggle, priority stripes, type badges, estimate pills,
  collapsible done section, `tabular-nums`. Quiet and dense: this is a dashboard people glance
  at, not a landing page. Keyboard-operable move menu, visible focus states,
  `prefers-reduced-motion`. Alpine handles drawer/menu state; htmx handles all server traffic.

---

## 10. CLI & operations

```
kanban serve   [--addr 127.0.0.1:8080] [--data ./data] [--base-url https://kanban.example.com]
               [--auth required|off] [--insecure-http] [--trusted-proxies CIDR,…] [--demo]
kanban token   create --name claude@rog --scope write [--project BMB] | list | revoke <name> | rotate <name>
kanban agent-config --client claude|codex|cursor|generic [--token <name>]   # prints paste-ready MCP config
kanban agent-md                                                              # prints the recommended CLAUDE.md/AGENTS.md snippet
kanban export [--project BMB] > board.json | kanban import board.json (idempotent by key)
kanban backup ./backups/          # VACUUM INTO, timestamped
kanban task purge <key>…          # admin hard delete (never over MCP)
kanban doctor                     # bind addr, base-url, TLS/auth status, db health, journal mode, wal size, redacted endpoint
kanban healthcheck                # exit 0/1; used by the image HEALTHCHECK (no shell in the image)
kanban mcp --stdio [--url … --token …]   # stdio bridge = HTTP client when --url is set; standalone file mode otherwise
kanban migrate [--dry-run] | kanban version
```
Env: `KANBAN_ADDR, KANBAN_DATA, KANBAN_BASE_URL, KANBAN_AUTH, KANBAN_INSECURE_HTTP,
KANBAN_ADMIN_TOKEN[_FILE], KANBAN_TRUSTED_PROXIES, KANBAN_LOG_LEVEL, KANBAN_LOG_FORMAT`.

**Startup banner (the 60-second path):**
```
basic-kanban-board-mcp v1.0.0
  Board:  http://127.0.0.1:8080          Agent setup: http://127.0.0.1:8080/agent-setup
  Admin token (shown once, now in your logs — rotate with `kanban token rotate admin`): kbn_…
  Paste into your MCP client:
  { "mcpServers": { "kanban": { "type": "http", "url": "http://127.0.0.1:8080/mcp",
                                "headers": { "Authorization": "Bearer kbn_…" } } } }
```

**Docker:** base `gcr.io/distroless/static-debian12:nonroot` (CA roots + tzdata + nonroot, no
shell), `ENTRYPOINT ["kanban","serve","--addr","0.0.0.0:8080","--data","/data"]`,
`HEALTHCHECK CMD ["kanban","healthcheck"]`, volume `/data`.
`docker run -d --name kanban -p 8080:8080 -v kanban-data:/data -e KANBAN_ADMIN_TOKEN=… ghcr.io/ultrathinker/basic-kanban-board-mcp`
**Releases:** GoReleaser → linux/darwin/windows × amd64/arm64, checksums, SBOM, multi-arch image.
Windows paths tested (owner is on Windows). `go install …/cmd/kanban@latest` quickstart.

---

## 11. Quality — invariant tests instead of coverage gates

Measured targets, reported not promised: binary and image size, idle RSS, cold start,
`board_get` p95 with 1 000 tasks, **compact ≤ 1 200 tokens (`len/4`) for 30 active tasks**
(measured ~1 058; the gate is a regression guard, and the claim we publish is the ratio
against JSON — ~81% smaller than minified, ~90% smaller than indented, same board).

Invariant tests that gate release:
1. 50 goroutines claim one task → exactly one wins; `start` under WIP=1 → exactly one moves.
2. Every compact line round-trips through the strict grammar regex (golden fixtures).
3. Token budget fixture (30 active tasks) ≤ 1 200 tokens, and the compact/JSON ratio for
   that same board stays at or better than 5× minified / 10× indented.
4. Version conflict returns `current`; lease renew does **not** change `version`; link add does.
5. Cycle detection incl. parent chains; depth-3 subtask rejected.
6. WIP / dependency / strict_done enforced inside the transaction (no TOCTOU).
7. Migrations up from every prior released version.
8. Spoofed `X-Forwarded-For` from an untrusted IP changes nothing.
9. `--auth off` with a non-loopback bind refuses; non-loopback + http base-url without
   `--insecure-http` refuses.
10. Idempotent replay returns the original response; mismatch errors.
11. SSE `resync` on a `Last-Event-ID` gap.
12. MCP conformance via go-sdk client: every tool, every error code, `/mcp/readonly` rejects
    `claim|start`, `structuredContent` matches `outputSchema`.
13. Verified-client matrix: Claude Code, Codex, Cursor, MCP Inspector — remote HTTP + bearer,
    smoke-tested per release; only source-verified compatibility is claimed.

CI: `go vet`, `staticcheck`, `govulncheck`, `gofmt`, `-race`. Conventional commits, semver.

---

## 12. Docs & launch

- **README first screen:** GIF of the activity feed while three agents work · the `docker run`
  line · where the token appears · one MCP config snippet · then the differentiator table
  (source-linked, dated) · verified-client matrix · local zero-TLS path and Caddy remote path.
  Lead with the owner's scenario: *three machines, four agent CLIs, one board.*
- `docs/MCP-TOOLS.md` (grammar + every tool + examples, single page — what LLMs will read),
  `docs/AGENT-SETUP.md`, `docs/DEPLOY.md`, `docs/SECURITY.md`, `llms.txt` at repo root and web
  root (documents the loop: `board_get` → `task_next(start)` → work → `task_update{note,
  if_version}`).
- Listings: official MCP registry (schemas inline), awesome-mcp-servers PR, Smithery/mcp.so,
  GitHub topics; Show HN, r/selfhosted, r/ClaudeAI. `--demo` seeds idempotent, visibly labeled
  sample data.
- **Before the first push to a public remote, squash history into a single initial commit.**
  A 23 MB `kanban.exe` was committed by an agent in `bd0bc1c` (wave 1). It is untracked and
  gitignored now, but it still sits in the object store, and a "minimal, single-binary" tool
  whose clone drags a stale binary contradicts its own pitch. There is no remote yet, so this
  costs nothing today and cannot be undone cheaply after publication.

---

## 13. Competitive landscape (condensed; full cards in the research logs)

| Project | Stars | Tools | Transport / auth | Fatal gap for our use case |
|---|---|---|---|---|
| Veritas Kanban | 0.8k | 42 | stdio only | password sessions hard-locked to loopback |
| Vibe Kanban | 28k | 34 | stdio only | company shut down 2026-04, no shared board |
| Scrumboy | 0.4k | 50 | HTTP + OAuth2.1 | no batching, 50 tools |
| Kan | 5.6k | 46 | stdio wrapper | Postgres, no priority/estimate/deps, 2 s polling |
| kandev | 0.8k | 55–65 | HTTP, auth optional | UUID-only, "just VPN it" |
| Leantime | 11.5k | 55 | HTTP | MCP only with a paid license (404 otherwise) |
| Plane | 59k | 30 | HTTP (official) | 13 containers, AGPL |
| Vikunja | 5.3k | 3rd-party | stdio | last-write-wins, silent 404 license gate |
| Kanboard | 9.9k | 3rd-party 26–171 | mostly stdio | raw passthrough bridges, polling UI |
| Planka | 12.5k | 3rd-party 27 | stdio/SSE | fair-code license, no priority |
| claude-task-master | 28k | 36 | stdio | files in repo, no UI, no multi-agent safety |
| Backlog.md | 6k | ~20 | stdio | files in repo, merge conflicts under concurrent agents |
| beads | ~20k | 15 | undocumented | Dolt dependency, no UI; **`bd ready` + atomic claim are the best ideas in the field** |

Stolen with attribution: human keys (Linear/Scrumboy/Vibe), `ready`/claim (beads), acceptance
criteria (Backlog.md), dependency-gated moves (agent-board), WIP limits (Vikunja/kandev),
`include`/toolset thinking (GitHub MCP), read-only endpoint (Linear), consolidation proof
(planka-mcp), batch envelope (vikunja-mcp), metadata valve (Kanboard).

---

## 14. Roadmap (4–5 weeks of focused agent-driven work; concurrency tests are never the thing cut)

| Milestone | Scope | Exit criterion |
|---|---|---|
| **M0 Skeleton** | repo, `AGENTS.md` for implementing agents, CI, `serve`/`healthz`/`readyz`/`healthcheck`, SQLite pools + migrations, config with bind/auth/TLS rules, token bootstrap, `doctor` | `docker run` prints the banner; refusal cases tested |
| **M1 Core + MCP** | domain, store, service; **compact grammar + claim/version golden tests first**; all 9 tools; `instructions`; `/mcp` + `/mcp/readonly` + stdio bridge; idempotency | Claude Code and Codex create/start/update tasks end-to-end; invariants 1–7, 10, 12 green |
| **M2 Web UI** | board, drawer, activity, overview, login, admin, SSE + resync, drag-and-drop, theme, `/agent-setup` | owner replaces the HTML page in daily use |
| **M3 Dogfood + polish** | export/import/backup/purge, `--demo`, `agent-config`/`agent-md`, verified-client matrix, docs set | three machines, three agents, one board — for a week without a manual fix |
| **M4 Launch** | README + GIF, GoReleaser, ghcr, registry listings, posts | `v1.0.0` |
| **v1.1** | webhooks (HMAC, retries, SSRF guard), public REST `/api/v1`, Homebrew; **then** decide the MCP-proxy plugin model with real plugin authors | |

---

## 15. Decisions closed by review (the former open questions)

| # | Question | Decision | Votes |
|---|---|---|---|
| 1 | compact vs JSON default | compact text default with a **versioned grammar + regex golden test**, `structuredContent` always | 3/3 |
| 2 | 9 tools vs 7 | **9** — discoverability of `claim`/`link` beats folding | 3/3 |
| 3 | `task_update` atomic default | **per-item results; `atomic:true` opt-in; `task_create` all-or-nothing** | 2/3 (Codex dissent: atomic) |
| 4 | subtask depth | **2**, firmly | 3/3 |
| 5 | priority int vs names | **names in API and compact**, ints internal | Codex names; Gemini/GLM ints+aliases → one spelling wins |
| 6 | focus | **per project** | 3/3 |
| 7 | claim TTL | **1 h**, clamp [60 s, 24 h], renew only by claimer | 3/3 |
| 8 | stdio shim | **keep**, as HTTP client when `--url` set | 3/3 |
| 9 | MIT vs AGPL | **MIT** | 3/3 |
| 10 | missing | `action: peek/claim/start`, reason counts, durable idempotency, `remediation`, `v`/`age`/`lease` in compact, `instructions`, `agent-config`, `doctor`, tickets for SSE | merged |
| 11 | cut | webhooks, REST, plugin proxy, hard delete over MCP, `relates/duplicates`, `start_at`, DoD templates, `/metrics`, coverage % gate | merged |
| 12 | name | keep `basic-kanban-board-mcp` (owner's call); brand via positioning; tagline "One board. Every agent. Nine tools." | 2/3 warned on "basic" |

---

## 16. Decision log

- 2026-09-06 · Go over .NET/Rust — adoption is primary; the niche speaks Go.
- 2026-09-06 · MIT over AGPL.
- 2026-09-06 · Server over files-in-repo.
- 2026-09-06 · 9 tools; dependencies, `next`, claim are core.
- 2026-09-06 · No JS build; SQLite only.
- 2026-09-06 · **v2:** v1 public surface = MCP + UI + CLI; webhooks/REST/plugins → v1.1.
- 2026-09-06 · **v2:** `task_next.action peek|claim|start`; leases never bump `version`;
  `if_version` required for replacement writes; per-item update batches; `blocks` only;
  names for priority; symbolic refs only; archive-only over MCP; `mode` on `project_upsert`;
  `--insecure-http`; default bind loopback; distroless image; SSE tickets.

---

## 17. Review synthesis — what each reviewer changed

**Unanimous, adopted as-is:** stack is right; compact default with real grammar +
`structuredContent`; keep 9 tools; depth 2; focus per project; TTL 1 h; stdio bridge stays;
MIT; tagline; `actor` param removed (token identity authoritative); `/mcp/readonly` fails loud;
`kanban healthcheck`; constant-time compare; unify `feat`/`feature`; `doctor`; `agent-config`;
writer/reader pools with per-connection pragmas; durable idempotency in the schema; startup
banner with paste-ready config; `llms.txt` + registry listings; webhooks out of v1.

**Codex (unique contributions):** Docker bind bug (`127.0.0.1` inside the container is
unreachable); distroless/CA roots; `--insecure-http`; `action: peek|claim|start`; required
`if_version`; sparse integer ranks; `project_upsert.mode` + project version; `{blocker,
blocked}`; cut REST/webhooks/plugin-proxy/hard-delete from v1; `instructions` at initialize;
readiness reason counts; verified-client matrix; `done_limit`/`claimed` enum/`view`; cut the
coverage % and size promises.

**Gemini (unique):** WIP dead-end → `peek` still returns ready work; simplify `task_update`
(drop `{after:key}`, mutually exclusive tag/acceptance params); case-insensitive keys; symbolic
`@ref`; `remediation` in errors; `(expired)`/stalled visibility; `0.0.0.0` loopback check.

**GLM (unique):** default bind `127.0.0.1`; body truncation in `task_next`; **leases must not
bump `version`**; `v7` and `age` in compact; `(lease 43m)` = remaining; SSE one-time tickets;
`task_get` notes opt-in; cycle detection across parent chains; `OR claimed_by = :me` in the
claim CAS; column names unique; project archive; `agent-md`; plan 4–5 weeks not 3; film the
activity feed, not drag-and-drop.

**Disagreements resolved:** atomic batches (per-item, 2/3, because all-or-nothing on loosely
related updates reintroduces one-item batches); claim-moves-or-not (explicit `start` satisfies
both camps); link types (`blocks` only, 2/3); priority representation (names, one spelling);
REST in v1 (cut — MCP is the product; UI uses internal endpoints); naming (owner's explicit
choice kept; concern recorded; revisit v1.1).

---

## 18. Deviations log

Every departure from this plan discovered during implementation goes here (see "How to use
this plan"). Newest last. Empty at v2.

| # | Date | Section | Planned | Actual | New fact that forced it | Impact |
|---|---|---|---|---|---|---|
| 1 | 2026-09-06 | §4, §11 | "go-sdk is pre-1.0, the API moves — pin and treat upgrades as deliberate" | `github.com/modelcontextprotocol/go-sdk` **v1.7.0** (2026-07-27) — past 1.0, stable API | Checked the module proxy before writing contracts | Lower risk than planned. Still pinned exactly; the "protocol conformance test" stays, the "expect churn" caveat is dropped |
| 2 | 2026-09-06 | §4, §9 | Hand-written `app.css` + htmx + SortableJS, "no build step at all" | **Tailwind CSS v4.3.3 via the standalone CLI binary** (no Node/npm) + htmx v4.0.0 + **Alpine.js v3.17.1** + SortableJS v1.15.7; generated CSS is committed and embedded | Owner's instruction: use the most popular technologies that produce a good-looking UI quickly. Tailwind is that, and its standalone binary keeps the no-npm rule intact | One build step in CI (download one binary, run it) — **not** an npm toolchain. Contributors still need no Node. Single Go binary unchanged. Generated CSS committed so `go build` alone always works |
| 3 | 2026-09-06 | §4 | Go 1.25 | **Go 1.27.0** | Latest stable at implementation time | None |
| 4 | 2026-09-06 | §5, §10 | `TokenRepo` had no way to change a token's secret | Added `TokenRepo.UpdateHash(tx, id, hash)` (+ `auth.TokenLookup.UpdateHash`, adapter, SQLite impl) | `kanban token rotate` was implemented as `Create` on an existing token, which violates the `tokens_name_uniq` / `tokens_hash_uniq` indexes — rotation would have failed in production, not just in tests | Frozen-contract change made by the coordinator; rotation now updates the row in place |
| 5 | 2026-09-06 | §6.1, §11 | compact `board_get` ≤ **600 tokens** for 30 active tasks | Gate raised to **1200 tokens**; the product claim becomes a measured ratio, not an absolute | The 600 was written into the plan before anything was measured, and the first fixture was bent to fit it (short titles, most fields omitted). Rewritten honestly, the same 30-task board measures **~1058 tokens / 4235 bytes**. Measured against the identical board: minified JSON **~5640 tokens (5.3×)**, indented JSON **~10650 tokens (10.1×)** — i.e. compact saves **~81%** and **~90%** respectively. Competing MCP servers overwhelmingly return `MarshalIndent` output, so 10× is the realistic comparison. Shrinking the grammar to reach 600 would drop information for the sake of a round number | **Resolved.** `domain.CompactTokenBudget = 1200` as a regression gate with headroom for wording but not for a new field. README/docs quote the ratio ("~90% fewer tokens than a JSON board dump"), never a bare absolute. Approved by the maintainer 2026-09-06 |
| 6 | 2026-09-06 | §9 | Live board updates via the htmx `sse` extension on `/events`, targeted fragment swaps | Native `EventSource` in `app.js`; SSE carries a *change signal*, the page re-fetches its own URL and swaps `#board` / `#activity-feed` | The vendored htmx is **v4.0.0, which has no extension mechanism at all** — no `defineExtension`, no `hx-ext`. The vendored `sse.min.js` is the v2.2.2 extension written against htmx 2's API; it threw a TypeError on every page load and `hx-ext="sse"` was inert. Fixing targets and payloads would not have helped, because the extension could never run | HTML still comes from one template set, so there is no second renderer to drift. Debounced 300 ms, suppressed during drag, `resync` reloads, bounded reconnect |
| 7 | 2026-09-06 | §4, §9 | htmx + Alpine.js are part of the front-end stack | Both still vendored but **no longer loaded**; `sortable.min.js` and `app.js` are the only scripts | htmx lost its only consumer when the inert `hx-ext` markup went (deviation 6). Alpine cannot run here at all: its standard build uses `new Function()`, which `script-src 'self'` refuses — and PLAN §8 mandates that CSP. Its single use (a copy button) is now plain JS | Restoring either is one `<script>` tag; the reasons are recorded in `web/static/vendor/LICENSES.md`. The page now has no framework at all, which suits "the basic one" |
| 8 | 2026-09-06 | §9 | Priority stripes, type badges, estimate pills | **Monochrome only.** Priority = rule width + title weight + an inverted label for the top two; type = the word in a fixed-width slot at a constant x; must-not-miss state = one vocabulary, the inverted block (paper on ink). No accent hue anywhere | Visual direction set by the maintainer 2026-09-06: the UI must read as "the basic one" on sight. Colour was never needed to *distinguish* type, only to make it fast — a fixed slot restores the speed. On a white page a solid black block is the loudest mark available, louder than a red chip, and it survives greyscale printing, colour blindness and bad screens | Enforced mechanically: `TestStylesheetIsGreyscale` rejects any hex where r≠g≠b plus hsl/lch/color-mix/gradients; `TestStylesheetHasNoElevation` rejects box-shadow and non-zero radius; `TestThemeTokensAreReachable` rejects `@theme` and dangling `var()` |
| 9 | 2026-09-06 | §8 | CSRF exempt for any request carrying a non-session credential | Exemption narrowed to request **headers** only (`Authorization`, `X-API-Key`) | The first implementation also exempted a `?ticket=` query parameter, reasoning that SSE tickets are only issued to a proven token holder. It never validated the ticket — only its presence — and a query string is entirely attacker-chosen, so any page could POST to `/admin/tokens?ticket=x` with the victim session cookie and skip CSRF outright. Found by an independent review; the coordinator had approved the original design and missed it | Only headers qualify, because a cross-origin form can aim a request anywhere but cannot set a header on it. Tickets are consumed on `GET /events`, which never reaches `verifyCSRF`, so the exemption bought nothing. Pinned by `TestCSRF_QueryParamCannotBypass` |
| 10 | 2026-09-06 | §9 | `GET /admin/backup`, `GET /admin/export`, `GET /p/{key}/export` | All three are **POST** with a CSRF token | An anchor cannot carry a CSRF token by construction, and `/admin/backup` is a GET that writes a file to disk — a state change behind a safe method | Templates submit them as forms; each handler verifies CSRF after the auth check |
| 11 | 2026-09-06 | §4, §9 | htmx, its SSE extension and Alpine.js vendored in `web/static/vendor/` | All three deleted, not merely unloaded | Deviation 7 left them on disk so restoring one would be a single `<script>` tag. An architecture review then pointed out they were still **embedded in the binary**: 95 KB of JavaScript that no page executes, shipped inside a product whose pitch is that it is small. Keeping a restore path is not worth carrying dead weight in every release | Only `sortable.min.js` (33 KB) remains, and it is loaded. `web/static/vendor/LICENSES.md` records what was removed and why, so "where did htmx go?" has an answer without archaeology |
| 12 | 2026-09-06 | §5, §7 | `store.Tx` exposed only `Now() time.Time`; batch writes had no per-item rollback | `Tx` gains `Nested(fn func(Tx) error) error` (SAVEPOINT-backed) and `Now()` returns an error | The nine tools promise **per-item** batch results — nine updates succeed, one conflicts. Without a nested unit the only way to honour that was the convention "validate everything before touching a row", which holds exactly as long as every future contributor remembers it. An architecture review named this the one change to make before publishing. Separately, `Now()` silently fell back to the process clock on failure, and that clock is the ordering authority for lease expiry, `done_at` and the event log | Frozen-contract change made by the coordinator. A failed item now rolls back its own writes **and** its staged in-memory events; the enclosing transaction stays usable. `Atomic: true` still aborts the whole batch on the first error. One behaviour change: a non-`*domain.Error` failure is now a loud call-level error instead of `ItemResult{OK: false, Err: nil}` |
| 13 | 2026-09-06 | §5, §6.7 | Hierarchy and dependency edges were not constrained to one project | Cross-project parents and blockers are refused in the service and blocked by triggers in migration `0002` | An authorization hole, not a tidiness issue: a token restricted to project A could parent or block its task against a guessed key in project B, leaking that key into board reads and making `task_next` refuse work for a reason the caller cannot see | Two distinct refusals: `forbidden` when the token cannot read the other project (message names only the project, never the task key, and is byte-identical whether or not the key exists), `validation` when it can — v1 has no cross-project graph. `HealthInfo.Migration` now reports 2 |
| 14 | 2026-09-06 | §11 | Cycle detection walked the wait-for graph to a depth of 16 | The depth limit is gone entirely | The batch API accepts 100 tasks and can trivially describe a longer chain, so a cycle beyond the horizon **committed** and left that component with no runnable leaf — `task_next` stalls there forever. Worse, the limit was hiding an exponential walk: the old query enumerated simple paths, so raising the number would have been a trap. Two sibling horizons (`parentChain` at 32 hops, `parentChainDepth` at 64) silently returned "no ancestor" and were removed the same way | The walk is a `UNION` recursive CTE bounded by its visited set, so it terminates on the graph rather than on a constant. The reported cycle path comes from a separate linear Go BFS, run only when a link is rejected |
| 15 | 2026-09-06 | §4, §8 | "One writer process per file" was documented, not enforced | An ownership lock on the data directory, taken **before** `store.Open` so migrations cannot race; plus a bounded admission queue in front of the single writer and a lifecycle owner for background maintenance | Every correctness argument in §7 assumes one process owns the file, and nothing stopped a second `kanban serve`. Migrations ran under their own advisory lock, which is not the same guarantee. The writer also had no admission boundary, so overload piled up behind `busy_timeout` and failed late and indistinguishably from a real conflict | A second server refuses with a message naming the lock file, the database and the command to run. A closed server never locks the next one out |
| 16 | 2026-09-06 | §6.2 | `task_next` returns body ≤2 KB and acceptance ≤10 items by default | Summary tier is body ≤256 B and acceptance ≤2 items; `detail:"full"` restores PLAN's numbers exactly | Measured: the tool an agent calls to pick its next piece of work cost **~2 575 tokens for three candidates**, against ~1 064 for reading the entire 30-task board. Asking "what next" was more expensive than reading everything. PLAN's own bounds land at ~2 575 — implementing them literally would have fixed nothing. One acceptance item costs ~41 tokens on the wire against ~4 for 64 bytes of body excerpt, so the summary budget goes to the excerpt and the criteria are represented by a count | Default is now ~949 tokens for three candidates, cheaper than a whole-board read, asserted by a test gate. `include` selects fields; the new `detail` says how much of each; the service returns the shaping as `NextResult.Projection` instead of the two layers inferring it separately. A clipped list carries `acceptance_total` so a short answer is never ambiguous |
| 17 | 2026-09-06 | §6.1 | Compact project header had no version segment | Header gains a trailing `· v<n>`; `CompactVersion` stays 1 | `project_upsert(mode:"update")` requires `if_version` and nothing published a project version, so an admin could not reconfigure a project through MCP at all — pressure for a tenth tool. The segment is trailing and additive, so a v1 parser ignores it; bumping the grammar version would force every future reader to handle two versions to gain nothing | Coordinator's call not to bump. Budget moved 1 062 → 1 064 tokens against the 1 200 ceiling. `board_get` structured content also gained project settings and estimate unit, and `task_get` gained a `notes_before` cursor — all additive extensions to existing tools, still nine |
| 18 | 2026-09-06 | §10, §14 | `kanban demo` and `serve --demo` seed idempotent, visibly-labeled sample data | Same behaviour, implemented in a new package `internal/demo` | Both were still stubs at the end of implementation: the subcommand returned "not implemented yet — it lands with the service layer (M1)", and `--demo` was parsed into `config.Config` and read by nobody, so a flag documented in `--help` and in `docs/DEPLOY.md` silently did nothing. That is the exact failure the "fail loud" rule exists to prevent, and `--demo` is on the sixty-second evaluation path (§14) | New package, so it is not in the AGENTS.md reference layout: it depends on `internal/service` and `internal/domain` only and goes through the same `Service` interface as MCP and the web UI, so the sample board cannot take a shape the product would refuse. Seeded before the listener opens; idempotent by project key, because `--demo` lives in a compose file and runs on every restart; fatal on failure rather than a warning. Four tests, including one that a pre-existing project is untouched |
| 19 | 2026-09-06 | §6.1 | Compact renders one `## <column>` section per column | A column with no task lines under it is skipped entirely; `CompactVersion` stays 1 | Found by looking at the first `--demo` board through the tool itself: `board_get` with no arguments is `view:"summary"`, where no column carries tasks by definition, so the cheapest read in the product — the one the whole pitch rests on — opened with four empty headings and nothing else. An empty heading is also ambiguous three ways: the column is empty, the view omitted its tasks, or a filter matched none of them | The header segment (`Backlog 6 · Doing 1/3 · Review 0`) already answers all three unambiguously, so nothing is lost. Summary is now exactly one line per project. Done was already skipped this way when hidden, so no parser could have relied on a section per column. Pinned by `TestRender_SummaryViewIsHeaderOnly`; one golden line removed |
| 20 | 2026-09-07 | §6 | acceptance shapes, task_create project errors, if_version discovery were as first shipped | `task_update.acceptance` now also accepts bare strings (sugar for `{text,done:false}`); `task_create` gives a "put project inside each item" remediation for a top-level or missing `project`; if_version schema says to read the version from board_get/task_get | An all-night dogfood run (a research program on a live board) surfaced the friction: acceptance was string-shaped on create but object-shaped on update, agents put `project` at the top level and got a terse SDK error, and after a context compaction an agent had no if_version to quote | All in `internal/mcp`, additive. A MiniMax agent implemented the acceptance union and the per-item error; the coordinator added the top-level-project interception and reviewed. Nine tools unchanged |
| 21 | 2026-09-07 | §5, §6 | Tasks had estimate but no record of actual effort, no reviewer distinct from assignee, no way to append to a body, and per-task JSON omitted the estimate unit | Added `actual` (+ derived `estimate_error = actual − estimate`), `reviewer`, `body_append`, and `estimate_unit` on the per-task read. Migration `0003` adds the two columns | Same dogfood run: the estimate/actual gap is the calibration signal a research board most wants; a review column needs a reviewer nameable apart from the doer; a large body clipped at a client transport boundary needs chunked append; and `"estimate":200` with no unit is ambiguous | Frozen spine (domain fields, migration, service.go) by the coordinator; store/service/mcp wiring + tests by a MiniMax agent, reviewed and verified live under `-race`. `actual`/`reviewer` clear via JSON null like estimate/assignee; `body`+`body_append` together is a validation error; compact renders `act`/`rev` sparsely so the token budget is unmoved. HealthInfo.Migration reports 3 |
