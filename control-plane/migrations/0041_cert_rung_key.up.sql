-- 0041_cert_rung_key.up.sql — re-key host_encoder_certification on the RUNG.
--
-- THE DEFECT (respec §4.7, fast-follow §9.1). Migration 0018 keyed the measured
-- encode envelope on
--     (host_id, gpu_index, encoder, profile_id, bitrate_kbps)
-- where `encoder` is the BACKEND family (va / nvenc / openh264, + vulkan since
-- 0019) and `profile_id` is a LAUNCH PROFILE id since 0036. So the key has no
-- codec dimension and no resolution dimension of its own — it inherits whatever
-- the launch profile happened to mean.
--
-- Encode cost is codec- AND resolution-dependent: h264, HEVC and AV1 at the same
-- resolution are very different loads on the same silicon, and 4K is a different
-- load from 1080p. Before Phase 4 that was harmless (one stream profile = one
-- resolution = one effectively-certified codec). Post-Phase-4 a launch profile
-- CHAINS rungs for up to all three codecs, and `launch_profile_rungs` only
-- constrains UNIQUE (launch_profile_id, stream_profile_id) — a chain may legally
-- hold two rungs of the same codec at different resolutions. So a verdict
-- measured on the h264 rung was being applied to an AV1 or HEVC launch at the
-- same launch profile and bitrate: it can wrongly cap a launch, or wrongly clear
-- one.
--
-- THE KEY: the RUNG (stream_profiles.id), not (profile_id, codec).
--   * A cert row records the sustainable envelope of ONE concrete
--     (resolution, fps, codec) at one bitrate. That object is exactly a rung.
--   * (profile_id, codec) is only unambiguous while every chain is
--     single-resolution, which the model does not promise.
--   * It matches what Phase 4 did to the OTHER wrong-grained per-profile table.
--     Migration 0032 re-keyed user_device_profile_history on (profile, codec);
--     Phase 4 then moved decode-failure history to RUNG grain (Store.RungFailures,
--     cert_history.go) for this exact reason, spelled out there: "that key is
--     wrong-grained in both directions: two rungs may share a codec at different
--     resolutions ... and decode failure is RESOLUTION-dependent". Encode cost is
--     resolution-dependent for the same reason.
--   * sessions.stream_profile_id (0036) already records the resolved rung, so a
--     cert joins directly to what actually ran.
--
-- `profile_id` is KEPT, still NOT NULL, and demoted to CONTEXT: the launch
-- profile the rung was certified under. It is no longer part of the key and no
-- longer read by the scheduler cap; it stays because the admin read filters on it
-- and because a rung on its own does not tell an operator which chain the bench
-- was exercising.
--
-- MIGRATING THE EXISTING ROWS — they are h264 measurements, not a guess.
-- The SPT-06 bench (Coordinator.launchCertCell) builds its CreateParams without
-- setting Codec, and the sessions INSERT coalesces an empty codec to 'h264'. So
-- every row this table has ever held was measured on an H.264 stream. They are
-- therefore re-pointed at the h264 rung of the launch profile they name — the
-- first one by position, which is the rung a launch would resolve to. 0036's
-- fan-out copied every column of the legacy stream profile into that rung, so the
-- migrated row's width/height/fps still match its new key exactly.
--
-- A row whose profile_id names no launch profile with an h264 rung cannot be
-- interpreted at the new grain (it is a measurement of an object that no longer
-- exists). Such rows are DELETED rather than guessed at; a missing cert is the
-- optimistic "uncertified" case, so deleting one can only remove a cap, never add
-- one. The full pre-migration table is snapshotted first
-- (_backup_0041_host_encoder_certification, same convention as 0036) so the down
-- path restores them.
BEGIN;

-- ── (1) Snapshot, so the down path is lossless. ────────────────────────────
CREATE TABLE _backup_0041_host_encoder_certification AS
    TABLE host_encoder_certification;

-- ── (2) The rung column. ───────────────────────────────────────────────────
ALTER TABLE host_encoder_certification
    ADD COLUMN stream_profile_id TEXT NULL;

UPDATE host_encoder_certification c
   SET stream_profile_id = (
       SELECT r.stream_profile_id
         FROM launch_profile_rungs r
         JOIN stream_profiles sp ON sp.id = r.stream_profile_id
        WHERE r.launch_profile_id = c.profile_id
          AND sp.codec = 'h264'
        ORDER BY r.position ASC
        LIMIT 1);

DO $$
DECLARE orphans TEXT;
BEGIN
    SELECT string_agg(DISTINCT profile_id, ', ' ORDER BY profile_id)
      INTO orphans
      FROM host_encoder_certification
     WHERE stream_profile_id IS NULL;
    IF orphans IS NOT NULL THEN
        RAISE NOTICE 'migration 0041: dropping encoder-certification rows whose profile_id names no launch profile with an h264 rung (uninterpretable at rung grain; snapshotted in _backup_0041_host_encoder_certification): %', orphans;
    END IF;
END $$;

DELETE FROM host_encoder_certification WHERE stream_profile_id IS NULL;

-- Two launch profiles MAY list the same rung (launch_profile_rungs is unique on
-- the pair, not on the rung), so two legacy rows could collapse onto one new key.
-- Refuse loudly rather than let an arbitrary row win a silent upsert collision.
DO $$
DECLARE dupes TEXT;
BEGIN
    SELECT string_agg(format('%s/gpu%s/%s/%s@%skbps', host_id, gpu_index, encoder,
                             stream_profile_id, bitrate_kbps), ', ')
      INTO dupes
      FROM (
          SELECT host_id, gpu_index, encoder, stream_profile_id, bitrate_kbps
            FROM host_encoder_certification
           GROUP BY host_id, gpu_index, encoder, stream_profile_id, bitrate_kbps
          HAVING COUNT(*) > 1
      ) d;
    IF dupes IS NOT NULL THEN
        RAISE EXCEPTION
            'migration 0041: two encoder-certification rows collapse onto one rung key; two launch profiles share an h264 rung and were certified separately. Delete the stale row(s) and re-run: %',
            dupes;
    END IF;
END $$;

ALTER TABLE host_encoder_certification
    ALTER COLUMN stream_profile_id SET NOT NULL;

-- ON DELETE CASCADE, deliberately, and NOT the NO ACTION that sessions.
-- stream_profile_id uses. A session row is history that must survive; a
-- certification row is a measurement OF the rung and is meaningless without it.
-- NO ACTION here would also make DeleteStreamProfile's actionable 409 path
-- (stream_profiles_store.go) fail at the database with an FK violation → 500,
-- which is the exact trap that was already fixed once for the session count.
ALTER TABLE host_encoder_certification
    ADD CONSTRAINT host_encoder_certification_stream_profile_id_fkey
    FOREIGN KEY (stream_profile_id) REFERENCES stream_profiles(id) ON DELETE CASCADE;

-- ── (3) Swap the upsert key. ───────────────────────────────────────────────
-- 0018 declared the unique key inline, so its name is Postgres-generated and
-- truncated. Find it by its exact column set rather than guessing the string.
DO $$
DECLARE cname TEXT;
BEGIN
    SELECT con.conname INTO cname
      FROM pg_constraint con
     WHERE con.conrelid = 'host_encoder_certification'::regclass
       AND con.contype = 'u'
       AND (SELECT array_agg(att.attname::text ORDER BY att.attname::text)
              FROM unnest(con.conkey) AS k
              JOIN pg_attribute att
                ON att.attrelid = con.conrelid AND att.attnum = k)
           = ARRAY['bitrate_kbps', 'encoder', 'gpu_index', 'host_id', 'profile_id'];
    IF cname IS NULL THEN
        RAISE EXCEPTION 'migration 0041: could not find migration 0018''s unique constraint on host_encoder_certification (host_id, gpu_index, encoder, profile_id, bitrate_kbps)';
    END IF;
    EXECUTE format('ALTER TABLE host_encoder_certification DROP CONSTRAINT %I', cname);
END $$;

ALTER TABLE host_encoder_certification
    ADD CONSTRAINT host_encoder_certification_key
    UNIQUE (host_id, gpu_index, encoder, stream_profile_id, bitrate_kbps);

-- The scheduler's per-launch lookup is now per rung, across all bench bitrates.
DROP INDEX host_encoder_certification_lookup_idx;
CREATE INDEX host_encoder_certification_lookup_idx
    ON host_encoder_certification (host_id, gpu_index, encoder, stream_profile_id);

COMMIT;
