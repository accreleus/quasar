-- #12 / #96: per-host enrollment tokens.
--
-- Replaces the single static ENROLLMENT_TOKEN as the primary way a host joins. That value
-- is shared by the whole fleet, never expires, cannot be rotated without a control-plane
-- restart, and — because enrollHost upserts on node_name — lets any holder take over an
-- ALREADY-ENROLLED host, not merely add a new one (#96). A minted token is hashed at rest,
-- single-use by default, expiring, and can be bound to one node_name.
--
-- Shape deliberately mirrors `invites` (0020): code stored as sha256 hex, plaintext shown
-- to the admin exactly once at mint, atomic consume via UPDATE ... RETURNING under the
-- row lock. One redemption model in this codebase, not two.
--
-- The static token keeps working: matching falls back to it, so an existing deployment
-- upgrades without touching its .env.
CREATE TABLE host_enrollments (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash   TEXT        NOT NULL UNIQUE,               -- sha256(hex) of the opaque token
    created_by   UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- NULL = usable by any node_name. Set = usable only by exactly this one, which is what
    -- makes a leaked token unable to become a host it was not minted for.
    node_name    TEXT,
    max_uses     INT         NOT NULL DEFAULT 1 CHECK (max_uses >= 1),
    used_count   INT         NOT NULL DEFAULT 0 CHECK (used_count >= 0 AND used_count <= max_uses),
    expires_at   TIMESTAMPTZ,                               -- NULL = no expiry
    revoked_at   TIMESTAMPTZ,                               -- non-null = unusable
    note         TEXT,
    last_used_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Admin list view is scoped to the minter, as with invites.
CREATE INDEX host_enrollments_created_by_idx ON host_enrollments (created_by);
