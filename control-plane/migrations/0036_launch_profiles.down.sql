-- 0036_launch_profiles.down.sql — SNAPSHOT RESTORE, not a computed collapse.
--
-- The 0036 fan-out is LOSSY in two directions and therefore cannot be inverted
-- by computation:
--   * a single h264 rung cannot be distinguished from "`codecs` was NULL" versus
--     "`codecs` was the default list explicitly stored"; those are different rows
--     and reconstructing the wrong one changes what the pre-0036 code reads;
--   * `future` and `unsupported` entries are dropped by the fan-out and leave no
--     trace in the rungs at all.
-- So this restores the `_backup_0036_*` snapshot the up migration took as its
-- first act.
--
-- ============================ TWO HONESTY CLAUSES ============================
--
--   1. Any admin write made after this migration was applied is LOST by the down
--      migration. The down path restores the pre-migration snapshot verbatim. If
--      an operator edited a stream profile, the policy, or an app's profile
--      assignment while 0036 was applied, that edit does not survive a rollback.
--
--   2. A launch profile created after this migration was applied is DROPPED, not
--      collapsed. It has no pre-migration counterpart and the fan-out cannot be
--      inverted. The down migration emits a RAISE NOTICE naming every such launch
--      profile before dropping it.
--
-- ============================================================================
BEGIN;

-- ── (1) Name every launch profile that has no pre-migration counterpart. ────
-- These are DROPPED, not collapsed (honesty clause 2). Anything pointing at one
-- is cleared below, because after the restore that id does not exist as a
-- stream_profiles row and the recreated foreign keys would refuse it.
DO $$
DECLARE lp RECORD;
BEGIN
    FOR lp IN
        SELECT id FROM launch_profiles
         WHERE id NOT IN (SELECT id FROM _backup_0036_stream_profiles)
         ORDER BY id
    LOOP
        RAISE NOTICE '0036 down: launch profile "%" was created after 0036 was applied and has no pre-migration counterpart — DROPPING it (the fan-out cannot be inverted). Anything referencing it is cleared to NULL.', lp.id;
    END LOOP;
END $$;

-- Clear post-0036 references that the restored (pre-0036) stream_profiles set
-- cannot satisfy. Rows present in the backup are restored verbatim below, so
-- this only affects rows created/edited after the up migration.
UPDATE apps
   SET default_profile_id = NULL
 WHERE default_profile_id IS NOT NULL
   AND default_profile_id NOT IN (SELECT id FROM _backup_0036_stream_profiles);

UPDATE stream_profile_policy
   SET global_default_profile_id = NULL
 WHERE global_default_profile_id IS NOT NULL
   AND global_default_profile_id NOT IN (SELECT id FROM _backup_0036_stream_profiles);

UPDATE user_profile_preferences
   SET default_profile_id = NULL
 WHERE default_profile_id IS NOT NULL
   AND default_profile_id NOT IN (SELECT id FROM _backup_0036_stream_profiles);

-- ── (2) Drop the three repointed foreign keys (they point at launch_profiles). ─
DO $$
DECLARE con RECORD;
BEGIN
    FOR con IN
        SELECT c.conname, c.conrelid::regclass::text AS tbl
          FROM pg_constraint c
          JOIN pg_class ref ON ref.oid = c.confrelid
         WHERE c.contype = 'f'
           AND ref.relname = 'launch_profiles'
           AND c.conrelid::regclass::text IN
               ('stream_profile_policy', 'apps', 'user_profile_preferences')
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', con.tbl, con.conname);
    END LOOP;
END $$;

-- ── (3) Drop the new objects. ──────────────────────────────────────────────
DROP TABLE IF EXISTS launch_profile_rungs;
DROP TABLE IF EXISTS launch_profiles;

-- Drops its own FK onto stream_profiles with it, which TRUNCATE below needs.
ALTER TABLE sessions DROP COLUMN IF EXISTS stream_profile_id;

-- ── (4) Restore stream_profiles verbatim from the snapshot. ────────────────
-- This drops the rung rows and restores the pre-migration `codecs` values in one
-- step — no reconstruction, no guessing.
TRUNCATE stream_profiles;
INSERT INTO stream_profiles SELECT * FROM _backup_0036_stream_profiles;

ALTER TABLE stream_profiles DROP COLUMN IF EXISTS codec;

-- ── (5) Restore the three referencing tables from their snapshots. ─────────
-- Each UPDATE is guarded by IS DISTINCT FROM so an UNCHANGED row is not written
-- at all. That is not an optimisation: all three tables carry a set_updated_at
-- trigger, so an unconditional UPDATE would bump updated_at on every row and the
-- round-trip proof (a byte-identical dump before up and after down) would fail
-- on a database nobody had touched. A row that genuinely changed while 0036 was
-- applied IS rewritten, and its updated_at legitimately moves — that is honesty
-- clause 1 in action, not a defect.
UPDATE stream_profile_policy p
   SET global_default_profile_id = b.global_default_profile_id,
       user_overrides_allowed    = b.user_overrides_allowed
  FROM _backup_0036_stream_profile_policy b
 WHERE p.id = b.id
   AND (p.global_default_profile_id IS DISTINCT FROM b.global_default_profile_id
     OR p.user_overrides_allowed    IS DISTINCT FROM b.user_overrides_allowed);

UPDATE user_profile_preferences u
   SET default_profile_id = b.default_profile_id
  FROM _backup_0036_user_profile_preferences b
 WHERE u.user_id = b.user_id
   AND u.default_profile_id IS DISTINCT FROM b.default_profile_id;

UPDATE apps a
   SET default_profile_id = b.default_profile_id,
       profile_policy     = b.profile_policy
  FROM _backup_0036_apps_profile b
 WHERE a.id = b.id
   AND (a.default_profile_id IS DISTINCT FROM b.default_profile_id
     OR a.profile_policy     IS DISTINCT FROM b.profile_policy);

-- ── (6) Widen the profile_policy CHECK back to include `custom`. ───────────
DO $$
DECLARE cn TEXT;
BEGIN
    SELECT conname INTO cn
      FROM pg_constraint
     WHERE conrelid = 'apps'::regclass
       AND contype = 'c'
       AND pg_get_constraintdef(oid) ILIKE '%profile_policy%';
    IF cn IS NOT NULL THEN
        EXECUTE format('ALTER TABLE apps DROP CONSTRAINT %I', cn);
    END IF;
END $$;

ALTER TABLE apps
    ADD CONSTRAINT apps_profile_policy_check
    CHECK (profile_policy IN ('inherit', 'prefer', 'force', 'custom'));

-- ── (7) Recreate the three foreign keys against stream_profiles. ───────────
ALTER TABLE stream_profile_policy
    ADD CONSTRAINT stream_profile_policy_global_default_profile_id_fkey
    FOREIGN KEY (global_default_profile_id) REFERENCES stream_profiles(id);

ALTER TABLE apps
    ADD CONSTRAINT apps_default_profile_id_fkey
    FOREIGN KEY (default_profile_id) REFERENCES stream_profiles(id);

ALTER TABLE user_profile_preferences
    ADD CONSTRAINT user_profile_preferences_default_profile_id_fkey
    FOREIGN KEY (default_profile_id) REFERENCES stream_profiles(id);

-- ── (8) Drop the snapshot tables. ─────────────────────────────────────────
DROP TABLE IF EXISTS _backup_0036_apps_profile;
DROP TABLE IF EXISTS _backup_0036_user_profile_preferences;
DROP TABLE IF EXISTS _backup_0036_stream_profile_policy;
DROP TABLE IF EXISTS _backup_0036_stream_profiles;

COMMIT;
