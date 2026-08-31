-- 0016_session_trace — Observability v2 (ST-01/ST-02; additive, signed off).
-- Discrete per-session trace events + the client/host clock-offset estimate.
-- Complements the periodic session_metrics samples (migration 0003): the diagnostic
-- bundle reads session_metrics JSONB JOINED WITH session_trace_events — there is no
-- new samples table and no second sample write path. Two reporters write events: the
-- agent (host-observable markers — ABR retarget, source swap, encoder drop, host
-- webrtc state; agent-api.md session_trace_event) and the browser (its own
-- playout/freeze/visibility/webrtc events; control-api.md trace-events ingest). The
-- `source` column keeps them distinct so the admin surface can reconcile a
-- host→browser timeline (the same pattern as session_metrics.source).
--
-- Plain Postgres only — NO CREATE EXTENSION, NO create_hypertable. This is a plain
-- relational table bounded by the SAME retention model as session_metrics (rolling
-- per-session window + terminal prune; the FK ON DELETE CASCADE reaps rows when a
-- session row is deleted). The schema is designed so a later Timescale conversion is
-- a clean isolated migration if longitudinal profiling is ever wanted — designed-for,
-- not built. Telemetry is observability, never access control and never a
-- session-state authority (schema.md).
-- Prose companion: docs/session-trace/contract-amendment.md §A (the schema delta) and
-- docs/session-trace/trace-format.md (event/clock taxonomy + retention/security).

BEGIN;

CREATE TABLE session_trace_events (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id UUID        NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    source     TEXT        NOT NULL CHECK (source IN ('agent','browser')),
    ts_unix_ms BIGINT      NOT NULL,
    type       TEXT        NOT NULL,
    payload    JSONB       NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Serves "recent events for a session" (the trace/bundle read) and per-session
-- ordering, mirroring session_metrics_session_ts_idx.
CREATE INDEX session_trace_events_session_ts_idx
    ON session_trace_events (session_id, ts_unix_ms DESC);

-- Serves the typed read (.../trace/events?types=) and the bundle's per-type windows.
CREATE INDEX session_trace_events_session_type_ts_idx
    ON session_trace_events (session_id, type, ts_unix_ms DESC);

-- One optional row per session: the client↔host clock-offset estimate AND its
-- uncertainty, so the browser-clock series can be aligned against the host-clock
-- series honestly. ABSENCE OF A ROW MEANS UNMEASURED — never interpreted as offset 0
-- (trace-format.md §4, no false precision).
CREATE TABLE session_trace_clock (
    session_id       UUID             PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    client_offset_ms DOUBLE PRECISION NOT NULL,
    uncertainty_ms   DOUBLE PRECISION NOT NULL,
    measured_at      TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ      NOT NULL DEFAULT now()
);

COMMIT;
