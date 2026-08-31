BEGIN;

-- AS10-11 (#207): per-(user, device, profile) client-performance certification
-- history. This is the data source behind profile eligibility's
-- ReasonHistoricalClientPerfFailed (eligibility.go): a client that demonstrably
-- failed to decode/present a profile (NOT while its tab was hidden) gets that
-- profile marked risky/ineligible the next time it asks GET /v1/me/profiles.
--
-- Latest-outcome-wins: a single row per (user, device, profile) is upserted on
-- every sustained outcome. A transient failure must NOT permanently ban a profile,
-- so a later sustained smooth run at the same profile overwrites the failure with
-- outcome='pass' (the eligibility consumer only treats outcome='fail' as a
-- historical failure).
--
-- device_key: the same client-generated localStorage UUID used by user_devices
-- (P4-08 / schema.md). The browser attaches it to each POST /v1/sessions/{id}/stats
-- sample (AS10-11, additive). When a sample carries no device_key the server falls
-- back to the empty-string sentinel ('') meaning "this user's default/unknown
-- device" — a coarser per-user key that still records the failure.
--
-- Additive-only: a brand-new table, no change to any existing table/column/type.
-- Amendment type: additive per schema.md.

CREATE TABLE user_device_profile_history (
    user_id        UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_key     TEXT        NOT NULL,
    profile_id     TEXT        NOT NULL,
    -- 'pass' | 'fail'. A 'fail' marks the profile as a historical client-perf
    -- failure for eligibility; 'pass' clears a prior failure (latest-outcome-wins).
    outcome        TEXT        NOT NULL CHECK (outcome IN ('pass', 'fail')),
    -- The client-health class that produced a 'fail' (decode_degrading /
    -- presentation_degrading / client_unsupported), for operator diagnostics.
    -- NULL for a 'pass'.
    failure_reason TEXT        NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, device_key, profile_id)
);

-- Serves the GET /v1/me/profiles lookup (all of a user's history) cheaply.
CREATE INDEX user_device_profile_history_user_idx
    ON user_device_profile_history (user_id);

COMMIT;
