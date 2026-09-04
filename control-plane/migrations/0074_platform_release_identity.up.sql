-- 0074 — platform-release identity + the detector's release cache
-- (platform-release amendment 1, #104/#106/#107; schema.md).
--
-- PURELY ADDITIVE: four nullable columns on hosts, one new table, two defaulted
-- columns on instance_settings. No existing column changes, no backfill, and no
-- behaviour change for any component that never reports or reads them — an
-- agent predating the amendment registers byte-identically and simply reads as
-- identity-unknown.

-- ── hosts: the identity an amendment-aware agent reports on `register` ───────
--
-- REPLACED WHOLESALE ON EVERY register (absent => NULL). This is the one place
-- identity differs from every keep-if-absent column beside it (storage, codecs,
-- readiness): those describe hardware an older agent merely fails to re-report,
-- these describe the binary connected RIGHT NOW, so a downgrade to a
-- pre-amendment agent must read as unknown rather than keep a commit that
-- describes nothing running.
ALTER TABLE hosts
    ADD COLUMN source_commit   TEXT NULL,
    ADD COLUMN built_at        TIMESTAMPTZ NULL,
    ADD COLUMN install_mode    TEXT NULL,
    ADD COLUMN updater_present BOOLEAN NULL;

-- NULL-tolerant: identity-unknown is the normal state of every existing row.
ALTER TABLE hosts
    ADD CONSTRAINT hosts_install_mode_check
    CHECK (install_mode IS NULL OR install_mode IN ('registry', 'source'));

COMMENT ON COLUMN hosts.source_commit IS
    'platform-release amendment 1: git commit the running agent binary was built from, 7-40 lowercase hex, stored exactly as sent. Wholesale-replaced on every register; absent => NULL.';
COMMENT ON COLUMN hosts.built_at IS
    'platform-release amendment 1: when that agent binary was built. Same wholesale-replace rule.';
COMMENT ON COLUMN hosts.install_mode IS
    'platform-release amendment 1: registry = pulled published images, source = built on the host. A source host can be told about a release but never given one.';
COMMENT ON COLUMN hosts.updater_present IS
    'platform-release amendment 1: NULL IS NOT false. NULL = no amendment-aware agent has registered (nobody has said); false = an agent looked and found none.';

-- ── platform_releases: the detector's cache of what has been published ──────
--
-- A cache of what EXISTS, never a record of what was done — the apply history
-- is amendment 2's table, deliberately separate.
CREATE TABLE platform_releases (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel        TEXT NOT NULL,
    version        TEXT NULL,
    source_commit  TEXT NOT NULL,
    built_at       TIMESTAMPTZ NOT NULL,
    schema_version INTEGER NOT NULL,
    prerelease     BOOLEAN NOT NULL DEFAULT false,
    manifest       JSONB NULL,
    notes          TEXT NOT NULL DEFAULT '',
    compare_url    TEXT NULL,
    discovered_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT platform_releases_channel_check CHECK (channel IN ('stable', 'edge')),
    -- The detector's idempotency key: re-running detection re-observes the same
    -- commits and must not accumulate duplicates. Keyed on (channel,
    -- source_commit) rather than source_commit alone because one commit can
    -- legitimately be published to both channels (an edge build later tagged).
    CONSTRAINT platform_releases_channel_commit_key UNIQUE (channel, source_commit)
);

-- A semver names exactly one release; two rows claiming 0.2.0 means something
-- upstream is wrong and must fail loudly rather than be shown twice. PARTIAL
-- because every edge row has a NULL version — a plain unique index would be
-- satisfied by them anyway (NULL <> NULL), so the partial form states the intent.
CREATE UNIQUE INDEX platform_releases_version_key
    ON platform_releases (version) WHERE version IS NOT NULL;

-- The release view selects on the instance's channel and orders by
-- schema_version then built_at (ADR 0002: schema_version is the ordering key
-- everywhere, because it is the only one with a consequence).
CREATE INDEX platform_releases_channel_order_idx
    ON platform_releases (channel, schema_version DESC, built_at DESC);

COMMENT ON TABLE platform_releases IS
    'platform-release amendment 1: one row per platform release this instance''s detector has seen. A cache of what exists, never a record of what was applied.';

-- ── instance_settings: which releases this instance is shown ────────────────
--
-- Both defaulted, so the singleton row upgrades in place with no seeding step
-- and a fresh instance follows the stable channel. release_edge_branch is
-- validated by the PATCH handler, not by a CHECK, which cannot express a git
-- ref name.
ALTER TABLE instance_settings
    ADD COLUMN release_channel     TEXT NOT NULL DEFAULT 'stable',
    ADD COLUMN release_edge_branch TEXT NOT NULL DEFAULT 'develop';

ALTER TABLE instance_settings
    ADD CONSTRAINT instance_settings_release_channel_check
    CHECK (release_channel IN ('stable', 'edge'));
