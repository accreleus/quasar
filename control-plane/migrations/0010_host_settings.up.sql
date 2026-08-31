-- Host runtime settings (admin UI, 2026-06-14). Per-host sparse overrides of the
-- node-agent runtime knobs (QUASAR_* env vars), validated server-side against the
-- hostcfg catalog. Prose companion: protocol/schema.md.
BEGIN;

CREATE TABLE host_settings (
    host_id    UUID        PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    overrides  JSONB       NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID        REFERENCES users(id)
);

COMMIT;
