-- 0007_project_coordinator — the project's coordinator (KANB-44).
--
-- ONE setting, no second participant catalogue: the participant list is
-- derived from tokens that have access to the project, so the only stored
-- fact is which of them coordinates it.
--
-- The column holds tokens.id, never the display name: names are unique
-- today but they identify a label, not a row — and the stable identifier
-- must survive a secret rotation, which is `UPDATE tokens SET hash = ?
-- WHERE id = ?` and therefore keeps the row, its id and its name intact
-- for free. ON DELETE SET NULL keeps the project readable if the token
-- row is ever hard-deleted by the admin CLI: an absent coordinator is a
-- state the product already understands, a dangling reference is not.

ALTER TABLE projects ADD COLUMN coordinator_token_id TEXT REFERENCES tokens(id) ON DELETE SET NULL;
