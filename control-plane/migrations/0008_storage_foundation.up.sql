-- P5-01 (signed off, PR #167) — storage & state foundation.
-- Additive: user_homes bookkeeping table + the two apps columns that opt an app
-- into a managed per-(user, app) home. No quota (operator decision at sign-off:
-- usage is visibility-only in v1). Prose companion: protocol/schema.md.

BEGIN;

ALTER TABLE apps
    ADD COLUMN managed_home        BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN home_container_path TEXT    NOT NULL DEFAULT '/home/quasar';

-- One row per provisioned per-(user, app) home, pinned to the host that holds it
-- (v1 homes are host-local; a shared-storage driver later relaxes host_id).
-- No CASCADE on user_id/app_id: deletion tombstones (gc_after) so the janitor
-- reaps the backing store (volume/dir) before the row goes (P5-05).
CREATE TABLE user_homes (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES users(id),
    app_id       UUID        NOT NULL REFERENCES apps(id),
    host_id      UUID        REFERENCES hosts(id),
    provider     TEXT        NOT NULL CHECK (provider IN ('volume','local')),
    ref          TEXT        NOT NULL,
    bytes_used   BIGINT      NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    gc_after     TIMESTAMPTZ
);

CREATE UNIQUE INDEX user_homes_user_app_host_idx ON user_homes (user_id, app_id, host_id);
CREATE INDEX user_homes_user_idx ON user_homes (user_id);
CREATE INDEX user_homes_gc_idx ON user_homes (gc_after) WHERE gc_after IS NOT NULL;

COMMIT;
