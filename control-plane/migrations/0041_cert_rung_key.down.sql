-- 0041_cert_rung_key.down.sql — revert host_encoder_certification to the 0018
-- launch-profile-grained key.
--
-- THIS DIRECTION IS LOSSY BY CONSTRUCTION and says so out loud. The up migration
-- ADDS a key dimension; removing it can collapse several rows onto one old key —
-- a chain certified on both its h264 and its av1 rung has two rows that the 0018
-- key cannot tell apart. Rather than let the unique constraint fail with a raw
-- violation, the newest measurement per old key is kept and the rest are dropped
-- with a NOTICE naming them. That is the only defensible collapse: the 0018 table
-- was upsert-latest, so "latest write wins" is exactly its own rule.
--
-- Rows the up migration deleted as uninterpretable are restored from
-- _backup_0041_host_encoder_certification, minus any whose host has since been
-- deleted (hosts(id) ON DELETE CASCADE would have reaped them anyway).
BEGIN;

-- ── (1) Undo the rung key. ─────────────────────────────────────────────────
ALTER TABLE host_encoder_certification
    DROP CONSTRAINT host_encoder_certification_key;

ALTER TABLE host_encoder_certification
    DROP CONSTRAINT host_encoder_certification_stream_profile_id_fkey;

DROP INDEX host_encoder_certification_lookup_idx;

-- The column is dropped at the end, but the restore below has to write NULL into
-- it first — the rows being restored are precisely the ones with no rung.
ALTER TABLE host_encoder_certification
    ALTER COLUMN stream_profile_id DROP NOT NULL;

-- ── (2) Restore rows 0041 dropped as uninterpretable. ──────────────────────
INSERT INTO host_encoder_certification (
    id, host_id, gpu_index, encoder, profile_id,
    width, height, fps, bitrate_kbps,
    verdict, encode_ms_p50, encode_ms_p95, encode_ms_max,
    output_fps, drop_rate, live_write_stable,
    sample_window_ms, sample_count, agent_version,
    measured_at, updated_at, stream_profile_id)
SELECT b.id, b.host_id, b.gpu_index, b.encoder, b.profile_id,
       b.width, b.height, b.fps, b.bitrate_kbps,
       b.verdict, b.encode_ms_p50, b.encode_ms_p95, b.encode_ms_max,
       b.output_fps, b.drop_rate, b.live_write_stable,
       b.sample_window_ms, b.sample_count, b.agent_version,
       b.measured_at, b.updated_at, NULL
  FROM _backup_0041_host_encoder_certification b
 WHERE NOT EXISTS (SELECT 1 FROM host_encoder_certification c WHERE c.id = b.id)
   AND EXISTS (SELECT 1 FROM hosts h WHERE h.id = b.host_id);

-- ── (3) Collapse onto the 0018 key: newest measurement wins. ───────────────
DO $$
DECLARE losers TEXT;
BEGIN
    SELECT string_agg(format('%s (%s/gpu%s/%s/%s@%skbps)', id, host_id, gpu_index,
                             encoder, profile_id, bitrate_kbps), ', ')
      INTO losers
      FROM (
          SELECT id, host_id, gpu_index, encoder, profile_id, bitrate_kbps,
                 row_number() OVER (
                     PARTITION BY host_id, gpu_index, encoder, profile_id, bitrate_kbps
                     ORDER BY measured_at DESC, id) AS rn
            FROM host_encoder_certification
      ) r
     WHERE r.rn > 1;
    IF losers IS NOT NULL THEN
        RAISE NOTICE 'migration 0041 (down): the 0018 key cannot represent per-rung verdicts; dropping all but the newest measurement per (host, gpu, encoder, launch profile, bitrate): %', losers;
    END IF;
END $$;

DELETE FROM host_encoder_certification c
 USING (
    SELECT id,
           row_number() OVER (
               PARTITION BY host_id, gpu_index, encoder, profile_id, bitrate_kbps
               ORDER BY measured_at DESC, id) AS rn
      FROM host_encoder_certification
 ) r
 WHERE r.id = c.id AND r.rn > 1;

-- ── (4) Restore the 0018 shape. ────────────────────────────────────────────
ALTER TABLE host_encoder_certification
    ADD CONSTRAINT host_encoder_certification_host_gpu_encoder_profile_bitrate_key
    UNIQUE (host_id, gpu_index, encoder, profile_id, bitrate_kbps);

CREATE INDEX host_encoder_certification_lookup_idx
    ON host_encoder_certification (host_id, gpu_index, encoder, profile_id);

ALTER TABLE host_encoder_certification DROP COLUMN stream_profile_id;

DROP TABLE _backup_0041_host_encoder_certification;

COMMIT;
