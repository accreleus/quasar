-- P5-05 erratum to P5-01: make user_homes.user_id and app_id NULLable with
-- ON DELETE SET NULL so that deleting a user/app orphans the row (NULL user_id)
-- rather than hard-failing the DELETE. Orphan rows (NULL user_id) are the
-- pending-GC records that the home janitor sweeps after a 24h grace period.

BEGIN;

-- Drop the NOT NULL constraints and re-add with ON DELETE SET NULL.
-- pgx does not allow adding ON DELETE clauses to existing FKs, so we drop + re-add.

ALTER TABLE user_homes
    ALTER COLUMN user_id DROP NOT NULL,
    DROP CONSTRAINT user_homes_user_id_fkey,
    ADD CONSTRAINT user_homes_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE user_homes
    ALTER COLUMN app_id DROP NOT NULL,
    DROP CONSTRAINT user_homes_app_id_fkey,
    ADD CONSTRAINT user_homes_app_id_fkey
        FOREIGN KEY (app_id) REFERENCES apps(id) ON DELETE SET NULL;

COMMIT;
