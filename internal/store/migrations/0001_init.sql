-- 0001_init — the whole v1 schema.
--
-- Conventions:
--   * ids are UUIDv7 text (time-sortable); the human handle is `key`
--   * timestamps are RFC3339 UTC text written from the DB clock
--   * key/tag/column lookups are case-insensitive: NOCASE collation where the
--     column is looked up by a name a caller typed
--   * every FK is declared; foreign_keys is ON per connection via the DSN

CREATE TABLE projects (
    id                   TEXT PRIMARY KEY,
    key                  TEXT NOT NULL COLLATE NOCASE,
    name                 TEXT NOT NULL,
    description          TEXT NOT NULL DEFAULT '',
    version              INTEGER NOT NULL DEFAULT 1,
    next_task_seq        INTEGER NOT NULL DEFAULT 1,
    focus_task_id        TEXT,
    estimate_unit        TEXT NOT NULL DEFAULT 'h',
    enforce_dependencies INTEGER NOT NULL DEFAULT 1,
    strict_done          INTEGER NOT NULL DEFAULT 0,
    claim_ttl_seconds    INTEGER NOT NULL DEFAULT 3600,
    archived_at          TEXT,
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL
);
CREATE UNIQUE INDEX projects_key_uniq ON projects(key);

CREATE TABLE columns (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT NOT NULL COLLATE NOCASE,
    position   INTEGER NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('backlog','active','done')),
    wip_limit  INTEGER
);
-- Column names are unique per project because every tool addresses a column by
-- name; without this constraint `column: "Doing"` is ambiguous.
CREATE UNIQUE INDEX columns_project_name_uniq ON columns(project_id, name);
CREATE INDEX columns_project_pos ON columns(project_id, position);

CREATE TABLE tasks (
    id                TEXT PRIMARY KEY,
    key               TEXT NOT NULL COLLATE NOCASE,
    project_id        TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    column_id         TEXT NOT NULL REFERENCES columns(id) ON DELETE RESTRICT,
    parent_id         TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    rank              INTEGER NOT NULL,

    title             TEXT NOT NULL,
    body              TEXT NOT NULL DEFAULT '',
    type              TEXT NOT NULL CHECK (type IN ('task','bug','feat','chore','doc','perf','research')),
    priority          INTEGER NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 4),
    estimate          REAL,
    tags              TEXT NOT NULL DEFAULT '[]',   -- JSON array, normalized+sorted
    assignee          TEXT,

    claimed_by        TEXT,
    claimed_at        TEXT,
    claim_expires_at  TEXT,

    acceptance        TEXT NOT NULL DEFAULT '[]',   -- JSON [{text,done}]
    due_at            TEXT,
    column_entered_at TEXT NOT NULL,
    started_at        TEXT,
    done_at           TEXT,

    version           INTEGER NOT NULL DEFAULT 1,
    metadata          TEXT NOT NULL DEFAULT '{}',   -- JSON object

    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    created_by        TEXT NOT NULL,
    updated_by        TEXT NOT NULL,
    archived_at       TEXT
);
CREATE UNIQUE INDEX tasks_key_uniq ON tasks(key);
-- The board read is the hot path: column + rank is its natural order.
CREATE INDEX tasks_column_rank ON tasks(column_id, rank);
CREATE INDEX tasks_project_archived ON tasks(project_id, archived_at);
CREATE INDEX tasks_parent ON tasks(parent_id);
CREATE INDEX tasks_updated ON tasks(project_id, updated_at);
-- task_next scans backlog candidates by priority then due date.
CREATE INDEX tasks_next ON tasks(project_id, archived_at, priority DESC, due_at, rank);
CREATE INDEX tasks_claim ON tasks(claimed_by, claim_expires_at);

CREATE TABLE links (
    blocker_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    blocked_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    type       TEXT NOT NULL DEFAULT 'blocks' CHECK (type IN ('blocks')),
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    PRIMARY KEY (blocker_id, blocked_id, type),
    CHECK (blocker_id <> blocked_id)
);
CREATE INDEX links_blocked ON links(blocked_id);

CREATE TABLE notes (
    id         TEXT PRIMARY KEY,
    task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    author     TEXT NOT NULL,
    body       TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX notes_task_created ON notes(task_id, created_at DESC);

-- Append-only. Backs the activity feed, SSE replay and any future audit.
CREATE TABLE events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         TEXT NOT NULL,
    actor      TEXT NOT NULL,
    type       TEXT NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id    TEXT,
    payload    TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX events_project_id_idx ON events(project_id, id);

CREATE TABLE tokens (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL COLLATE NOCASE,
    hash         BLOB NOT NULL,
    scopes       TEXT NOT NULL,          -- JSON array
    project_keys TEXT NOT NULL DEFAULT '[]',
    created_at   TEXT NOT NULL,
    last_used_at TEXT,
    revoked_at   TEXT
);
CREATE UNIQUE INDEX tokens_name_uniq ON tokens(name);
-- Authentication looks a token up by hash; the comparison at the call site is
-- still constant-time.
CREATE UNIQUE INDEX tokens_hash_uniq ON tokens(hash);

CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    token_id     TEXT NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    expires_at   TEXT NOT NULL
);
CREATE INDEX sessions_expires ON sessions(expires_at);

CREATE TABLE idempotency (
    token_id     TEXT NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
    key          TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    response     BLOB NOT NULL,
    expires_at   TEXT NOT NULL,
    PRIMARY KEY (token_id, key)
);
CREATE INDEX idempotency_expires ON idempotency(expires_at);
