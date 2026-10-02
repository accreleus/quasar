-- 0098: the host's container engine, its version and its engine mode (amendment 17,
-- RH-07 #396; protocol/schema.md hosts.engine / engine_version / engine_mode and
-- "RH07 — amendment 17"). Reported on every register and replaced wholesale.
--
-- Purely additive: three nullable columns and their CHECKs, no default, no backfill.
-- Informational: no admission, scheduling or release decision reads them.
--
-- Once applied, never deploy a control-plane binary embedding only <= 0097:
-- boot's m.Up() crash-loops on a database ahead of the binary.
BEGIN;

ALTER TABLE hosts ADD COLUMN engine TEXT NULL;
ALTER TABLE hosts ADD COLUMN engine_version TEXT NULL;
ALTER TABLE hosts ADD COLUMN engine_mode TEXT NULL;
ALTER TABLE hosts
    ADD CONSTRAINT hosts_engine_check
        CHECK (engine IS NULL OR engine ~ '^[a-z][a-z0-9-]{0,31}$');
ALTER TABLE hosts
    ADD CONSTRAINT hosts_engine_version_check
        CHECK (engine_version IS NULL OR engine_version ~ '^[!-~]{1,64}$');
ALTER TABLE hosts
    ADD CONSTRAINT hosts_engine_mode_check
        CHECK (engine_mode IS NULL OR engine_mode IN ('rootful', 'rootless'));

COMMENT ON COLUMN hosts.engine IS
    'amendment 17: the container engine the host''s agent drives (docker, podman, or another lowercase token). NULL = unknown.';
COMMENT ON COLUMN hosts.engine_version IS
    'amendment 17: the engine''s own product version, opaque. NULL = unknown.';
COMMENT ON COLUMN hosts.engine_mode IS
    'amendment 17: rootful or rootless, as the engine reports itself. NULL = unknown. Not the storage code''s "rootless" (no storage root).';

COMMIT;
