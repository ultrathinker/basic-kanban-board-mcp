-- 0009_column_kind_waiting — widens columns.kind to allow 'waiting' (KANB-52).
--
-- Waiting is a column kind for parked work: visible and counted as open,
-- not a task_next candidate, not part of any active column's WIP, and not
-- a source of "done" for anything it blocks. It exists so a project can
-- name that state explicitly instead of overloading an active column for
-- it, which is the workaround this card replaces.
--
-- SQLite has no ALTER TABLE for widening a CHECK constraint, only the
-- documented recreate-copy-drop-rename recipe — that is why this migration
-- rebuilds the whole table instead of a single ALTER statement. Nothing
-- about the shape changes: same columns, same types, same indexes. Every
-- existing row's kind is copied byte for byte, so no existing column's
-- kind changes because of this migration — widening the vocabulary is not
-- the same as putting anything new into it.
--
-- tasks.column_id carries `REFERENCES columns(id) ON DELETE RESTRICT`. With
-- foreign key enforcement on, DROP TABLE performs an implicit delete of
-- every row first, which would trip that RESTRICT against every existing
-- task even though nothing here is really being deleted for good. The
-- store's Go migration runner (migrate.go) knows this file needs the table
-- rebuilt and toggles PRAGMA foreign_keys off for the duration — off is a
-- documented no-op inside an already-open transaction, so it has to happen
-- outside this migration's own write transaction — and runs
-- PRAGMA foreign_key_check immediately after, so a rebuild that somehow
-- lost a referenced row fails loudly right here instead of surfacing as a
-- dangling column_id months later.

CREATE TABLE columns_new (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT NOT NULL COLLATE NOCASE,
    position   INTEGER NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('backlog','active','done','waiting')),
    wip_limit  INTEGER
);

INSERT INTO columns_new (id, project_id, name, position, kind, wip_limit)
SELECT id, project_id, name, position, kind, wip_limit FROM columns;

DROP TABLE columns;
ALTER TABLE columns_new RENAME TO columns;

-- Same two indexes 0001_init.sql declared; DROP TABLE took them with it.
CREATE UNIQUE INDEX columns_project_name_uniq ON columns(project_id, name);
CREATE INDEX columns_project_pos ON columns(project_id, position);
