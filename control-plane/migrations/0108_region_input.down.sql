-- Drops every user's Region and Input settings and the install defaults.
-- Roll the control-plane binary back to one embedding only <= 0107 only after
-- this down migration.
BEGIN;

DROP TABLE IF EXISTS user_region_input;

ALTER TABLE instance_settings
	DROP COLUMN IF EXISTS default_timezone,
	DROP COLUMN IF EXISTS default_language,
	DROP COLUMN IF EXISTS default_region_format,
	DROP COLUMN IF EXISTS default_keyboard_layout,
	DROP COLUMN IF EXISTS default_keyboard_variant,
	DROP COLUMN IF EXISTS default_match_cursor,
	DROP COLUMN IF EXISTS default_mouse_sensitivity,
	DROP COLUMN IF EXISTS default_raw_mouse,
	DROP COLUMN IF EXISTS default_flip_scroll,
	DROP COLUMN IF EXISTS default_nintendo_layout;

COMMIT;
