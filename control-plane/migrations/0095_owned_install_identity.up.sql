-- 0095: owned installs, the recovery actor's and seed's identity on hosts
-- (amendment 14, #353; protocol/schema.md hosts and "RH06 — amendment 14").
--
-- Purely additive: three nullable columns and one widened CHECK, no backfill.
-- pre_update_dump and the developer_apply attempt kind (the rest of amendment
-- 14's schema step) land with the slices that first write them.
--
-- Once applied, never deploy a control-plane binary embedding only <= 0094:
-- boot's m.Up() crash-loops on a database ahead of the binary.
BEGIN;

ALTER TABLE hosts
    DROP CONSTRAINT hosts_install_mode_check;
ALTER TABLE hosts
    ADD CONSTRAINT hosts_install_mode_check
    CHECK (install_mode IS NULL OR install_mode IN ('registry', 'source', 'owned'));

-- Replaced wholesale on every register (absent => NULL), like 0074's identity
-- columns, and NULL on every host that is not owned.
ALTER TABLE hosts
    ADD COLUMN recovery_actor_version       TEXT NULL,
    ADD COLUMN recovery_actor_source_commit TEXT NULL,
    ADD COLUMN seed_version                 TEXT NULL;

COMMENT ON COLUMN hosts.install_mode IS
    'amendments 1 and 14: registry = pulled published images, source = built on the host, owned = created and replaced by the machine''s recovery actor. A source host can be told about a release but never given one.';
COMMENT ON COLUMN hosts.recovery_actor_version IS
    'amendment 14: MAJOR.MINOR.PATCH[-prerelease] of the recovery actor serving this owned host''s machine; an unparseable report is stored NULL. Wholesale-replaced on every register.';
COMMENT ON COLUMN hosts.recovery_actor_source_commit IS
    'amendment 14: git commit that recovery actor was built from, 7-40 lowercase hex, stored exactly as sent. Wholesale-replaced on every register.';
COMMENT ON COLUMN hosts.seed_version IS
    'amendment 14: opaque version of the seed the recovery actor last saw, stored as sent. Informational only (ADR 0007). Wholesale-replaced on every register.';

COMMIT;
