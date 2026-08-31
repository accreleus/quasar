-- 0033_gpu_vram_telemetry.down.sql — drop the live VRAM telemetry columns.
--
-- Purely additive up-migration, so the down is a clean drop: the samples are
-- transient observations (rewritten every heartbeat), never history worth
-- preserving. Admission on a rolled-back binary is slots-only, which is exactly
-- the fail-open behaviour the veto abstains to.
BEGIN;

ALTER TABLE gpus
    DROP COLUMN vram_sample_agent_ms,
    DROP COLUMN vram_sampled_at,
    DROP COLUMN vram_mb_free,
    DROP COLUMN vram_mb_used;

COMMIT;
