-- 0034_app_kind_favourites.up.sql — UI-P1 (app classification + per-user
-- favourites), signed off 2026-07-27.
--
-- (1) apps.kind — a presentation-only library classification
-- (control-api.md AppListItem.kind). Nothing in scheduling, admission,
-- profile/codec resolution, or the agent wire reads it. Default 'game' makes
-- every existing row valid with no backfill.
--
-- (2) user_app_favourites — a per-(user, app) join carrying no payload beyond
-- created_at. The PRESENCE of the row is the fact: no boolean column, no soft
-- delete. The composite primary key (user_id, app_id) is both the uniqueness
-- constraint and the idempotency key for PUT /v1/me/favourites/{app_id}
-- (INSERT ... ON CONFLICT DO NOTHING). Both FKs are ON DELETE CASCADE
-- (deliberately not SET NULL — a favourite has no meaning once its user or
-- app is gone). The (app_id) index is required: Postgres does not
-- auto-index the referencing side of a FK, and DELETE /v1/apps/{id} is a
-- real operator path that cascades through this table.
--
-- Purely additive: one new defaulted column, one new table. No existing
-- table, column, type, constraint, or the session state machine changes.
BEGIN;

ALTER TABLE apps
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'game' CHECK (kind IN ('game', 'desktop'));

CREATE TABLE user_app_favourites (
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_id     UUID        NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, app_id)
);

CREATE INDEX user_app_favourites_app_idx ON user_app_favourites (app_id);

COMMIT;
