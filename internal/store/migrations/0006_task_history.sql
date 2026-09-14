-- 0006_task_history — the task lifecycle journal (KANB-30).
--
-- WHY A JOURNAL AND NOT A SNAPSHOT OF METRICS. tasks.done_at is ONE mark: it
-- is overwritten every time a card enters a done column and cleared when it
-- leaves one, so a card closed, reopened and closed again remembers only the
-- last closing. The events table cannot stand in for it either — it is pruned
-- by age (DELETE FROM events WHERE ts < ?). Neither can answer "how many were
-- open last March". A journal can, and — unlike a frozen snapshot of derived
-- numbers — it can be re-read under a NEW definition of "done", "leaf" or
-- "archived" when those definitions move again, which they already have.
--
-- KEPT FOREVER. Like progress_marks and chat_messages, rows in task_history
-- are NEVER pruned, thinned, aggregated or rolled up. The events pruner
-- (DELETE FROM events WHERE ts < ?) names the events table and ONLY the
-- events table; whoever writes the next cleanup job must not extend it to
-- this table, to task_history_origin, to progress_marks or to chat_messages.
-- There is no retention window here and there must never be one: deleting an
-- old row does not save space worth having, it silently rewrites a chart that
-- someone already read and believed.
--
-- THE ONLY DELETION THAT MAY EVER TOUCH THIS TABLE is the two ON DELETE
-- CASCADE clauses below, and they are not age-based: a journal row disappears
-- exactly when the task (or project) row it describes is physically deleted,
-- which is an admin CLI operation. That is deliberate. The live counters stop
-- counting a hard-deleted task instantly, so if the journal kept counting it,
-- replaying the journal and asking the board would give two different answers
-- — the exact defect KANB-31 exists to prevent.
--
-- ADDITIVE. This migration creates two new tables and touches nothing that
-- already exists: no ALTER, no DROP, no UPDATE and no DELETE against any
-- pre-existing table. It runs against the live board database.

-- task_history_origin holds ONE row: the instant from which the journal is
-- trustworthy. Everything before it was never recorded and cannot be honestly
-- recovered — we know when cards were created, but not when they moved, were
-- finished, reopened or archived. A chart must therefore draw NOTHING before
-- this instant rather than a line reconstructed from tasks.created_at: half a
-- truth on a chart is worse than an honest gap, because nobody can see which
-- half it is.
CREATE TABLE task_history_origin (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    started_at TEXT NOT NULL
);

INSERT INTO task_history_origin(id, started_at)
VALUES (1, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- task_history is append-only. One row per observed lifecycle change, written
-- by the STORE inside the very same transaction as the mutation it describes
-- (internal/store/task_history.go) — never by the service layer, where the
-- next new code path would simply forget to call it and the loss would
-- surface months later as a kink in a chart instead of as a red test.
--
-- kind values:
--   exists       baseline seed only: "this card was already here at origin",
--                carrying its archived flag, column + column kind, parent and
--                estimate. reconstructed = 1.
--   created      a task was created (carries the same full state).
--   moved        a task changed column (from/to column and the KIND of each).
--   archived     a task was archived.
--   restored     a task was un-archived.
--   estimate     a task's estimate changed (old and new).
--   parent       a task's parent changed (old and new) — this moves the set
--                of leaves, which is what the estimate rollup is computed over.
--   column_kind  a COLUMN's kind changed (e.g. active -> done). No task row is
--                touched by that edit, yet every card sitting in the column
--                changes bucket. Without this row a replay would disagree with
--                the live board and nothing would notice.
--
-- reconstructed marks rows that were NOT observed as they happened: only the
-- baseline seed below sets it to 1. Observed rows are 0 and stay 0, so a
-- reader can always tell what was measured from what was merely asserted.
CREATE TABLE task_history (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            TEXT NOT NULL,
    actor         TEXT NOT NULL,
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id       TEXT REFERENCES tasks(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN (
                      'exists','created','moved','archived','restored',
                      'estimate','parent','column_kind')),
    reconstructed INTEGER NOT NULL DEFAULT 0 CHECK (reconstructed IN (0,1)),

    archived      INTEGER CHECK (archived IS NULL OR archived IN (0,1)),
    from_column   TEXT,
    from_kind     TEXT,
    to_column     TEXT,
    to_kind       TEXT,
    old_estimate  REAL,
    new_estimate  REAL,
    old_parent    TEXT,
    new_parent    TEXT,

    -- Every kind except column_kind describes one task and must name it.
    CHECK (kind = 'column_kind' OR task_id IS NOT NULL)
);

-- The two reads: a project's whole journal in order, and one task's journal.
CREATE INDEX task_history_project_ts ON task_history(project_id, ts, id);
CREATE INDEX task_history_task ON task_history(task_id, id);

-- BASELINE. The journal is being started on a board that is already half full,
-- so future events alone cannot produce even the first point of a chart. The
-- baseline is written as SEEDED EVENTS in this same journal rather than as a
-- separate snapshot table, on purpose: a snapshot would create a SECOND way to
-- reconstruct a moment ("load the snapshot, then apply events"), and two
-- implementations of one concept drifting apart where nobody looks is exactly
-- the defect KANB-31 is opened to prevent. There is one replay path, and it
-- starts from these rows.
--
-- Every task row gets one, archived ones included — archivedness is part of
-- the state being recorded, not a reason to omit the card. The timestamp is
-- the origin instant for all of them, and reconstructed = 1 tells every reader
-- that these were asserted at migration time, not observed as they happened.
-- Nothing here is derived from tasks.created_at: creation we know, but the
-- moves, completions and archivings of that period we do not, and inventing
-- them would be the half-truth this design refuses.
INSERT INTO task_history(
    ts, actor, project_id, task_id, kind, reconstructed,
    archived, to_column, to_kind, new_estimate, new_parent)
SELECT
    (SELECT started_at FROM task_history_origin),
    'migration:0006_task_history',
    t.project_id,
    t.id,
    'exists',
    1,
    CASE WHEN t.archived_at IS NULL THEN 0 ELSE 1 END,
    t.column_id,
    c.kind,
    t.estimate,
    t.parent_id
FROM tasks t
JOIN columns c ON c.id = t.column_id
ORDER BY t.created_at, t.rowid;
