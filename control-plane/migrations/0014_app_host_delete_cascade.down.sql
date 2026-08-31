-- Reverse of 0014: restore RESTRICT FK behaviour for sessions.app_id,
-- sessions.host_id, and user_homes.host_id.
-- WARNING: will fail if any rows with NULL user_homes.host_id exist after SET NULL triggered.

BEGIN;

ALTER TABLE sessions
    DROP CONSTRAINT sessions_app_id_fkey,
    ADD CONSTRAINT sessions_app_id_fkey
        FOREIGN KEY (app_id) REFERENCES apps(id);

ALTER TABLE sessions
    DROP CONSTRAINT sessions_host_id_fkey,
    ADD CONSTRAINT sessions_host_id_fkey
        FOREIGN KEY (host_id) REFERENCES hosts(id);

ALTER TABLE user_homes
    DROP CONSTRAINT user_homes_host_id_fkey,
    ADD CONSTRAINT user_homes_host_id_fkey
        FOREIGN KEY (host_id) REFERENCES hosts(id);

COMMIT;
