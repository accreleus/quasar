-- 0032_codec_scoped_history.down.sql — drop the codec dimension.
--
-- Codec-scoped rows (codec <> '') cannot exist in the narrower key, so they are
-- deleted; profile-level rows ('') survive with their pre-0032 meaning intact.
BEGIN;

DELETE FROM user_device_profile_history WHERE codec <> '';

ALTER TABLE user_device_profile_history
    DROP CONSTRAINT user_device_profile_history_key;

ALTER TABLE user_device_profile_history
    DROP COLUMN codec;

ALTER TABLE user_device_profile_history
    ADD CONSTRAINT user_device_profile_history_user_id_device_key_profile_id_key
        UNIQUE (user_id, device_key, profile_id);

COMMIT;
