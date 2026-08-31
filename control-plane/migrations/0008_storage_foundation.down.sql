BEGIN;

DROP TABLE IF EXISTS user_homes;

ALTER TABLE apps
    DROP COLUMN IF EXISTS managed_home,
    DROP COLUMN IF EXISTS home_container_path;

COMMIT;
