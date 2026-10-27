-- Refuses while anything is on preview: switch the channel off preview and delete
-- the preview rows first. Dropping release_check loses the admin's daily/weekly choice.
BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM instance_settings WHERE release_channel = 'preview')
       OR EXISTS (SELECT 1 FROM platform_releases WHERE channel = 'preview') THEN
        RAISE EXCEPTION '0107 down: the preview channel is in use (instance_settings.release_channel or platform_releases rows on preview)';
    END IF;
END $$;

ALTER TABLE platform_releases
    DROP CONSTRAINT IF EXISTS platform_releases_channel_check;
ALTER TABLE platform_releases
    ADD CONSTRAINT platform_releases_channel_check
    CHECK (channel IN ('stable', 'edge'));

ALTER TABLE instance_settings DROP COLUMN release_check;

ALTER TABLE instance_settings
    DROP CONSTRAINT IF EXISTS instance_settings_release_channel_check;
ALTER TABLE instance_settings
    ADD CONSTRAINT instance_settings_release_channel_check
    CHECK (release_channel IN ('stable', 'edge', 'beta'));

COMMIT;
