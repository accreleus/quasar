-- 0118 — the swap compat-tool guard compares mapping commit order, not the mapping transaction's
-- start time (#1555). A mapping keeps a revision assigned at INSERT (and again on replace), and
-- SetMapping holds mappingCapLock from before that INSERT until it commits, so revision order is
-- commit order. sessions.compat_mapping_revision is the highest revision the launch read, so a
-- mapping committed after it is refused even when its updated_at (now(), the transaction start)
-- predates the session. Additive: a NOT NULL defaulted column and a nullable one.
BEGIN;

CREATE SEQUENCE compat_tool_mappings_revision_seq;

ALTER TABLE compat_tool_mappings
    ADD COLUMN revision BIGINT NOT NULL DEFAULT nextval('compat_tool_mappings_revision_seq');

ALTER TABLE sessions ADD COLUMN compat_mapping_revision BIGINT NULL;

COMMIT;
