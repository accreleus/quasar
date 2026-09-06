BEGIN;
ALTER TABLE instance_settings ADD COLUMN steam_preparation_enabled boolean NOT NULL DEFAULT true,
 ADD COLUMN steam_preparation_revision bigint NOT NULL DEFAULT 1 CHECK (steam_preparation_revision > 0),
 ADD COLUMN steam_preparation_image jsonb;
ALTER TABLE hosts ADD COLUMN source_preparation_connection_id text,
 ADD COLUMN source_policy_versions jsonb,
 ADD COLUMN source_preparation jsonb,
 ADD COLUMN source_preparation_reported_at timestamptz;
-- Freeze eligibility together with the adopted version/digest. Catalog refreshes
-- alone cannot authorize a different image or silently change an adoption.
CREATE FUNCTION quasar_refresh_steam_preparation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE identity jsonb;
BEGIN
 IF TG_OP = 'DELETE' THEN
  IF OLD.image_id <> 'steam' THEN RETURN NULL; END IF;
 ELSIF NEW.image_id <> 'steam' THEN RETURN NULL;
 ELSIF TG_OP = 'UPDATE' AND NEW.registry_ref IS NOT DISTINCT FROM OLD.registry_ref AND NEW.version IS NOT DISTINCT FROM OLD.version THEN RETURN NULL;
 END IF;
 SELECT jsonb_build_object('image_id',ii.image_id,'registry_ref',ii.registry_ref,'version',ii.version)
 INTO identity FROM installed_images ii JOIN image_catalog ic ON ic.id=ii.image_id
 WHERE ii.image_id='steam' AND ic.kind='prebuilt' AND ic.library_provider='steam'
 AND ic.runtime->>'managed_home'='true'
 AND ii.registry_ref ~ '^ghcr.io/accreleus/quasar-steam@sha256:[0-9a-f]{64}$';
 UPDATE instance_settings SET steam_preparation_image=identity,
 steam_preparation_revision=steam_preparation_revision+1
 WHERE id=true AND steam_preparation_image IS DISTINCT FROM identity;
 RETURN NULL;
END $$;
CREATE TRIGGER steam_preparation_adoption AFTER INSERT OR DELETE OR UPDATE OF registry_ref,version ON installed_images
 FOR EACH ROW EXECUTE FUNCTION quasar_refresh_steam_preparation();
UPDATE instance_settings s SET steam_preparation_image=(
 SELECT jsonb_build_object('image_id',ii.image_id,'registry_ref',ii.registry_ref,'version',ii.version)
 FROM installed_images ii JOIN image_catalog ic ON ic.id=ii.image_id
 WHERE ii.image_id='steam' AND ic.kind='prebuilt' AND ic.library_provider='steam'
 AND ic.runtime->>'managed_home'='true'
 AND ii.registry_ref ~ '^ghcr.io/accreleus/quasar-steam@sha256:[0-9a-f]{64}$');
COMMIT;
