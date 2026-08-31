-- 0021_metrics_native_source.up.sql — P9-07 (native-client live metrics).
-- Widen session_metrics's source CHECK to accept 'native' (protocol/control-api.md
-- P9-01 amendment: POST /v1/sessions/{id}/stats accepts an optional
-- client: "native" discriminator, additive alongside the existing 'agent'/'browser'
-- reporters). One-way at deploy time (CLAUDE.md migrations note): a binary built
-- against this migration must not be rolled back below it once deployed.
ALTER TABLE session_metrics
    DROP CONSTRAINT session_metrics_source_check;
ALTER TABLE session_metrics
    ADD CONSTRAINT session_metrics_source_check
    CHECK (source IN ('agent', 'browser', 'native'));
