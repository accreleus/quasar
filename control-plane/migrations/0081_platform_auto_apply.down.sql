-- Dropping platform_auto_apply loses the opt-in, which is the safe direction:
-- the column's absence reads as "off" to any binary that predates it.
--
-- Dropping `unattended` loses which runs were automatic, so the failure
-- suppression forgets itself: after a rollback, an unattended run that had
-- failed on a release becomes indistinguishable from an admin's own failed run
-- and the release is eligible again. Nothing is corrupted; a bad release may be
-- re-attempted once. Export platform_apply_runs before rolling back if that
-- history matters.
BEGIN;

DROP INDEX IF EXISTS platform_apply_runs_release_recent_idx;

ALTER TABLE platform_apply_runs
    DROP COLUMN IF EXISTS unattended;

ALTER TABLE instance_settings
    DROP COLUMN IF EXISTS platform_auto_apply;

COMMIT;
