BEGIN;
DROP INDEX IF EXISTS app_artwork_icon_asset_idx;
DROP INDEX IF EXISTS app_artwork_logo_asset_idx;
ALTER TABLE app_artwork
	DROP COLUMN IF EXISTS logo_icon_checked,
	DROP COLUMN IF EXISTS icon_asset,
	DROP COLUMN IF EXISTS logo_asset;
ALTER TABLE apps
	DROP COLUMN IF EXISTS icon_url,
	DROP COLUMN IF EXISTS logo_url,
	DROP COLUMN IF EXISTS store_details_at,
	DROP COLUMN IF EXISTS controller_support,
	DROP COLUMN IF EXISTS release_date,
	DROP COLUMN IF EXISTS publisher,
	DROP COLUMN IF EXISTS developer,
	DROP COLUMN IF EXISTS genres;
COMMIT;
