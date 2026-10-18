-- Refuses while a parked session is live: turn steam_warm_enabled off and wait
-- for the next session.parked_expire run (60 s), which stops them. The terminal
-- parked rows are deleted (metrics, trace, clock and token rows cascade):
-- dropping `kind` without that would bring each one back as a play in the
-- user's history, highlights and quota.
BEGIN;

DO $$
BEGIN
	IF EXISTS (SELECT 1 FROM sessions
	           WHERE kind = 'parked' AND state NOT IN ('stopped', 'failed')) THEN
		RAISE EXCEPTION '0105 down: a parked session is still live; set steam_warm_enabled to false and wait for session.parked_expire to stop it';
	END IF;
END $$;

DELETE FROM sessions WHERE kind = 'parked';

DROP INDEX IF EXISTS sessions_one_parked_per_user;
ALTER TABLE user_profile_preferences DROP COLUMN IF EXISTS steam_dialogs;
ALTER TABLE users DROP COLUMN IF EXISTS steam_warm_suppressed;
ALTER TABLE instance_settings
	DROP COLUMN IF EXISTS steam_warm_idle_minutes,
	DROP COLUMN IF EXISTS steam_warm_rewarm,
	DROP COLUMN IF EXISTS steam_warm_enabled;
ALTER TABLE sessions
	DROP COLUMN IF EXISTS launch_progress,
	DROP COLUMN IF EXISTS parked_until,
	DROP COLUMN IF EXISTS kind;

COMMIT;
