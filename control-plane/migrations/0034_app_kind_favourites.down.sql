-- 0034_app_kind_favourites.down.sql — reverse of 0034 in symmetric order.
BEGIN;

DROP TABLE IF EXISTS user_app_favourites;

ALTER TABLE apps
    DROP COLUMN kind;

COMMIT;
