-- 0054_image_catalog.up.sql — app-image catalog P1 (spec A phase 1, 2026-08-07)
-- (docs/design/plans/2026-08-07-image-management-spec.md, protocol/control-api.md
-- "App-image catalog + management" amendment, protocol/openapi.yaml
-- ImageCatalogEnvelope/CatalogImage/ImageHostState).
--
-- P1 is READ-ONLY: manifest fetch/validate/cache + GET /v1/admin/images +
-- POST /v1/admin/images/sync. No agent-api change, no host_images, no
-- installed_images — those land in later phases (P2/P3). Additive, ships dark.
--
-- image_catalog is the cached upstream offer, mirroring the quasar-images
-- manifest 1:1 (id is the manifest's stable id, never reused for a different
-- image). Cached deliberately: a GitHub outage must never affect launches,
-- only the admin's ability to see/apply an update. `raw` retains the full
-- decoded manifest entry (including the runtime superset object) so later
-- phases can act on fields P1 does not interpret, without a migration to add
-- them.
BEGIN;

CREATE TABLE image_catalog (
    id                 TEXT PRIMARY KEY,
    manifest_version   INTEGER NOT NULL,
    display_name       TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    kind               TEXT NOT NULL CHECK (kind IN ('prebuilt', 'template')),
    version            TEXT NOT NULL,
    registry_ref       TEXT,
    dockerfile         TEXT,
    build_args         JSONB NOT NULL DEFAULT '{}'::jsonb,
    artwork            JSONB NOT NULL DEFAULT '{}'::jsonb,
    runtime            JSONB NOT NULL DEFAULT '{}'::jsonb,
    library_provider   TEXT,
    min_quasar_version TEXT,
    fetched_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    raw                JSONB NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE image_catalog IS
    'Cached quasar-images manifest (P1, image-management spec). Upserted by POST /v1/admin/images/sync; a fetch failure never touches this table, so a GitHub outage cannot affect launches. id is the manifest entry id, stable and never reused for a different image.';
COMMENT ON COLUMN image_catalog.runtime IS
    'Raw manifest runtime superset object (preset_name/gpu/no_new_privileges/managed_home/home_container_path/args/env/mounts). P1 caches it only — installing an image and deriving a runtime_presets row from it is a later phase.';
COMMENT ON COLUMN image_catalog.raw IS
    'The full decoded manifest entry as fetched, for forward-compatibility with fields P1 does not interpret.';

-- Per-instance operator knobs for the catalog. Both additive, both default to
-- the documented ship-dark posture: notify (never silently auto-update) and
-- the main branch of the manifest repo.
ALTER TABLE instance_settings
    ADD COLUMN image_update_policy TEXT NOT NULL DEFAULT 'notify'
        CHECK (image_update_policy IN ('manual', 'auto', 'notify')),
    ADD COLUMN image_catalog_ref TEXT NOT NULL DEFAULT 'stable';

COMMENT ON COLUMN instance_settings.image_update_policy IS
    'Image-management spec P3+: manual|auto|notify. P1 stores it but does not yet act on it (no install/update path exists). Default notify.';
COMMENT ON COLUMN instance_settings.image_catalog_ref IS
    'The pinned quasar-images ref POST /v1/admin/images/sync fetches the manifest at. Default stable — the published branch of accreleus/quasar-images (that repo has NO main branch; stable is the default and develop is its persistent integration branch). An operator can hold a known-good commit instead.';

COMMIT;
