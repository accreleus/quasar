-- Drops the override table and the three derived columns. The loss is the
-- admins' override decisions, and the effect is that nothing blocks — an older
-- control plane does not read readiness at all (schema.md rollback note).
BEGIN;

DROP TABLE host_readiness_overrides;

ALTER TABLE gpus
    DROP COLUMN readiness_blocked;

ALTER TABLE hosts
    DROP COLUMN readiness_block_host,
    DROP COLUMN readiness_block_homes;

COMMIT;
