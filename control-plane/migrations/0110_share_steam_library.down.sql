-- Refuses while a session mounts a shared Steam library: without the column the
-- single-writer guard stops counting it. Recovery after a refusal: once those
-- sessions have ended, run `migrate ... force 110`, then the down again.
BEGIN;

DO $$
BEGIN
	IF EXISTS (SELECT 1 FROM sessions
	           WHERE shared_home_app_id IS NOT NULL AND state NOT IN ('stopped', 'failed')) THEN
		RAISE EXCEPTION '0110 down: a session sharing a Steam library is still live; end it first';
	END IF;
END $$;

DROP INDEX IF EXISTS sessions_shared_home_live_idx;
ALTER TABLE sessions DROP COLUMN IF EXISTS shared_home_app_id;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_share_steam_library_ck;
ALTER TABLE apps DROP COLUMN IF EXISTS share_steam_library;
COMMIT;
