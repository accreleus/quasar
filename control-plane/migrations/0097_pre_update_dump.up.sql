-- 0097: the pre-update dump reference on control-plane attempts (amendment 14,
-- #353; protocol/schema.md platform_apply_attempts.pre_update_dump and
-- "RH06 — amendment 14"). Written by the migrating-update slice (#364).
--
-- Purely additive: one nullable column and its two CHECKs, no backfill. The
-- name is the one the recovery actor's `restore` command takes; the dump itself
-- stays in that machine's machine state, never in Postgres.
--
-- Once applied, never deploy a control-plane binary embedding only <= 0096:
-- boot's m.Up() crash-loops on a database ahead of the binary.
BEGIN;

ALTER TABLE platform_apply_attempts
    ADD COLUMN pre_update_dump TEXT NULL;
ALTER TABLE platform_apply_attempts
    ADD CONSTRAINT platform_apply_attempts_pre_update_dump_target_check
        CHECK (pre_update_dump IS NULL OR target = 'control_plane');
ALTER TABLE platform_apply_attempts
    ADD CONSTRAINT platform_apply_attempts_pre_update_dump_length_check
        CHECK (pre_update_dump IS NULL OR octet_length(pre_update_dump) <= 255);

COMMENT ON COLUMN platform_apply_attempts.pre_update_dump IS
    'amendment 14: the name of the pre-update dump the recovery actor took before replacing the control plane with a migrating release, as its restore command takes it. Opaque; NULL on every host attempt, a non-migrating or external-database control-plane attempt, and a registry control plane.';

COMMIT;
