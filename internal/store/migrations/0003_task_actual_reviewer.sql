-- 0003_task_actual_reviewer — record fields for research/estimation workflows.
--
-- actual:   the effort a task really took, same unit as estimate. Recorded
--           after the fact; the estimate/actual gap is a calibration signal.
-- reviewer: who is expected to check the work, distinct from assignee (who
--           does it). Free text like assignee — a human or agent name.
--
-- Both are nullable and default NULL, so every existing row is valid without
-- backfill and the columns are additive: code that does not select them is
-- unaffected.

ALTER TABLE tasks ADD COLUMN actual REAL;
ALTER TABLE tasks ADD COLUMN reviewer TEXT;
