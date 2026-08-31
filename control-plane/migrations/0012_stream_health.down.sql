BEGIN;

ALTER TABLE sessions
    DROP COLUMN IF EXISTS health_state_changed_at,
    DROP COLUMN IF EXISTS health_state_reason,
    DROP COLUMN IF EXISTS health_state;

COMMIT;
