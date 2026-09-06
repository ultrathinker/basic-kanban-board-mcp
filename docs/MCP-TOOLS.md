# MCP Tools Reference

basic-kanban-board-mcp provides nine native Model Context Protocol (MCP) tools over streamable-HTTP with mandatory bearer token authentication.

Every task write is a batch, every read defaults to a token-efficient compact text format, and every task carries an authoritative version for optimistic concurrency.

---

## Quick Reference: The Nine Tools

| Tool | Purpose | Output Shape | Write Type |
|---|---|---|---|
| [`board_get`](#1-board_get) | Read full board or project summary (compact text default) | Compact text / JSON (`data.projects[]`) | Read-only |
| [`task_next`](#2-task_next) | Peek, claim, or start the highest-priority ready task | `data.tasks[]` | Read / Mutation |
| [`task_get`](#3-task_get) | Fetch tasks by key in request order (preserves missing keys) | `data.items[]` (`{key, ok, task}`) | Read-only |
| [`task_create`](#4-task_create) | Create tasks atomically (all-or-nothing) with `@<ref>` links | `data.tasks[]` | Atomic batch |
| [`task_update`](#5-task_update) | Update tasks with optimistic concurrency (`if_version`) | `data.items[]` (`{key, ok, task}`) | Per-item batch (atomic opt-in) |
| [`task_link`](#6-task_link) | Add or remove `blocks` dependency edges atomically | `data.tasks[]` | Atomic |
| [`task_claim`](#7-task_claim) | Claim, renew, or release a task lease (atomic CAS) | `data.task` | Single task |
| [`task_remove`](#8-task_remove) | Archive or restore tasks (soft delete only) | `data.items[]` (`{key, ok, task}`) | Per-item batch |
| [`project_upsert`](#9-project_upsert) | Create or update project columns and configuration | `data` (`projectOut`) | Single project |

> **Hard Rule:** There are nine tools. No tenth tool exists or will be added. Additional capabilities are expressed via parameters.

---

## Core Protocol Principles

1. **Actor Identity Comes From the Bearer Token:**
   No tool accepts an `actor` parameter. Your identity is automatically derived from the name of the bearer token configured in your client. Every task creation, claim, lease renewal, note, and update event is permanently attributed to that token identity.

2. **The Agent Operating Loop:**
   - **Step 1:** Call `board_get(project: "KEY")` at session start. Use the default compact format; it costs ~1 060 tokens for 30 tasks (~90% fewer tokens than standard indented JSON).
   - **Step 2:** Call `task_next(project: "KEY", action: "start")` to claim the top ready task and transition it to the active column in a single round trip. Use `action: "peek"` if you want to inspect without claiming.
   - **Step 3:** While working, log incremental progress by calling `task_update` with only `key` and `note`. Notes are append-only, never bump `version`, and never conflict with concurrent teammates.
   - **Step 4:** When editing mutable fields (`title`, `body`, `type`, `priority`, `estimate`, `tags`, `assignee`, `column`, `rank`, `parent`, `acceptance`, `due_at`, `metadata`), always supply `if_version` set to the version returned by your last read.
   - **Step 5:** When finished, transition the task to Done: `task_update(patches: [{key: "KEY-1", column: "Done", if_version: N}])`.

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
       "remediation": "The current server state is attached as `current` — merge your change into it and retry with if_version=3. No re-read needed.",
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
2. **Project header:** `# KEY Name · focus KEY|none · Col1 count[/wip] · Col2 count · Done total (shown shown|hidden) · v<version>`
   - Each non-done column is formatted as `Name count` or `Name count/wip` if a WIP limit is configured.
   - The done segment reports total archived/done tasks and whether any are currently rendered.
   - `v<version>` is the **project** configuration version and always comes last. Send it back as
     `project_upsert`'s `if_version`: this is what lets the default compact read feed the write that
     follows it, with no second call. It is the only segment the header gained since the grammar was written.
3. **Column header:** `## ColumnName` (the Done section is omitted when `done_limit=0`).
4. **Task line:** `- KEY [priority type] Title · est <n><unit> · @<assignee> · lease <actor> <remaining>|expired · sub <done>/<total> · blocked-by <KEY>,<KEY> · #tag #tag · v<version> · age <duration>`
   - `[priority type]`: If priority is `none`, only `[type]` is rendered (e.g. `[task]`). When non-zero, rendered as `[high bug]`, `[critical feat]`.
   - Title: internal whitespace is collapsed, newlines stripped, and literal ` · ` replaced with ` - `.
   - Labeled suffixes appear in fixed order, separated by ` · `, and are omitted when empty:
     - `est <n><unit>`: e.g. `est 2h`, `est 1.5d`. `<unit>` is the project's configured `estimate_unit`.
     - `@<assignee>`: assignee username or agent name.
     - `lease <actor> <remaining>|expired`: time remaining on lease (`43m`, `2h`, `45s`, `3d`) or `expired`. Never elapsed time.
     - `sub <done>/<total>`: subtasks completion status.
     - `blocked-by <KEY>,<KEY>`: comma-separated open blocker task keys.
     - `#tag #tag`: tags prefixed with `#`, sorted lexicographically, separated by spaces.
     - `v<version>`: **Always present** on every task line. Enables direct read-modify-write without extra reads.
     - `age <duration>`: time elapsed since task entered the current column (rendered for active columns only).

### Worked Example

```text
compact_version=1
# BMB BeeMemoryBank · focus BMB-14 · Doing 2/3 · Review 1 · Backlog 1 · Done 40 (hidden) · v9
## Doing
- BMB-14 [high bug] Fix WAL checkpoint race · est 2h · @alex · lease claude@rog 43m · sub 1/3 · #sync · v7 · age 2h
- BMB-17 [medium feat] Encrypted FTS index · est 8h · lease codex@desk expired · blocked-by BMB-14,BMB-9 · v3 · age 3d
## Review
- BMB-12 [task] Squash migrations 41-44 · est 1h · @alex · v2 · age 1d
## Backlog
- BMB-18 [critical bug] Sync loses tombstones · #sync · v1
```

---

## Tool Reference

### 1. `board_get`

Reads the board. Emits compact text by default; `structuredContent` always contains full JSON.

#### Parameters

| Name | Type | Required | Default | Bounds / Enum | Description |
|---|---|---|---|---|---|
| `project` | string | Optional | `""` | 2–8 chars | Project key (e.g. `"BMB"`). When omitted, returns summary of all accessible projects. |
| `view` | string | Optional | `"tasks"` (or `"summary"`) | `"tasks"`, `"summary"` | `"tasks"` returns column tasks; `"summary"` returns counts only. Defaults to `"summary"` if `project` is omitted. |
| `done_limit` | integer | Optional | `0` | `0`–`200` | Max number of completed tasks to include (most recently done first). |
| `filter` | object | Optional | `null` | — | Filter criteria applied to tasks. |
| `filter.columns` | string[] | Optional | `null` | — | Column names to include (OR match). |
| `filter.types` | string[] | Optional | `null` | `task`, `bug`, `feat`, `chore`, `doc`, `perf`, `research` | Types to include (OR match). |
| `filter.priority_min` | string | Optional | `null` | `none`, `low`, `medium`, `high`, `critical` | Inclusive minimum priority threshold. |
| `filter.tags` | string[] | Optional | `null` | Max 20 tags, ≤40 chars | Tags to match (OR match). |
| `filter.assignee` | string | Optional | `null` | ≤80 chars | Exact assignee match. |
| `filter.claimed` | string | Optional | `null` | `"any"`, `"mine"`, `"unclaimed"`, `"other"` | Lease status filter. |
| `filter.blocked` | boolean | Optional | `null` | — | `true` for tasks with open blockers; `false` for tasks with none. |
| `filter.q` | string | Optional | `null` | — | Substring search matching title or body. |
| `filter.updated_since`| string | Optional | `null` | RFC3339 | Only tasks updated on or after timestamp. |
| `include` | string[] | Optional | `[]` | `"body"`, `"acceptance"`, `"notes"`, `"links"`, `"metadata"` | Additional fields to expand on each task in JSON output. Selected fields come back whole — `board_get` has no bounded tier. |
| `format` | string | Optional | `"compact"` | `"compact"`, `"json"` | `"compact"` renders compact text grammar; `"json"` returns indented JSON text. |

#### Project fields

Each entry in `data.projects[]` carries the project's whole configuration, not just its identity:
`version`, `estimate_unit`, `enforce_dependencies`, `strict_done`, `claim_ttl_seconds`, `archived`,
`description`, and the full ordered `columns[]` including columns holding no tasks. This is the
published source of `project_upsert`'s `if_version` — read the board, modify what you read, send it
back. There is no separate project-read tool and none is needed.

#### Example Call & Response

```json
// Call
{
  "project": "BMB",
  "done_limit": 0,
  "format": "compact"
}

// Response Content (Text)
compact_version=1
# BMB BeeMemoryBank · focus BMB-14 · Doing 1/3 · Backlog 2 · Done 10 (hidden) · v9
## Doing
- BMB-14 [high bug] Fix WAL checkpoint race · est 2h · @alex · lease claude@rog 43m · v7 · age 2h
## Backlog
- BMB-18 [critical bug] Sync loses tombstones · #sync · v1
- BMB-19 [task] Update dependencies · v1

// Response structuredContent
{
  "ok": true,
  "op": "board_get",
  "data": {
    "projects": [
      {
        "key": "BMB",
        "name": "BeeMemoryBank",
        "version": 9,
        "focus_key": "BMB-14",
        "estimate_unit": "h",
        "enforce_dependencies": true,
        "strict_done": false,
        "claim_ttl_seconds": 3600,
        "done_total": 10,
        "done_shown": 0,
        "columns": [
          {
            "name": "Doing",
            "kind": "active",
            "wip_limit": 3,
            "count": 1,
            "tasks": [
              {
                "key": "BMB-14",
                "project": "BMB",
                "column": "Doing",
                "column_kind": "active",
                "type": "bug",
                "priority": "high",
                "title": "Fix WAL checkpoint race",
                "estimate": 2,
                "assignee": "alex",
                "claimed_by": "claude@rog",
                "version": 7,
                "ready": false,
                "created_at": "2026-09-05T12:00:00Z",
                "updated_at": "2026-09-06T10:00:00Z",
                "created_by": "alex",
                "updated_by": "claude@rog"
              }
            ]
          }
        ]
      }
    ]
  },
  "meta": { "count": 3 }
}
```

---

### 2. `task_next`

Finds, claims, or starts the next runnable backlog task based on priority, due date, and dependencies.

#### Selection Algorithm
Candidates must be unarchived tasks in `backlog` columns of accessible projects. A candidate is ready only if:
1. It is a **runnable leaf**: it has no incomplete subtasks (`sub_done == sub_total`).
2. It has no open `blocks` incoming links (`len(blocked_by) == 0`).
3. Its parent task has no open blockers.
4. Its lease is either unassigned, expired, or already held by the calling actor.

Candidates are ranked by: `priority desc` → `due_at asc (nulls last)` → `rank asc` → `created_at asc` → `key asc`.

#### Parameters

| Name | Type | Required | Default | Bounds / Enum | Description |
|---|---|---|---|---|---|
| `project` | string | Optional | `""` | 2–8 chars | Project key. When omitted, inspects all accessible projects. |
| `action` | string | Optional | `"peek"` | `"peek"`, `"claim"`, `"start"` | Action to perform. |
| `limit` | integer | Optional | `3` | `1`–`10` | Number of candidate tasks to return. |
| `include` | string[] | Optional | `["body", "acceptance"]` | `"body"`, `"acceptance"`, `"notes"`, `"links"` | **Which** fields come back on each task. |
| `detail` | string | Optional | `"summary"` | `"summary"`, `"full"` | **How much** of each included field comes back. See below. |

#### Response projection (`include` vs `detail`)

`task_next` chooses work; it is not a card reader. Selecting a field and deciding how much of it to
return are separate choices, and the response says which bounds were applied:

| `detail` | `body` | `acceptance` |
|---|---|---|
| `"summary"` (default) | first 256 bytes, then `… +N chars` | first 2 items, plus `acceptance_total` when there are more |
| `"full"` | first 2 048 bytes, then `… +N chars` | first 10 items, plus `acceptance_total` |

Neither tier returns an unbounded body: for a whole card, call `task_get`. `meta.projection` reports
the bounds actually applied (`{"body":"bounded","body_limit_bytes":256,"acceptance":"bounded","acceptance_limit":2}`),
so a short body is never ambiguous between "the task is short" and "the answer was clipped".

Measured on ten worst-case candidates (2 KB bodies, 50 criteria each): ~2 930 tokens at `"summary"`
against ~19 773 for the same call before bounded projections existed. The default 3-candidate answer
is ~949 tokens — under the ~1 064 it costs to read the entire 30-task board.

#### Actions
- `peek`: Returns up to `limit` ready tasks. Succeeds even if WIP in active columns is full (`meta.wip_full: true`). Does not mutate.
- `claim`: Atomically leases the top candidate without changing its column.
- `start`: In a single atomic transaction, checks active column WIP, leases `tasks[0]`, moves it to the first active column (e.g. `Doing`), and records `started_at`. Fails with `wip_exceeded` if the column is full.

#### Example Call & Response

```json
// Call
{
  "project": "BMB",
  "action": "start",
  "limit": 1
}

// Response structuredContent
{
  "ok": true,
  "op": "task_next",
  "data": {
    "tasks": [
      {
        "key": "BMB-18",
        "project": "BMB",
        "column": "Doing",
        "column_kind": "active",
        "type": "bug",
        "priority": "critical",
        "title": "Sync loses tombstones",
        "claimed_by": "claude@rog",
        "claim_expires_at": "2026-09-06T19:05:00Z",
        "lease_remaining_seconds": 3600,
        "version": 2,
        "ready": true,
        "tags": ["sync"],
        "body": "When reconnecting after a network partition, tombstone records are dropped… +1804 chars",
        "acceptance": [
          { "text": "Tombstones survive a reconnect", "done": false },
          { "text": "Replay is idempotent", "done": false }
        ],
        "acceptance_total": 9,
        "created_at": "2026-09-06T08:00:00Z",
        "updated_at": "2026-09-06T18:05:00Z",
        "created_by": "alex",
        "updated_by": "claude@rog"
      }
    ]
  },
  "meta": {
    "count": 1,
    "started_key": "BMB-18",
    "claimed_key": "BMB-18",
    "wip_full": false,
    "projection": {
      "body": "bounded",
      "body_limit_bytes": 256,
      "acceptance": "bounded",
      "acceptance_limit": 2
    },
    "reasons": {
      "blocked_dependency": 2,
      "wip_full": 0,
      "claimed_by_other": 1,
      "parent_incomplete": 0,
      "not_leaf": 1
    },
    "blocked_top": [
      { "key": "BMB-17", "blocked_by": ["BMB-14", "BMB-9"] }
    ]
  }
}
```

---

### 3. `task_get`

Fetches tasks by key. Returns `data.items[]` in exact request order.

#### Parameters

| Name | Type | Required | Default | Bounds / Enum | Description |
|---|---|---|---|---|---|
| `keys` | string[] | **Required** | — | 1–50 items | List of task keys (case-insensitive, e.g. `["bmb-14", "BMB-18"]`). |
| `include` | string[] | Optional | `["body", "acceptance", "links"]` | `"body"`, `"acceptance"`, `"notes"`, `"links"`, `"metadata"` | Fields to expand. Every selected field comes back whole — this is the full-detail read. `notes` returns up to 20 most recent notes. |
| `notes_before` | string | Optional | `null` | RFC3339 | Cursor for the next, older page of notes: returns only notes created strictly before it. Requires `"notes"` in `include` — passing it without is refused, not ignored. |

#### Behavior
Missing or malformed keys are never dropped. They are reported in place with `ok: false` and a `not_found` error envelope.

#### Paging notes
Notes come newest-first, 20 per page. A task with older notes carries `notes_next_before` on its
`task` object; send that value back as `notes_before` to fetch the next page, and repeat until the
field is absent. The cursor is per task, because each task's history ends at a different point —
in a multi-key call, use the cursor from the task you are paging.

#### Example Call & Response

```json
// Call
{
  "keys": ["BMB-14", "BMB-999"]
}

// Response structuredContent
{
  "ok": true,
  "op": "task_get",
  "data": {
    "items": [
      {
        "key": "BMB-14",
        "ok": true,
        "task": {
          "key": "BMB-14",
          "project": "BMB",
          "column": "Doing",
          "column_kind": "active",
          "type": "bug",
          "priority": "high",
          "title": "Fix WAL checkpoint race",
          "version": 7,
          "ready": false,
          "body": "Detailed investigation into checkpoint locks...",
          "acceptance": [
            { "text": "Reproduction test case in Go", "done": true },
            { "text": "Zero lock contention under benchmark", "done": false }
          ]
        }
      },
      {
        "key": "BMB-999",
        "ok": false,
        "error": {
          "code": "not_found",
          "message": "task \"BMB-999\" not found",
          "remediation": "Check the key (case-insensitive, form PROJ-N) or call board_get to list what exists."
        }
      }
    ]
  },
  "meta": {
    "count": 2,
    "not_found": ["BMB-999"]
  }
}
```

---

### 4. `task_create`

Creates one or more tasks in an atomic, all-or-nothing batch (1–100 items).

#### Symbolic Reference (`@<ref>`) Syntax
Within a single batch, items can reference each other before database keys exist.
- Define `ref: "name"` on a task.
- Point to it in `blocked_by` or `parent` using `@<ref>` (e.g. `blocked_by: ["@name"]`).
- Literal example:
  ```json
  "tasks": [
    { "project": "BMB", "ref": "scaffold", "title": "Scaffold project" },
    { "project": "BMB", "title": "Build parser", "blocked_by": ["@scaffold"] }
  ]
  ```

#### Parameters

| Name | Type | Required | Default | Bounds / Limits | Description |
|---|---|---|---|---|---|
| `tasks` | object[] | **Required** | — | 1–100 items | Array of task definitions. All commit or none commit. |
| `tasks[].project` | string | **Required** | — | 2–8 chars | Project key. |
| `tasks[].title` | string | **Required** | — | 1–200 chars | Short imperative summary (newlines stripped). |
| `tasks[].body` | string | Optional | `""` | ≤64 KB | Markdown description. |
| `tasks[].type` | string | Optional | `"task"` | `task`, `bug`, `feat`, `chore`, `doc`, `perf`, `research` | Task type. |
| `tasks[].priority` | string | Optional | `"none"` | `none`, `low`, `medium`, `high`, `critical` | Priority level. |
| `tasks[].estimate` | float | Optional | `null` | > 0 | Estimated effort in project unit. |
| `tasks[].tags` | string[] | Optional | `[]` | ≤20 tags, ≤40 chars | Lowercase labels (no spaces, no `#`). |
| `tasks[].assignee` | string | Optional | `null` | ≤80 chars | Free text assignee name. |
| `tasks[].column` | string | Optional | First backlog | ≤40 chars | Destination column name. |
| `tasks[].parent` | string | Optional | `null` | Existing key or `@<ref>` | Parent task key (subtask depth limit 2). |
| `tasks[].blocked_by` | string[] | Optional | `[]` | Existing keys or `@<ref>` | Open blocker dependencies. |
| `tasks[].acceptance` | string[] | Optional | `[]` | ≤50 items, ≤500 chars | Checklist criteria (starts unchecked). |
| `tasks[].due_at` | string | Optional | `null` | RFC3339 | Due date timestamp. |
| `tasks[].metadata` | object | Optional | `null` | ≤16 KB | Arbitrary JSON key-value store. |
| `tasks[].ref` | string | Optional | `""` | — | Symbolic identifier for intra-batch links. |
| `tasks[].idempotency_key` | string | Optional | `""` | — | Deduplication key valid for 24 hours. |

#### Example Call & Response

```json
// Call
{
  "tasks": [
    {
      "project": "BMB",
      "ref": "db-schema",
      "title": "Define database schema",
      "type": "chore",
      "priority": "high"
    },
    {
      "project": "BMB",
      "title": "Implement query methods",
      "type": "feat",
      "priority": "medium",
      "blocked_by": ["@db-schema"]
    }
  ]
}

// Response structuredContent
{
  "ok": true,
  "op": "task_create",
  "data": {
    "tasks": [
      {
        "key": "BMB-20",
        "project": "BMB",
        "column": "Backlog",
        "column_kind": "backlog",
        "type": "chore",
        "priority": "high",
        "title": "Define database schema",
        "version": 1,
        "ready": true
      },
      {
        "key": "BMB-21",
        "project": "BMB",
        "column": "Backlog",
        "column_kind": "backlog",
        "type": "feat",
        "priority": "medium",
        "title": "Implement query methods",
        "version": 1,
        "ready": false,
        "blocked_by": ["BMB-20"]
      }
    ]
  },
  "meta": { "count": 2, "replayed": false }
}
```

---

### 5. `task_update`

Updates one or more tasks. Results are reported per-item by default (`atomic: false`). Setting `atomic: true` commits the entire batch or none of it.

#### Optimistic Concurrency (`if_version`)
`if_version` is **required** for any replacement-style mutation (`title`, `body`, `type`, `priority`, `estimate`, `tags`, `assignee`, `column`, `rank`, `parent`, `acceptance`, `due_at`, `metadata_merge`).
- If another process updated the task, the server returns `error.code = "conflict"`.
- The current server task is attached directly under `error.current`.
- **Remediation:** Merge your patch into it and retry immediately using `if_version = current.version`. No follow-up read is needed.

#### Non-version-bumping Mutations
Appending a `note`, lease actions, or toggling `focus` do **not** increment a task's `version`. If you send `if_version: 3` alongside only a `note`, the response will report `version: 3`. This confirms the write landed.

#### Three-State Fields (Set / Clear / Leave Alone)
For `estimate`, `assignee`, `due_at`, and `parent`:
- Omit the field: leaves the existing value untouched.
- Provide a value: updates the field.
- Provide JSON `null`: clears the field to empty/null.

#### Parameters

| Name | Type | Required | Default | Bounds / Limits | Description |
|---|---|---|---|---|---|
| `patches` | object[] | **Required** | — | 1–100 items | Array of task patch objects. |
| `atomic` | boolean | Optional | `false` | — | If `true`, fails entire batch on any error. |
| `patches[].key` | string | **Required** | — | Case-insensitive | Target task key (e.g. `"BMB-14"`). |
| `patches[].if_version`| integer | Cond. | `null` | ≥ 1 | Required for replacement fields. |
| `patches[].title` | string | Optional | — | 1–200 chars | Replaces title. |
| `patches[].body` | string | Optional | — | ≤64 KB | Replaces body markdown. |
| `patches[].type` | string | Optional | — | 7 enum types | Replaces type. |
| `patches[].priority` | string | Optional | — | 5 enum names | Replaces priority. |
| `patches[].estimate` | float | Optional | — | Value or `null` | Replaces estimate; `null` clears. |
| `patches[].assignee` | string | Optional | — | Value or `null` | Replaces assignee; `null` clears. |
| `patches[].due_at` | string | Optional | — | RFC3339 or `null` | Replaces due date; `null` clears. |
| `patches[].tags` | string[] | Optional | — | Mutually exclusive with `tags_add`/`tags_remove` | Replaces entire tag list. |
| `patches[].tags_add` | string[] | Optional | — | ≤20 tags | Appends tags without replacing existing. |
| `patches[].tags_remove`| string[] | Optional | — | ≤20 tags | Removes specified tags. |
| `patches[].column` | string | Optional | — | Destination col | Moves task. Enforces dependencies, WIP, and strict_done. |
| `patches[].rank` | string | Optional | — | `"top"`, `"bottom"` | Repositions card within column. |
| `patches[].parent` | string | Optional | — | Key or `null` | Reparents task; `null` makes it top-level. |
| `patches[].acceptance` | object[] | Optional | — | Mutually exclusive with check/add | Replaces full checklist `[{text, done}]`. |
| `patches[].acceptance_check`| int[]| Optional | — | 0-indexed indices | Ticks items as done. Requires `if_version`. |
| `patches[].acceptance_add`| string[]| Optional | — | ≤50 items | Appends new checklist items. |
| `patches[].note` | string | Optional | — | ≤16 KB | Appends an audit note. Does not bump version. |
| `patches[].focus` | boolean | Optional | — | — | `true` sets project focus; `false` clears if currently focused. |
| `patches[].metadata_merge`| object| Optional | — | ≤16 KB | Shallow merges keys. Setting a key to `null` deletes it. |
| `patches[].force` | boolean | Optional | `false` | Admin scope only | Bypasses `blocked`, `wip_exceeded`, or `strict_done`. |
| `patches[].reason` | string | Optional | `""` | Required if `force: true` | Audit reason recorded in activity log. |

#### Example Call & Response

```json
// Call
{
  "patches": [
    {
      "key": "BMB-14",
      "if_version": 7,
      "column": "Review",
      "note": "PR #42 opened with the checkpoint fix."
    }
  ]
}

// Response structuredContent
{
  "ok": true,
  "op": "task_update",
  "data": {
    "items": [
      {
        "key": "BMB-14",
        "ok": true,
        "task": {
          "key": "BMB-14",
          "project": "BMB",
          "column": "Review",
          "column_kind": "active",
          "type": "bug",
          "priority": "high",
          "title": "Fix WAL checkpoint race",
          "version": 8,
          "ready": false
        }
      }
    ]
  },
  "meta": { "count": 1, "warnings": [] }
}
```

---

### 6. `task_link`

Adds or removes `blocks` dependency edges atomically.

#### Constraints
- Self-links (`blocker == blocked`) are rejected.
- Cycles (including chains through subtask parent trees) are rejected with `code: "cycle"`.
- Bumps the `version` of **both** endpoint tasks, because their readiness state has changed.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `add` | object[] | Optional | Array of `{ "blocker": "KEY-A", "blocked": "KEY-B" }`. |
| `remove` | object[] | Optional | Array of `{ "blocker": "KEY-A", "blocked": "KEY-B" }`. |

#### Example Call & Response

```json
// Call
{
  "add": [
    { "blocker": "BMB-12", "blocked": "BMB-18" }
  ]
}

// Response structuredContent
{
  "ok": true,
  "op": "task_link",
  "data": {
    "tasks": [
      { "key": "BMB-12", "blocks": ["BMB-18"], "version": 3 },
      { "key": "BMB-18", "blocked_by": ["BMB-12"], "ready": false, "version": 2 }
    ]
  },
  "meta": { "count": 2 }
}
```

---

### 7. `task_claim`

Manages single-task lease ownership using atomic compare-and-swap (CAS).

#### Lease Rules
- Does **not** increment task `version`.
- A caller can always renew or release their own lease without `force`.
- An expired former owner cannot renew if another actor has claimed the task.
- `force: true` allows an admin to steal an active lease held by another actor.

#### Parameters

| Name | Type | Required | Default | Bounds / Enum | Description |
|---|---|---|---|---|---|
| `key` | string | **Required** | — | Case-insensitive | Task key. |
| `action` | string | **Required** | — | `"claim"`, `"renew"`, `"release"` | `"claim"` takes a free/expired lease; `"renew"` extends yours; `"release"` gives it up. |
| `ttl_seconds` | integer | Optional | `0` (project default) | 60–86400 (1m–24h) | Requested lease duration. Default is 3600 seconds (1 hour). |
| `force` | boolean | Optional | `false` | Admin scope only | Steals an unexpired lease held by another actor. |

#### Example Call & Response

```json
// Call
{
  "key": "BMB-14",
  "action": "renew",
  "ttl_seconds": 3600
}

// Response structuredContent
{
  "ok": true,
  "op": "task_claim",
  "data": {
    "task": {
      "key": "BMB-14",
      "version": 7,
      "claimed_by": "claude@rog"
    },
    "claimed_by": "claude@rog",
    "claim_expires_at": "2026-09-06T20:00:00Z",
    "lease_remaining_seconds": 3600
  }
}
```

---

### 8. `task_remove`

Archives or restores tasks (soft-delete). Hard delete is deliberately omitted from MCP (available only via CLI `kanban task purge`).

#### Archiving Semantics
- Clears `claimed_by`, active leases, and project `focus`.
- Preserves links (hidden while archived).
- Bumps task `version`.

#### Parameters

| Name | Type | Required | Default | Description |
|---|---|---|---|---|
| `items` | object[] | **Required** | — | 1–100 items: `[{ "key": "KEY-1", "if_version": N }]`. |
| `cascade_subtasks` | boolean | Optional | `true` | When true, archiving a parent also archives its subtasks. |
| `restore` | boolean | Optional | `false` | When true, restores previously archived tasks back to Backlog. |

#### Example Call & Response

```json
// Call
{
  "items": [
    { "key": "BMB-19", "if_version": 1 }
  ],
  "cascade_subtasks": true
}

// Response structuredContent
{
  "ok": true,
  "op": "task_remove",
  "data": {
    "items": [
      {
        "key": "BMB-19",
        "ok": true,
        "task": {
          "key": "BMB-19",
          "archived_at": "2026-09-06T18:05:00Z",
          "version": 2
        }
      }
    ]
  },
  "meta": { "count": 1, "warnings": [] }
}
```

---

### 9. `project_upsert`

Creates or modifies project definition, workflow columns, and board-level settings.

#### Strict Mode Requirement
`mode` is **REQUIRED** (`"create"` or `"update"`). There is no automatic create-or-update fallback: a typo in an immutable project key must never silently fork the board into a second project.

#### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `mode` | string | **Required** | `"create"` or `"update"`. No default. |
| `key` | string | **Required** | 2–8 alphanumeric characters starting with a letter (e.g. `"BMB"`). |
| `name` | string | Cond. | Project display name. Required for `mode: "create"`. |
| `description` | string | Optional | Markdown description of the project. |
| `if_version` | integer | Cond. | Required for `mode: "update"`. The project `version` from `board_get` — its `data.projects[].version`, or the trailing `v<version>` on the compact project header. |
| `columns` | object[] | Optional | Full ordered column layout: `[{ "name": "Backlog", "kind": "backlog" }]`. |
| `remove_columns` | object[] | Cond. | Required for any existing column absent from `columns`: `[{ "name": "Old", "move_tasks_to": "Backlog" }]`. Prevents accidental orphan data loss. |
| `settings` | object | Optional | Board settings object. |
| `settings.estimate_unit` | string | Optional | Default `"h"`. (e.g. `"h"`, `"d"`). |
| `settings.enforce_dependencies` | boolean | Optional | Default `true`. Disallows moving blocked tasks to active columns. |
| `settings.strict_done` | boolean | Optional | Default `false`. Requires all acceptance items checked before entering Done. |
| `settings.claim_ttl_seconds` | integer | Optional | Default `3600`. Clamped to 60–86400 seconds. |
| `archived` | boolean | Optional | `true` archives project; `false` unarchives. |

#### Default Columns
When `columns` is omitted during `mode: "create"`, the project is initialized with:
- **Backlog** (`kind: "backlog"`)
- **Doing** (`kind: "active"`, `wip_limit: 3`)
- **Review** (`kind: "active"`)
- **Done** (`kind: "done"`)

#### Example Call & Response

```json
// Call
{
  "mode": "create",
  "key": "CORE",
  "name": "Core Infrastructure"
}

// Response structuredContent
{
  "ok": true,
  "op": "project_upsert",
  "data": {
    "key": "CORE",
    "name": "Core Infrastructure",
    "version": 1,
    "estimate_unit": "h",
    "enforce_dependencies": true,
    "strict_done": false,
    "claim_ttl_seconds": 3600,
    "archived": false,
    "columns": [
      { "name": "Backlog", "kind": "backlog" },
      { "name": "Doing", "kind": "active", "wip_limit": 3 },
      { "name": "Review", "kind": "active" },
      { "name": "Done", "kind": "done" }
    ]
  },
  "meta": { "count": 4 }
}
```

---

## Error Handling & Domain Codes

Every tool error returns an error envelope with a machine-readable code, an explanatory message, and an actionable remediation string.

### Error Codes (`domain.Code`)

| Code | Trigger | What It Means | Concrete Remediation |
|---|---|---|---|
| `not_found` | Task key or project key does not exist. | The referenced entity was not found in storage. | Check the key spelling (case-insensitive `PROJ-N`) or call `board_get` to list existing keys. |
| `validation` | Invalid field length, empty title, unparseable date, or contradictory params. | The request violated a syntactic or structural limit. | Correct the offending parameter identified in `field` according to tool documentation. |
| `conflict` | Sent `if_version` does not match the stored task version. | Another agent or process updated the task since your last read. | The server state is attached under `error.current`. Merge your patch into it and retry immediately using `if_version = current.version`. Do not re-read. |
| `blocked` | Task moved to active/done column while incoming `blocks` links remain open. | `enforce_dependencies` is enabled and predecessor tasks are not yet in `done`. | Complete the blocker tasks first, remove the link using `task_link`, or move with `force: true` and `reason` (requires admin scope). |
| `wip_exceeded` | Active column is at its configured `wip_limit`. | Too many concurrent items are already in this column. | Finish or move an existing card out of the column, pick another task with `task_next(action: "peek")`, or move with `force: true` (admin scope). |
| `claimed` | Attempted to claim or start a task leased by someone else. | Another actor holds an active lease on this task. | Pick another ready task with `task_next`, wait for the lease to expire, or steal it with `task_claim` using `force: true` (admin scope). |
| `forbidden` | Action prohibited by token scope or endpoint constraints. | Attempted mutation on `/mcp/readonly`, used `force` without admin scope, or accessed an unauthorized project. | Use an admin or write token, run mutations against `/mcp` instead of `/mcp/readonly`, or request project access. |
| `cycle` | Link creation would introduce a circular dependency or parent-child conflict. | Dependencies must form a Directed Acyclic Graph (DAG); subtasks cannot block their parents. | Delete an existing conflicting link with `task_link` before creating new dependencies. |
| `rate_limited` | Exceeded 600 req/min per token or 20 req/min on login. | Too many HTTP requests were dispatched. | Wait for the period specified in the `Retry-After` header. Note: A 100-item batch counts as only 1 request. |
| `payload_too_large` | Request body exceeds 1 MB (`MaxRequestBodyBytes`). | The batch size or body contents exceeded the transport limit. | Reduce batch size or trim large markdown descriptions. |
| `idempotency_mismatch` | Reused an `idempotency_key` within 24 hours with a different payload. | An idempotency key was reused for a different mutation. | Generate a fresh UUID for distinct requests, or ensure identical payload on retries. |
