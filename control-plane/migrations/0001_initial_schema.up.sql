-- Quasar control-plane initial schema (P1-C, FROZEN).
-- Authoritative DDL; prose companion is protocol/schema.md.
-- Postgres >= 14. No extensions required (gen_random_uuid() is core since PG13).

BEGIN;

-- updated_at maintenance ------------------------------------------------------
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- users -----------------------------------------------------------------------
CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,                          -- argon2id PHC string; never plaintext
    role          TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('user','admin')),
    disabled_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));
CREATE TRIGGER users_set_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- auth_tokens -----------------------------------------------------------------
CREATE TABLE auth_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,                    -- sha256(hex) of opaque bearer token
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    user_agent   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX auth_tokens_user_id_idx ON auth_tokens (user_id);

-- apps (library) --------------------------------------------------------------
CREATE TABLE apps (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                 TEXT NOT NULL,
    description          TEXT NOT NULL DEFAULT '',
    cover_url            TEXT,
    runtime_spec         JSONB NOT NULL DEFAULT '{}'::jsonb,   -- agent-consumed launch spec
    default_vram_mb      INT NOT NULL DEFAULT 1024,
    default_encode_slots INT NOT NULL DEFAULT 1,
    default_width        INT NOT NULL DEFAULT 1920,
    default_height       INT NOT NULL DEFAULT 1080,
    default_fps          INT NOT NULL DEFAULT 60,
    default_bitrate_kbps INT NOT NULL DEFAULT 15000,
    enabled              BOOLEAN NOT NULL DEFAULT true,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER apps_set_updated_at BEFORE UPDATE ON apps
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- hosts (node agents) ---------------------------------------------------------
CREATE TABLE hosts (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    node_name          TEXT NOT NULL UNIQUE,             -- stable agent identity for reconnect mapping
    node_secret_hash   TEXT,                             -- sha256 of per-node credential
    status             TEXT NOT NULL DEFAULT 'offline'
                         CHECK (status IN ('online','offline','draining')),
    agent_version      TEXT,
    cpu_cores          INT,
    mem_mb             INT,
    last_registered_at TIMESTAMPTZ,
    last_heartbeat_at  TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- gpus (per-GPU capacity) -----------------------------------------------------
CREATE TABLE gpus (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id            UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    index              INT NOT NULL,
    vendor             TEXT,
    model              TEXT,
    vram_mb_total      INT NOT NULL,
    encode_slots_total INT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (host_id, index)
);
CREATE TRIGGER gpus_set_updated_at BEFORE UPDATE ON gpus
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- sessions (the scheduled, resource-reserved unit) ----------------------------
CREATE TABLE sessions (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                     UUID NOT NULL REFERENCES users(id),
    app_id                      UUID NOT NULL REFERENCES apps(id),
    host_id                     UUID REFERENCES hosts(id),
    gpu_id                      UUID REFERENCES gpus(id),
    state                       TEXT NOT NULL DEFAULT 'pending'
                                 CHECK (state IN ('pending','assigned','starting','running','stopping','stopped','failed')),
    state_detail                TEXT,
    error_message               TEXT,
    -- launch params (drive the P1-5 pipeline)
    width                       INT NOT NULL,
    height                      INT NOT NULL,
    fps                         INT NOT NULL,
    bitrate_kbps                INT NOT NULL,
    h264_profile                TEXT NOT NULL DEFAULT 'constrained-baseline'
                                 CHECK (h264_profile IN ('constrained-baseline','main','high')),
    -- reservation (held while state in assigned/starting/running)
    reserved_vram_mb            INT NOT NULL DEFAULT 0,
    reserved_encode_slots       INT NOT NULL DEFAULT 0,
    -- single-use signaling token
    signaling_token_hash        TEXT UNIQUE,
    signaling_token_expires_at  TIMESTAMPTZ,
    signaling_token_consumed_at TIMESTAMPTZ,
    -- timestamps
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    assigned_at                 TIMESTAMPTZ,
    started_at                  TIMESTAMPTZ,
    ended_at                    TIMESTAMPTZ,
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_created_idx ON sessions (user_id, created_at DESC);
CREATE INDEX sessions_host_idx ON sessions (host_id);
-- cheap reservation-sum lookups (only active sessions hold a reservation)
CREATE INDEX sessions_active_gpu_idx ON sessions (gpu_id)
    WHERE state IN ('assigned','starting','running');
CREATE TRIGGER sessions_set_updated_at BEFORE UPDATE ON sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMIT;
