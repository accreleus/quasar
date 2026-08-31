-- 0061_runtime_preset_network.up.sql — first-run experience §S2: container
-- network is a PER-APP REQUIREMENT carried by the runtime preset, not a host
-- env var (operator directive, 2026-08-09).
--
-- WHY A COLUMN AND NOT `QUASAR_CONTAINER_NETWORK` ON THE HOST. App containers
-- run `--network none` by default (node-agent/src/session/container.rs) and that
-- hardened default is correct for almost everything. But Steam's FIRST boot must
-- reach the internet (it downloads steamui.so) or it clean-exits and the session
-- dies as "media path interrupted" (#463). Flipping the host env var would open
-- the network for EVERY app on that host to fix ONE app — the requirement
-- belongs to the app/image, travels with it to every host, and is expressed
-- where the rest of that image's container configuration already lives: its
-- runtime preset.
--
-- '' = INHERIT, and it is the default, so this migration changes nothing: every
-- existing preset reads '' and the agent keeps taking its existing fallback
-- chain (QUASAR_CONTAINER_NETWORK, else `none`). Only a preset that states a
-- value overrides it.
--
-- `host` IS NOT IN THE CHECK, and that is the security boundary of this feature
-- (review, Alice round 2 on PR #464). `--network host` does not merely widen a
-- container's reach — it removes the network namespace, so the app shares the
-- host's stack: every service on host loopback (the control plane, Postgres, the
-- docker proxy, any admin-only port an operator assumed a tenant workload could
-- not reach) becomes reachable, and the container can bind host ports itself. A
-- preset is PORTABLE — it is materialized from a catalog image manifest authored
-- elsewhere and installed by an admin approving an APP, not an infrastructure
-- change — so allowing `host` here would let a manifest dissolve the isolation
-- boundary on every host that installs it. `none` (isolated) and `bridge`
-- (outbound access, which is all Steam's steamui.so download needs) are the real
-- per-app requirement. An operator who genuinely wants host networking states it
-- on the host they administer via the agent's QUASAR_CONTAINER_NETWORK knob,
-- which is deliberately not expressible in any object that travels between
-- machines.
--
-- The CHECK is the database's half of a defence-in-depth chain that also exists
-- in the admin CRUD (400), in P5 manifest materialization (install fails) and in
-- the agent (session fails). It is an INDEPENDENT boundary, not a delegation:
-- it also covers a hand-run UPDATE or a bulk operator script that never goes
-- through Go. An arbitrary string must never reach `docker run --network`.
--
-- Purely additive: one new NOT NULL DEFAULT '' column on runtime_presets. No
-- existing column, constraint, default, or behaviour changes.
BEGIN;

ALTER TABLE runtime_presets
    ADD COLUMN network TEXT NOT NULL DEFAULT ''
        CONSTRAINT runtime_presets_network_ck CHECK (network IN ('', 'none', 'bridge'));

COMMENT ON COLUMN runtime_presets.network IS
    'S2. Docker network mode for app containers launched from an app inheriting this preset. '''' (default) = inherit the agent''s host default (QUASAR_CONTAINER_NETWORK, else none). Otherwise none|bridge. ''host'' is deliberately NOT accepted: it removes the container network namespace, and a preset is portable (materialized from a catalog manifest) — an operator sets QUASAR_CONTAINER_NETWORK on a specific host instead. An app''s own runtime_spec.network overrides this at launch.';

COMMIT;
