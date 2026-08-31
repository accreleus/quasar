-- 0046_default_profile_catalog.up.sql — the default profile catalog
-- (docs/design/plans/2026-08-01-default-profile-catalog.md, Michael-approved
-- 2026-08-01).
--
-- WHAT THIS DOES. Converges the seeded catalog to the approved default:
--   * 24 user-path encode rungs — 4k/1440p/1080p/720p at 60/90/120 fps, with
--     hevc+av1 rungs at >=1080p, h264 rungs at <=1080p, and per-codec bitrates
--     derived from the validated 1080p60-h264 [3000, 8000] anchor
--     (resolution ^0.75, fps x1.25/x1.45, hevc x0.65, av1 x0.55).
--   * 12 user-facing launch profiles — chains ordered av1 > hevc > one av1
--     step-down > same-fps 1080p h264 floor. No native h264 rung above 1080p:
--     browser h264 is constrained-baseline only, so the floor drops RESOLUTION
--     and keeps FRAME RATE (spec §2).
--   * The 720p30 debug profile and every non-catalog object are untouched.
--
-- IDEMPOTENT BY DESIGN, AND WHY. Tower was converged to this exact catalog via
-- the admin API before this migration ships (the live-apply half of the same
-- approval). When Tower's control plane is next rebuilt from this branch, boot
-- runs m.Up() and this file executes against the already-converged database —
-- so every statement is an UPSERT or a guarded delete, and running it there is
-- a no-op. A FRESH database reaches this point with 0015's old catalog fanned
-- out by 0036 (single-h264 chains at old bitrates); the same UPSERTs retune the
-- shared ids and add the rest.
--
-- HONESTY CLAUSES (same posture as 0036):
--   1. This migration REWRITES the rung lists of the 12 catalog launch
--      profiles. An operator's hand-tuned chain on one of those ids does not
--      survive. (The catalog ids are the product's defaults; a bespoke chain
--      belongs under its own id.) Launch-profile VISIBILITY is preserved on
--      conflict — if an operator demoted 4k120 to debug, this does not
--      resurrect it.
--   2. The down migration restores the pre-0046 snapshot verbatim; admin edits
--      made after 0046 was applied are lost by rolling back, and objects
--      created after 0046 whose rungs sessions already reference are kept (with
--      a RAISE NOTICE) rather than breaking the sessions FK.
--
-- Retired-rung handling: rungs this catalog unlists (the typo '1440p60-h256',
-- the native >=1440p h264 rungs) are DELETED ONLY IF nothing references them —
-- sessions.stream_profile_id and host_encoder_certification rows are the
-- blockers, and a blocked delete is a RAISE NOTICE + skip, never an error. An
-- orphaned internal rung is harmless; a failed migration is not.
BEGIN;

-- ── (0) Snapshot for the down migration. ─────────────────────────────────────
CREATE TABLE _backup_0046_stream_profiles     AS TABLE stream_profiles;
CREATE TABLE _backup_0046_launch_profiles     AS TABLE launch_profiles;
CREATE TABLE _backup_0046_launch_profile_rungs AS TABLE launch_profile_rungs;

-- ── (1) The 24 rungs. ────────────────────────────────────────────────────────
-- Derivations (spec §4): min_offer = 1.2 x nominal, recommended = 1.5 x
-- nominal, headroom 1.5; playout0 75/50/40 (720p60 / other-60 / 90+120);
-- max_startup_rtt 40 on 90/120 fps; high_refresh required on 90/120;
-- hardware encoder required everywhere except 720p60-h264 and 1080p60-h264
-- (openh264 reach, best-effort); h264_profile is 'constrained-baseline' on
-- h264 rungs (browser decode reality) and an inert 'main' on hevc/av1 rungs.
-- A rung is never offered standalone: visibility 'internal', codecs NULL
-- (legacy column, dead since 0036).
INSERT INTO stream_profiles (
    id, display_name, width, height, fps, h264_profile,
    nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
    headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
    high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
    visibility, sort_order, codecs, codec
) VALUES
    -- 720p — h264 only
    ('720p60-h264',   'H.264 · 720p · 60 FPS',   1280,  720,  60, 'constrained-baseline',  4500,  5400,  6750, 1.5, 1500,  0,  720, 'none',     false, 'recommended', 75, 'internal', 120, NULL, 'h264'),
    ('720p90-h264',   'H.264 · 720p · 90 FPS',   1280,  720,  90, 'constrained-baseline',  5500,  6600,  8250, 1.5, 2000, 40,  720, 'required', true,  'recommended', 40, 'internal', 116, NULL, 'h264'),
    ('720p120-h264',  'H.264 · 720p · 120 FPS',  1280,  720, 120, 'constrained-baseline',  6500,  7800,  9750, 1.5, 2500, 40,  720, 'required', true,  'recommended', 40, 'internal', 113, NULL, 'h264'),
    -- 1080p — full codec ladder
    ('1080p60-h264',  'H.264 · 1080p · 60 FPS',  1920, 1080,  60, 'constrained-baseline',  8000,  9600, 12000, 1.5, 3000,  0, 1080, 'none',     false, 'recommended', 50, 'internal', 100, NULL, 'h264'),
    ('1080p90-h264',  'H.264 · 1080p · 90 FPS',  1920, 1080,  90, 'constrained-baseline', 10000, 12000, 15000, 1.5, 3500, 40, 1080, 'required', true,  'supported',   40, 'internal',  96, NULL, 'h264'),
    ('1080p120-h264', 'H.264 · 1080p · 120 FPS', 1920, 1080, 120, 'constrained-baseline', 11500, 13800, 17250, 1.5, 4000, 40, 1080, 'required', true,  'supported',   40, 'internal',  93, NULL, 'h264'),
    ('1080p60-hevc',  'HEVC · 1080p · 60 FPS',   1920, 1080,  60, 'main',                  5000,  6000,  7500, 1.5, 2000,  0, 1080, 'none',     true,  'recommended', 50, 'internal', 100, NULL, 'hevc'),
    ('1080p90-hevc',  'HEVC · 1080p · 90 FPS',   1920, 1080,  90, 'main',                  6500,  7800,  9750, 1.5, 2500, 40, 1080, 'required', true,  'supported',   40, 'internal',  96, NULL, 'hevc'),
    ('1080p120-hevc', 'HEVC · 1080p · 120 FPS',  1920, 1080, 120, 'main',                  7500,  9000, 11250, 1.5, 3000, 40, 1080, 'required', true,  'supported',   40, 'internal',  93, NULL, 'hevc'),
    ('1080p60-av1',   'AV1 · 1080p · 60 FPS',    1920, 1080,  60, 'main',                  4500,  5400,  6750, 1.5, 1800,  0, 1080, 'none',     true,  'recommended', 50, 'internal', 100, NULL, 'av1'),
    ('1080p90-av1',   'AV1 · 1080p · 90 FPS',    1920, 1080,  90, 'main',                  5500,  6600,  8250, 1.5, 2000, 40, 1080, 'required', true,  'supported',   40, 'internal',  96, NULL, 'av1'),
    ('1080p120-av1',  'AV1 · 1080p · 120 FPS',   1920, 1080, 120, 'main',                  6500,  7800,  9750, 1.5, 2500, 40, 1080, 'required', true,  'supported',   40, 'internal',  93, NULL, 'av1'),
    -- 1440p — hevc + av1 (no native h264: spec §2)
    ('1440p60-hevc',  'HEVC · 1440p · 60 FPS',   2560, 1440,  60, 'main',                  8000,  9600, 12000, 1.5, 3000,  0, 1440, 'none',     true,  'supported',   50, 'internal',  80, NULL, 'hevc'),
    ('1440p90-hevc',  'HEVC · 1440p · 90 FPS',   2560, 1440,  90, 'main',                 10000, 12000, 15000, 1.5, 3800, 40, 1440, 'required', true,  'supported',   40, 'internal',  76, NULL, 'hevc'),
    ('1440p120-hevc', 'HEVC · 1440p · 120 FPS',  2560, 1440, 120, 'main',                 11500, 13800, 17250, 1.5, 4500, 40, 1440, 'required', true,  'supported',   40, 'internal',  73, NULL, 'hevc'),
    ('1440p60-av1',   'AV1 · 1440p · 60 FPS',    2560, 1440,  60, 'main',                  7000,  8400, 10500, 1.5, 2600,  0, 1440, 'none',     true,  'supported',   50, 'internal',  80, NULL, 'av1'),
    ('1440p90-av1',   'AV1 · 1440p · 90 FPS',    2560, 1440,  90, 'main',                  9000, 10800, 13500, 1.5, 3300, 40, 1440, 'required', true,  'supported',   40, 'internal',  76, NULL, 'av1'),
    ('1440p120-av1',  'AV1 · 1440p · 120 FPS',   2560, 1440, 120, 'main',                 10000, 12000, 15000, 1.5, 4000, 40, 1440, 'required', true,  'supported',   40, 'internal',  73, NULL, 'av1'),
    -- 4k — hevc + av1 (no native h264: spec §2)
    ('4k60-hevc',     'HEVC · 4K · 60 FPS',      3840, 2160,  60, 'main',                 15000, 18000, 22500, 1.5, 5500,  0, 2160, 'none',     true,  'risky',       50, 'internal',  60, NULL, 'hevc'),
    ('4k90-hevc',     'HEVC · 4K · 90 FPS',      3840, 2160,  90, 'main',                 18500, 22200, 27750, 1.5, 7000, 40, 2160, 'required', true,  'risky',       40, 'internal',  56, NULL, 'hevc'),
    ('4k120-hevc',    'HEVC · 4K · 120 FPS',     3840, 2160, 120, 'main',                 21500, 25800, 32250, 1.5, 8000, 40, 2160, 'required', true,  'risky',       40, 'internal',  53, NULL, 'hevc'),
    ('4k60-av1',      'AV1 · 4K · 60 FPS',       3840, 2160,  60, 'main',                 12500, 15000, 18750, 1.5, 4500,  0, 2160, 'none',     true,  'risky',       50, 'internal',  60, NULL, 'av1'),
    ('4k90-av1',      'AV1 · 4K · 90 FPS',       3840, 2160,  90, 'main',                 15500, 18600, 23250, 1.5, 5500, 40, 2160, 'required', true,  'risky',       40, 'internal',  56, NULL, 'av1'),
    ('4k120-av1',     'AV1 · 4K · 120 FPS',      3840, 2160, 120, 'main',                 18000, 21600, 27000, 1.5, 6500, 40, 2160, 'required', true,  'risky',       40, 'internal',  53, NULL, 'av1')
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
    codec                            = EXCLUDED.codec;

-- ── (2) The 12 launch profiles. ──────────────────────────────────────────────
-- Existing ids keep their identity (apps.default_profile_id,
-- user_profile_preferences and sessions.profile_id all point at these), so an
-- app pinned to 1440p120 rides the retune with no edit. VISIBILITY is
-- deliberately NOT in the conflict update: an operator demotion (e.g. 4k120 to
-- 'debug' after a failed decode check) survives re-convergence.
INSERT INTO launch_profiles (id, display_name, description, visibility, sort_order) VALUES
    ('4k120',    '4K · 120 FPS',    '4K AV1/HEVC; falls back to 1440p AV1, then the 1080p120 H.264 floor.', 'user', 10),
    ('4k90',     '4K · 90 FPS',     '4K AV1/HEVC; falls back to 1440p AV1, then the 1080p90 H.264 floor.',  'user', 15),
    ('4k60',     '4K · 60 FPS',     '4K AV1/HEVC; falls back to 1440p AV1, then the 1080p60 H.264 floor.',  'user', 20),
    ('1440p120', '1440p · 120 FPS', '1440p AV1/HEVC; falls back to 1080p AV1, then the H.264 floor.',       'user', 30),
    ('1440p90',  '1440p · 90 FPS',  '1440p AV1/HEVC; falls back to 1080p AV1, then the H.264 floor.',       'user', 35),
    ('1440p60',  '1440p · 60 FPS',  '1440p AV1/HEVC; falls back to 1080p AV1, then the H.264 floor.',       'user', 40),
    ('1080p120', '1080p · 120 FPS', '1080p codec ladder: AV1, HEVC, H.264.',                                'user', 50),
    ('1080p90',  '1080p · 90 FPS',  '1080p codec ladder: AV1, HEVC, H.264.',                                'user', 55),
    ('1080p60',  '1080p · 60 FPS',  '1080p codec ladder: AV1, HEVC, H.264. The recommended default tier.',  'user', 60),
    ('720p120',  '720p · 120 FPS',  '720p H.264.',                                                          'user', 63),
    ('720p90',   '720p · 90 FPS',   '720p H.264.',                                                          'user', 66),
    ('720p60',   '720p · 60 FPS',   '720p H.264.',                                                          'user', 70)
ON CONFLICT (id) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    description  = EXCLUDED.description,
    sort_order   = EXCLUDED.sort_order;

-- ── (3) The chains. Order is preference; the h264 floor is last everywhere. ──
-- Honesty clause 1: this REPLACES the rung lists of exactly these 12 ids.
DELETE FROM launch_profile_rungs WHERE launch_profile_id IN
    ('4k120','4k90','4k60','1440p120','1440p90','1440p60',
     '1080p120','1080p90','1080p60','720p120','720p90','720p60');

INSERT INTO launch_profile_rungs (launch_profile_id, stream_profile_id, position) VALUES
    ('4k120',    '4k120-av1',     1), ('4k120',    '4k120-hevc',    2), ('4k120',    '1440p120-av1', 3), ('4k120',    '1080p120-h264', 4),
    ('4k90',     '4k90-av1',      1), ('4k90',     '4k90-hevc',     2), ('4k90',     '1440p90-av1',  3), ('4k90',     '1080p90-h264',  4),
    ('4k60',     '4k60-av1',      1), ('4k60',     '4k60-hevc',     2), ('4k60',     '1440p60-av1',  3), ('4k60',     '1080p60-h264',  4),
    ('1440p120', '1440p120-av1',  1), ('1440p120', '1440p120-hevc', 2), ('1440p120', '1080p120-av1', 3), ('1440p120', '1080p120-h264', 4),
    ('1440p90',  '1440p90-av1',   1), ('1440p90',  '1440p90-hevc',  2), ('1440p90',  '1080p90-av1',  3), ('1440p90',  '1080p90-h264',  4),
    ('1440p60',  '1440p60-av1',   1), ('1440p60',  '1440p60-hevc',  2), ('1440p60',  '1080p60-av1',  3), ('1440p60',  '1080p60-h264',  4),
    ('1080p120', '1080p120-av1',  1), ('1080p120', '1080p120-hevc', 2), ('1080p120', '1080p120-h264', 3),
    ('1080p90',  '1080p90-av1',   1), ('1080p90',  '1080p90-hevc',  2), ('1080p90',  '1080p90-h264',  3),
    ('1080p60',  '1080p60-av1',   1), ('1080p60',  '1080p60-hevc',  2), ('1080p60',  '1080p60-h264',  3),
    ('720p120',  '720p120-h264',  1),
    ('720p90',   '720p90-h264',   1),
    ('720p60',   '720p60-h264',   1);

-- ── (4) Retired rungs — guarded delete. ──────────────────────────────────────
-- The typo rung and the native >=1440p h264 rungs the catalog unlists. A rung
-- still referenced (sessions.stream_profile_id, a cert row, or a non-catalog
-- launch profile's chain) is SKIPPED with a NOTICE: orphaned internal rungs are
-- harmless, failed migrations are not. host_encoder_certification's
-- profile_id (legacy, launch-profile grain) and stream_profile_id (rung grain,
-- 0041) are un-FK'd TEXT, but deleting a rung under a live cert row would
-- strand it, so both block too.
DO $$
DECLARE
    rid TEXT;
BEGIN
    FOREACH rid IN ARRAY ARRAY['1440p60-h256','4k120-h264','4k60-h264','1440p120-h264','1440p60-h264'] LOOP
        IF NOT EXISTS (SELECT 1 FROM stream_profiles WHERE id = rid) THEN
            CONTINUE;
        END IF;
        IF EXISTS (SELECT 1 FROM sessions WHERE stream_profile_id = rid)
           OR EXISTS (SELECT 1 FROM launch_profile_rungs WHERE stream_profile_id = rid)
           OR EXISTS (SELECT 1 FROM host_encoder_certification
                       WHERE profile_id = rid OR stream_profile_id = rid) THEN
            RAISE NOTICE '0046: retired rung % is still referenced (sessions, a chain, or a cert row); leaving it orphaned as visibility=internal.', rid;
            UPDATE stream_profiles SET visibility = 'internal' WHERE id = rid;
        ELSE
            DELETE FROM stream_profiles WHERE id = rid;
        END IF;
    END LOOP;
END $$;

COMMIT;
