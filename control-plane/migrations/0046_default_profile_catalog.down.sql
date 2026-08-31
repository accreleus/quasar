-- 0046_default_profile_catalog.down.sql — snapshot restore (same posture as
-- 0036's down: the retune is lossy, so up snapshots and down refills).
--
-- Honesty clauses (mirror of the up file's):
--   1. Admin edits made while 0046 was applied are lost — the snapshot is
--      pre-0046 verbatim.
--   2. A rung ADDED by 0046 (not in the snapshot) that sessions or cert rows
--      now reference is KEPT with a RAISE NOTICE rather than breaking the
--      sessions.stream_profile_id FK. A launch profile added by 0046 that an
--      app/policy/preference now references blocks loudly (FK) — repoint it
--      first, the same posture as the admin DELETE's 409.
--
-- Restore order is FK order: stream_profiles (rungs must exist before chains
-- reference them — the up migration deleted retirees that snapshot chains
-- list), then launch_profiles, then launch_profile_rungs, then trim additions.
BEGIN;

-- ── (1) Stream profiles: restore snapshot rows verbatim (covers the retunes
-- and resurrects guarded-deleted retirees).
INSERT INTO stream_profiles SELECT * FROM _backup_0046_stream_profiles
ON CONFLICT (id) DO UPDATE SET
    display_name                     = EXCLUDED.display_name,
    width                            = EXCLUDED.width,
    height                           = EXCLUDED.height,
    fps                              = EXCLUDED.fps,
    h264_profile                     = EXCLUDED.h264_profile,
    nominal_bitrate_kbps             = EXCLUDED.nominal_bitrate_kbps,
    min_offer_bandwidth_kbps         = EXCLUDED.min_offer_bandwidth_kbps,
    recommended_offer_bandwidth_kbps = EXCLUDED.recommended_offer_bandwidth_kbps,
    headroom_factor                  = EXCLUDED.headroom_factor,
    abr_floor_kbps                   = EXCLUDED.abr_floor_kbps,
    max_startup_rtt_ms               = EXCLUDED.max_startup_rtt_ms,
    min_decode_height                = EXCLUDED.min_decode_height,
    high_refresh_display             = EXCLUDED.high_refresh_display,
    hardware_encoder_required        = EXCLUDED.hardware_encoder_required,
    browser_client                   = EXCLUDED.browser_client,
    playout0_ms                      = EXCLUDED.playout0_ms,
    visibility                       = EXCLUDED.visibility,
    sort_order                       = EXCLUDED.sort_order,
    codecs                           = EXCLUDED.codecs,
    codec                            = EXCLUDED.codec,
    updated_at                       = EXCLUDED.updated_at;

-- ── (2) Launch profiles: drop additions (a referenced one blocks — see
-- honesty clause 2), restore snapshot rows verbatim.
DELETE FROM launch_profiles WHERE id NOT IN (SELECT id FROM _backup_0046_launch_profiles);
INSERT INTO launch_profiles SELECT * FROM _backup_0046_launch_profiles
ON CONFLICT (id) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    description  = EXCLUDED.description,
    visibility   = EXCLUDED.visibility,
    sort_order   = EXCLUDED.sort_order,
    updated_at   = EXCLUDED.updated_at;

-- ── (3) Chains: replace wholesale from the snapshot.
DELETE FROM launch_profile_rungs;
INSERT INTO launch_profile_rungs SELECT * FROM _backup_0046_launch_profile_rungs;

-- ── (4) Remove 0046-added rungs where nothing references them.
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN SELECT id FROM stream_profiles
              WHERE id NOT IN (SELECT id FROM _backup_0046_stream_profiles) LOOP
        IF EXISTS (SELECT 1 FROM sessions WHERE stream_profile_id = r.id)
           OR EXISTS (SELECT 1 FROM launch_profile_rungs WHERE stream_profile_id = r.id)
           OR EXISTS (SELECT 1 FROM host_encoder_certification
                       WHERE profile_id = r.id OR stream_profile_id = r.id) THEN
            RAISE NOTICE '0046 down: added rung % is still referenced; keeping it.', r.id;
        ELSE
            DELETE FROM stream_profiles WHERE id = r.id;
        END IF;
    END LOOP;
END $$;

DROP TABLE _backup_0046_launch_profile_rungs;
DROP TABLE _backup_0046_launch_profiles;
DROP TABLE _backup_0046_stream_profiles;

COMMIT;
