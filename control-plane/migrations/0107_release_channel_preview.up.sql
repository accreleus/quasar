-- 0107 — the preview release channel and the detection check (amendment 48, #840).
--
-- Two widened CHECKs and one nullable column; no data change. release_check NULL
-- follows the channel (weekly on every channel that exists before this), so the
-- migration changes no schedule on upgrade. Semantics: schema.md instance_settings
-- and platform_releases.
BEGIN;

ALTER TABLE instance_settings
    DROP CONSTRAINT IF EXISTS instance_settings_release_channel_check;
ALTER TABLE instance_settings
    ADD CONSTRAINT instance_settings_release_channel_check
    CHECK (release_channel IN ('stable', 'edge', 'beta', 'preview'));

ALTER TABLE instance_settings
    ADD COLUMN release_check TEXT NULL
    CONSTRAINT instance_settings_release_check_check
    CHECK (release_check IN ('daily', 'weekly'));

ALTER TABLE platform_releases
    DROP CONSTRAINT IF EXISTS platform_releases_channel_check;
ALTER TABLE platform_releases
    ADD CONSTRAINT platform_releases_channel_check
    CHECK (channel IN ('stable', 'edge', 'preview'));

COMMIT;
