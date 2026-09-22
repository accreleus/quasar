-- 0086_gpu_codecs.up.sql — per-GPU codec sets (amendment 12, #296).
--
-- PURELY ADDITIVE IN SHAPE: one nullable column, no default, no backfill, no
-- key change. `gpus.codecs` is what the agent reports for that GPU
-- (agent-api.md `capacity.gpus[].codecs`), written wholesale with the GPU row.
--
-- NULL MEANS "INHERIT hosts.codecs", never "encodes nothing": every row that
-- exists when this runs is NULL, and every older agent keeps writing NULL, so
-- this migration changes no placement or rung decision on its own — a GPU's
-- set narrows only once an amendment-aware agent reports one.
--
-- NOT additive in MEANING elsewhere: `hosts.codecs` is reworded to the union
-- of this column over usable GPUs (the agent computes it; the column itself
-- is unchanged), and control-api.md "Rung resolution" / "Admission control"
-- read the placed GPU's set instead of the host's.
--
-- ROLLBACK, standing repo rule (CLAUDE.md "Migrations are one-way"): once
-- applied, never deploy a control-plane binary embedding only <= 0085.
BEGIN;

ALTER TABLE gpus ADD COLUMN codecs JSONB;

COMMENT ON COLUMN gpus.codecs IS
    'Amendment 12 (#296): the GPU codec set (agent-api.md capacity.gpus[].codecs), a JSON array subset of ["h264","h265","av1"]. NULL means inherit hosts.codecs (and a host with that NULL too is h264-only) — every row written by an agent that predates this amendment. Written wholesale with the GPU row on each capacity upsert, no keep-if-absent rule. Read only through gpuCodecSetSQL (control-plane/internal/session/placement.go).';

COMMIT;
