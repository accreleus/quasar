-- Dev-only agent auth (#399): mark throwaway, auto-reaped identities.
--
-- ephemeral_expires_at is NULL for every real account (the overwhelming majority
-- of rows) and non-NULL only for identities minted by POST /v1/dev/agent-session.
-- The reaper deletes expired rows; migration 0007's ON DELETE CASCADE already
-- takes their sessions, tokens and device bindings with them.
--
-- The index is PARTIAL for exactly that reason: the reaper's predicate is
-- `ephemeral_expires_at IS NOT NULL AND ephemeral_expires_at < now()`, so a full
-- index would be almost entirely NULL entries paid for on every real signup.
BEGIN;

ALTER TABLE users ADD COLUMN ephemeral_expires_at TIMESTAMPTZ;

COMMENT ON COLUMN users.ephemeral_expires_at IS
    'Non-NULL marks a throwaway dev-agent identity (#399); the reaper deletes the row at/after this instant.';

CREATE INDEX users_ephemeral_expires_at_idx
    ON users (ephemeral_expires_at)
    WHERE ephemeral_expires_at IS NOT NULL;

COMMIT;
