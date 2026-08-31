-- 0060_provider_app_suspension.down.sql — drop the suspension marker.
--
-- Dropping the column loses only the "why is this off" bookkeeping; no app's
-- enabled state changes, so a rollback leaves the catalogue exactly as it is
-- (suspended apps stay disabled, which is the safe direction — the pre-0060 code
-- would re-enable them on the next discovery enable).
BEGIN;

ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_suspended_implies_disabled_ck;
DROP INDEX IF EXISTS apps_library_discovery_suspended_idx;
ALTER TABLE apps DROP COLUMN IF EXISTS library_discovery_suspended;

COMMIT;
