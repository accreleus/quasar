-- 0045_library_discovery.down.sql — reverse of 0045 in symmetric order.
--
-- WHAT IS LOST, stated plainly:
--
--   * every observation (the per-user record of what was seen installed). It is
--     rebuilt in full by the next successful scan, so this is a cache loss, not
--     a data loss;
--   * every operator-written appid rule. THIS ONE IS NOT REBUILT. Rolling this
--     migration down and then forward again resurrects every appid an admin had
--     ignored, because layer 1 is a code constant and layer 2 was the only
--     record of their decision. An operator who intends to roll forward again
--     should dump library_appid_rules first;
--   * the discovery toggle, which reverts to "off" on a roll-forward.
--
-- WHAT IS DELIBERATELY *NOT* TOUCHED: the discovered `apps` rows and their
-- entitlements. Dropping the tables that explain WHERE a tile came from is not a
-- reason to delete the tile — `apps` cascades to user_app_favourites and
-- app_artwork, so a DELETE here would destroy every user's favourite of a
-- discovered game and its artwork irreversibly, which is the same call §8.2
-- makes for suppression and 0044's down migration makes for tiles. The tiles
-- survive as ordinary derived tiles (origin='discovered', which 0044 owns) and
-- keep working; only the machinery that would refresh them is gone.
--
-- NOTE the one-way rule in CLAUDE.md: never roll a control-plane binary back
-- BELOW the DB's applied migration version. This file is for a deliberate,
-- operator-run down-migration, not for "deploy main over the phase branch".
BEGIN;

ALTER TABLE instance_settings
    DROP COLUMN IF EXISTS library_discovery_enabled;

DROP TABLE IF EXISTS library_appid_rules;

-- The two indexes on library_observations and the three on library_scans go
-- with their tables; naming them would be noise, unlike 0044 where the indexes
-- outlived the columns they were named for.
DROP TABLE IF EXISTS library_observations;
DROP TABLE IF EXISTS library_scans;

COMMIT;
