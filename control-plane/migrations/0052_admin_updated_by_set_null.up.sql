-- host_settings.updated_by / console_config.updated_by → ON DELETE SET NULL.
--
-- Both columns were added as a bare REFERENCES users(id) (migrations 0010 and
-- 0022), i.e. NO ACTION: once an admin has saved host settings or console config,
-- deleting that admin fails with a foreign-key violation (23503). Migration 0017
-- fixed exactly this for stream_profile_policy.updated_by; these two were missed.
--
-- Two consequences, one old and one new:
--   - DELETE /v1/users/{id} on a real admin who ever touched either surface has
--     always failed. That is a pre-existing bug, fixed here.
--   - The dev-agent reaper (#399) deletes ephemeral identities, and role=admin
--     mints are the endpoint's stated purpose. An ephemeral admin that saved host
--     settings would become undeletable and be retried every sweep, forever.
--
-- Same treatment, same reasoning as 0017: the audit trail's VALUE (which admin
-- last changed this) must not outrank the ability to delete an account. The
-- config row survives with updated_by = NULL.
--
-- Constraint names per information_schema: <table>_updated_by_fkey.
BEGIN;

ALTER TABLE host_settings
    DROP CONSTRAINT host_settings_updated_by_fkey,
    ADD CONSTRAINT host_settings_updated_by_fkey
        FOREIGN KEY (updated_by) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE console_config
    DROP CONSTRAINT console_config_updated_by_fkey,
    ADD CONSTRAINT console_config_updated_by_fkey
        FOREIGN KEY (updated_by) REFERENCES users(id) ON DELETE SET NULL;

COMMIT;
