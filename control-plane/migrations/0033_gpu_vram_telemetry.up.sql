-- 0033_gpu_vram_telemetry.up.sql — live per-GPU VRAM telemetry (#383 §3.3).
--
-- Admission drops the *declared* per-app VRAM dimension (a guess typed into an
-- admin form, never a cap) and gains a live free-VRAM VETO on top of the encode
-- slot reservation. The veto needs a place to put the agent's periodic sample.
--
-- All four columns are NULLABLE and NULL means UNKNOWN — never 0-as-unknown.
-- A 0 in vram_mb_free is a real "this GPU has no memory left"; a NULL is "we do
-- not know", and the admission veto ABSTAINS on NULL (fails OPEN, spec §2
-- property 2). Nothing about availability may become load-bearing on telemetry.
--
--   vram_mb_used         last sampled used VRAM (MB)
--   vram_mb_free         last sampled free VRAM (MB) — what the veto reads
--   vram_sampled_at      DB now() AT INGEST, never the agent's clock. The
--                        in-flight debit compares this against sessions.started_at
--                        (also DB now()); mixing an agent clock in would silently
--                        kill the debit or make a dead agent's sample look fresh.
--   vram_sample_agent_ms the agent's own ts_unix_ms, kept for ONE purpose: the
--                        monotonicity guard. A displaced "zombie" agent
--                        connection can keep writing heartbeats until its read
--                        deadline expires; the ingest UPDATE is conditional on
--                        this value strictly increasing so a late zombie write
--                        cannot overwrite a fresher sample with pre-restart data.
--
-- Additive-only: four nullable columns, no default, no backfill. An un-upgraded
-- agent simply never populates them ⇒ the veto abstains ⇒ slots-only admission,
-- which is today's behaviour minus declared VRAM.
BEGIN;

ALTER TABLE gpus
    ADD COLUMN vram_mb_used         INT,
    ADD COLUMN vram_mb_free         INT,
    ADD COLUMN vram_sampled_at      TIMESTAMPTZ,
    ADD COLUMN vram_sample_agent_ms BIGINT;

COMMIT;
