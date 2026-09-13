-- Reverse of 0080. What is lost is the dedupe record, so a re-applied 0080
-- announces the currently-available release once more — noisy, never wrong.

DROP TABLE IF EXISTS platform_release_notifications;

ALTER TABLE instance_settings
    DROP COLUMN IF EXISTS release_webhook_url,
    DROP COLUMN IF EXISTS release_webhook_enabled;
