-- Schema rollback only. Does not touch user_homes or physical backing data.
ALTER TABLE sessions
    DROP CONSTRAINT IF EXISTS sessions_managed_home_binding_ck,
    DROP COLUMN IF EXISTS managed_home_mount_sha256,
    DROP COLUMN IF EXISTS managed_home_id;
DROP TRIGGER IF EXISTS managed_home_claim_host_deleted_before ON hosts;
DROP FUNCTION IF EXISTS managed_home_claim_host_deleted();
DROP TRIGGER IF EXISTS rh05_guard_user_delete_home_hold ON users;
DROP TRIGGER IF EXISTS rh05_guard_parent_app_delete_home_hold ON apps;
DROP TRIGGER IF EXISTS rh05_guard_managed_home_claim_delete ON managed_home_claims;
DROP FUNCTION IF EXISTS rh05_guard_user_delete_home_hold_fn();
DROP FUNCTION IF EXISTS rh05_guard_parent_app_delete_home_hold_fn();
DROP FUNCTION IF EXISTS rh05_guard_managed_home_claim_delete_fn();
DROP INDEX IF EXISTS managed_home_claims_pending_home_session_idx;
DROP TABLE IF EXISTS managed_home_claims;
