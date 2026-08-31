-- 0058_image_preset_link.up.sql — image management P5 (library-provider
-- auto-ensure + runtime-preset materialization). protocol/schema.md P5
-- amendment, docs/design/plans/2026-08-08-image-management-p5-spec.md.
--
-- Purely additive: two new nullable FK columns (both ON DELETE SET NULL) plus
-- the two indexes those columns need. Nothing changes behaviour on its own — an
-- instance that never installs a runtime-bearing catalog image reads
-- installed_images.runtime_preset_id = NULL and behaves exactly as it did under
-- P4.
BEGIN;

-- installed_images.runtime_preset_id — the MANAGED runtime_presets row
-- materialized from this image's manifest `runtime` block at install (P5). It is
-- the missing link that makes an installed image launchable: without it a
-- launchable app derived from a catalog image has no preset to inherit its
-- container configuration from. NULL until a runtime-bearing image is installed,
-- and re-NULLed (never blocking the delete) if the preset row is later removed —
-- hence ON DELETE SET NULL, declared up front per CLAUDE.md's FK discipline
-- (never a bare cascade surprise).
ALTER TABLE installed_images
    ADD COLUMN runtime_preset_id UUID NULL REFERENCES runtime_presets(id) ON DELETE SET NULL;

COMMENT ON COLUMN installed_images.runtime_preset_id IS
    'P5. The managed runtime_presets row materialized from this image''s manifest runtime block at install (runtime_presets.managed_image_id = this image''s id). NULL until installed / when the image carries no runtime block; SET NULL if the preset row is later deleted (never blocks the delete).';

-- Postgres does not auto-index the referencing side of a foreign key.
CREATE INDEX installed_images_runtime_preset_id_idx ON installed_images (runtime_preset_id);

-- runtime_presets.managed_image_id — marks a preset as IMAGE-MANAGED (P5
-- materialized it from a catalog image's manifest) rather than admin-authored.
-- NULL = admin-authored (every pre-P5 row, no backfill). P5 keys its
-- upsert on this column so it only ever updates the presets it owns and NEVER
-- clobbers a hand-made admin preset — even on a name collision, the managed row
-- is a separate row keyed here, not the admin's. ON DELETE SET NULL so removing
-- a catalog image demotes its managed preset to an ordinary (now admin-owned)
-- preset rather than deleting it or blocking the catalog delete.
ALTER TABLE runtime_presets
    ADD COLUMN managed_image_id TEXT NULL REFERENCES image_catalog(id) ON DELETE SET NULL;

COMMENT ON COLUMN runtime_presets.managed_image_id IS
    'P5. Set = this preset was materialized from the catalog image of this id (image-managed); NULL = admin-authored. P5 upserts keyed on this column never touch an admin preset (NULL). ON DELETE SET NULL demotes a managed preset to admin-owned when its image leaves the catalog.';

-- PARTIAL UNIQUE (managed_image_id IS NOT NULL): at most one managed preset per
-- catalog image. This is what makes the materialization upsert idempotent
-- (ON CONFLICT (managed_image_id) re-materializes the SAME row on every
-- install/update) and enforces the "one managed row per image" invariant in the
-- database rather than only in code. Admin presets keep managed_image_id NULL,
-- and Postgres treats NULLs as distinct, so any number of admin presets coexist.
CREATE UNIQUE INDEX runtime_presets_managed_image_id_uk
    ON runtime_presets (managed_image_id) WHERE managed_image_id IS NOT NULL;

COMMIT;
