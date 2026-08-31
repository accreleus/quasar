-- LP-SEC-01 (W1 security wave) — invites + device binding. Additive, signed off 2026-07-07.
-- Prose companion: protocol/schema.md § instance_settings / § invites / § user_devices /
-- § auth_tokens / § sessions, and docs/w1-security/LP-SEC-01-contract.md §A.
--
-- Purely additive: one new singleton (instance_settings), one new table (invites), and
-- nullable/defaulted columns on user_devices / auth_tokens / sessions. No existing table,
-- column, type, constraint, or the session state machine is touched. Frozen 0001–0019 intact.
--
-- One-way-at-deploy (CLAUDE.md): once a stack applies 0020 its control-plane binary must embed
-- >= 0020, or boot crash-loops on the missing migration.

BEGIN;

-- (0) instance_settings — one global row (singleton) of admin-settable, instance-wide config.
--     Mirrors the stream_profile_policy singleton idiom (0015). The row itself is NOT seeded
--     here: the control plane seeds it at boot from REGISTRATION_MODE (idempotent, like
--     bootstrap-admin) so the env default applies at runtime; thereafter the admin UI /
--     PATCH /v1/admin/settings is authoritative. registration_mode default 'closed' = the
--     invitation system is OFF on a fresh install.
CREATE TABLE instance_settings (
    id                BOOLEAN     PRIMARY KEY DEFAULT true CHECK (id),
    registration_mode TEXT        NOT NULL DEFAULT 'closed'
                        CHECK (registration_mode IN ('closed','invite_only','open')),
    updated_by        UUID        REFERENCES users(id) ON DELETE SET NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER instance_settings_set_updated_at BEFORE UPDATE ON instance_settings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- (1) invites — admin-minted redemption codes. Code stored HASHED (sha256 hex), like
--     auth_tokens.token_hash; plaintext shown to the admin exactly once at mint. Redemption is
--     atomic single-use (or bounded multi-use) — see the UPDATE...RETURNING in SEC-03.
CREATE TABLE invites (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    code_hash  TEXT        NOT NULL UNIQUE,                 -- sha256(hex) of the opaque code (>=128-bit)
    created_by UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT        NOT NULL DEFAULT 'user' CHECK (role IN ('user','admin')),
    max_uses   INT         NOT NULL DEFAULT 1   CHECK (max_uses >= 1),
    used_count INT         NOT NULL DEFAULT 0   CHECK (used_count >= 0 AND used_count <= max_uses),
    expires_at TIMESTAMPTZ,                                 -- NULL = no expiry
    revoked_at TIMESTAMPTZ,                                 -- non-null = unusable
    note       TEXT,                                        -- admin free-text
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Admin list view is scoped to the minter; cheap lookup.
CREATE INDEX invites_created_by_idx ON invites (created_by);

-- (2) user_devices +name/+trusted (LP-SEC-01). name = user display label; trusted = advisory
--     trust posture only in W1 (NOT an authorization input — enforcement is the token binding).
ALTER TABLE user_devices
    ADD COLUMN name    TEXT,
    ADD COLUMN trusted BOOLEAN NOT NULL DEFAULT false;

-- (3) auth_tokens.device_id — the token<->device binding that makes device revocation real.
--     ON DELETE SET NULL: deleting a device row never cascades a token away; revocation is an
--     explicit token expire/revoke (SEC-05), done before any row delete. NULL for pre-0020
--     tokens and no-device_key logins (backfill caveat D4 — not device-revocable until re-login).
ALTER TABLE auth_tokens
    ADD COLUMN device_id UUID REFERENCES user_devices(id) ON DELETE SET NULL;
CREATE INDEX auth_tokens_device_id_idx ON auth_tokens (device_id) WHERE device_id IS NOT NULL;

-- (4) sessions.device_id — optional read-only linkage so the account UI can show/end a device's
--     live session. Does not change scheduling, the session state machine, or the agent wire.
ALTER TABLE sessions
    ADD COLUMN device_id UUID REFERENCES user_devices(id) ON DELETE SET NULL;

COMMIT;
