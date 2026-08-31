-- 0031_multi_codec.up.sql — multi-codec (HEVC/AV1) foundation
-- (docs/design/plans/2026-07-22-multi-codec-hevc-av1-spec.md §3.2/§5).
--
-- Three additive, ship-dark changes (zero behavior change until an admin flips a
-- profile's codec status to launchable):
--
--   1. sessions.codec — the single video codec resolved server-side at launch and
--      sent to the agent in session_assign.stream.codec. WIRE vocabulary
--      (h264|h265|av1); the catalog's "hevc" maps to the wire "h265" in exactly
--      one place in the Go code (session/codec.go catalogToWire). Defaults to
--      'h264' so every pre-existing / legacy / tier / override launch keeps
--      today's behavior with no code aware of the column.
--   2. stream_profiles.codecs — per-profile ordered codec-preference list
--      ([{codec,status}], catalog vocabulary h264|hevc|av1). NULL ⇒ the in-code
--      default (h264 launchable, hevc/av1 future) so existing rows need no
--      backfill; the admin /v1/admin/stream-profiles write path materializes it
--      when an operator enables a codec.
--   3. hosts.codecs — the WIRE codec set the host's active encoder path can
--      produce, reported additively on the agent capacity report. NULL ⇒ the
--      control plane assumes ['h264'] (old agents keep working).
BEGIN;

ALTER TABLE sessions
    ADD COLUMN codec TEXT NOT NULL DEFAULT 'h264'
        CHECK (codec IN ('h264', 'h265', 'av1'));

ALTER TABLE stream_profiles
    ADD COLUMN codecs JSONB NULL;

ALTER TABLE hosts
    ADD COLUMN codecs JSONB NULL;

COMMIT;
