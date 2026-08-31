-- #473 — hard removal of the docker-volume managed-home driver (operator
-- direction 2026-08-25). Quasar is unreleased, so this is a clean removal, not
-- a deprecation: no migration path is owed to an existing volume-backed home
-- (the devbox gets a virgin redeploy the same night this lands).
--
-- WHAT THIS DOES AND DOES NOT TOUCH.
--
-- internal/settings.ValidStorageProvider now rejects "volume" outright (a
-- PATCH /v1/admin/settings carrying it gets a 400 naming the removal and
-- pointing at QUASAR_HOME_ROOT), so instance_settings.storage_provider can
-- never be WRITTEN as 'volume' again through the application. This migration
-- is the one-time DATA fix for a row that already holds it: a plain UPDATE,
-- not a schema change. It deliberately does NOT touch the CHECK constraint
-- (`storage_provider IN ('auto','local','volume')`, migration 0030) or
-- user_homes.provider's CHECK (`IN ('volume','local')`, migration 0008) —
-- those are the wire shape protocol/schema.md documents, a frozen contract
-- (CLAUDE.md) that needs Opus + explicit human sign-off to change, out of
-- scope for this control-plane-only ticket. Leaving the CHECK in place is
-- also what lets a legacy user_homes row (a home actually backed by a docker
-- volume from before this removal) keep existing and being read/reaped by the
-- node-agent's GC — see #473's control-plane inventory: node-agent's home GC
-- (`session::gc`) still recognizes "local" only for reaping and logs+skips
-- anything else, so an old volume-backed row is left for manual cleanup
-- rather than mishandled, which this migration does not need to change.
--
-- Only instance_settings.storage_provider is coerced — the single value that
-- drives internal/storage.Manager.resolveDriver's per-launch decision.
-- Individual user_homes.provider rows are bookkeeping for homes that already
-- exist; #473's operator direction is explicit that no back-compat migration
-- for those is required, so they are left as recorded history.
BEGIN;

UPDATE instance_settings
SET storage_provider = 'local'
WHERE storage_provider = 'volume';

COMMIT;
