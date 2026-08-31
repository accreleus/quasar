-- Reverse of 0056_image_actions.up.sql. Dropping these columns loses the pin
-- set, every resolved digest, and the persisted sync state — all of which a
-- later sync (and an admin re-pinning) reconstructs. No other table references
-- them.
BEGIN;

ALTER TABLE instance_settings
    DROP COLUMN IF EXISTS image_synced_at,
    DROP COLUMN IF EXISTS image_sync_error;

ALTER TABLE image_catalog
    DROP COLUMN IF EXISTS registry_digest;

ALTER TABLE installed_images
    DROP COLUMN IF EXISTS pinned;

COMMIT;
