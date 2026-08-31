-- 0055_host_images.up.sql — image management P2 (ensure images onto hosts),
-- docs/design/plans/2026-08-08-image-management-p2-spec.md §"Data model",
-- protocol/schema.md image-management amendment, protocol/agent-api.md
-- §image_ensure / §image_remove / §image_state / §register.
--
-- Additive and dark: nothing writes these tables until an agent reports
-- image_state or an operator seeds installed_images (the install API is P3).
-- Every existing launch is unaffected — the scheduler's image filter only
-- engages for an app whose image matches an INSTALLED catalog entry, so a
-- fleet with an empty installed_images places exactly as it did before.
BEGIN;

-- host_images — per-host presence of a managed catalog image. This is the
-- table that makes multi-host real: the control plane holds no per-host GPU
-- state (invariant #1), but it does hold the fleet-wide record of which host
-- has which image, fed exclusively by the agent's image_state reports.
CREATE TABLE host_images (
    host_id    UUID        NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    image_id   TEXT        NOT NULL REFERENCES image_catalog(id) ON DELETE CASCADE,
    version    TEXT        NOT NULL DEFAULT '',
    state      TEXT        NOT NULL DEFAULT 'absent'
                           CHECK (state IN ('absent', 'pulling', 'building', 'ready', 'failed')),
    error      TEXT        NOT NULL DEFAULT '',
    bytes      BIGINT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (host_id, image_id)
);

COMMENT ON TABLE host_images IS
    'Per-host presence of a managed catalog image (image-management P2). Written only from the agent-api image_state stream and from register reconciliation; an image_id absent from image_catalog is dropped, never stored (the FK enforces it). The scheduler refuses to place an app on a host whose row for the app''s catalog image is not ready.';
COMMENT ON COLUMN host_images.state IS
    'absent|pulling|building|ready|failed. building is reserved for the P4 template-build amendment: it is accepted by this CHECK so the P4 wire needs no migration, but no P2 agent ever sends it.';
COMMENT ON COLUMN host_images.error IS
    'Operator-readable failure message from the agent (state=failed only) — never a raw docker error blob. Empty for every other state.';
COMMENT ON COLUMN host_images.bytes IS
    'Best-effort size: bytes downloaded so far while pulling, on-disk size on ready when cheaply known. NULL when unknown — never a fabricated zero.';

-- Index the reverse direction: "which hosts have image X" is the shape both
-- GET /v1/admin/images (hosts[] per catalog entry) and the ensure
-- orchestration read. The PK covers (host_id, image_id) for the per-host
-- lookups the scheduler does.
CREATE INDEX host_images_image_idx ON host_images (image_id);

-- installed_images — the per-instance adoption set: which catalog images this
-- Quasar instance has decided to run, and at which version. P2 introduces it
-- minimally because ensure-everywhere needs to know WHAT to ensure; the
-- install/uninstall API (and pin / runtime_preset_id columns) is P3.
CREATE TABLE installed_images (
    image_id     TEXT PRIMARY KEY REFERENCES image_catalog(id) ON DELETE CASCADE,
    version      TEXT        NOT NULL,
    registry_ref TEXT        NOT NULL DEFAULT '',
    lazy         BOOLEAN     NOT NULL DEFAULT false,
    installed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE installed_images IS
    'Per-instance adoption set (image-management P2, minimal). A lazy=false row is ensured onto every connected host; lazy=true is pulled on demand (P3 UX). The install/uninstall API is P3 — P2 seeds this via the ensure path it exercises.';
COMMENT ON COLUMN installed_images.version IS
    'The catalog version ensured across the fleet. A host whose host_images row names a different version is not ready at this version and is re-ensured.';
COMMENT ON COLUMN installed_images.registry_ref IS
    'The concrete registry ref captured AT ADOPTION TIME (review round: version/ref drift). image_catalog.registry_ref moves on every catalog sync, but this column is frozen the moment an image is adopted at `version`, so every ensure dispatched for this adoption pulls the SAME bits the version label names — even after a later sync moves the catalog forward. Empty only for a template (P4) entry with nothing to pull yet.';

COMMIT;
