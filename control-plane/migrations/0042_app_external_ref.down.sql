-- 0042_app_external_ref.down.sql — reverse of 0042 in symmetric order.
--
-- Dropping the columns loses which apps were tagged with a provider appid, so a
-- re-apply leaves every app back on the fuzzy title path until an admin re-tags
-- them. Nothing else depends on them at Phase 1 (the artwork rows themselves are
-- keyed on app_id and survive), so the loss is a tagging, not a cache.
--
-- The two CHECKs and the partial index would all be dropped implicitly by
-- DROP COLUMN; they are named explicitly first so this file reads as the exact
-- inverse of the up migration rather than relying on cascade behaviour.
BEGIN;

DROP INDEX IF EXISTS apps_external_ref_idx;

ALTER TABLE apps
    DROP CONSTRAINT IF EXISTS apps_external_id_ck,
    DROP CONSTRAINT IF EXISTS apps_external_source_ck;

ALTER TABLE apps
    DROP COLUMN IF EXISTS external_id,
    DROP COLUMN IF EXISTS external_source;

COMMIT;
