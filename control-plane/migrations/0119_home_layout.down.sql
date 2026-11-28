BEGIN;

ALTER TABLE user_ui_preferences DROP COLUMN IF EXISTS home_layout;
ALTER TABLE instance_settings DROP COLUMN IF EXISTS home_layout;

COMMIT;
