-- stream_profile_policy.updated_by → ON DELETE SET NULL (2026-06-26).
--
-- The singleton stream_profile_policy row records which admin last updated the
-- policy via updated_by, a bare REFERENCES users(id) (NO ACTION) added in
-- migration 0015. With NO ACTION, deleting that admin (DELETE /v1/users/{id},
-- #154) fails with a foreign-key violation. Switch the FK to SET NULL so the
-- policy row survives the admin's deletion with updated_by = NULL.
--
-- Constraint name per information_schema: stream_profile_policy_updated_by_fkey

BEGIN;

ALTER TABLE stream_profile_policy
    DROP CONSTRAINT stream_profile_policy_updated_by_fkey,
    ADD CONSTRAINT stream_profile_policy_updated_by_fkey
        FOREIGN KEY (updated_by) REFERENCES users(id) ON DELETE SET NULL;

COMMIT;
