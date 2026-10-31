BEGIN;
DROP INDEX IF EXISTS sessions_shared_home_live_idx;
ALTER TABLE sessions DROP COLUMN IF EXISTS shared_home_app_id;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_share_steam_library_ck;
ALTER TABLE apps DROP COLUMN IF EXISTS share_steam_library;
COMMIT;
