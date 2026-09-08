BEGIN;

ALTER TABLE host_encoder_certification DROP COLUMN driver_identity;
ALTER TABLE gpus DROP COLUMN driver_identity;

COMMIT;
