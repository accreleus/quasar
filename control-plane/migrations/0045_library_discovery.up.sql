-- 0045_library_discovery.up.sql — Steam library discovery, PHASE 4
-- (docs/design/plans/2026-07-29-steam-library-discovery-spec.md §7.6, §8.2, §10, §11.2).
--
-- NOT 0044, which the spec's §4.3 and §13 both say. Those sections were written
-- when Phases 1–3 were expected to share one migration; Phase 1 took 0042,
-- Phase 2 took 0043 and Phase 3 took 0044, so Phase 4's tables land here at 0045.
-- 0044_derived_tiles.up.sql records the same correction at its head, because a
-- migration whose number is already taken fails on boot — the control-plane
-- crash-loop class CLAUDE.md warns about.
--
-- THREE TABLES AND ONE COLUMN. Discovery runs agent-side (invariant #1: the
-- control plane never touches a host filesystem), pulls over the same additive
-- HTTP surface the #175 GC reaper uses, and holds all of its state here
-- (invariant #5: state is external, and there is no job queue in this codebase —
-- a table plus FOR UPDATE SKIP LOCKED is the Postgres-native answer).
BEGIN;

-- ---------------------------------------------------------------------------
-- library_scans — one row per (user, provider app, host) scan job (§7.6).
--
-- THIS TABLE IS THE WHOLE REASON THE AGENT NEVER LEARNS A USER.
-- protocol/agent-api.md records the P2-01 verdict that per-user concerns never
-- reach the agent. The pull payload honours that literally: the agent is handed
-- a scan_id and a path and nothing else, and the (user_id, app_id, host_id)
-- mapping is resolved HERE, on receipt of the report. There is no user id, no
-- username and no user-derived field in either direction on the wire.
--
-- `state` is a four-value ladder and one of the four is terminal-with-work-done:
--   pending   enqueued by the janitor, waiting for an agent to claim it
--   claimed   an agent holds it; a claim older than 30 minutes is reaped back to
--             pending by the same janitor pass
--   reported  the agent reported success AND reconciliation committed. Those are
--             one transaction (see below), so there is no "reported but not yet
--             reconciled" state to represent
--   failed    the agent reported {ok:false}, or refused the path. A FAILED SCAN
--             CHANGES NOTHING (§7.7 step 1): revocation is driven by absence, and
--             absence is exactly what a transient error looks like
--
-- WHY THERE IS NO `entries` COLUMN AND NO `reconciled_at`. §7.7's five steps run
-- in the SAME transaction that sets state='reported', with the report body still
-- in hand. That is strictly stronger than persisting the entries and reconciling
-- later: steps 3 and 5 (create the tile, grant its entitlement) are ordered
-- inside one commit, so there is never a window where a tile exists with no
-- entitlement — a user seeing nothing where the feature just claimed to add
-- something. If the transaction fails, the scan stays `claimed`, the 30-minute
-- reaper returns it to `pending`, and the next agent re-walks the tree. Nothing
-- is lost because the filesystem, not this table, is the source of truth.
CREATE TABLE library_scans (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_id      UUID NOT NULL REFERENCES apps(id)  ON DELETE CASCADE,   -- the PROVIDER app
    host_id     UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    state       TEXT NOT NULL CHECK (state IN ('pending','claimed','reported','failed')),
    claimed_at  TIMESTAMPTZ,
    reported_at TIMESTAMPTZ,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one OPEN scan per triple. Partial on ('pending','claimed') so the
-- history of completed scans is unbounded by this index — it constrains the
-- queue, not the log. It is also what makes the janitor's enqueue safe to run
-- concurrently with itself: a duplicate INSERT is a unique violation, not a
-- second walk of the same tree.
CREATE UNIQUE INDEX library_scans_open_uk ON library_scans (user_id, app_id, host_id)
    WHERE state IN ('pending', 'claimed');

-- The claim query's index: pending scans on one host, oldest first.
CREATE INDEX library_scans_claim_idx ON library_scans (host_id, created_at)
    WHERE state = 'pending';

-- ---------------------------------------------------------------------------
-- library_observations — "who was seen to have this installed, and when."
-- The sole input to provider entitlement grants and revokes (§7.6).
--
-- IT RECORDS EVERY OBSERVED APPID, INCLUDING ONES THE DENYLIST SUPPRESSES.
-- That is deliberate and it is what replaces the first draft's "Filtered" tab:
-- without it there is no record of what discovery chose not to publish, and a
-- game the built-in denylist wrongly caught would be invisible, with no way for
-- an admin to find it (§8.2). The "Seen, not published" admin read is a SELECT
-- over this table and nothing else.
--
-- `name` is carried here rather than only on the tile for exactly that reason: a
-- suppressed appid has no tile to carry it. It is also half of the denylist key
-- (§8.2 matches on appid OR name prefix), so it is load-bearing for correctness
-- and not only for presentation.
--
-- THE SECOND-ORDER PII, SAID OUT LOUD (§9): this table is a per-user record of
-- which games a person has installed. That is inherent to the feature, admins
-- can see it by design, and it cascades away with the user. What it does NOT
-- contain is `LastOwner`, the SteamID64 in every manifest: the agent's parser is
-- a key ALLOW-LIST and the report struct has no field capable of holding it.
CREATE TABLE library_observations (
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_app_id   UUID NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    external_source TEXT NOT NULL,
    -- §10 point 3, the DB backstop. The appid ends up in STEAM_STARTUP_FLAGS,
    -- which is word-split by `read -r -a` in the quasar-steam entrypoint, so it
    -- is argument-injection surface fed by a background job that parsed a file
    -- on disk. Same regex as apps_external_id_ck, crud.steamAppIDPattern and
    -- session.composeSteamFlags.
    external_id     TEXT NOT NULL CHECK (external_id ~ '^[1-9][0-9]{0,9}$'),
    name            TEXT NOT NULL DEFAULT '',
    host_id         UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, parent_app_id, external_source, external_id, host_id)
);

-- The revoke direction (§7.7 step 5): "does this user still have an observation
-- for this appid on ANY host". The primary key leads with user_id so it already
-- serves that, but the reconciler also asks the parent-scoped question for the
-- "Seen, not published" read, which the PK cannot answer without a scan.
CREATE INDEX library_observations_parent_idx
    ON library_observations (parent_app_id, external_source, external_id);

-- ---------------------------------------------------------------------------
-- library_appid_rules — LAYER 2 of the denylist, operator-writable, overlaid at
-- runtime over the layer-1 code constant (§8.2, internal/library/denylist.go).
--
-- One table, two directions, no second code path. The primary key IS the
-- idempotency key: a rule is set, replaced or deleted, never accumulated.
--
-- The ladder, evaluated per observed appid, first match wins:
--   1. an `allow` rule exists      -> publish
--   2. an `ignore` rule exists     -> suppress
--   3. the built-in denylist matches the appid OR a name prefix -> suppress
--   4. otherwise                   -> publish
--
-- Rule 1 above rule 3 is what makes a wrongly-denylisted game recoverable
-- WITHOUT A RELEASE, which is the entire reason this table exists: updating
-- layer 1 is a code change and an operator cannot wait for one.
-- Rule 2 above rule 4 is what makes an operator's Ignore stick.
--
-- THIS TABLE IS ADMIN-WRITABLE AND TAKES AN APPID STRAIGHT FROM AN HTTP BODY,
-- so it needs the §10 CHECK as much as `apps` does — arguably more, since the
-- handler validation above it is the newest code in the feature.
CREATE TABLE library_appid_rules (
    parent_app_id   UUID NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    external_source TEXT NOT NULL,
    external_id     TEXT NOT NULL CHECK (external_id ~ '^[1-9][0-9]{0,9}$'),
    -- 'ignore' suppresses an appid the built-in denylist does not know about.
    -- 'allow'  un-suppresses a game the built-in denylist wrongly caught.
    rule        TEXT NOT NULL CHECK (rule IN ('ignore', 'allow')),
    note        TEXT NOT NULL DEFAULT '',
    -- SET NULL rather than CASCADE: deleting the operator who wrote a rule must
    -- not silently un-suppress every appid they ignored. Same call as
    -- entitlements.granted_by_user (0043).
    created_by  UUID NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (parent_app_id, external_source, external_id)
);

-- ---------------------------------------------------------------------------
-- instance_settings.library_discovery_enabled — THE master switch, and the ONLY
-- switch (§11.2, operator decision 1).
--
-- ONE SWITCH, NOT TWO. An earlier draft carried a second `library_auto_publish`
-- column beside this one. Under operator decision 1, auto-publish IS the
-- behaviour — the review queue does not exist — so a second toggle would only
-- select between "publish" and a review path that was never built. A boolean
-- whose `false` branch is unimplemented is precisely the half-built second path
-- that decision exists to avoid, so it is not here.
--
-- Default false: SHIP-DARK, the same posture artwork holds. With this false the
-- janitor returns before its first query — no scan rows, no agent work, no
-- third-party calls.
--
-- READ PER PASS, NEVER AT BOOT. internal/artwork/provider.go:85-90 records what
-- happens when a feature caches an operator setting at startup: the admin flips
-- it in the UI and nothing happens until someone restarts the control plane.
ALTER TABLE instance_settings
    ADD COLUMN library_discovery_enabled BOOLEAN NOT NULL DEFAULT false;

COMMIT;
