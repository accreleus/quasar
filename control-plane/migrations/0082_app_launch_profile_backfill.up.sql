-- 0082 — stamp the managed image's launch profile onto console-created apps (#171).
--
-- DATA ONLY: no DDL, no new column, no CHECK changed. Idempotent: a row whose
-- spec already carries the stamped values is not touched (so re-running, or a
-- second control plane racing the first, changes nothing and bumps no
-- updated_at — the apps_set_updated_at trigger fires only on rows written).
--
-- An app created in the admin console against an IMAGE-MANAGED runtime preset
-- (runtime_presets.managed_image_id set) was stored with whatever the editor
-- sent for `gpu` / `no_new_privileges` / `systempaths_unconfined` — in practice
-- `gpu: false`, which the editor believed inert. The agent acts on it: no GPU
-- reaches the container, Vulkan resolves to a software renderer, and every
-- desktop image (KDE, XFCE) refuses to start. Provider-created apps never had
-- the problem because images.providerRuntimeSpec copied the manifest's values
-- at install; this migration applies the same rule to the rows it skipped.
--
-- Rule (images.ApplyLaunchProfile): `gpu` takes the manifest's value, TRUE when
-- the manifest is silent; `no_new_privileges` and `systempaths_unconfined` are
-- written only when the manifest states them. Every other key is preserved
-- (`||` merges; it does not replace).
--
-- Scope: apps with a managed preset that are neither derived tiles
-- (parent_app_id IS NULL — apps_derived_shape_ck requires a tile's spec to stay
-- '{}') nor provider-created (library_provider = '' — those already carry the
-- values copied at install, and must not be moved to a catalog version newer
-- than the image they adopted). internal/crud draws the same two lines at
-- write time.
BEGIN;

WITH stamped AS (
    SELECT apps.id,
           apps.runtime_spec
               || jsonb_build_object('gpu', COALESCE((ic.runtime->>'gpu')::boolean, true))
               || CASE WHEN ic.runtime ? 'no_new_privileges'
                       THEN jsonb_build_object('no_new_privileges', (ic.runtime->>'no_new_privileges')::boolean)
                       ELSE '{}'::jsonb END
               || CASE WHEN ic.runtime ? 'systempaths_unconfined'
                       THEN jsonb_build_object('systempaths_unconfined', (ic.runtime->>'systempaths_unconfined')::boolean)
                       ELSE '{}'::jsonb END AS spec
    FROM apps
    JOIN runtime_presets rp ON rp.id = apps.runtime_preset_id
    JOIN image_catalog  ic ON ic.id = rp.managed_image_id
    WHERE apps.parent_app_id IS NULL
      AND apps.library_provider = ''
)
UPDATE apps
SET runtime_spec = stamped.spec
FROM stamped
WHERE apps.id = stamped.id
  AND apps.runtime_spec IS DISTINCT FROM stamped.spec;

COMMIT;
