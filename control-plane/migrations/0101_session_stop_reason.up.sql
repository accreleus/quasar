-- 0101_session_stop_reason.up.sql — control-api.md amendment 24 (#516): the
-- session_stop reason the control plane recorded when it moved the session to
-- `stopping`. Prose: protocol/schema.md `sessions.stop_reason`.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS stop_reason TEXT;
