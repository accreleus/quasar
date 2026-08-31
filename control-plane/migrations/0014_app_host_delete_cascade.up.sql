-- Thread 1 (2026-06-24): enable cascade deletion for apps and hosts so that
-- DELETE FROM apps / DELETE FROM hosts cleanly removes terminal session history
-- and updates user_homes references without manual cleanup.
--
-- Pre-existing cascade / set-null FKs (already correct, left unchanged):
--   gpus.host_id            → ON DELETE CASCADE        (migration 0001)
--   user_homes.app_id       → ON DELETE SET NULL       (migration 0009)
--   session_metrics(id)     → cascades from sessions   (migration 0003)
--   session_tokens(id)      → cascades from sessions   (migration 0011 / 0013)
--
-- Changes in this migration:
--   sessions.app_id         RESTRICT   → CASCADE
--   sessions.host_id        RESTRICT   → CASCADE
--   user_homes.host_id      no cascade → SET NULL

BEGIN;

-- sessions.app_id: was bare REFERENCES apps(id) from migration 0001 (RESTRICT).
-- Constraint name per information_schema: sessions_app_id_fkey
ALTER TABLE sessions
    DROP CONSTRAINT sessions_app_id_fkey,
    ADD CONSTRAINT sessions_app_id_fkey
        FOREIGN KEY (app_id) REFERENCES apps(id) ON DELETE CASCADE;

-- sessions.host_id: was bare REFERENCES hosts(id) from migration 0001 (RESTRICT).
-- Constraint name: sessions_host_id_fkey
ALTER TABLE sessions
    DROP CONSTRAINT sessions_host_id_fkey,
    ADD CONSTRAINT sessions_host_id_fkey
        FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE;

-- user_homes.host_id: was bare REFERENCES hosts(id) from migration 0008 (RESTRICT).
-- Constraint name: user_homes_host_id_fkey
ALTER TABLE user_homes
    DROP CONSTRAINT user_homes_host_id_fkey,
    ADD CONSTRAINT user_homes_host_id_fkey
        FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE SET NULL;

COMMIT;
