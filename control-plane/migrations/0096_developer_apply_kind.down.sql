-- 0096 down. A developer apply is rewritten to 'apply' before the CHECK narrows
-- (narrowing first fails on a live row): it was an apply of a digest set, and
-- its release_id is already NULL. History loses only which button was pressed.
BEGIN;

UPDATE platform_apply_attempts SET kind = 'apply' WHERE kind = 'developer_apply';
ALTER TABLE platform_apply_attempts
    DROP CONSTRAINT platform_apply_attempts_kind_check;
ALTER TABLE platform_apply_attempts
    ADD CONSTRAINT platform_apply_attempts_kind_check
        CHECK (kind IN ('apply', 'revert', 'auto_revert'));

COMMIT;
