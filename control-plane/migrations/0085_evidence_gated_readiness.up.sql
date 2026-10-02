-- 0085 — evidence-gated readiness (amendment 11, #260/#262).
--
-- ADDITIVE IN SHAPE: three booleans defaulting false, so every existing row
-- starts unblocked and no backfill is needed — the next readiness report
-- computes the verdict. No existing column, type or constraint changes.
--
-- NOT additive in MEANING: hosts.readiness stops being advisory. The scheduler
-- still never parses it; admission reads only the derived columns below, which
-- the control plane recomputes from the report and the host's overrides in the
-- same transaction as either write (schema.md, control-api.md "Evidence-gated
-- readiness").
BEGIN;

ALTER TABLE hosts
    ADD COLUMN readiness_block_host BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN readiness_block_homes BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN hosts.readiness_block_host IS
    'DERIVED (amendment 11): true while the stored readiness report has a check with blocks.scope = "host", status fail and no override. Written only by the verdict function. Admission excludes the host while it is true AND readiness_reported_at is fresh. Not served — the host body''s readiness_gate is recomputed from readiness at read time.';

COMMENT ON COLUMN hosts.readiness_block_homes IS
    'DERIVED (amendment 11): as readiness_block_host, for blocks.scope = "homes" — excludes the host for a launch that would mount a managed home. Not served.';

ALTER TABLE gpus
    ADD COLUMN readiness_blocked BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN gpus.readiness_blocked IS
    'DERIVED (amendment 11): true while the host''s stored readiness report has a check with blocks.scope = "gpu", blocks.gpu_index = this GPU''s index, status fail and no override. A GPU index the report names but the host does not have is ignored. Not served.';

-- One row is an admin's recorded decision to launch on a host despite one named
-- failing check. check_id is agent-owned and is a foreign key to nothing: an id
-- the current report no longer contains makes the row inert, not invalid.
-- Populated by #263; nothing writes it yet.
CREATE TABLE host_readiness_overrides (
    host_id    UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    check_id   TEXT NOT NULL CHECK (length(check_id) BETWEEN 1 AND 128),
    created_by UUID NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (host_id, check_id)
);

COMMIT;
