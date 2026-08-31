-- 0056_image_actions.up.sql — image management P3 (install / uninstall / pin /
-- update, update-policy application, digest pinning, persisted sync state).
-- docs/design/plans/2026-08-08-image-management-p3-spec.md §"Data model",
-- protocol/schema.md P3 amendment, protocol/control-api.md
-- §"App-image management P3".
--
-- Purely additive: four NOT NULL DEFAULT / nullable columns on three existing
-- tables. Nothing changes behaviour on its own — an instance that never calls
-- the new routes reads pinned=false, registry_digest='' and behaves exactly as
-- it did under P2.
BEGIN;

-- installed_images.pinned — P1 declared `pinned` on the wire and always
-- answered false because there was no column. This is that column: a pinned
-- adoption is frozen against EVERY update path (the auto policy and an
-- explicit POST /v1/admin/images/{id}/update, which answers 409 while pinned).
ALTER TABLE installed_images
    ADD COLUMN pinned BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN installed_images.pinned IS
    'P3. When true this adoption''s (version, registry_ref) is frozen: the auto update policy skips it and POST /v1/admin/images/{id}/update answers 409 conflict. Unpinning does not itself update — the next auto sync or an explicit update does.';

-- image_catalog.registry_digest — the #440 fix. registry_ref is the manifest's
-- human-readable TAG (display); this is the content-digest form
-- (name@sha256:<64hex>) resolved from that tag at sync time. Adoption and every
-- ensure dispatch use THIS, never the mutable tag, so two hosts adopting the
-- same version label can never end up on different bits.
--
-- Empty is a legitimate, expected state: registry resolution failure never
-- fails a sync (protocol/control-api.md §Digest pinning). Installing an image
-- whose digest is empty is refused (409 digest_unresolved) until a later sync
-- resolves it.
ALTER TABLE image_catalog
    ADD COLUMN registry_digest TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN image_catalog.registry_digest IS
    'P3 (#440). Content-digest form (registry/name@sha256:<64hex>) resolved from registry_ref''s tag at sync. Empty when the last sync could not resolve it — never a failure, but install/update of that image is refused (409 digest_unresolved) until a later sync fills it. Cleared whenever registry_ref itself moves, so a stale digest can never outlive the tag it was resolved from.';

-- instance_settings sync state — P1 held the last sync''s outcome in a process
-- variable, which invariant #5 (state is external) tolerated only because no
-- phase depended on it surviving a restart. It does now: an operator who
-- restarts the control plane after a failed sync must still see WHY the
-- catalog is stale.
ALTER TABLE instance_settings
    ADD COLUMN image_sync_error TEXT        NOT NULL DEFAULT '',
    ADD COLUMN image_synced_at  TIMESTAMPTZ;

COMMENT ON COLUMN instance_settings.image_sync_error IS
    'P3. The last image-catalog sync''s failure message, or empty when the last sync succeeded. Backs ImageCatalogEnvelope.sync_error (empty ⇒ null on the wire); the wire shape is unchanged, only the storage moved out of process memory.';
COMMENT ON COLUMN instance_settings.image_synced_at IS
    'P3. When the image catalog last synced SUCCESSFULLY. Backs ImageCatalogEnvelope.fetched_at. NULL until the first successful sync; a failed sync leaves the previous value intact (the cached catalog it describes is still what is being served).';

COMMIT;
