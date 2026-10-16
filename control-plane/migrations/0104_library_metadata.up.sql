-- 0104_library_metadata.up.sql — control-api.md amendment 38 (#720): per-title
-- store details and the logo and icon crops. Prose: protocol/schema.md `apps`,
-- `app_artwork` and the 0104 migration line. No data is written here: the
-- library scan backfill fills store details, the artwork sweep logos and icons.
BEGIN;

ALTER TABLE apps
	ADD COLUMN IF NOT EXISTS genres TEXT[] NOT NULL DEFAULT '{}',
	ADD COLUMN IF NOT EXISTS developer TEXT,
	ADD COLUMN IF NOT EXISTS publisher TEXT,
	ADD COLUMN IF NOT EXISTS release_date TEXT,
	ADD COLUMN IF NOT EXISTS controller_support TEXT
		CHECK (controller_support IS NULL OR controller_support IN ('full', 'partial')),
	ADD COLUMN IF NOT EXISTS store_details_at TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS logo_url TEXT,
	ADD COLUMN IF NOT EXISTS icon_url TEXT;

ALTER TABLE app_artwork
	ADD COLUMN IF NOT EXISTS logo_asset TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS icon_asset TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS logo_icon_checked BOOLEAN NOT NULL DEFAULT false;

-- The orphan sweep's keep-set reads these (ReferencedAssets).
CREATE INDEX IF NOT EXISTS app_artwork_logo_asset_idx ON app_artwork (logo_asset) WHERE logo_asset <> '';
CREATE INDEX IF NOT EXISTS app_artwork_icon_asset_idx ON app_artwork (icon_asset) WHERE icon_asset <> '';

COMMIT;
