BEGIN;

ALTER TABLE instance_settings
    DROP COLUMN IF EXISTS image_manifest_changed_at,
    DROP COLUMN IF EXISTS image_manifest_changed,
    DROP COLUMN IF EXISTS image_manifest_url,
    DROP COLUMN IF EXISTS image_manifest_ref,
    DROP COLUMN IF EXISTS image_manifest_commit_sha,
    DROP COLUMN IF EXISTS image_manifest_prev_sha256,
    DROP COLUMN IF EXISTS image_manifest_sha256;

COMMIT;
