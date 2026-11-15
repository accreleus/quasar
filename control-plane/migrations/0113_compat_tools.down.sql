-- Drops the catalogue, the mappings, the default and the install records. Tools installed on a host
-- stay on its disk, unmounted. Semantics: schema.md.
BEGIN;

DROP INDEX IF EXISTS sessions_compat_tool_names_live_idx;
ALTER TABLE sessions DROP COLUMN IF EXISTS compat_tool_names;
ALTER TABLE hosts
    DROP COLUMN IF EXISTS compat_tools_reported_at,
    DROP COLUMN IF EXISTS compat_tools_free_bytes;
ALTER TABLE instance_settings
    DROP COLUMN IF EXISTS compat_tools_disk_reserve_gib,
    DROP COLUMN IF EXISTS compat_tools_max_enabled,
    DROP COLUMN IF EXISTS compat_tool_default_id;
DROP TABLE IF EXISTS host_compat_tools;
DROP TABLE IF EXISTS compat_tool_mappings;
DROP TABLE IF EXISTS compat_tools;

COMMIT;
