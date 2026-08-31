-- Down migration for 0068 is a deliberate no-op.
--
-- The up migration is a lossy data coercion (UPDATE instance_settings SET
-- storage_provider = 'local' WHERE storage_provider = 'volume'): once applied,
-- there is no column recording which rows were 'volume' before it ran, so
-- there is nothing this file could restore without guessing. Since #473 is a
-- hard removal with no back-compat requirement (Quasar is unreleased), and
-- CLAUDE.md's migration policy is one-way at deploy time regardless, a
-- fabricated "put it back to volume" step would be worse than doing nothing:
-- it would silently resurrect a driver the application no longer accepts
-- writes for (internal/settings.ValidStorageProvider still rejects "volume"),
-- so the instance would be stuck on a value nothing can set again.
BEGIN;
COMMIT;
