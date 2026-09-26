-- 0097 down. History loses which dump a failed migrating update named; the dumps
-- themselves are in each machine's machine state and are untouched.
BEGIN;

ALTER TABLE platform_apply_attempts
    DROP COLUMN pre_update_dump;

COMMIT;
