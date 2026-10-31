-- Refuses once a release was requested or recorded: the agent may already hold
-- `released`, and an older control plane quarantines an agent journal that holds it.
-- Roll the control-plane binary back to one embedding only <= 0108 only after this
-- down migration.
BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM host_config_attempts
               WHERE phase = 'released' OR release_requested_at IS NOT NULL) THEN
        RAISE EXCEPTION '0109 down: a recovery release was requested or recorded; an older control plane cannot read an agent journal that holds it';
    END IF;
END $$;

ALTER TABLE host_config_attempts DROP COLUMN IF EXISTS release_requested_at;
ALTER TABLE host_config_attempts DROP CONSTRAINT host_config_attempts_phase_check;
ALTER TABLE host_config_attempts ADD CONSTRAINT host_config_attempts_phase_check
    CHECK (phase IN ('offered','accepted','activating','awaiting_startup','verifying',
        'applied','failed','recovery_verifying','recovery_awaiting_startup','recovered',
        'uncertain','revoked_unstarted'));

COMMIT;
