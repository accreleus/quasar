-- 0024_host_observability_2.up.sql — host-observability-2. Agent-reported CPU
-- marketing name + per-GPU stable render-node path (both carried on the
-- existing `capacity` report, agent-api.md), plus a persisted pending_restart
-- flag so GET/PATCH .../settings and the new POST .../restart endpoint can
-- report a real value instead of the prior hardcoded false (host-runtime-
-- settings-admin promised this on the host row; no column ever backed it).
-- Additive; changes no existing column. Prose: protocol/schema.md
-- `hosts.cpu_model` / `gpus.render_node`, protocol/control-api.md
-- host-observability-2 amendment + POST /v1/admin/hosts/{id}/restart.
BEGIN;

ALTER TABLE hosts
    ADD COLUMN cpu_model TEXT,
    ADD COLUMN pending_restart BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE gpus
    ADD COLUMN render_node TEXT;

COMMIT;
