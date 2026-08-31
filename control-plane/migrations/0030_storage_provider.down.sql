-- Revert storage-config (0030) — drop the storage_provider column. See the up
-- migration + CLAUDE.md's one-way-at-deploy rule: only roll back a stack whose
-- binary still embeds <= this version.

BEGIN;

ALTER TABLE instance_settings DROP COLUMN IF EXISTS storage_provider;

COMMIT;
