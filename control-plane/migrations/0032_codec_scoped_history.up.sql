-- 0032_codec_scoped_history.up.sql — codec-aware decode-failure verdicts.
--
-- Live multi-codec validation (Tower, 2026-07-24) surfaced a design gap in the
-- AS10-11 certification history: a client_unsupported verdict from an h265
-- session (experimental vulkanh265enc bitstream) wrote a per-(user, device,
-- profile) fail row, which made the whole profile ineligible — even though the
-- failure was codec-specific and the same profile streams fine on h264 (and on
-- nvenc h265). The history had no codec dimension.
--
-- Fix: add a `codec` column to the history key.
--   codec = ''      — profile-level verdict (all pre-existing rows, and rows
--                     written by presentation_degrading fails / pass outcomes,
--                     which are genuinely codec-independent).
--   codec = 'h264' | 'h265' | 'av1' (wire vocabulary) — a decode-side failure
--                     (client_unsupported / decode_degrading) recorded against
--                     the codec the session actually streamed.
--
-- Consumers split accordingly (cert_history.go):
--   - eligibility (ProfileFailures) treats codec IN ('', 'h264') as
--     profile-blocking: '' is the legacy/profile-level verdict, and h264 is the
--     guaranteed resolution floor — if h264 itself fails decode, the profile is
--     effectively dead on that device, exactly today's behavior.
--   - the launch codec resolver (CodecFailures) skips a failed non-h264 codec
--     (clamp 4) so the session degrades to the next candidate / the h264 floor
--     instead of the profile vanishing from the picker.
--
-- Additive-only: new column with a default, unique key widened to include it.
-- Existing rows keep codec = '' (their meaning is unchanged).
BEGIN;

ALTER TABLE user_device_profile_history
    ADD COLUMN codec TEXT NOT NULL DEFAULT ''
        CHECK (codec IN ('', 'h264', 'h265', 'av1'));

-- Widen the latest-outcome-wins upsert key from (user, device, profile) to
-- (user, device, profile, codec). The old auto-named constraint came from the
-- inline UNIQUE in 0013.
ALTER TABLE user_device_profile_history
    DROP CONSTRAINT user_device_profile_history_user_id_device_key_profile_id_key;

ALTER TABLE user_device_profile_history
    ADD CONSTRAINT user_device_profile_history_key
        UNIQUE (user_id, device_key, profile_id, codec);

COMMIT;
