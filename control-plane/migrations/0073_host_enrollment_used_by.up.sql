-- #12 / #96 review follow-up: two fixes to host_enrollments (0072).
--
-- (1) used_by_node_name — last_used_at records THAT a token was redeemed without
--     recording by whom, which is the one fact an operator needs when a token
--     turns out to have been used by a machine they did not expect. NULL until
--     first redemption; on a multi-use token the most recent redeemer wins.
--
-- (2) created_by drops NOT NULL and its FK becomes ON DELETE SET NULL. The row
--     must OUTLIVE its minter: a token minted by an ephemeral DX admin identity
--     (users.ephemeral_expires_at, #399) was cascaded away mid-flight when the
--     reaper deleted that identity, taking a credential a host was in the middle
--     of enrolling with. The Go side and openapi.yaml already model
--     created_by_user_id as nullable and the admin list LEFT JOINs users, so
--     nothing above the database has to change. (`invites` carries the same
--     latent bug — its TestListSurvivesAMissingMinter has to disable FK
--     enforcement to construct the state — fixing it is a separate change.)
--
-- The constraint name is Postgres's own for an inline column REFERENCES, and the
-- DROP is deliberately not IF EXISTS: a wrong name must fail loudly here rather
-- than silently leave the CASCADE in place beside a second, weaker FK.
ALTER TABLE host_enrollments ADD COLUMN used_by_node_name TEXT;

COMMENT ON COLUMN host_enrollments.used_by_node_name IS
    'node_name presented at the most recent redemption; NULL until first redeemed.';

ALTER TABLE host_enrollments ALTER COLUMN created_by DROP NOT NULL;
ALTER TABLE host_enrollments DROP CONSTRAINT host_enrollments_created_by_fkey;
ALTER TABLE host_enrollments
    ADD CONSTRAINT host_enrollments_created_by_fkey
    FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL;
