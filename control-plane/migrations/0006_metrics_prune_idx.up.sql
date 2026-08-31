-- #148 — index the retention prune path.
-- PruneSessionMetrics deletes on (session_id, created_at < cutoff); the only
-- existing index is (session_id, ts_unix_ms), so every prune heap-scans all of
-- the session's rows. The prune runs on BOTH ingestion paths (agent push and
-- browser stats POST), i.e. every few seconds per live session.

BEGIN;

CREATE INDEX session_metrics_session_created_idx
    ON session_metrics (session_id, created_at);

COMMIT;
