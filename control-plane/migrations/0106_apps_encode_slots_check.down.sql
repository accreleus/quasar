-- Drops the CHECK only. The rows 0106 repaired to 1 stay at 1: their old value
-- (<= 0) was the defect and is not recorded.
BEGIN;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_default_encode_slots_ck;
COMMIT;
