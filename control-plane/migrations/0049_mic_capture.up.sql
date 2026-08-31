-- 0049_mic_capture.up.sql — microphone capture amendment (2026-08-02)
-- (docs/design/plans/2026-08-02-microphone-capture-spec.md §3.5,
-- protocol/control-api.md "`mic` — microphone capture request",
-- protocol/schema.md sessions.mic + instance_settings.mic_capture_enabled).
--
-- Two columns, additive, both default false — SHIP-DARK, the same posture
-- library_discovery_enabled (0045) already holds.
--
-- sessions.mic is the GRANTED state (launch request `mic:true` AND the
-- instance setting on), not the requested state — the requested value is
-- never persisted, only what the launch actually dispatched to the agent
-- (session_assign.stream.mic). Every pre-amendment row and every non-mic
-- launch reads false, unchanged.
--
-- instance_settings.mic_capture_enabled is the instance-wide admin gate,
-- read at launch time (never cached at boot — same discipline
-- library_discovery_enabled already requires, per internal/artwork/
-- provider.go:85-90's recorded lesson about what happens otherwise).
BEGIN;

ALTER TABLE sessions
    ADD COLUMN mic BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE instance_settings
    ADD COLUMN mic_capture_enabled BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN sessions.mic IS
    'Microphone capture amendment (migration 0049). The GRANTED state for this session (launch request mic:true AND instance_settings.mic_capture_enabled at launch time), sent to the agent as session_assign.stream.mic. Default false — every pre-amendment session and every non-mic launch is unchanged.';
COMMENT ON COLUMN instance_settings.mic_capture_enabled IS
    'Microphone capture amendment (migration 0049). Instance-wide gate for client microphone capture, admin-settable via PATCH /v1/admin/settings, read per launch. Default false — ship-dark; a missing singleton row reads as false too.';

COMMIT;
