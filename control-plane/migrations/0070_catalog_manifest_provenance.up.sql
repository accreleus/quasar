-- 0070_catalog_manifest_provenance.up.sql — #548 manifest provenance
-- (VISIBILITY ONLY; the operator decision is explicitly NOT to sign the
-- manifest, not to pin an immutable ref, and never to block a sync on change).
--
-- The app catalog is fetched by unauthenticated HTTPS GET from
-- raw.githubusercontent.com at instance_settings.image_catalog_ref, whose
-- default `stable` is a MUTABLE branch. Image BYTES are digest-pinned once the
-- manifest is trusted (image_catalog.registry_digest, #440) but nothing
-- authenticates the manifest itself, so a force-push to that branch silently
-- changes what every host installs on the next sync. These columns make that
-- swap impossible to miss: they record where the served catalog came from and
-- keep the previous digest so "changed" is computable.
--
-- Purely additive, and written in the SAME transaction as the catalog upsert
-- (internal/images/store.go recordProvenance) so provenance can never describe
-- a manifest other than the one whose rows are stored.
BEGIN;

ALTER TABLE instance_settings
    ADD COLUMN image_manifest_sha256      TEXT    NOT NULL DEFAULT '',
    ADD COLUMN image_manifest_prev_sha256 TEXT    NOT NULL DEFAULT '',
    ADD COLUMN image_manifest_commit_sha  TEXT    NOT NULL DEFAULT '',
    ADD COLUMN image_manifest_ref         TEXT    NOT NULL DEFAULT '',
    ADD COLUMN image_manifest_url         TEXT    NOT NULL DEFAULT '',
    ADD COLUMN image_manifest_changed     BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN image_manifest_changed_at  TIMESTAMPTZ;

COMMENT ON COLUMN instance_settings.image_manifest_sha256 IS
    '#548. Lowercase-hex sha256 of the manifest BYTES the currently-served catalog was parsed from. Empty until the first successful sync; a failed sync leaves it (and every column below) intact, because the catalog it describes is still what is being served.';
COMMENT ON COLUMN instance_settings.image_manifest_prev_sha256 IS
    '#548. The digest immediately before image_manifest_sha256, so "the manifest changed" is computable across a control-plane restart. Empty until a change has ever been observed; never cleared afterwards.';
COMMENT ON COLUMN instance_settings.image_manifest_commit_sha IS
    '#548. The upstream commit sha the manifest was actually fetched at (resolve-then-fetch, Store.resolveCatalogRef). Empty when ref resolution failed and the fetch fell back to the mutable ref — an expected state, never a sync failure.';
COMMENT ON COLUMN instance_settings.image_manifest_ref IS
    '#548. The CONFIGURED ref (instance_settings.image_catalog_ref) as of that sync, not the resolved sha — image_manifest_commit_sha is the resolved half. Recorded separately so a later change to the ref is visible against the catalog fetched under the old one.';
COMMENT ON COLUMN instance_settings.image_manifest_url IS
    '#548. The exact URL fetched (raw.githubusercontent.com/<repo>/<commit-or-ref>/<path>), which pins down the QUASAR_IMAGE_CATALOG_REPO/_PATH in force at that sync.';
COMMENT ON COLUMN instance_settings.image_manifest_changed IS
    '#548. True when the LAST successful sync''s digest differed from the previously recorded one. Self-clears on the next unchanged sync; image_manifest_prev_sha256/_changed_at keep the durable record.';
COMMENT ON COLUMN instance_settings.image_manifest_changed_at IS
    '#548. When image_manifest_sha256''s current value was first observed. NULL until the first successful sync.';

COMMIT;
