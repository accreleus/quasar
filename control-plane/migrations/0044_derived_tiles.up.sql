-- 0044_derived_tiles.up.sql — Steam library discovery, PHASE 3
-- (docs/design/plans/2026-07-29-steam-library-discovery-spec.md §1.2, §2, §4.1, §4.5).
--
-- NOT 0042: Phase 1 (apps.external_source / external_id) took that number and
-- Phase 2 (entitlements) took 0043. §4.1 of the spec lands five columns in one
-- migration because it was written when Phases 1–3 shared one; two of those five
-- are already here, so this migration lands the remaining three plus the shape
-- constraint that makes them mean something.
--
-- THE SPEC'S PHASE-4 MIGRATION NUMBER IS NOW STALE BY ONE. §4.3 and §13 both say
-- `library_scans` / `library_observations` / `library_appid_rules` and the
-- instance_settings column land in "0044", written when Phases 1–3 were expected
-- to share a single migration. Phase 3 took 0044 (this file), so Phase 4's tables
-- land at 0045. Recorded here rather than only in the spec because this is the
-- directory the next implementer looks at first, and a migration whose number is
-- already taken fails on boot — the crash-loop class CLAUDE.md warns about.
--
-- A DERIVED TILE is an apps row with parent_app_id set. It carries IDENTITY and
-- PRESENTATION only (name, artwork, external_source/external_id, enabled,
-- entitlements, favourites) and borrows EVERYTHING EXECUTABLE from its parent at
-- launch: image, runtime_spec, runtime_preset, managed_home/home_container_path,
-- the resource columns admission reads, and — the load-bearing one — the user's
-- home. It contributes exactly one thing to execution: an env override,
-- STEAM_STARTUP_FLAGS, merged over the parent's runtime_spec.env at dispatch.
--
-- The merge happens AT LAUNCH, never flattened into the tile on save. That is the
-- UI-P3 runtime-preset lesson applied verbatim (internal/session/runtime_preset.go):
-- flattening would make every tile a frozen copy, so an image bump or a new mount
-- on the Steam app would reach none of them without a re-sync.
BEGIN;

ALTER TABLE apps
    -- ON DELETE CASCADE so the database can never hold an orphan tile pointing at
    -- a parent that is gone. The API layer refuses to delete a provider app that
    -- has tiles unless the request explicitly opts in (409 listing the N tiles) —
    -- cascade is the INTEGRITY BACKSTOP under an application-layer confirmation,
    -- not the primary UX. Nothing may rely on cascade being the thing a user sees.
    ADD COLUMN parent_app_id    UUID NULL REFERENCES apps(id) ON DELETE CASCADE,
    -- 'manual'     an operator created this app (every app that exists today)
    -- 'discovered' a library-discovery sync created it. NOTHING WRITES THIS YET —
    --              that is Phase 4. It is in the CHECK now so Phase 4 needs no
    --              ALTER, and so the admin list can filter on it from day one.
    ADD COLUMN origin           TEXT NOT NULL DEFAULT 'manual',
    -- '' = this app is not a library provider. 'steam' = an operator has marked
    -- this app as the Steam client whose installed games get scanned (§1.1).
    -- OPERATOR-SET AND NEVER INFERRED from the image name: image names change, and
    -- a wrong inference here starts a filesystem scan of somebody's home.
    --
    -- Deliberately SEPARATE from apps.kind: library_provider='steam' is the
    -- FUNCTIONAL trigger for scanning, kind='launcher' is PRESENTATION. §4.5.3
    -- forbids any server path branching on kind, so the two must not be conflated.
    ADD COLUMN library_provider TEXT NOT NULL DEFAULT '';

ALTER TABLE apps
    ADD CONSTRAINT apps_library_provider_ck CHECK (library_provider IN ('', 'steam')),
    ADD CONSTRAINT apps_origin_ck           CHECK (origin IN ('manual', 'discovered')),

    -- THE LOAD-BEARING CONSTRAINT OF THE WHOLE FEATURE. Read this before changing
    -- anything above.
    --
    -- A derived tile carries identity only. `runtime_spec = '{}'::jsonb` is what
    -- makes "the tile contributes no runtime" STRUCTURAL rather than conventional:
    -- there is no schema validation anywhere on the runtime_spec write path (raw
    -- JSONB in, json.RawMessage through, opaque on the wire, serde at the agent),
    -- so a database CHECK is the only place this can live.
    --
    -- IT EXISTS BECAUSE THE VALIDATED EXPERIMENT DID THE OTHER THING. The Tower
    -- proof-of-concept that showed direct game launch working hardcoded a host
    -- path into one tile's runtime_spec.mounts. That is explicitly NOT the
    -- shipping mechanism — it freezes a host path into a fleet-wide catalogue row
    -- and it silently stops tracking the parent — and this CHECK is what stops
    -- anyone reproducing it.
    --
    -- The other four conjuncts each close a different hole:
    --   external_source/external_id non-empty — a tile with no provider identity
    --     has nothing to launch; composeSteamFlags would have no appid.
    --   managed_home = false — the tile must never own a home. §2's homeAppID rule
    --     routes every storage decision to the PARENT, and a tile with
    --     managed_home = true would provision a second, empty home of its own.
    --   runtime_preset_id IS NULL — the preset is resolved from the parent at
    --     launch; a preset on the tile would be a second, competing merge source.
    --   library_provider = '' — a tile cannot itself be a provider. Otherwise a
    --     scan of a tile would enqueue against a home the tile does not own.
    ADD CONSTRAINT apps_derived_shape_ck CHECK (
        parent_app_id IS NULL
        OR (external_source <> '' AND external_id <> ''
            AND managed_home = false
            AND runtime_preset_id IS NULL
            AND runtime_spec = '{}'::jsonb
            AND library_provider = '')
    );

-- One tile per (provider app, source, appid), FLEET-WIDE. This is what makes the
-- catalogue bounded regardless of user count: three users with overlapping Steam
-- libraries share one row per game plus per-user entitlements, rather than one
-- apps row per (user, game). Entitlements are what make that dedup possible.
--
-- PARTIAL on parent_app_id IS NOT NULL: Postgres does not treat NULLs as equal in
-- a UNIQUE index, so a full index would already permit unlimited (NULL, '', '')
-- rows — but writing the partiality explicitly says the uniqueness is a statement
-- about TILES, not about the whole catalogue, and keeps every pre-0044 app out of
-- the index entirely.
CREATE UNIQUE INDEX apps_parent_external_uk
    ON apps (parent_app_id, external_source, external_id)
    WHERE parent_app_id IS NOT NULL;

-- The "does this app have derived tiles" lookup, which the delete guard runs on
-- every DELETE /v1/admin/apps/{id}. Postgres does not auto-index the referencing
-- side of a foreign key, and this one is self-referential, so without it the
-- guard (and the ON DELETE CASCADE) is a sequential scan of apps.
CREATE INDEX apps_parent_app_id_idx ON apps (parent_app_id) WHERE parent_app_id IS NOT NULL;

-- apps.kind GAINS 'launcher' (§4.5.1, operator decision 2). The Steam provider app
-- stays visible in the user library under a Launcher category rather than being
-- hidden once its games are discovered.
--
-- Migration 0034 declared this CHECK INLINE and therefore unnamed, so it carries a
-- server-generated name. The name was VERIFIED against the live database before
-- this file was written (`SELECT conname FROM pg_constraint WHERE conrelid =
-- 'apps'::regclass` → apps_kind_check) rather than trusted to convention: a
-- DROP CONSTRAINT naming a constraint that does not exist fails the migration, and
-- a failed migration on boot is the control-plane crash-loop class CLAUDE.md warns
-- about.
--
-- Widening is data-safe in itself — no existing row can violate a strictly larger
-- allow-list — so there is no backfill and no validation scan, and the recreate is
-- deliberately NOT `NOT VALID`: the table is small, every row already satisfies the
-- predicate, and the rest of the feature relies on a validated constraint.
--
-- The DB CHECK is the BACKSTOP. The real gate is validAppKind in
-- internal/crud/handler.go, exactly as it is for the other two values.
ALTER TABLE apps DROP CONSTRAINT apps_kind_check;
ALTER TABLE apps ADD CONSTRAINT apps_kind_check CHECK (kind IN ('game', 'desktop', 'launcher'));

COMMIT;
