-- Schema rollback only. Does not touch user_homes or physical backing data.
ALTER TABLE sessions
    DROP CONSTRAINT IF EXISTS sessions_managed_home_binding_ck,
    DROP COLUMN IF EXISTS managed_home_mount_sha256,
    DROP COLUMN IF EXISTS managed_home_id;
DROP TRIGGER IF EXISTS managed_home_claim_host_deleted_before ON hosts;
DROP FUNCTION IF EXISTS managed_home_claim_host_deleted();
DROP TABLE IF EXISTS managed_home_claims;
