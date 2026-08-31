-- 0023_host_observability.up.sql — host-observability. Agent-reported storage
-- volumes + resolved runtime settings, both carried on the existing `capacity`
-- report (additive, agent-api.md). Additive; changes no existing column.
-- Prose: protocol/schema.md `hosts.storage` / `hosts.effective_settings`,
-- protocol/control-api.md host-observability amendment.
BEGIN;

ALTER TABLE hosts
    ADD COLUMN storage JSONB,
    ADD COLUMN effective_settings JSONB;

COMMIT;
