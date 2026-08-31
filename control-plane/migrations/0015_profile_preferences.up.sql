-- Stream profile defaults and override policy (2026-06-24).
-- Additive: omitted profile_id launches now resolve through app/user/global
-- preferences before falling back to the existing device recommendation.
BEGIN;

CREATE TABLE stream_profiles (
    id                                TEXT PRIMARY KEY,
    display_name                      TEXT NOT NULL,
    width                             INTEGER NOT NULL CHECK (width > 0),
    height                            INTEGER NOT NULL CHECK (height > 0),
    fps                               INTEGER NOT NULL CHECK (fps > 0),
    h264_profile                      TEXT NOT NULL CHECK (h264_profile IN ('constrained-baseline', 'main', 'high')),
    nominal_bitrate_kbps              INTEGER NOT NULL CHECK (nominal_bitrate_kbps > 0),
    min_offer_bandwidth_kbps          INTEGER NOT NULL DEFAULT 0 CHECK (min_offer_bandwidth_kbps >= 0),
    recommended_offer_bandwidth_kbps  INTEGER NOT NULL DEFAULT 0 CHECK (recommended_offer_bandwidth_kbps >= 0),
    headroom_factor                   DOUBLE PRECISION NOT NULL DEFAULT 1.5 CHECK (headroom_factor > 0),
    abr_floor_kbps                    INTEGER NOT NULL DEFAULT 0 CHECK (abr_floor_kbps >= 0),
    max_startup_rtt_ms                INTEGER NOT NULL DEFAULT 0 CHECK (max_startup_rtt_ms >= 0),
    min_decode_height                 INTEGER NOT NULL DEFAULT 0 CHECK (min_decode_height >= 0),
    high_refresh_display              TEXT NOT NULL DEFAULT 'none'
        CHECK (high_refresh_display IN ('none', 'recommended', 'required')),
    hardware_encoder_required         BOOLEAN NOT NULL DEFAULT false,
    browser_client                    TEXT NOT NULL DEFAULT 'supported'
        CHECK (browser_client IN ('recommended', 'supported', 'risky')),
    playout0_ms                       INTEGER NOT NULL DEFAULT 50 CHECK (playout0_ms >= 0),
    visibility                        TEXT NOT NULL DEFAULT 'user'
        CHECK (visibility IN ('user', 'debug', 'internal')),
    sort_order                        INTEGER NOT NULL DEFAULT 0,
    updated_at                        TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO stream_profiles (
    id, display_name, width, height, fps, h264_profile,
    nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
    headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
    high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
    visibility, sort_order
) VALUES
    ('4k120', '4K · 120 FPS', 3840, 2160, 120, 'high', 75000, 90000, 112500, 1.5, 20000, 40, 2160, 'required', true, 'risky', 40, 'user', 10),
    ('4k60', '4K · 60 FPS', 3840, 2160, 60, 'high', 40000, 48000, 60000, 1.5, 12000, 0, 2160, 'none', true, 'risky', 50, 'user', 20),
    ('1440p120', '1440p · 120 FPS', 2560, 1440, 120, 'high', 35000, 42000, 52500, 1.5, 10000, 40, 1440, 'required', true, 'risky', 40, 'user', 30),
    ('1440p60', '1440p · 60 FPS', 2560, 1440, 60, 'high', 20000, 24000, 30000, 1.5, 6000, 0, 1440, 'none', true, 'supported', 50, 'user', 40),
    ('1080p120', '1080p · 120 FPS', 1920, 1080, 120, 'high', 20000, 24000, 30000, 1.5, 6000, 40, 1080, 'required', true, 'supported', 40, 'user', 50),
    ('1080p60', '1080p · 60 FPS', 1920, 1080, 60, 'high', 12000, 14400, 18000, 1.5, 4000, 0, 1080, 'none', false, 'recommended', 50, 'user', 60),
    ('720p60', '720p · 60 FPS', 1280, 720, 60, 'high', 8000, 9600, 12000, 1.5, 2500, 0, 720, 'none', false, 'recommended', 75, 'user', 70),
    ('720p30', '720p · 30 FPS (debug)', 1280, 720, 30, 'constrained-baseline', 3000, 0, 4500, 1.5, 1000, 0, 720, 'none', false, 'supported', 100, 'debug', 80);

CREATE TRIGGER stream_profiles_set_updated_at BEFORE UPDATE ON stream_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE stream_profile_policy (
    id                       BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    global_default_profile_id TEXT REFERENCES stream_profiles(id),
    user_overrides_allowed   BOOLEAN NOT NULL DEFAULT true,
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by               UUID REFERENCES users(id)
);

INSERT INTO stream_profile_policy (id, global_default_profile_id, user_overrides_allowed)
VALUES (true, NULL, true);

CREATE TRIGGER stream_profile_policy_set_updated_at BEFORE UPDATE ON stream_profile_policy
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE user_profile_preferences (
    user_id            UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    default_profile_id TEXT REFERENCES stream_profiles(id),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER user_profile_preferences_set_updated_at BEFORE UPDATE ON user_profile_preferences
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE apps
    ADD COLUMN default_profile_id TEXT REFERENCES stream_profiles(id),
    ADD COLUMN profile_policy TEXT NOT NULL DEFAULT 'inherit'
        CHECK (profile_policy IN ('inherit', 'prefer', 'force', 'custom'));

COMMIT;
