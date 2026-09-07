-- 0004_task_outcome_conclusion — record the epistemic status of a result.
--
-- outcome:    where a task's result stands, independent of the column it is in.
--             A task can be Done yet "refuted" (its conclusion turned out
--             wrong) or "moot" (the work was not needed). This is what "done"
--             alone cannot express. Defaults to 'open' (not yet judged), so
--             every existing row is valid without backfill. The CHECK mirrors
--             the type/priority columns: an invalid outcome is a bug, and the
--             database is the backstop under the service-layer validation.
-- conclusion: the post-hoc takeaway — what was learned or decided — kept
--             distinct from body (the brief) and from notes (the running log).
--             Empty by default.
--
-- Both are additive: code that does not select them is unaffected, and the
-- non-NULL defaults mean no UPDATE is needed for pre-existing tasks.

ALTER TABLE tasks ADD COLUMN outcome TEXT NOT NULL DEFAULT 'open'
    CHECK (outcome IN ('open','holds','refuted','superseded','moot'));
ALTER TABLE tasks ADD COLUMN conclusion TEXT NOT NULL DEFAULT '';
