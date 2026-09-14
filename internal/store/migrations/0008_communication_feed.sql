-- 0008_communication_feed — structured chat messages and command acceptances
-- (KANB-45..47: the project feed, addressed postings, atomic acceptance).
--
-- chat_messages grows the columns the feed publishes. kind defaults to
-- 'update' so every pre-existing row stays valid without a backfill. The
-- addressing columns hold tokens.id values — never display names: the id is
-- the identity that survives secret rotation, a name is a label another token
-- can later reuse. resolved_executor is written ONCE, at send time, and never
-- recomputed — a later coordinator change must not silently readdress old
-- commands. recipient keeps what the sender asked for ('all' or a token id or
-- NULL when unset); the resolution of "unset means the coordinator" happened
-- at send time and its outcome lives in resolved_executor.
--
-- idempotency_key is per AUTHORIZED sender: (author_token_id, idempotency_key)
-- is unique among keyed rows, and — unlike the 24h idempotency table for
-- task_create — it is NOT expired: the message lives as long as the project
-- does, so its retry guard must too.
--
-- command_acceptances is the durable link "command -> tasks -> acceptor"
-- (KANB-47). One row per command message, ever: the acceptance is keyed by
-- the command, not by each token, so a second token cannot accept what a
-- first one already took. task_keys are denormalized beside task_ids because
-- task keys are immutable (PROJ-N) while the acceptance row must answer
-- "which tasks came of this command" without a join after a consumer restart.

ALTER TABLE chat_messages ADD COLUMN kind TEXT NOT NULL DEFAULT 'update';
ALTER TABLE chat_messages ADD COLUMN author_token_id TEXT REFERENCES tokens(id);
ALTER TABLE chat_messages ADD COLUMN recipient TEXT;
ALTER TABLE chat_messages ADD COLUMN resolved_executor TEXT;
ALTER TABLE chat_messages ADD COLUMN reply_to_id TEXT REFERENCES chat_messages(id);
ALTER TABLE chat_messages ADD COLUMN idempotency_key TEXT;

CREATE UNIQUE INDEX chat_messages_sender_idem
    ON chat_messages(author_token_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Forward (oldest-first) feed reads: created_at ASC, id ASC with a strict
-- cursor needs the tie-break column inside the index.
CREATE INDEX chat_messages_project_asc
    ON chat_messages(project_id, created_at, id);

CREATE TABLE command_acceptances (
    id           TEXT PRIMARY KEY,
    message_id   TEXT NOT NULL UNIQUE REFERENCES chat_messages(id) ON DELETE CASCADE,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    accepted_by  TEXT NOT NULL REFERENCES tokens(id),
    task_ids     TEXT NOT NULL,
    task_keys    TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    created_at   TEXT NOT NULL
);
CREATE INDEX command_acceptances_project ON command_acceptances(project_id, message_id);
