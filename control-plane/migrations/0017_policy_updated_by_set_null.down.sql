BEGIN;

ALTER TABLE stream_profile_policy
    DROP CONSTRAINT stream_profile_policy_updated_by_fkey,
    ADD CONSTRAINT stream_profile_policy_updated_by_fkey
        FOREIGN KEY (updated_by) REFERENCES users(id);

COMMIT;
