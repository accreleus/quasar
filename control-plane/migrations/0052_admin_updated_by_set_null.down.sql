-- Restore the bare (NO ACTION) FKs from migrations 0010 / 0022.
BEGIN;

ALTER TABLE host_settings
    DROP CONSTRAINT host_settings_updated_by_fkey,
    ADD CONSTRAINT host_settings_updated_by_fkey
        FOREIGN KEY (updated_by) REFERENCES users(id);

ALTER TABLE console_config
    DROP CONSTRAINT console_config_updated_by_fkey,
    ADD CONSTRAINT console_config_updated_by_fkey
        FOREIGN KEY (updated_by) REFERENCES users(id);

COMMIT;
