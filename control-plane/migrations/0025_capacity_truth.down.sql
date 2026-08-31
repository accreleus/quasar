BEGIN;
DROP INDEX IF EXISTS gpus_schedulable_idx;
ALTER TABLE gpus DROP COLUMN reported;
ALTER TABLE hosts DROP COLUMN capacity_reason, DROP COLUMN capacity_detection;
COMMIT;
