-- Reverse of 0039. Dropping app_artwork loses provenance (which blob backs
-- which app, and which matches an admin had corrected); the cached blobs on
-- disk survive, so a re-apply re-fetches rather than re-uses them. Acceptable:
-- the down path exists for a rollback, not as a routine operation, and the only
-- data lost is a cache plus its bookkeeping.
DROP INDEX IF EXISTS idx_app_artwork_hero_asset;
DROP INDEX IF EXISTS idx_app_artwork_tile_asset;
DROP TABLE IF EXISTS app_artwork;
ALTER TABLE apps DROP COLUMN IF EXISTS hero_url;
