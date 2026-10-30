-- 0109 — the administrator's release of an uncertain restart attempt (amendment 51, #656).
--
-- One widened CHECK and one nullable column; no data change. `released` is the agent's
-- final phase after a release; release_requested_at is the durable request the control
-- plane re-sends until the agent reports it. Semantics: schema.md host_config_attempts.
BEGIN;

ALTER TABLE host_config_attempts DROP CONSTRAINT host_config_attempts_phase_check;
ALTER TABLE host_config_attempts ADD CONSTRAINT host_config_attempts_phase_check
    CHECK (phase IN ('offered','accepted','activating','awaiting_startup','verifying',
        'applied','failed','recovery_verifying','recovery_awaiting_startup','recovered',
        'uncertain','revoked_unstarted','released'));
ALTER TABLE host_config_attempts ADD COLUMN release_requested_at TIMESTAMPTZ NULL;

COMMIT;
