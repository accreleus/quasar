ALTER TABLE sessions
    DROP COLUMN IF EXISTS app_log_tail,
    DROP COLUMN IF EXISTS failure_code;

ALTER TABLE hosts
    DROP COLUMN IF EXISTS readiness_reported_at,
    DROP COLUMN IF EXISTS readiness;
