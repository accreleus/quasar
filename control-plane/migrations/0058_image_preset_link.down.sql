-- Reverse of 0058_image_preset_link.up.sql. Dropping these columns loses every
-- managed-preset linkage and the managed/admin distinction; a later install
-- re-materializes both. No other table references either column.
BEGIN;

DROP INDEX IF EXISTS runtime_presets_managed_image_id_uk;

ALTER TABLE runtime_presets
    DROP COLUMN IF EXISTS managed_image_id;

DROP INDEX IF EXISTS installed_images_runtime_preset_id_idx;

ALTER TABLE installed_images
    DROP COLUMN IF EXISTS runtime_preset_id;

COMMIT;
