BEGIN;

-- The narrowed CHECK cannot be added while a row sits on beta. Fold it back to
-- the channel beta's rows come from, which is also the column's default.
UPDATE instance_settings SET release_channel = 'stable' WHERE release_channel = 'beta';

ALTER TABLE instance_settings
    DROP CONSTRAINT IF EXISTS instance_settings_release_channel_check;

ALTER TABLE instance_settings
    ADD CONSTRAINT instance_settings_release_channel_check
    CHECK (release_channel IN ('stable', 'edge'));

COMMIT;
