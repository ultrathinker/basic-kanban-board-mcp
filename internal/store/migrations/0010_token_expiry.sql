-- 0010_token_expiry — a token may carry an expiry instant (KANB-60).
--
-- Executor keys are issued by an orchestrating agent for one agent's working
-- session, and a key nobody remembers to revoke must still stop working on
-- its own: the expiry is checked at authentication, so an expired key is
-- refused and drops out of every project's participant list without anyone
-- cleaning anything up.
--
-- NULL means "never expires", which is what every row that exists before
-- this migration keeps meaning, and what `kanban token create` keeps
-- producing. The value is the same RFC 3339 UTC text every other timestamp
-- column in this schema holds, written from the database clock.

ALTER TABLE tokens ADD COLUMN expires_at TEXT;
