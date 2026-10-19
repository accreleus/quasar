-- 0105_steam_warm.up.sql — control-api.md amendment 39 (#759): warm Steam,
-- parked sessions. Prose: protocol/schema.md `sessions`, `instance_settings`,
-- `users`, `user_profile_preferences` and the 0105 migration line. Every
-- existing session is a play and the switch defaults off, so nothing changes
-- on upgrade.
-- Locks: the CHECK-validating ADD COLUMN and the non-concurrent unique index
-- scan `sessions` under ACCESS EXCLUSIVE / SHARE inside this transaction. Fine at
-- today's row counts; a large history table makes this a short write outage.
BEGIN;

ALTER TABLE sessions
	ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'play'
		CHECK (kind IN ('play', 'parked')),
	ADD COLUMN IF NOT EXISTS parked_until TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS launch_progress JSONB;

-- At most one live parked session per user. Includes stopping: a parked Steam
-- that is shutting down still holds the user's home.
CREATE UNIQUE INDEX IF NOT EXISTS sessions_one_parked_per_user ON sessions (user_id)
	WHERE kind = 'parked' AND state IN ('pending', 'assigned', 'starting', 'running', 'stopping');

ALTER TABLE instance_settings
	ADD COLUMN IF NOT EXISTS steam_warm_enabled BOOLEAN NOT NULL DEFAULT false,
	ADD COLUMN IF NOT EXISTS steam_warm_rewarm TEXT NOT NULL DEFAULT 'library_open'
		CHECK (steam_warm_rewarm IN ('library_open', 'next_steam_launch')),
	ADD COLUMN IF NOT EXISTS steam_warm_idle_minutes INT NOT NULL DEFAULT 10
		CHECK (steam_warm_idle_minutes BETWEEN 1 AND 60);

ALTER TABLE users
	ADD COLUMN IF NOT EXISTS steam_warm_suppressed BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE user_profile_preferences
	ADD COLUMN IF NOT EXISTS steam_dialogs TEXT NOT NULL DEFAULT 'show'
		CHECK (steam_dialogs IN ('show', 'dismiss_safe'));

COMMIT;
