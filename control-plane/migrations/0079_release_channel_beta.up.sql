-- 0079_release_channel_beta.up.sql — the beta release channel (#121).
--
-- One widened CHECK and nothing else. `beta` selects the rows the stable channel
-- already caches and keeps the prereleases among them, so `platform_releases`
-- gains no rows, no channel value, and no re-detection on a switch; its own
-- CHECK stays IN ('stable','edge'). Semantics: schema.md instance_settings.
BEGIN;

ALTER TABLE instance_settings
    DROP CONSTRAINT IF EXISTS instance_settings_release_channel_check;

ALTER TABLE instance_settings
    ADD CONSTRAINT instance_settings_release_channel_check
    CHECK (release_channel IN ('stable', 'edge', 'beta'));

COMMIT;
