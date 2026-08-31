-- P4-01/P4-05 — per-session telemetry (additive, signed off).
-- Append-only time-series of performance samples, one row per sample per source.
-- Two independent reporters write here: the agent (host-observable encode numbers,
-- agent-api.md session_metrics) and the browser (its own getStats(), control-api.md
-- POST /v1/sessions/{id}/stats). The `source` column keeps them distinct so the
-- admin surface can reconcile a host→browser timeline. Telemetry is observability,
-- never access control and never a session-state authority (schema.md). The table is
-- bounded by the P4-05 retention policy (rolling per-session window + terminal-state
-- prune; the FK cascade reaps rows when a session row is deleted).
-- Prose companion: protocol/schema.md § session_metrics.

BEGIN;

CREATE TABLE session_metrics (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id UUID        NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    source     TEXT        NOT NULL CHECK (source IN ('agent','browser')),
    ts_unix_ms BIGINT      NOT NULL,
    metrics    JSONB       NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Serves "latest N samples for a session" (the admin read) and per-source ordering.
CREATE INDEX session_metrics_session_ts_idx
    ON session_metrics (session_id, ts_unix_ms DESC);

COMMIT;
