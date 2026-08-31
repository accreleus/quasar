-- 0042_app_external_ref.up.sql — Steam library discovery, PHASE 1 ONLY
-- (docs/design/plans/2026-07-29-steam-library-discovery-spec.md §4.1, §12).
--
-- An external ref is "this app IS provider X's title Y" — today only
-- ('steam', <appid>). Phase 1 uses it for exactly one thing: artwork resolution
-- by appid instead of by fuzzy title (§12). An exact id beats any fuzzy title
-- match by construction, and the fuzzy matcher is 7-for-7 wrong on the live
-- catalogue, so this is a defect fix as much as a new capability.
--
-- SCOPE. §4.1 of the spec lands FIVE columns in one migration; four of them
-- (parent_app_id, origin, library_provider, and the derived-tile shape CHECK +
-- the (parent, source, id) unique index) belong to Phase 3 and are deliberately
-- NOT here. Phase 1 is independently shippable and ships only what it reads.
--
-- Purely additive: two new columns, each NOT NULL DEFAULT '' so every existing
-- row is valid with no backfill and no behaviour change. '' means "this app is
-- not a provider title", which is every app that exists today.
BEGIN;

ALTER TABLE apps
    ADD COLUMN external_source TEXT NOT NULL DEFAULT '',
    ADD COLUMN external_id     TEXT NOT NULL DEFAULT '';

ALTER TABLE apps
    ADD CONSTRAINT apps_external_source_ck CHECK (external_source IN ('', 'steam')),

    -- ARGUMENT-INJECTION CONTAINMENT (spec §10, point 3), not mere tidiness.
    -- A Steam appid is destined for STEAM_STARTUP_FLAGS, which the quasar-steam
    -- entrypoint word-splits with `read -r -a` — so anything stored here is
    -- eventually read by a shell-adjacent consumer as ARGUMENTS. The value is
    -- validated at four independent points (agent parse, control-plane ingest,
    -- here, and launch-time render); this one is the ONLY one that survives an
    -- admin editing the field by hand later, which is precisely why it is a
    -- database CHECK and not just a handler guard.
    --
    -- '^[1-9][0-9]{0,9}$' = a bare positive integer, no leading zero, no sign,
    -- no whitespace, no separators — so '0', '007', '1 2', '1;rm -rf /' and
    -- '-applaunch 480 -foo' are all rejected at the storage layer.
    ADD CONSTRAINT apps_external_id_ck CHECK (external_id = '' OR external_id ~ '^[1-9][0-9]{0,9}$');

-- The artwork resolver's lookup direction ("which app is steam:<appid>"), and
-- the index Phase 3's reconciler will reuse. PARTIAL on external_id <> '': every
-- app in a pre-discovery catalogue has '', so a full index would be almost
-- entirely one dead key.
CREATE INDEX apps_external_ref_idx
    ON apps (external_source, external_id) WHERE external_id <> '';

COMMIT;
