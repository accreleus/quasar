-- 0035_runtime_presets.up.sql — UI-P3 (runtime presets), signed off 2026-07-27.
--
-- A runtime preset is a REUSABLE container configuration — image, launch
-- arguments, environment, mounts and the managed-home storage defaults — that
-- many apps inherit instead of repeating. It is deliberately NOT called a
-- "launch profile": UI-P4's launch profiles are the quality/encode chain, an
-- unrelated object. Runtime preset / stream profile / launch profile are three
-- distinct nouns and must stay distinct in the schema, the API and the UI.
--
-- (1) runtime_presets — the shared configuration itself. The four container
-- fields mirror the shape of apps.runtime_spec ({image, args, env, mounts}),
-- but as first-class columns rather than one opaque JSONB blob, because the
-- admin UI edits them field by field and the merge at launch reads them
-- individually. `args`/`mounts` are JSONB arrays and `env` a JSONB object for
-- the same reason apps.runtime_spec is JSONB: this is agent-internal launch
-- detail the scheduler never reads.
--
-- (2) apps.runtime_preset_id — nullable. **NULL means the app carries
-- everything itself, which is exactly today's behaviour**, so this migration
-- changes no existing app: every existing row gets NULL and its dispatched
-- runtime_spec at launch is byte-identical to before (control-plane
-- `session.GetLaunchApp` returns the stored spec untouched when the column is
-- NULL — it does not even round-trip it through a JSON re-encode).
--
-- ON DELETE RESTRICT — chosen deliberately:
--   * The user-facing rule is that deleting an in-use preset is refused with a
--     409 at the application layer (crud.deleteRuntimePreset), which is what
--     produces an actionable error and the admin UI's disabled Delete button.
--     The FK is the BACKSTOP, not the gate — it exists so that no other path
--     (a future code path, a hand-run DELETE, a bulk operator script) can get
--     past the rule silently.
--   * SET NULL would be actively dangerous here: it would silently strip the
--     image/env/mounts out from under every app using the preset, and those
--     apps would then launch with a smaller spec instead of failing — a silent
--     misconfiguration of exactly the kind this feature exists to prevent.
--   * CASCADE would delete apps, which is absurd for a shared config object.
-- RESTRICT therefore fails loudly, and the 409 above means an operator should
-- never actually see the FK error.
--
-- Purely additive: one new table and one new nullable column. No existing
-- table, column, type, constraint, default, or the session state machine
-- changes.
BEGIN;

CREATE TABLE runtime_presets (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                TEXT        NOT NULL UNIQUE,
    description         TEXT        NOT NULL DEFAULT '',
    image               TEXT        NOT NULL DEFAULT '',
    args                JSONB       NOT NULL DEFAULT '[]'::jsonb,
    env                 JSONB       NOT NULL DEFAULT '{}'::jsonb,
    mounts              JSONB       NOT NULL DEFAULT '[]'::jsonb,
    managed_home        BOOLEAN     NOT NULL DEFAULT false,
    home_container_path TEXT        NOT NULL DEFAULT '/home/quasar',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER runtime_presets_set_updated_at BEFORE UPDATE ON runtime_presets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE apps
    ADD COLUMN runtime_preset_id UUID NULL REFERENCES runtime_presets(id) ON DELETE RESTRICT;

-- Postgres does not auto-index the referencing side of a foreign key, and both
-- the in-use check behind the 409 and the "Used by" list in the admin UI are
-- per-preset lookups over this column.
CREATE INDEX apps_runtime_preset_id_idx ON apps (runtime_preset_id);

COMMIT;
