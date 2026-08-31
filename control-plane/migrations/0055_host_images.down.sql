-- 0055_host_images.down.sql — reverses 0055 exactly. Both tables are P2
-- additions with no dependants, so dropping them restores the 0054 schema.
BEGIN;

DROP TABLE IF EXISTS installed_images;
DROP INDEX IF EXISTS host_images_image_idx;
DROP TABLE IF EXISTS host_images;

COMMIT;
