-- 0054_image_catalog.down.sql — reverse 0054_image_catalog.up.sql

BEGIN;

ALTER TABLE instance_settings
    DROP COLUMN IF EXISTS image_update_policy,
    DROP COLUMN IF EXISTS image_catalog_ref;

DROP TABLE IF EXISTS image_catalog;

COMMIT;
