-- P2-01 — per-user concurrent-session quota (additive, signed off).
-- Adds users.max_concurrent_sessions: the cap on simultaneously-active sessions
-- a user may hold, enforced at launch (control-api.md session_quota_exceeded).
-- "Active" = sessions in state {pending, assigned, starting, running}.
-- Defaulted, so existing rows need no backfill. Prose companion: protocol/schema.md.

BEGIN;

ALTER TABLE users
    ADD COLUMN max_concurrent_sessions INT NOT NULL DEFAULT 3
        CHECK (max_concurrent_sessions >= 0);

COMMIT;
