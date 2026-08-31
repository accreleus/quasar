-- 0049_mic_capture.down.sql — reverse of 0049.
--
-- Drops both columns. What is lost: the per-session record of whether
-- microphone capture was granted (sessions.mic) and the instance-wide
-- admin gate (instance_settings.mic_capture_enabled), which reverts to
-- "off" on a roll-forward, same as library_discovery_enabled's down
-- migration (0045).
BEGIN;

ALTER TABLE instance_settings
    DROP COLUMN IF EXISTS mic_capture_enabled;

ALTER TABLE sessions
    DROP COLUMN IF EXISTS mic;

COMMIT;
