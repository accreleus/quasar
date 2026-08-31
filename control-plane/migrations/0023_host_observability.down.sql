BEGIN;

ALTER TABLE hosts
    DROP COLUMN IF EXISTS effective_settings,
    DROP COLUMN IF EXISTS storage;

COMMIT;
