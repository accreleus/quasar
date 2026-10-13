-- 0100_pending_provider_entitlement_modes.up.sql — amendment 21 (#490): an
-- entitlement mode requested before its provider app exists. EnsureProviderApp
-- applies and deletes the row when it creates the app; no row means 'all'.
-- Prose: protocol/schema.md `pending_provider_entitlement_modes`.
BEGIN;

CREATE TABLE pending_provider_entitlement_modes (
    provider     TEXT PRIMARY KEY,
    mode         TEXT NOT NULL CHECK (mode IN ('all', 'user', 'none')),
    -- SET NULL, never CASCADE: a cascade would delete the request and the app
    -- would be created open to everyone. A 'user' row with no requester applies
    -- as zero grants.
    requested_by UUID NULL REFERENCES users(id) ON DELETE SET NULL,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
