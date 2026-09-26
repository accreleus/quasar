-- 0096: the developer apply attempt kind (amendment 14, owner addition on #353;
-- protocol/schema.md platform_apply_attempts.kind and "RH06 — amendment 14").
--
-- A plain CHECK swap: an admin's developer apply of an arbitrary digest set to
-- one owned target (control-api.md "Developer apply"), with release_id NULL.
-- pre_update_dump, the rest of amendment 14's attempt columns, lands with the
-- migrating-update slice that first writes it.
--
-- Once applied, never deploy a control-plane binary embedding only <= 0095:
-- boot's m.Up() crash-loops on a database ahead of the binary.
BEGIN;

ALTER TABLE platform_apply_attempts
    DROP CONSTRAINT platform_apply_attempts_kind_check;
ALTER TABLE platform_apply_attempts
    ADD CONSTRAINT platform_apply_attempts_kind_check
        CHECK (kind IN ('apply', 'revert', 'auto_revert', 'developer_apply'));

COMMIT;
