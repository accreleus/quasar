-- 0113 — compatibility tools (amendment 56, #1066). Additive: three new tables, nullable or
-- defaulted columns. No seeding: the Valve and recommended rows are reconciled at boot.
-- Semantics: schema.md.
BEGIN;

CREATE TABLE compat_tools (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind                    TEXT NOT NULL DEFAULT 'proton' CHECK (kind IN ('proton')),
    family                  TEXT NOT NULL CHECK (family IN ('valve','ge')),
    source                  TEXT NOT NULL CHECK (source IN ('steam','recommended','tag')),
    tag                     TEXT NULL UNIQUE CHECK (tag IS NULL OR tag ~ '^GE-Proton[0-9]{1,3}-[0-9]{1,4}$'),
    steam_name              TEXT NULL UNIQUE CHECK (steam_name IS NULL OR steam_name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
    display_name            TEXT NOT NULL,
    recommended             BOOLEAN NOT NULL DEFAULT false,
    name_conflict           BOOLEAN NOT NULL DEFAULT false,
    pin_conflict            BOOLEAN NOT NULL DEFAULT false,
    first_enabled_at        TIMESTAMPTZ NULL,
    force_remove_pending_at TIMESTAMPTZ NULL,
    enabled                 BOOLEAN NOT NULL DEFAULT false,
    asset_name              TEXT NULL,
    asset_size_bytes        BIGINT NULL CHECK (asset_size_bytes IS NULL OR asset_size_bytes BETWEEN 1 AND 2147483648),
    asset_sha512            TEXT NULL UNIQUE CHECK (asset_sha512 IS NULL OR asset_sha512 ~ '^[0-9a-f]{128}$'),
    created_by              UUID NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT compat_tools_shape CHECK (
        (family = 'valve' AND source = 'steam' AND tag IS NULL AND asset_name IS NULL
            AND asset_size_bytes IS NULL AND asset_sha512 IS NULL
            AND steam_name IS NOT NULL AND steam_name ~ '^proton_(experimental|hotfix|[0-9]{1,3})$')
        OR
        (family = 'ge' AND source IN ('recommended','tag') AND tag IS NOT NULL
            AND asset_name IN (tag || '-x86_64.tar.gz', tag || '.tar.gz')
            AND asset_size_bytes IS NOT NULL AND asset_sha512 IS NOT NULL
            AND (steam_name IS NULL OR (steam_name ~ '^GE-Proton'
                 AND lower(steam_name) !~ '^(proton_|steamlinuxruntime|legacysteamruntime)'))))
);

CREATE TRIGGER compat_tools_set_updated_at BEFORE UPDATE ON compat_tools
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE compat_tool_mappings (
    provider    TEXT NOT NULL CHECK (provider IN ('steam')),
    external_id TEXT NOT NULL CHECK (external_id ~ '^[1-9][0-9]{0,9}$'),
    tool_id     UUID NOT NULL REFERENCES compat_tools(id) ON DELETE RESTRICT,
    forced      BOOLEAN NOT NULL DEFAULT false,
    updated_by  UUID NULL REFERENCES users(id) ON DELETE SET NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, external_id)
);

CREATE INDEX compat_tool_mappings_tool_idx ON compat_tool_mappings (tool_id);

CREATE TABLE host_compat_tools (
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    name    TEXT NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
    tag     TEXT NOT NULL CHECK (tag ~ '^GE-Proton[0-9]{1,3}-[0-9]{1,4}$'),
    sha512  TEXT NOT NULL CHECK (sha512 ~ '^[0-9a-f]{128}$'),
    bytes   BIGINT NOT NULL CHECK (bytes >= 0),
    PRIMARY KEY (host_id, name)
);

ALTER TABLE instance_settings
    ADD COLUMN compat_tool_default_id UUID NULL REFERENCES compat_tools(id) ON DELETE RESTRICT,
    ADD COLUMN compat_tools_max_enabled INT NOT NULL DEFAULT 8
        CHECK (compat_tools_max_enabled BETWEEN 1 AND 32),
    ADD COLUMN compat_tools_disk_reserve_gib INT NOT NULL DEFAULT 10
        CHECK (compat_tools_disk_reserve_gib BETWEEN 1 AND 1024);

ALTER TABLE hosts
    ADD COLUMN compat_tools_free_bytes BIGINT NULL,
    ADD COLUMN compat_tools_reported_at TIMESTAMPTZ NULL;

ALTER TABLE sessions ADD COLUMN compat_tool_names TEXT[] NULL;

CREATE INDEX sessions_compat_tool_names_live_idx ON sessions USING GIN (compat_tool_names)
    WHERE state NOT IN ('stopped','failed');

COMMIT;
