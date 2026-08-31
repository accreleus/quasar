-- P4-01/P4-08 — per-device connection + decode-capability probe (additive, signed off).
-- One row per (user, device): a login-time probe of codec/decode + network capability,
-- captured after authentication and stored owner-scoped. Phase 4 writes it; no optimizer
-- consumes it (that is a later phase). Phase 7 surfaces/manages the device list.
-- Privacy: device_key is a client-generated random UUID persisted in localStorage —
-- app-scoped, NOT a hardware fingerprint and NOT a cross-site supercookie (schema.md).
-- Prose companion: protocol/schema.md § user_devices.

BEGIN;

CREATE TABLE user_devices (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_key   TEXT        NOT NULL,
    user_agent   TEXT        NULL,
    capabilities JSONB       NOT NULL DEFAULT '{}',
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, device_key)
);

-- Serves the Phase-7 "list a user's devices" surface cheaply.
CREATE INDEX user_devices_user_id_idx ON user_devices (user_id);

COMMIT;
