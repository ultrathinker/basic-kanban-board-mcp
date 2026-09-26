-- 0011_project_idle_after — a project may set its own idle threshold (KANB-67).
--
-- board_get's attention line names a card in an active column that has gone
-- without movement for longer than a threshold. Until now that threshold was
-- the project's claim TTL, but how long a lease lasts and when a card counts
-- as abandoned are different questions: a board whose review cycle is days
-- long saw the line light up every hour.
--
-- NULL means "not set", which is what every project that exists before this
-- migration keeps meaning: the threshold stays claim_ttl_seconds, so no
-- existing board changes behaviour by being migrated.

ALTER TABLE projects ADD COLUMN idle_after_seconds INTEGER;
