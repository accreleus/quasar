-- 0109 — the admin release of an uncertain attempt's recovery hold (amendment 51, #656).
--
-- One nullable column; no data change. A journal replay must not re-plant a hold an
-- administrator released. Semantics: schema.md host_config_attempts.
BEGIN;

ALTER TABLE host_config_attempts ADD COLUMN recovery_released_at TIMESTAMPTZ NULL;

COMMIT;
