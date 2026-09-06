BEGIN;
DROP TRIGGER steam_preparation_adoption ON installed_images;
DROP FUNCTION quasar_refresh_steam_preparation();
ALTER TABLE hosts DROP COLUMN source_preparation_connection_id, DROP COLUMN source_policy_versions, DROP COLUMN source_preparation, DROP COLUMN source_preparation_reported_at;
ALTER TABLE instance_settings DROP COLUMN steam_preparation_enabled, DROP COLUMN steam_preparation_revision, DROP COLUMN steam_preparation_image;
COMMIT;
