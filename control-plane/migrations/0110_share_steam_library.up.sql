-- 0110 — share the user's Steam library into a desktop session (amendment 53, #1001).
--
-- apps.share_steam_library is the admin's per-app switch, refused on a derived tile and on a
-- library provider. sessions.shared_home_app_id is the Steam parent whose home a session also
-- mounts: the single-writer guard treats it as holding that home. Semantics: schema.md.
BEGIN;

ALTER TABLE apps ADD COLUMN share_steam_library BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE apps ADD CONSTRAINT apps_share_steam_library_ck
    CHECK (NOT share_steam_library OR (parent_app_id IS NULL AND library_provider = ''));

ALTER TABLE sessions ADD COLUMN shared_home_app_id UUID NULL REFERENCES apps(id) ON DELETE SET NULL;
CREATE INDEX sessions_shared_home_live_idx ON sessions (user_id, shared_home_app_id)
    WHERE shared_home_app_id IS NOT NULL AND state NOT IN ('stopped','failed');

COMMIT;
