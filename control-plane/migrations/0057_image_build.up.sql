-- 0057_image_build.up.sql — image management P4 (template builds: agent-side
-- docker build). docs/design/plans/2026-08-08-image-management-p4-spec.md
-- §"Data model", protocol/schema.md P4 amendment, protocol/agent-api.md
-- §image_build.
--
-- Purely additive: two NOT NULL DEFAULT '' columns on two existing tables.
-- Nothing changes behaviour on its own — an instance that never syncs/installs
-- a kind=template catalog entry reads context_sha='' and local_tag='' and
-- behaves exactly as it did under P3.
BEGIN;

-- image_catalog.context_sha — the resolved commit sha a template's build
-- context is pinned to, the deterministic analogue of P3's registry_digest for
-- a prebuilt entry. Resolved once per sync (the manifest's own catalog ref
-- resolved to a commit sha) and stamped onto every kind=template row that sync
-- wrote. Sent to the agent as the pinned ref inside image_build's context_url
-- (a codeload tarball URL), never a floating branch/tag.
--
-- Empty is a legitimate, expected state: sha resolution failure never fails a
-- sync (mirrors #440's digest-resolution contract). Installing a template
-- whose context_sha is empty is refused (409 context_unresolved) until a later
-- sync resolves it.
ALTER TABLE image_catalog
    ADD COLUMN context_sha TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN image_catalog.context_sha IS
    'P4. Commit sha the template''s build context (quasar-images repo, the configured catalog ref) is pinned to at sync — the template analogue of registry_digest. Empty when the last sync could not resolve it; install/update of that template is then refused (409 context_unresolved) until a later sync fills it.';

-- installed_images.local_tag — the CP-assigned, deterministic build tag
-- (quasar-local/<image_id>:<version>) captured at adoption for a template
-- install/update. A template's installed_images.registry_ref stays empty and
-- local_tag carries the tag; a prebuilt's local_tag stays empty and
-- registry_ref carries the digest ref — dispatch (image_ensure vs image_build)
-- and launch placement match an app's image against whichever of the two is
-- populated (never a registry ref, and never pushed anywhere).
ALTER TABLE installed_images
    ADD COLUMN local_tag TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN installed_images.local_tag IS
    'P4. CP-assigned local build tag (quasar-local/<image_id>:<version>) for a template adoption, captured at install/update — the template analogue of the P3 rule that registry_ref holds the immutable adopted ref. Empty for a prebuilt adoption.';

-- installed_images build-input snapshot — EVERY build-defining input frozen at
-- adoption (review 2026-08-08). Reading these live from image_catalog at
-- dispatch was the template twin of the #440 fleet-split bug: a later catalog
-- sync could rebuild DIFFERENT bits under the adopted version, or blank
-- context_sha and make an adopted template un-dispatchable. Dispatch reads these
-- frozen columns, never the live catalog; an admin update re-snapshots them
-- transactionally with version/local_tag. All empty/'{}' for a prebuilt adoption.
--
-- context_repo — the "owner/name" repo the build context tarball is fetched from
-- (the configured catalog repo at adoption time). Frozen so dispatch never
-- consults a live env snapshot whose authority could drift from the adopted sha.
-- context_sha — the resolved commit sha the build context is pinned to, copied
-- from image_catalog.context_sha at adoption (the catalog's own column stays the
-- sync-time resolution that GATES install, but is never the dispatch source once
-- adopted). dockerfile — the manifest's dockerfile path, split at dispatch into
-- (context_subdir, dockerfile). build_args — the validated string=>string build
-- args, sent verbatim into image_build.
ALTER TABLE installed_images
    ADD COLUMN context_repo TEXT NOT NULL DEFAULT '',
    ADD COLUMN context_sha  TEXT NOT NULL DEFAULT '',
    ADD COLUMN dockerfile   TEXT NOT NULL DEFAULT '',
    ADD COLUMN build_args    JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMENT ON COLUMN installed_images.context_repo IS
    'P4. "owner/name" of the build-context repo, frozen at adoption — dispatch builds image_build.context_url from this + context_sha, never a live env snapshot.';
COMMENT ON COLUMN installed_images.context_sha IS
    'P4. Commit sha the template build context is pinned to, copied from image_catalog.context_sha AT ADOPTION. The dispatch source once adopted (the catalog column only gates install). Empty for a prebuilt adoption.';
COMMENT ON COLUMN installed_images.dockerfile IS
    'P4. Manifest dockerfile path frozen at adoption; split at dispatch into image_build''s (context_subdir, dockerfile). Empty for a prebuilt adoption.';
COMMENT ON COLUMN installed_images.build_args IS
    'P4. Validated string=>string docker build args frozen at adoption; sent verbatim into image_build. ''{}'' for a prebuilt adoption.';

COMMIT;
