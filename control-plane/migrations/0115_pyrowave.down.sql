-- Refuses while a live session was granted PyroWave: an older binary would not record its
-- switches. Recovery after a refusal: once those sessions have ended, run
-- `migrate ... force 115`, then the down again.
BEGIN;

DO $$
BEGIN
	IF EXISTS (SELECT 1 FROM sessions
	           WHERE pyrowave->>'allowed' = 'true' AND state NOT IN ('stopped', 'failed')) THEN
		RAISE EXCEPTION '0115 down: a session granted PyroWave is still live; end it first';
	END IF;
END $$;

ALTER TABLE instance_settings DROP COLUMN IF EXISTS pyrowave_max_per_gpu;
ALTER TABLE sessions
    DROP COLUMN IF EXISTS pyrowave_slot,
    DROP COLUMN IF EXISTS pyrowave;
ALTER TABLE launch_profiles
    DROP COLUMN IF EXISTS pyrowave_kbps,
    DROP COLUMN IF EXISTS pyrowave_mode;

COMMIT;
