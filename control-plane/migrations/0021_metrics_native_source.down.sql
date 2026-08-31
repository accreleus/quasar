-- 0021_metrics_native_source.down.sql — revert to the 0003 source set.
-- Only safe pre-deploy: any 'native' rows must be removed first or the narrower
-- CHECK cannot be applied (never run this against a live stack post-deploy — see
-- CLAUDE.md's one-way-migration rule).
DELETE FROM session_metrics WHERE source = 'native';
ALTER TABLE session_metrics
    DROP CONSTRAINT session_metrics_source_check;
ALTER TABLE session_metrics
    ADD CONSTRAINT session_metrics_source_check
    CHECK (source IN ('agent', 'browser'));
