-- 0108_region_input.up.sql — amendment 49 (#845): per-user Region and Input
-- settings and their install defaults. Prose: protocol/schema.md
-- `user_region_input`, `instance_settings` and the 0108 migration line. Purely
-- additive: nothing a session sees changes until a user or an admin saves.
BEGIN;

ALTER TABLE instance_settings
	ADD COLUMN IF NOT EXISTS default_timezone TEXT NOT NULL DEFAULT 'UTC',
	ADD COLUMN IF NOT EXISTS default_language TEXT NOT NULL DEFAULT 'en-US'
		CHECK (default_language ~ '^[a-z]{2,3}-[A-Z]{2}$'),
	ADD COLUMN IF NOT EXISTS default_region_format TEXT
		CHECK (default_region_format ~ '^[a-z]{2,3}-[A-Z]{2}$'),
	ADD COLUMN IF NOT EXISTS default_keyboard_layout TEXT NOT NULL DEFAULT 'us'
		CHECK (default_keyboard_layout ~ '^[A-Za-z0-9_-]{1,32}$'),
	ADD COLUMN IF NOT EXISTS default_keyboard_variant TEXT NOT NULL DEFAULT ''
		CHECK (default_keyboard_variant ~ '^[A-Za-z0-9_-]{0,64}$'),
	ADD COLUMN IF NOT EXISTS default_match_cursor TEXT NOT NULL DEFAULT 'auto'
		CHECK (default_match_cursor IN ('auto', 'on', 'off')),
	ADD COLUMN IF NOT EXISTS default_mouse_sensitivity DOUBLE PRECISION NOT NULL DEFAULT 1
		CHECK (default_mouse_sensitivity BETWEEN 0.25 AND 4),
	ADD COLUMN IF NOT EXISTS default_raw_mouse BOOLEAN NOT NULL DEFAULT false,
	ADD COLUMN IF NOT EXISTS default_flip_scroll BOOLEAN NOT NULL DEFAULT false,
	ADD COLUMN IF NOT EXISTS default_nintendo_layout BOOLEAN NOT NULL DEFAULT false;

-- NULL = not set: the field follows the browser or the install default. The
-- timezone is checked against the tz database by the handlers, not here.
CREATE TABLE IF NOT EXISTS user_region_input (
	user_id           UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	timezone          TEXT,
	language          TEXT CHECK (language ~ '^[a-z]{2,3}-[A-Z]{2}$'),
	region_format     TEXT CHECK (region_format ~ '^[a-z]{2,3}-[A-Z]{2}$'),
	keyboard_layout   TEXT CHECK (keyboard_layout ~ '^[A-Za-z0-9_-]{1,32}$'),
	keyboard_variant  TEXT CHECK (keyboard_variant ~ '^[A-Za-z0-9_-]{0,64}$'),
	match_cursor      TEXT CHECK (match_cursor IN ('auto', 'on', 'off')),
	mouse_sensitivity DOUBLE PRECISION CHECK (mouse_sensitivity BETWEEN 0.25 AND 4),
	raw_mouse         BOOLEAN,
	flip_scroll       BOOLEAN,
	nintendo_layout   BOOLEAN,
	updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
	CHECK ((keyboard_layout IS NULL) = (keyboard_variant IS NULL))
);

DROP TRIGGER IF EXISTS user_region_input_set_updated_at ON user_region_input;
CREATE TRIGGER user_region_input_set_updated_at BEFORE UPDATE ON user_region_input
	FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMIT;
