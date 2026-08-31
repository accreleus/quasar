-- 0043_entitlements.up.sql — Steam library discovery, PHASE 2
-- (docs/design/plans/2026-07-29-steam-library-discovery-spec.md §6.2, §6.4, §13).
--
-- NOT 0042: Phase 1 (apps.external_source / external_id) shipped first and took
-- that number. The spec was written when Phase 1 and Phase 2 shared a migration;
-- §13's "0042 continued, or 0043 if Phase 2 shipped separately" is the clause
-- that applies.
--
-- An entitlement is "this subject may see and launch this app". The roadmap owns
-- the object, not this feature (§6.1): `subject_type`/`subject_id` is deliberately
-- wider than the Steam use case so an admin grant, "everyone", and a future group
-- are all expressible without a retrofit. Phase 2 ships only ('user', 'all') —
-- operator-confirmed. 'group' is additive when it comes: a new CHECK value and a
-- third partial unique index, no shape change.
--
-- THIS MIGRATION IS AN AUTHORIZATION BOUNDARY. Everything below the CREATE TABLE
-- exists because turning on filtering against an empty table is a fleet-wide
-- lockout that passes every automated gate (§6.4, §17 row 1). Read the backfill
-- comment before changing anything here.
BEGIN;

CREATE TABLE entitlements (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_type TEXT NOT NULL CHECK (subject_type IN ('user', 'all')),
    -- NULL for 'all', a user for 'user'. ON DELETE CASCADE, deliberately not
    -- SET NULL: a NULLed subject_id on a 'user' row would violate the shape
    -- CHECK below anyway, and an entitlement has no meaning once its subject or
    -- its app is gone (the same call user_app_favourites made in 0034).
    subject_id   UUID NULL REFERENCES users(id) ON DELETE CASCADE,
    app_id       UUID NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    -- 'admin'     an operator granted it (audited via admin_activity)
    -- 'provider'  a library-discovery sync wrote it, and a sync may revoke it.
    --             NOTHING WRITES THIS YET — it is Phase 4. It is in the CHECK now
    --             so Phase 4 needs no ALTER, and so a revoke of one is already a
    --             working path the day the first one is written.
    -- 'migration' the backfill below, and only the backfill
    granted_by      TEXT NOT NULL CHECK (granted_by IN ('admin', 'provider', 'migration')),
    -- Who performed an 'admin' grant. SET NULL rather than CASCADE: deleting the
    -- operator who granted an entitlement must not silently revoke it.
    granted_by_user UUID NULL REFERENCES users(id) ON DELETE SET NULL,
    -- Free-form provenance for a 'provider' grant (Phase 4: which scan/appid).
    -- '' for everything Phase 2 writes.
    source_ref      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The two legal shapes and nothing else: ('all', NULL) or ('user', <uuid>).
    -- Written as an equivalence rather than two ORed clauses so it stays one
    -- readable line when 'group' is added (it becomes `subject_type = 'all'` on
    -- the left and nothing else changes).
    CONSTRAINT entitlements_subject_shape_ck
      CHECK ((subject_type = 'all') = (subject_id IS NULL))
);

-- TWO PARTIAL UNIQUE INDEXES, NOT ONE PLAIN UNIQUE — this is a correctness
-- requirement, not a style choice. Postgres does not treat NULLs as equal in a
-- UNIQUE constraint, so a single `UNIQUE (subject_type, subject_id, app_id)`
-- would consider every ('all', NULL, <app>) row distinct from every other and
-- silently permit unlimited duplicate 'all' rows for the same app. That is not
-- merely untidy: the filter predicate uses EXISTS (§6.3) so duplicates would not
-- corrupt the list, but a revoke UI that deletes "the" row would leave the app
-- still visible, and the grant path's ON CONFLICT idempotency would have nothing
-- to conflict on. Splitting by shape gives each shape a real uniqueness key.
--
-- (Postgres 15+ has NULLS NOT DISTINCT, which would also work; two partial
-- indexes are used instead because they also serve as the lookup indexes for the
-- two halves of the filter predicate and carry no version floor.)
CREATE UNIQUE INDEX entitlements_all_uk  ON entitlements (app_id)             WHERE subject_type = 'all';
CREATE UNIQUE INDEX entitlements_user_uk ON entitlements (subject_id, app_id) WHERE subject_type = 'user';

-- The app-direction lookup (GET /v1/admin/apps/{id}/entitlements) and the index
-- DELETE /v1/apps/{id} needs: Postgres does not auto-index the referencing side
-- of a FK, and an app delete cascades through this table.
CREATE INDEX entitlements_app_idx ON entitlements (app_id);

-- THE BACKFILL. It is in this transaction, not a follow-up job, and it is the
-- single most load-bearing statement in the migration.
--
-- GET /v1/apps becomes entitlement-filtered in the same deploy that creates this
-- table. Against an empty table that filter returns nothing — EVERY user's
-- library goes blank on EVERY deployment in existence, simultaneously. And it
-- would ship: `go-test-db` passes (tests create their own entitlements), the web
-- build passes, the migration applies cleanly, and the control plane boots. There
-- is no automated gate between "empty table" and "every user's library is empty"
-- other than this INSERT.
--
-- Granting ('all', 'migration') for every app that exists makes Phase 2's day-one
-- behaviour change EXACTLY ZERO: every user sees precisely the apps they saw
-- before, because every app is visible to everyone, which is what "no
-- entitlements" meant before this table existed. Every subsequent narrowing is
-- then a deliberate admin action measured against a visible baseline.
--
-- DISABLED APPS ARE INCLUDED. The filter is ANDed with `apps.enabled = true`, so
-- a disabled app is invisible either way today — but skipping it here would mean
-- re-enabling an app silently failed to bring it back, months later, with nothing
-- to point at. The backfill is over the catalogue, not over what is currently
-- visible.
--
-- granted_by='migration' (not 'admin') so this row is distinguishable forever:
-- an operator auditing "who made this app public" gets "the 0043 backfill did,
-- because it was public before entitlements existed" rather than a false
-- attribution to whoever happened to be the first admin.
INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
SELECT 'all', NULL, id, 'migration' FROM apps;

COMMIT;
