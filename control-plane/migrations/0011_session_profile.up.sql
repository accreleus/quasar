BEGIN;

-- AS10-03: record which first-class stream profile (AS10-01 catalog) a session
-- was launched from. The concrete width/height/fps/bitrate_kbps/h264_profile
-- columns already carry the resolved values; this column records the *selection*
-- so the session can be tied back to a profile (UX, admin oversight, AS10-04
-- floor wiring).
--
-- Nullable, no default: a legacy/tier/override launch names no profile and
-- leaves this NULL. Additive-only — no change to existing columns, types, or
-- constraints. Amendment type: additive per the schema.md amendment rules.

ALTER TABLE sessions
    ADD COLUMN profile_id TEXT NULL;

COMMIT;
