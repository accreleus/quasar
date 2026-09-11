-- 0083 down. The new vocabulary is rewritten to its nearest old value before
-- the CHECKs are narrowed — narrowing first fails on any live row in the new
-- states — and the two columns are dropped. The loss is which runs were partial
-- and which reverts were automatic: history becomes vaguer, never wrong about
-- what is installed. Export platform_apply_runs / platform_apply_attempts first
-- if that distinction matters.
BEGIN;

UPDATE platform_apply_runs SET state = 'succeeded' WHERE state = 'succeeded_partial';
ALTER TABLE platform_apply_runs
    DROP CONSTRAINT platform_apply_runs_state_check;
ALTER TABLE platform_apply_runs
    ADD CONSTRAINT platform_apply_runs_state_check
        CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'cancelled'));

ALTER TABLE platform_apply_runs
    DROP CONSTRAINT IF EXISTS platform_apply_runs_skipped_check;
ALTER TABLE platform_apply_runs
    DROP COLUMN IF EXISTS skipped,
    DROP COLUMN IF EXISTS retry_of;

UPDATE platform_apply_attempts SET kind = 'revert' WHERE kind = 'auto_revert';
ALTER TABLE platform_apply_attempts
    DROP CONSTRAINT platform_apply_attempts_kind_check;
ALTER TABLE platform_apply_attempts
    ADD CONSTRAINT platform_apply_attempts_kind_check
        CHECK (kind IN ('apply', 'revert'));

COMMIT;
