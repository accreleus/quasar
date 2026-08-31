-- The per-host storage root becomes THE storage control (operator decision,
-- 2026-08-10). internal/storage.Manager.resolveDriver no longer downgrades
-- 'auto' to the volume driver when a host has no effective home root; that
-- configuration is now a loud launch error naming the remedy.
--
-- THIS MIGRATION EXISTS FOR THE INSTANCES THAT WERE LIVING ON THAT FALLBACK.
-- An instance sitting on the DEFAULT 'auto' with no storage root anywhere is
-- working today: every managed home it has ever created is a docker volume, and
-- launches succeed. Change the resolver underneath it and every one of those
-- launches starts failing. So those instances are pinned to explicit 'volume',
-- which is unchanged, still supported, and exactly what they were already
-- getting — the setting simply starts saying so out loud.
--
-- THE PREDICATE, CLAUSE BY CLAUSE. Each one exists to avoid a specific wrong
-- answer in the opposite direction: pinning a healthy instance to a legacy
-- driver is as bad as breaking a working one.
--
--   storage_provider = 'auto'
--     Explicit 'volume' and explicit 'local' are untouched. Only 'auto' ever
--     resolved two ways, so only 'auto' can be relying on the fallback.
--
--   there is at least one LIVE volume-backed home
--     Evidence, not inference. user_homes.provider records what a past
--     resolution actually produced, which is the only fallback-reliance signal
--     that survives in the database — the control plane's own QUASAR_HOME_ROOT
--     env fallback is invisible to SQL, so no purely-configuration predicate
--     could be trusted. gc_after IS NULL because a tombstoned home is on its way
--     out and is not something to preserve a driver for; an instance whose only
--     volume homes are tombstoned has nothing left to break.
--
--   there is NO live local-backed home
--     The guard against regressing a working instance. A local home can only
--     exist if some host genuinely had a root at some point, which means 'auto'
--     is resolving the way the new rule wants it to. Such an instance may well
--     ALSO hold old volume homes from before the root was configured — those
--     keep working untouched (EnsureHome reuses each row's stored ref), and
--     pinning the instance to 'volume' over them would silently send all its
--     FUTURE homes back to the legacy driver.
--
--   no host exposes a home_root in the database
--     host_settings.overrides->>'home_root' is the admin's per-host override and
--     hosts.effective_settings->>'home_root' is the agent's own reported baseline
--     — the first two rungs of hostcfg.Store.HomeRoot's resolution ladder. If
--     either is set anywhere, a root exists and the instance is configured for
--     local storage even if it has not launched anything since. Without this
--     clause an operator who set a root five minutes ago but has not launched
--     yet would be pinned to legacy on the strength of homes created before they
--     fixed it.
--
-- WHAT THIS DELIBERATELY DOES NOT DO. A mixed fleet — one host with a root, one
-- without, all live homes volume-backed — is left on 'auto'. Launches on the
-- rootless host will now fail loudly, naming that host and the remedy, which is
-- the correct outcome: the alternative is dragging the configured host back to
-- the volume driver to spare the unconfigured one, and that punishes the
-- operator for having done the work. The failure is per-host, actionable, and
-- says where to click.
--
-- Instances with NO homes at all (fresh installs) are also left alone: nothing
-- depends on the old behaviour, and the loud error plus the setup wizard is
-- exactly the path they should be on.

BEGIN;

UPDATE instance_settings
SET storage_provider = 'volume'
WHERE storage_provider = 'auto'
  AND EXISTS (
        SELECT 1 FROM user_homes
        WHERE provider = 'volume' AND gc_after IS NULL
      )
  AND NOT EXISTS (
        SELECT 1 FROM user_homes
        WHERE provider = 'local' AND gc_after IS NULL
      )
  AND NOT EXISTS (
        SELECT 1 FROM host_settings
        WHERE COALESCE(overrides->>'home_root', '') <> ''
      )
  AND NOT EXISTS (
        SELECT 1 FROM hosts
        WHERE COALESCE(effective_settings->>'home_root', '') <> ''
      );

COMMIT;
