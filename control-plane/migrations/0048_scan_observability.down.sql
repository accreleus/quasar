-- 0048_scan_observability.down.sql — reverse of 0048.
--
-- The eight count columns carry no foreign-key-shaped state and nothing else
-- reads them structurally (they are a reporting surface, not a join key), so
-- they are dropped unconditionally — same posture as 0047's instance_settings
-- column drop.
BEGIN;

ALTER TABLE library_scans
    DROP COLUMN IF EXISTS observed,
    DROP COLUMN IF EXISTS suppressed,
    DROP COLUMN IF EXISTS created,
    DROP COLUMN IF EXISTS disabled,
    DROP COLUMN IF EXISTS granted,
    DROP COLUMN IF EXISTS revoked,
    DROP COLUMN IF EXISTS rejected,
    DROP COLUMN IF EXISTS backfilled;

COMMIT;
