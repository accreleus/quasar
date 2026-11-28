-- 0119 — server-owned Home layout (Amendment 101, #1701). NULL = not set: the user falls back to
-- the admin default, the admin default to the built-in one. Validation is the write handler's
-- (internal/crud/home_layout.go), so the kind set grows without a migration.
BEGIN;

ALTER TABLE instance_settings ADD COLUMN home_layout JSONB NULL;
ALTER TABLE user_ui_preferences ADD COLUMN home_layout JSONB NULL;

COMMIT;
