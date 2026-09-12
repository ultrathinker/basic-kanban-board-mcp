-- 0005_progress_and_chat — progress estimates and project-scoped chat.
--
-- progress_marks is the append-only history of progress estimates. Rows are
-- NEVER pruned or thinned: the trend is the data, so an assessor revises its
-- answer by appending a new mark, never by overwriting an old one. task_id
-- NULL marks an estimate of the PROJECT as a whole; a non-NULL task_id marks
-- one (project_id, task_id, assessor) track. assessor is the estimating
-- agent's self-declared name and is deliberately not checked against
-- anything: there is no registry, any agent may assess. eta is an optional
-- RFC3339 forecast of when the work will finish.
--
-- chat_messages keeps the AI conversation scoped to a project. Also forever:
-- the events pruner (DELETE FROM events WHERE ts < ?) names the events table
-- only and must never touch either of these tables.
--
-- Indexes serve the three reads: the full history of a scope in time order,
-- the latest mark per assessor within a scope, and a project's chat newest
-- first.

CREATE TABLE progress_marks (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id    TEXT REFERENCES tasks(id) ON DELETE CASCADE,
    assessor   TEXT NOT NULL,
    percent    INTEGER NOT NULL CHECK (percent BETWEEN 0 AND 100),
    eta        TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX progress_marks_scope_created ON progress_marks(project_id, task_id, created_at);
CREATE INDEX progress_marks_assessor_created ON progress_marks(project_id, task_id, assessor, created_at DESC);

CREATE TABLE chat_messages (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    author     TEXT NOT NULL,
    body       TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX chat_messages_project_created ON chat_messages(project_id, created_at DESC);
