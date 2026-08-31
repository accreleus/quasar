-- storage-config — instance-wide storage_provider setting. Additive, signed off
-- on quasar-protocol main @ ca9043b.
-- Prose companion: protocol/schema.md § instance_settings (storage-config amendment).
--
-- Purely additive: one NOT NULL DEFAULT column on the instance_settings singleton.
-- No existing table, column, type, constraint, or the session state machine is
-- touched. Because the column carries a DEFAULT ('auto'), the singleton row does
-- not need re-seeding — the admin UI / PATCH /v1/admin/settings is authoritative
-- once set (like registration_mode), and QUASAR_STORAGE_PROVIDER is a boot fallback.
--
-- One-way-at-deploy (CLAUDE.md): once a stack applies 0030 its control-plane binary
-- must embed >= 0030, or boot crash-loops on the missing migration.

BEGIN;

ALTER TABLE instance_settings
    ADD COLUMN storage_provider TEXT NOT NULL DEFAULT 'auto'
        CHECK (storage_provider IN ('auto','local','volume'));

COMMIT;
