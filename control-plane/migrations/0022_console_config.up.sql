-- 0022_console_config.up.sql — CM-01 (console-mode). Per-host console-config
-- (local display + local audio + local input) and agent-reported console
-- capabilities (DRM connectors, host audio sinks, physical input devices).
-- Additive; changes no existing table. Prose: protocol/schema.md
-- `console_config`, protocol/control-api.md §"Console mode (CM-01)".
BEGIN;

CREATE TABLE console_config (
    host_id    UUID        PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    config     JSONB       NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID        REFERENCES users(id)
);

-- Agent-reported console capabilities (rides agent-api.md capacity.console_capabilities);
-- written by the capacity handler, not the admin PATCH.
CREATE TABLE console_capabilities (
    host_id      UUID        PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    capabilities JSONB       NOT NULL DEFAULT '{}',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
