-- 0083: self-update hardening (amendment 9, #185). Purely additive.
--
-- Two widened CHECKs and two columns on tables that already exist; no backfill,
-- and no behaviour change for a run that skips nothing or an attempt that is
-- never automatically restored.
--
--   - platform_apply_runs.state gains 'succeeded_partial': the run reached the
--     end of its host list with nothing failed but passed over a host that was
--     behind the release (any skip except up_to_date). Terminal, not a failure.
--   - platform_apply_runs.retry_of: the succeeded_partial run this one was
--     started to finish. Provenance only — nothing reads it to choose a target.
--   - platform_apply_runs.skipped: the served `skipped` array, persisted:
--     succeeded_partial is decided from it, and a partial run whose explanation
--     evaporated on a crash would be a state with no reason.
--   - platform_apply_attempts.kind gains 'auto_revert': the host's updater put
--     the previous digests back itself after a failed health wait, and the
--     control plane recorded that beside the failed apply.
--
-- Standing repo rule: once applied, never deploy a control-plane binary
-- embedding only <= 0082 — boot crash-loops on a database ahead of the binary.
BEGIN;

ALTER TABLE platform_apply_runs
    DROP CONSTRAINT platform_apply_runs_state_check;
ALTER TABLE platform_apply_runs
    ADD CONSTRAINT platform_apply_runs_state_check
        CHECK (state IN ('pending', 'running', 'succeeded', 'succeeded_partial', 'failed', 'cancelled'));

ALTER TABLE platform_apply_runs
    ADD COLUMN retry_of UUID NULL REFERENCES platform_apply_runs(id) ON DELETE SET NULL,
    ADD COLUMN skipped  JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE platform_apply_runs
    ADD CONSTRAINT platform_apply_runs_skipped_check
        CHECK (jsonb_typeof(skipped) = 'array' AND octet_length(skipped::text) <= 16384);

ALTER TABLE platform_apply_attempts
    DROP CONSTRAINT platform_apply_attempts_kind_check;
ALTER TABLE platform_apply_attempts
    ADD CONSTRAINT platform_apply_attempts_kind_check
        CHECK (kind IN ('apply', 'revert', 'auto_revert'));

COMMIT;
