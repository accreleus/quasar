-- 0095 down. 'owned' is rewritten to NULL before the CHECK narrows (narrowing
-- first fails on a live owned row). NULL, not 'registry', because that is how a
-- control plane predating amendment 14 must read an owned host: identity-
-- unknown and never applied to (protocol/agent-api.md §register). Everything
-- lost here is re-reported on the host's next register.
BEGIN;

UPDATE hosts SET install_mode = NULL WHERE install_mode = 'owned';
ALTER TABLE hosts
    DROP CONSTRAINT hosts_install_mode_check;
ALTER TABLE hosts
    ADD CONSTRAINT hosts_install_mode_check
    CHECK (install_mode IS NULL OR install_mode IN ('registry', 'source'));

ALTER TABLE hosts
    DROP COLUMN IF EXISTS seed_version,
    DROP COLUMN IF EXISTS recovery_actor_source_commit,
    DROP COLUMN IF EXISTS recovery_actor_version;

COMMENT ON COLUMN hosts.install_mode IS
    'platform-release amendment 1: registry = pulled published images, source = built on the host. A source host can be told about a release but never given one.';

COMMIT;
