-- Drops the per-GPU codec column. The loss is per-GPU codec knowledge; every
-- GPU reads back as inheriting its host's codecs, the pre-0086 behaviour.
BEGIN;

ALTER TABLE gpus DROP COLUMN codecs;

COMMIT;
