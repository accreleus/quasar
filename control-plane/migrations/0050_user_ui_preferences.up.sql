-- Per-user client UI presentation preferences (2026-08-05).
-- Additive: nothing reads this table except the /v1/me/ui-preferences handlers.
BEGIN;

CREATE TABLE user_ui_preferences (
    user_id         UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    session_overlay JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER user_ui_preferences_set_updated_at BEFORE UPDATE ON user_ui_preferences
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMIT;
