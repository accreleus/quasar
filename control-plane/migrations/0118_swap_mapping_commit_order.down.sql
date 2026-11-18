-- Down drops the watermark and the revision; mapping writes keep their updated_at meaning.
BEGIN;

ALTER TABLE sessions DROP COLUMN IF EXISTS compat_mapping_revision;
ALTER TABLE compat_tool_mappings DROP COLUMN IF EXISTS revision;
DROP SEQUENCE IF EXISTS compat_tool_mappings_revision_seq;

COMMIT;
