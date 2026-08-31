-- Reverse of 0002_user_session_quota.up.sql.

BEGIN;

ALTER TABLE users DROP COLUMN max_concurrent_sessions;

COMMIT;
