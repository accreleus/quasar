-- 0078_gpu_driver_identity.up.sql — driver identity for encoder certification (#144).
--
-- A certification row records encode_ms measured on one silicon + driver + encode-stack
-- combination. Nothing carried that combination, so after a driver change the old
-- measurement stayed applicable until it expired.
--
-- `gpus.driver_identity` is the agent's current report (agent-api.md
-- `capacity.gpus[].driver_identity`); `host_encoder_certification.driver_identity` is
-- what it was when the bench ran, stamped at write time.
--
-- Both are NULLABLE and NULL means UNKNOWN, never "no driver". Every row that exists
-- when this runs is NULL, and matching FAILS OPEN on NULL: a legacy row stays eligible
-- until a measurement carrying an identity replaces it. Dropping the caps instead would
-- launch every host at a rung it may not sustain, which is the worse failure.
BEGIN;

ALTER TABLE gpus ADD COLUMN driver_identity TEXT;
ALTER TABLE host_encoder_certification ADD COLUMN driver_identity TEXT;

COMMIT;
