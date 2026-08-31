-- 0035_runtime_presets.down.sql — reverse of 0035 in symmetric order.
-- The column must go before the table: apps.runtime_preset_id REFERENCES
-- runtime_presets(id), so dropping the table first would fail on the dependent
-- foreign key. Dropping the column drops its index with it.
BEGIN;

ALTER TABLE apps
    DROP COLUMN IF EXISTS runtime_preset_id;

DROP TABLE IF EXISTS runtime_presets;

COMMIT;
