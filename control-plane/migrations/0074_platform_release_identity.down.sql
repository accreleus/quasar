-- Reverse of 0074. What is lost is the detected-release cache (re-detected on
-- the next detection run) and every host's reported identity (re-reported on
-- the next register), so nothing here is unrecoverable.

ALTER TABLE instance_settings
    DROP CONSTRAINT IF EXISTS instance_settings_release_channel_check;
ALTER TABLE instance_settings
    DROP COLUMN IF EXISTS release_edge_branch,
    DROP COLUMN IF EXISTS release_channel;

DROP TABLE IF EXISTS platform_releases;

ALTER TABLE hosts DROP CONSTRAINT IF EXISTS hosts_install_mode_check;
ALTER TABLE hosts
    DROP COLUMN IF EXISTS updater_present,
    DROP COLUMN IF EXISTS install_mode,
    DROP COLUMN IF EXISTS built_at,
    DROP COLUMN IF EXISTS source_commit;
