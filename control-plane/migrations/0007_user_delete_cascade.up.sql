-- Admin user deletion (additive, admin-gated — control-api.md §Authorization
-- exception class). Deleting a user removes their session history; session
-- rows cascade onward to session_metrics (0003). Active sessions are refused
-- at the API layer before the DELETE is attempted, so only terminal history
-- is ever cascaded.

BEGIN;

ALTER TABLE sessions
    DROP CONSTRAINT sessions_user_id_fkey,
    ADD CONSTRAINT sessions_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;

COMMIT;
