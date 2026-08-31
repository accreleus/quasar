-- 0044_derived_tiles.down.sql — reverse of 0044 in symmetric order.
--
-- WHAT IS LOST, stated plainly. Dropping parent_app_id does NOT delete the derived
-- tiles: it leaves them as ordinary apps with runtime_spec = '{}' and no image, so
-- they become unlaunchable rows carrying a Steam appid, their artwork and their
-- users' favourites. That is the deliberate choice over a DELETE — the same call
-- §8.2 makes for suppression: a delete cascades user_app_favourites and
-- app_artwork irreversibly, and an operator who rolls forward again wants those
-- back. An operator who truly wants the tiles gone can DELETE FROM apps WHERE
-- external_id <> '' AND runtime_spec = '{}'::jsonb themselves, having read this.
--
-- NOTE the one-way rule in CLAUDE.md: never roll a control-plane binary back BELOW
-- the DB's applied migration version. This file is for a deliberate, operator-run
-- down-migration, not for "deploy main over the phase branch".
BEGIN;

-- kind narrows back FIRST, before anything else can fail, and it needs the UPDATE.
-- A narrowing CHECK is validated against existing rows on creation, so any row left
-- at 'launcher' would abort this migration — and a down-migration that cannot apply
-- is not a down-migration. Rewriting them to 'game' is lossy (the operator's
-- Launcher classification is gone) but kind is presentation-only by contract
-- (§4.5.3: nothing in scheduling, admission, profile/codec resolution or the agent
-- wire reads it), so the loss is cosmetic and re-applying 0044 does not restore it.
UPDATE apps SET kind = 'game' WHERE kind = 'launcher';

ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_kind_check;
ALTER TABLE apps ADD CONSTRAINT apps_kind_check CHECK (kind IN ('game', 'desktop'));

DROP INDEX IF EXISTS apps_parent_app_id_idx;
DROP INDEX IF EXISTS apps_parent_external_uk;

-- The three CHECKs and the two indexes above would be dropped implicitly by the
-- DROP COLUMNs below (every one of them references a dropped column); they are
-- named explicitly first so this file reads as the exact inverse of the up
-- migration rather than relying on cascade behaviour.
ALTER TABLE apps
    DROP CONSTRAINT IF EXISTS apps_derived_shape_ck,
    DROP CONSTRAINT IF EXISTS apps_origin_ck,
    DROP CONSTRAINT IF EXISTS apps_library_provider_ck;

ALTER TABLE apps
    DROP COLUMN IF EXISTS library_provider,
    DROP COLUMN IF EXISTS origin,
    DROP COLUMN IF EXISTS parent_app_id;

COMMIT;
