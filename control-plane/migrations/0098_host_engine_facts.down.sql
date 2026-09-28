-- 0098 down: drops only the three amendment-17 columns (their CHECKs go with them).
BEGIN;
ALTER TABLE hosts DROP COLUMN engine_mode;
ALTER TABLE hosts DROP COLUMN engine_version;
ALTER TABLE hosts DROP COLUMN engine;
COMMIT;
