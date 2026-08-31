-- Reverse of 0057_image_build.up.sql. Dropping these columns loses every
-- resolved template context sha and every adopted build tag — a later sync
-- (and an admin re-installing) reconstructs both. No other table references
-- them.
BEGIN;

ALTER TABLE installed_images
    DROP COLUMN IF EXISTS build_args,
    DROP COLUMN IF EXISTS dockerfile,
    DROP COLUMN IF EXISTS context_sha,
    DROP COLUMN IF EXISTS context_repo,
    DROP COLUMN IF EXISTS local_tag;

ALTER TABLE image_catalog
    DROP COLUMN IF EXISTS context_sha;

COMMIT;
