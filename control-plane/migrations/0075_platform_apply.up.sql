-- 0075 — platform-release apply: runs and attempts
-- (platform-release amendment 2, #104/#114/#116; schema.md
-- `platform_apply_runs` / `platform_apply_attempts`).
--
-- PURELY ADDITIVE: two new tables, no existing column touched. Where
-- `platform_releases` (0074) caches what EXISTS, these record what was DONE.
-- The two partial unique indexes below are not optimizations: they ARE the
-- single-flight guarantees the API refuses on (`run_active`,
-- `attempt_in_flight`), in the database rather than in code, so "two admins
-- clicked at once" is impossible rather than merely unlikely.

-- ── platform_apply_runs: one row per FLEET apply (#117 drives it) ───────────
CREATE TABLE platform_apply_runs (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- RESTRICT: detection never deletes a row, so only an operator cleanup
    -- could, and a cleanup must not silently erase the record of a fleet-wide
    -- change.
    release_id          UUID NOT NULL REFERENCES platform_releases(id) ON DELETE RESTRICT,
    state               TEXT NOT NULL,
    force               BOOLEAN NOT NULL DEFAULT false,
    requested_by        UUID NULL REFERENCES users(id) ON DELETE SET NULL,
    -- A persisted FLAG, not a state: a fleet run's first target is a
    -- control-plane restart, so an in-memory signal would not survive it.
    cancel_requested    BOOLEAN NOT NULL DEFAULT false,
    cancel_requested_at TIMESTAMPTZ NULL,
    current_target      TEXT NULL,
    current_host_id     UUID NULL REFERENCES hosts(id) ON DELETE SET NULL,
    error               TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at          TIMESTAMPTZ NULL,
    finished_at         TIMESTAMPTZ NULL,

    CONSTRAINT platform_apply_runs_state_check
        CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')),
    CONSTRAINT platform_apply_runs_target_check
        CHECK (current_target IS NULL OR current_target IN ('control_plane', 'host')),
    CONSTRAINT platform_apply_runs_error_len_check
        CHECK (octet_length(error) <= 4096)
);

-- At most one ACTIVE fleet run per instance. On the constant expression (1)
-- because the uniqueness is instance-wide: there is no column to key it on.
CREATE UNIQUE INDEX platform_apply_runs_active_uk
    ON platform_apply_runs ((1)) WHERE state IN ('pending', 'running');

CREATE INDEX platform_apply_runs_history_idx
    ON platform_apply_runs (created_at DESC, id DESC);

COMMENT ON TABLE platform_apply_runs IS
    'platform-release amendment 2: one row per fleet apply — the control plane first, then every eligible host in sequence (ADR 0002).';

-- ── platform_apply_attempts: one row per attempt to move ONE target ─────────
CREATE TABLE platform_apply_attempts (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- NULL for a standalone per-host apply or revert: those are attempts with
    -- no run, because a run carries fleet ordering and a single-host action has
    -- none.
    run_id             UUID NULL REFERENCES platform_apply_runs(id) ON DELETE CASCADE,
    kind               TEXT NOT NULL,
    target             TEXT NOT NULL,
    host_id            UUID NULL REFERENCES hosts(id) ON DELETE CASCADE,
    -- SET NULL, not RESTRICT: requested_digests, not this column, is the
    -- authority for what an attempt did. NULL is legitimate on a revert.
    release_id         UUID NULL REFERENCES platform_releases(id) ON DELETE SET NULL,
    requested_digests  JSONB NOT NULL,
    previous_digests   JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- Minted and persisted BEFORE the command is sent: the agent that receives
    -- it is normally destroyed by carrying it out, so an id it invented would
    -- die with it.
    updater_request_id UUID NULL,
    state              TEXT NOT NULL,
    reason             TEXT NULL,
    sessions_remaining INTEGER NULL,
    force              BOOLEAN NOT NULL DEFAULT false,
    output             TEXT NOT NULL DEFAULT '',
    requested_by       UUID NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at         TIMESTAMPTZ NULL,
    finished_at        TIMESTAMPTZ NULL,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT platform_apply_attempts_kind_check
        CHECK (kind IN ('apply', 'revert')),
    CONSTRAINT platform_apply_attempts_target_check
        CHECK (target IN ('control_plane', 'host')),
    -- The control-plane target has no host and a host target always has one.
    CONSTRAINT platform_apply_attempts_host_target_check
        CHECK ((target = 'host') = (host_id IS NOT NULL)),
    CONSTRAINT platform_apply_attempts_state_check
        CHECK (state IN ('queued', 'waiting_sessions', 'pending', 'pulling',
                         'recreating', 'verifying', 'succeeded', 'failed', 'cancelled')),
    -- A failure always says why, and nothing else ever carries a reason.
    CONSTRAINT platform_apply_attempts_reason_check
        CHECK ((state = 'failed') = (reason IS NOT NULL)),
    CONSTRAINT platform_apply_attempts_sessions_check
        CHECK (sessions_remaining IS NULL OR sessions_remaining >= 0),
    CONSTRAINT platform_apply_attempts_requested_len_check
        CHECK (octet_length(requested_digests::text) <= 4096),
    CONSTRAINT platform_apply_attempts_previous_len_check
        CHECK (octet_length(previous_digests::text) <= 4096),
    -- Twice the 4096 used elsewhere: a compose failure's tail is the one
    -- artifact an operator has. The agent truncates to this bound before
    -- sending, so this CHECK fails the report, never the apply.
    CONSTRAINT platform_apply_attempts_output_len_check
        CHECK (octet_length(output) <= 8192)
);

-- At most one OPEN attempt per target. The zero UUID stands in for the
-- control-plane target: a plain partial index on a nullable column would not
-- make the NULL rows collide (NULL is never equal to NULL), so the control
-- plane could otherwise have two open attempts. THIS IS the attempt_in_flight
-- refusal and the wire's "single-flight per host: refuse, never queue".
CREATE UNIQUE INDEX platform_apply_attempts_open_target_uk
    ON platform_apply_attempts (COALESCE(host_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE state NOT IN ('succeeded', 'failed', 'cancelled');

-- A release_state maps to EXACTLY one attempt. Partial: a queued attempt has no
-- request id yet and those NULLs must not collide.
CREATE UNIQUE INDEX platform_apply_attempts_request_uk
    ON platform_apply_attempts (updater_request_id) WHERE updater_request_id IS NOT NULL;

CREATE INDEX platform_apply_attempts_history_idx
    ON platform_apply_attempts (created_at DESC, id DESC);

CREATE INDEX platform_apply_attempts_host_idx
    ON platform_apply_attempts (host_id, created_at DESC);

CREATE TRIGGER platform_apply_attempts_set_updated_at BEFORE UPDATE ON platform_apply_attempts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE platform_apply_attempts IS
    'platform-release amendment 2: one row per attempt to move one target to one digest set. The apply history, and the only durable record of the previous digests — which is what makes a revert possible at all.';
