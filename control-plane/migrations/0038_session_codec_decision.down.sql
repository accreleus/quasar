-- 0038_session_codec_decision.down.sql — reverse of 0038.
--
-- The up migration adds two nullable observability columns and touches nothing
-- else, so dropping them is a genuine inverse: no session loses a stream value, a
-- reservation, a profile or a rung, and the pre-UI-P6 behaviour (a session object
-- with no codec_decision and no negotiated_codec) returns exactly.
--
-- The honest caveat: the recorded decisions and the browser-reported codecs ARE
-- discarded, and there is nowhere to preserve them to — the pre-0038 schema has no
-- representation for either. They are diagnostics for sessions that have already
-- ended by the time anyone would run this, and they are reproduced on the next
-- launch.
BEGIN;

ALTER TABLE sessions DROP COLUMN IF EXISTS negotiated_codec;
ALTER TABLE sessions DROP COLUMN IF EXISTS codec_decision;

COMMIT;
