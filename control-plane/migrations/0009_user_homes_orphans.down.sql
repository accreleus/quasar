-- Reverse of 0009: restore NOT NULL + no-cascade FKs on user_homes.
-- WARNING: this will fail if any rows have NULL user_id or app_id.

BEGIN;

ALTER TABLE user_homes
    DROP CONSTRAINT user_homes_app_id_fkey,
    ADD CONSTRAINT user_homes_app_id_fkey
        FOREIGN KEY (app_id) REFERENCES apps(id),
    ALTER COLUMN app_id SET NOT NULL;

ALTER TABLE user_homes
    DROP CONSTRAINT user_homes_user_id_fkey,
    ADD CONSTRAINT user_homes_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id),
    ALTER COLUMN user_id SET NOT NULL;

COMMIT;
