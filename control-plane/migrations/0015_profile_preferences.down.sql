BEGIN;

ALTER TABLE apps
    DROP COLUMN IF EXISTS profile_policy,
    DROP COLUMN IF EXISTS default_profile_id;

DROP TABLE IF EXISTS user_profile_preferences;
DROP TABLE IF EXISTS stream_profile_policy;
DROP TABLE IF EXISTS stream_profiles;

COMMIT;
