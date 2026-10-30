-- Roll the control-plane binary back to one embedding only <= 0108 only after
-- this down migration. A released attempt replayed afterwards holds the host again.
BEGIN;

ALTER TABLE host_config_attempts DROP COLUMN IF EXISTS recovery_released_at;

COMMIT;
