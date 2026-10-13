-- Drops the stored requests: a provider app created after this rollback gets
-- the 'all' grant again, as before amendment 21.
DROP TABLE IF EXISTS pending_provider_entitlement_modes;
