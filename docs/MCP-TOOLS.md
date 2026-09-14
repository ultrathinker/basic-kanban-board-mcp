# MCP Tools Reference

basic-kanban-board-mcp exposes 13 native Model Context Protocol (MCP) tools over streamable-HTTP with mandatory bearer token authentication.

Every task write is a batch, every read defaults to a token-efficient compact text format, and every task carries an authoritative version for optimistic concurrency.

---

## Quick Reference: The 13 Tools

| Tool | Purpose |
|---|---|
| [`board_get`](#1-board_get) | Read the board. |
| [`task_next`](#2-task_next) | Find, claim or start the next ready task. |
| [`task_get`](#3-task_get) | Fetch whole tasks by key — this is the tool for full detail; task_next deliberately returns bounded summaries. |
| [`task_create`](#4-task_create) | Create one or more tasks in a single atomic batch (all-or-nothing). |
| [`task_update`](#5-task_update) | Update one or more tasks. |
| [`task_link`](#6-task_link) | Add or remove `blocks` dependencies, atomically. |
| [`task_claim`](#7-task_claim) | Claim, renew or release the lease on a single task with an atomic compare-and-swap. |
| [`task_remove`](#8-task_remove) | Archive (or restore) tasks. |
| [`project_upsert`](#9-project_upsert) | Create or update one project: identity, columns and settings. |
| [`project_post`](#10-project_post) | Post a live progress update to the project chat feed. |
| [`progress_set`](#11-progress_set) | Record a progress assessment and completion forecast for a task or an entire project. |
| [`progress_history`](#12-progress_history) | Read the full progress-mark history behind one metric: a project's manual estimate (project only) or one task's summary estimate (task). |
| [`board_guide`](#13-board_guide) | Read the operating guide for this kanban board: identity rules, the canonical read-modify-write loop, lease behaviour, the compact grammar version, and the error envelope. |

> **Tool count:** 13 tools on the full server (this file). 5 on `/mcp/readonly`: board_get, task_get, task_next with claim/start disabled, progress_history, board_guide. The count comes from the registry, not a constant.

---

## Core Protocol Principles

1. **Actor Identity Comes From the Bearer Token:**
   No tool accepts an `actor` parameter. Your identity is automatically derived from the name of the bearer token configured in your client. Every task creation, claim, lease renewal, note, and update event is permanently attributed to that token identity.

2. **The Agent Operating Loop:**
   - **Step 1:** Call `board_get(project: "KEY")` at session start. Use the default compact format; it costs ~1 064 tokens for 30 tasks (~90% fewer tokens than standard indented JSON).
   - **Step 2:** Call `task_next(project: "KEY", action: "start")` to claim the top ready task and transition it to the active column in a single round trip. Use `action: "peek"` if you want to inspect without claiming.
   - **Step 3:** While working, log incremental progress by calling `task_update` with only `key` and `note`. Notes are append-only, never bump `version`, and never conflict with concurrent teammates.
   - **Step 4:** When editing mutable fields (`title`, `body`, `type`, `priority`, `estimate`, `tags`, `assignee`, `column`, `rank`, `parent`, `acceptance`, `due_at`, `metadata`), always supply `if_version` set to the version returned by your last read.
   - **Step 5:** When finished, transition the task to Done: `task_update(patches: [{key: "KEY-1", column: "Done", if_version: N}])`.

   **Canonical loop — one task, zero wasted reads.** The next `if_version` always comes from the `version` field of your most recent response for that task, never from a fresh `task_get`:
   ```jsonc
   // 1. take the next ready task; data.tasks[0] is the started task, post-mutation
   task_next({ "project": "BMB", "action": "start" })
   //   -> data.tasks[0] = { "key": "BMB-14", "column": "Doing", "version": 6, ... }

   // 2. log progress — note never bumps the version, so you still hold v6
   task_update({ "patches": [{ "key": "BMB-14", "note": "scaffolding done" }] })

   // 3. finish — chain if_version from the version you last saw (6)
   task_update({ "patches": [{ "key": "BMB-14", "column": "Done", "if_version": 6 }] })
   //   -> data.items[0].task.version = 7   (chain your NEXT edit from 7)
   ```

3. **Authoritative Post-Write Versioning (`versionEchoRule`):**
   The `version` integer returned on every task object is the value **AFTER** the call completes, and it is **authoritative**. Chain your next `if_version` directly from this value without re-reading the task.

   Under `domain.BumpsTaskVersion`, appending a `note`, changing a lease (`task_claim`), or setting project `focus` **deliberately do not bump a task's version**. Therefore, if an update response echoes the identical version number you sent in `if_version`, the write succeeded and the version legitimately did not change. Do not issue a redundant `task_get`.

4. **Two Read Tool Output Shapes:**
   - `task_next` and `task_create` return a flat list: `data.tasks[]`.
   - `task_get` and `task_update` return a keyed list: `data.items[]`, where each element is `{ "key": string, "ok": bool, "task": {...}, "error": {...} }`. For `task_get`, any key that does not exist on the board is preserved in place with `ok: false` and a `not_found` error envelope.

5. **Envelope Structure:**
   All successful calls return:
   ```json
   {
     "ok": true,
     "op": "task_update",
     "data": { ... },
     "meta": {
       "count": 1,
       "warnings": []
     }
   }
   ```
   All domain failures return `ok: false` with `isError: true` on the tool response:
   ```json
   {
     "ok": false,
     "op": "task_update",
     "error": {
       "code": "conflict",
       "message": "version mismatch: you sent if_version=2, current is 3",
       "remediation": "The current server state is attached as `current` -- merge your change into it and retry with if_version=3. No re-read needed.",
       "current": { ... }
     }
   }
   ```

---

## Compact Board Grammar (`compact_version=1`)

The default output format for `board_get` is compact text (`format: "compact"`). The grammar is line-oriented, human-readable, and designed to fit within tight context windows.

### Measured Token Comparison (30 Active Tasks)

| Format | Measured Size | Tokens (`len/4`) | Savings vs Compact |
|---|---|---|---|
| **Compact text (`compact_version=1`)** | **~4 258 bytes** | **~1 064 tokens** | **Baseline** |
| Minified JSON (`Marshal`) | ~22 560 bytes | ~5 640 tokens | ~81% reduction |
| Indented JSON (`MarshalIndent`) | ~42 600 bytes | ~10 650 tokens | ~90% reduction |

### Grammar Rules

1. **Version header:** Line 1 must be `compact_version=1`.
2. **Project header:** `# KEY Name -- focus KEY|none -- Col1 count[/wip] -- Col2 count -- Done total (shown shown|hidden) -- v<version>`
   - Each non-done column is formatted as `Name count` or `Name count/wip` if a WIP limit is configured.
   - The done segment reports total archived/done tasks and whether any are currently rendered.
   - `v<version>` is the **project** configuration version and always comes last.
3. **Column header:** `## ColumnName` (the Done section is omitted when `done_limit=0`).
4. **Task line:** `- KEY [priority type] Title -- est <n><unit> -- @<assignee> -- lease <actor> <remaining>|expired -- sub <done>/<total> -- blocked-by <KEY>,<KEY> -- #tag #tag -- v<version> -- age <duration>`
   - `[priority type]`: If priority is `none`, only `[type]` is rendered. When non-zero, rendered as `[high bug]`, `[critical feat]`.
   - Title: internal whitespace is collapsed, newlines stripped, and literal ` -- ` replaced with ` - `.
   - Labeled suffixes appear in fixed order, separated by ` -- `, and are omitted when empty:
     - `est <n><unit>`: e.g. `est 2h`, `est 1.5d`. `<unit>` is the project's configured `estimate_unit`.
     - `@<assignee>`: assignee username or agent name.
     - `lease <actor> <remaining>|expired`: time remaining on lease (`43m`, `2h`, `45s`, `3d`) or `expired`.
     - `sub <done>/<total>`: subtasks completion status.
     - `blocked-by <KEY>,<KEY>`: comma-separated open blocker task keys.
     - `#tag #tag`: tags prefixed with `#`, sorted lexicographically.
     - `v<version>`: **Always present** on every task line. Enables direct read-modify-write without extra reads.
     - `age <duration>`: time elapsed since task entered the current column.

### Worked Example

```text
compact_version=1
# BMB BeeMemoryBank -- focus BMB-14 -- Doing 2/3 -- Review 1 -- Backlog 1 -- Done 40 (hidden) -- v9
## Doing
- BMB-14 [high bug] Fix WAL checkpoint race -- est 2h -- @alex -- lease claude@rog 43m -- sub 1/3 -- #sync -- v7 -- age 2h
- BMB-17 [medium feat] Encrypted FTS index -- est 8h -- lease codex@desk expired -- blocked-by BMB-14,BMB-9 -- v3 -- age 3d
## Review
- BMB-12 [task] Squash migrations 41-44 -- est 1h -- @alex -- v2 -- age 1d
## Backlog
- BMB-18 [critical bug] Sync loses tombstones -- #sync -- v1
```

---

## Tool Reference

### 1. `board_get`

Read the board. Compact text by default — about 1,000 tokens for 30 active tasks, roughly 90% smaller than the same board as indented JSON, so it is cheap enough to call at the start of every session. structuredContent is always full JSON.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `done_limit` | integer | Optional | how many done tasks to include, most recently done first |
| `filter` | any | Optional |  |
| `format` | string | Optional | compact = the token-cheap text grammar, json = pretty JSON text (structuredContent is always JSON either way) |
| `include` | any | Optional | widen the per-task fields returned |
| `project` | string | Optional | project key; omitted = every accessible project |
| `view` | string | Optional | tasks = full board, summary = counts only; default depends on whether project is set |

#### Example Call

```jsonc
board_get({ "done_limit": <value> })
```

---

### 2. `task_next`

Find, claim or start the next ready task. `peek` never takes anything (even when WIP is full); `claim` leases the top candidate without moving it; `start` leases it and moves it into the first active column atomically. Returns `data.tasks[]`: a flat list of task objects, best candidate first (task_get returns `data.items[]` instead, because it answers per requested key). `meta.reasons` counts why the rest were not offered.
This is a chooser, so it answers cheaply: bodies come back as a 256-byte excerpt ending in "… +N chars" and acceptance as the first 2 items with `acceptance_total` when there are more. `detail:"full"` widens that to 2048 bytes and 10 items; `meta.projection` always states which bounds were applied. Once you have chosen, task_get returns the whole card.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `action` | string | Optional | peek looks without taking, claim leases the top candidate, start leases and moves it into the first active column |
| `detail` | string | Optional | HOW MUCH of each included field: summary clips body and acceptance to an excerpt, full widens them. Both are bounded — call task_get for a whole card. |
| `include` | any | Optional | WHICH per-task fields come back. How much of each is a separate choice: see detail. |
| `limit` | integer | Optional | how many ready candidates to consider/return |
| `project` | string | Optional | project key; omitted = every accessible project |

#### Example Call

```jsonc
task_next({ "action": <value> })
```

---

### 3. `task_get`

Fetch whole tasks by key — this is the tool for full detail; task_next deliberately returns bounded summaries. Returns `data.items[]`: one `{key, ok, task|error}` entry per requested key, in request order — a key that does not exist is reported in place with ok:false, never silently dropped. (task_next and task_create return a flat `data.tasks[]` instead, because neither answers per requested key.)
Notes are opt-in via include and come newest-first, 20 at a time; when a task has older ones the result carries `notes_next_before` — send it back as `notes_before` to read the next page.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `include` | any | Optional | widen the per-task fields returned |
| `keys` | any | Required | task keys, case-insensitive |
| `notes_before` | string | Optional | RFC3339 cursor for paging back through notes: returns the notes created strictly before it, newest first. Requires "notes" in include; take the value from a previous result's notes_next_before. |

#### Example Call

```jsonc
task_get({ "keys": <value> })
```

---

### 4. `task_create`

Create one or more tasks in a single atomic batch (all-or-nothing). To link items of the same batch, give one a `ref` and point at it from another by prefixing that name with a single @: an item created as `{"ref": "scaffold", ...}` is referenced as `"blocked_by": ["@scaffold"]`. Returns `data.tasks[]`: the created tasks, in request order, each carrying its assigned `key` and `version`.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `project` | any | Optional | do NOT set this: project is a per-item field — put it inside each element of tasks[] |
| `tasks` | any | Required | all-or-nothing: either every task is created, or none are |

#### Example Call

```jsonc
task_create({ "tasks": <value> })
```

---

### 5. `task_update`

Update one or more tasks. Per-item results by default (atomic:false); set atomic:true to make the whole batch commit or none of it does. if_version is required for any replacement-style field. Returns `data.items[]`: one `{key, ok, task|error}` entry per patch, in request order. The `version` on every returned task is the value AFTER the call and is authoritative — chain your next `if_version` from it and never re-read a task just to learn its version. `note`, lease operations and `focus` do not move a task's version by design, so a result echoing the same version you sent means the write landed and the version legitimately did not change.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `atomic` | boolean | Optional | false (default) = per-item results; true = the whole batch commits or none of it does |
| `patches` | any | Required |  |

#### Example Call

```jsonc
task_update({ "patches": <value> })
```

---

### 6. `task_link`

Add or remove `blocks` dependencies, atomically. Rejects self-links and cycles (including through parent chains).

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `add` | any | Optional |  |
| `remove` | any | Optional |  |

#### Example Call

```jsonc
task_link({ "add": <value> })
```

---

### 7. `task_claim`

Claim, renew or release the lease on a single task with an atomic compare-and-swap. Same actor never needs force; an expired former owner cannot renew after another actor won.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `action` | string | Required | claim = take a free/expired lease, renew = extend your own, release = give it up |
| `force` | boolean | Optional | admin scope only: steal a live lease held by someone else |
| `key` | string | Required | task key, case-insensitive |
| `ttl_seconds` | integer | Optional | lease duration in seconds; clamped to the supported range; 0 = project default |

#### Example Call

```jsonc
task_claim({ "action": <value>, "key": <value> })
```

---

### 8. `task_remove`

Archive (or restore) tasks. Archiving clears claim and focus and keeps links (hidden while archived). Hard delete is never available over MCP — use `kanban task purge` (admin CLI).

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `cascade_subtasks` | any | Optional | also archive subtasks of an archived parent |
| `items` | any | Required |  |
| `restore` | boolean | Optional | restore instead of archive |

#### Example Call

```jsonc
task_remove({ "items": <value> })
```

---

### 9. `project_upsert`

Create or update one project: identity, columns and settings. `mode` is REQUIRED and has no default — despite the name this tool never guesses create-vs-update, because a typo in a project key must not silently fork the board into a second project.
Create: {"mode":"create","key":"TEST","name":"Smoke Test"} — `name` is required; omitting `columns` gives the default set (Backlog, Doing (WIP 3), Done).
Update: {"mode":"update","key":"TEST","if_version":3,"name":"New name"} — `if_version` is required and is the version your last read of the project returned.
Not sure which one applies? Call board_get with no `project` first: every project you can reach comes back with its key and version.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `archived` | any | Optional |  |
| `columns` | any | Optional | the full desired column list, in order, when provided |
| `description` | any | Optional |  |
| `description_append` | any | Optional | append this text to the description instead of replacing it; mutually exclusive with description |
| `if_version` | any | Optional | required when mode:update — the project version your last read returned |
| `key` | string | Required | project key, case-insensitive |
| `mode` | string | Required | REQUIRED, no default. "create" makes a new project (name is then required too); "update" edits an existing one (if_version is then required too). There is deliberately no upsert-by-guess: a mistyped key would silently fork the board into a second project. |
| `name` | string | Optional | display name; required when mode:create |
| `remove_columns` | any | Optional | required for every existing column absent from columns |
| `settings` | any | Optional |  |

#### Example Call

```jsonc
project_upsert({ "key": <value>, "mode": <value> })
```

---

### 10. `project_post`

Post a live progress update to the project chat feed. This is a real-time broadcast for the human watching the board right now, not a post-mortem or final report for posterity. Post short messages periodically during your work (approximately every 5 minutes), not just once at the end: share what is being done now, what is planned next, and what went wrong or surprised you. Reference tasks by their key directly in the text (e.g. KANB-7) — they become clickable links on the board. Returns `data`: the posted message with its id, author, body and timestamp.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `author` | string | Required | agent identity / display name (e.g. Claude, Codex, Zoë) |
| `body` | string | Required | message text; mention task keys like KANB-7 for clickable links |
| `project` | string | Required | project key, case-insensitive |

#### Example Call

```jsonc
project_post({ "author": <value>, "body": <value>, "project": <value> })
```

---

### 11. `progress_set`

Record a progress assessment and completion forecast for a task or an entire project. Provide periodic assessments as work proceeds (e.g. every few steps or significant discoveries), not just at the start or finish. Progress marks form an append-only calibration history: revisions never overwrite prior marks. Overestimating and underestimating are expected and harmless; rolling your progress estimate backward (e.g. from 70% down to 45%) is a normal and valuable signal reflecting discovered complexity, not an admission of defeat. Specify exactly one target: either `task` (e.g. KANB-3) to assess a specific task, or `project` (e.g. KANB) to assess the project as a whole. `eta` is an RFC3339 timestamp forecasting the expected finish date and time (e.g. 2026-09-12T18:00:00Z), not a remaining duration. Returns `data` containing the recorded mark and the updated summary progress for the assessed scope.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `assessor` | string | Required | agent self-declared name / identity (free-form text) |
| `eta` | any | Optional | RFC3339 timestamp forecasting target completion date and time (not duration remaining) |
| `percent` | integer | Required | progress percentage, integer 0..100 |
| `project` | any | Optional | project key; exactly one of task or project must be provided |
| `task` | any | Optional | task key (e.g. KANB-3); exactly one of task or project must be provided |

#### Example Call

```jsonc
progress_set({ "assessor": <value>, "percent": <value> })
```

---

### 12. `progress_history`

Read the full progress-mark history behind one metric: a project's manual estimate (project only) or one task's summary estimate (task). This is the same append-only calibration history progress_set writes — nothing here is averaged, rounded or decimated, so a revision like 70% rolled back to 45% is visible exactly as it happened, in the order it happened. Specify exactly one target: either `task` (e.g. KANB-3) to read one task's history, or `project` (e.g. KANB) to read the project's own manual history. Returns `data.marks[]` in chronological order (oldest first) — each with its assessor, percent, timestamp and eta forecast if one was given — plus `data.total`, the full mark count before the `limit` cap.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `limit` | integer | Optional | how many of the most recent marks to return; older marks beyond this cap are simply not included. Defaults to the maximum. |
| `project` | any | Optional | project key; exactly one of task or project must be provided |
| `task` | any | Optional | task key (e.g. KANB-3); exactly one of task or project must be provided |

#### Example Call

```jsonc
progress_history({ "limit": <value> })
```

---

### 13. `board_guide`

Read the operating guide for this kanban board: identity rules, the canonical read-modify-write loop, lease behaviour, the compact grammar version, and the error envelope. Without a `project`, returns the common guide plus the list of tools registered on this server (their count is computed, not a constant — adding a tool appears here automatically). With `project`, appends that project's settings (estimate unit, claim TTL, strict_done, enforce_dependencies, archive state) so an agent can read-modify-write against the project's own rules without guessing. This tool is read-only and exists on every server, including /mcp/readonly. Returns `data` with `guide`, `tool_count`, `tools[]`, and (optionally) `project`.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `project` | any | Optional | optional project key; when present, the response also carries that project's estimate unit, claim TTL, strict_done and enforce_dependencies settings |

#### Example Call

```jsonc
board_guide({ "project": <value> })
```

---

## Error Handling & Domain Codes

All failures return `{ok: false, error: {code, message, remediation}}` with `isError` set on the tool response. `remediation` names the concrete next action.

### Error Codes (`domain.Code`)

| Code | Meaning | Typical Remediation |
|---|---|---|
| `not_found` | The target key does not exist on a board the actor can see. | Re-list with board_get and retry with the correct key. |
| `validation` | An input field failed validation (type, format, length, enum). | See the `message`; the named field is the offender. |
| `conflict` | `if_version` does not match the current version, or another writer raced. | Merge your change into `error.current` and retry with `if_version=N+1`. |
| `blocked` | An open `blocks` dependency prevents the operation. | Finish or remove the open blocker and retry. |
| `wip_exceeded` | The destination column has no WIP slot. | Finish another task in that column, or wait for a lease to expire. |
| `claimed` | Another actor holds a live lease. | Wait for the lease to expire, or use `task_claim force: true` if you are admin. |
| `forbidden` | Token lacks the required scope, project access, or admin role. | Use a token with the right scope or project_keys. |
| `cycle` | The proposed link or parent chain would create a cycle. | Restructure the chain so it is acyclic. |
| `rate_limited` | The token exceeded its per-minute budget. | Slow down; the budget resets every minute. |
| `payload_too_large` | The request body exceeded 1 MiB. | Split the batch; the cap is `MaxRequestBodyBytes`. |
| `idempotency_mismatch` | The `idempotency_key` was reused with a different request body. | Use a fresh `idempotency_key`. |
