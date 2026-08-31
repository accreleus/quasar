-- Reverse of 0016_session_trace.up.sql.

BEGIN;

DROP TABLE IF EXISTS session_trace_clock;
DROP TABLE IF EXISTS session_trace_events;

COMMIT;
