-- Revert LP-SEC-01 (0020) — drop the additions in reverse. See the up migration + CLAUDE.md's
-- one-way-at-deploy rule: only roll back a stack whose binary still embeds <= this version.

BEGIN;

ALTER TABLE sessions DROP COLUMN IF EXISTS device_id;

DROP INDEX IF EXISTS auth_tokens_device_id_idx;
ALTER TABLE auth_tokens DROP COLUMN IF EXISTS device_id;

ALTER TABLE user_devices DROP COLUMN IF EXISTS trusted;
ALTER TABLE user_devices DROP COLUMN IF EXISTS name;

DROP TABLE IF EXISTS invites;

DROP TRIGGER IF EXISTS instance_settings_set_updated_at ON instance_settings;
DROP TABLE IF EXISTS instance_settings;

COMMIT;
