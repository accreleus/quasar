-- Background-jobs framework (design: docs/design/plans/2026-08-12-jobs-framework-and-viewer.md).
--
-- Before this migration Quasar had eighteen independent goroutines and threads on
-- hand-rolled tickers and NO record anywhere of a job having run: "when did the
-- artwork grabber last run" was unanswerable without reading container logs. Every
-- property an operator wants — last run, duration, result, next run, is it running,
-- may it run at 3pm — is a property of a RECORD OF A RUN, and no such record existed.
--
-- `jobs` is the registry row. Its columns are split by OWNERSHIP and the split is
-- load-bearing:
--   * identity columns (name, description, plane, scope, managed) are owned by CODE
--     and reconciled at every boot by jobs.Registry.Sync;
--   * schedule columns (enabled, schedule_kind, interval_secs, window_*, timezone,
--     history_limit) are owned by an ADMIN and are never overwritten by a sync.
-- A boot that clobbered an admin's 02:00-06:00 window because a developer edited a
-- Definition literal would make the whole surface untrustworthy, so Sync writes the
-- two sets with different SQL rather than one blanket upsert.
--
-- `job_runs` is every run of every job on either plane — a control-plane sweep and an
-- agent-side warm-up produce the same row shape, distinguished only by `host_id`.
-- A `pending` row IS the next run: there is deliberately no `next_run_at` column on
-- `jobs`, because a denormalized next-run timestamp is exactly the kind of derived
-- state that drifts out of sync with the thing it describes.
--
-- NOTE ON SEEDING: there is none here. Which jobs exist is code-owned and reconciled
-- at boot (jobs.Registry.Sync, the same shape as settings.Seed), so adopting a job
-- never means editing an applied migration.
--
-- ROLLBACK: standing repo rule (CLAUDE.md) — once this is applied, never deploy a
-- control-plane binary that embeds only <= 0065; boot's golang-migrate m.Up() will
-- crash-loop with "no migration found for version 66".

BEGIN;

CREATE TABLE jobs (
    id              TEXT PRIMARY KEY,
    name            TEXT        NOT NULL,
    description     TEXT        NOT NULL DEFAULT '',
    plane           TEXT        NOT NULL CHECK (plane IN ('control', 'agent')),
    scope           TEXT        NOT NULL CHECK (scope IN ('instance', 'host')),
    -- managed=false is the "listed but not adopted" state (design §3.7). Such a row
    -- exists so the Jobs page can show an operator that a goroutine they cannot
    -- otherwise see is running, and so the list of unmanaged rows doubles as the
    -- adoption backlog. It is never scheduled and has no run history.
    managed         BOOLEAN     NOT NULL DEFAULT TRUE,
    enabled         BOOLEAN     NOT NULL DEFAULT TRUE,
    schedule_kind   TEXT        NOT NULL CHECK (schedule_kind IN ('interval', 'event', 'manual')),
    -- >= 60s floor: the dispatcher tick is 10s, and a job that wants to run more
    -- often than once a minute is not a background job.
    interval_secs   INTEGER              CHECK (interval_secs IS NULL OR interval_secs >= 60),
    -- Permitted run window, evaluated in `timezone`. A window that wraps midnight
    -- (22:00 -> 04:00) is legal. The window governs STARTING a run, never stopping
    -- one: killing a half-finished dedupe pass or a half-built template is worse
    -- than overrunning by minutes.
    window_start    TIME,
    window_end      TIME,
    -- 0 = Sunday .. 6 = Saturday, matching Go's time.Weekday. Empty = every day.
    -- A day constrains the instant the window OPENS, so a wrapping window on
    -- {5} (Friday) runs Friday 22:00 -> Saturday 04:00.
    window_days     SMALLINT[]  NOT NULL DEFAULT '{}',
    timezone        TEXT        NOT NULL DEFAULT 'UTC',
    history_limit   INTEGER     NOT NULL DEFAULT 50 CHECK (history_limit BETWEEN 1 AND 500),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT jobs_window_paired     CHECK ((window_start IS NULL) = (window_end IS NULL)),
    CONSTRAINT jobs_interval_needed   CHECK (schedule_kind <> 'interval' OR interval_secs IS NOT NULL),
    CONSTRAINT jobs_window_days_valid CHECK (window_days <@ ARRAY[0,1,2,3,4,5,6]::SMALLINT[])
);

CREATE TRIGGER jobs_set_updated_at BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE job_runs (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id         TEXT        NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    -- NULL for scope='instance' jobs; the target host for scope='host' jobs. The
    -- agreement between this and jobs.scope is enforced in Go (jobs.Store), not by
    -- a CHECK: a cross-table invariant is not expressible as a row constraint, and
    -- a tautological CHECK that pretends otherwise is worse than an honest comment.
    host_id        UUID                 REFERENCES hosts(id) ON DELETE CASCADE,
    state          TEXT        NOT NULL CHECK (state IN
                       ('pending','running','succeeded','failed','deferred','skipped','aborted')),
    trigger        TEXT        NOT NULL CHECK (trigger IN ('schedule','manual','event')),
    actor_user_id  UUID                 REFERENCES users(id) ON DELETE SET NULL,
    -- Attempt counter for the deferral ladder (30s doubling, capped at 15 min).
    -- PERSISTED, unlike the in-memory Backoff the #488 warm-up scheduler uses today,
    -- so a give-up/back-off decision survives an agent reconnect and a control-plane
    -- restart -- and, for the first time, is visible to an operator.
    attempt        INTEGER     NOT NULL DEFAULT 1 CHECK (attempt >= 1),
    scheduled_for  TIMESTAMPTZ NOT NULL,
    claimed_at     TIMESTAMPTZ,
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    -- params: the opaque per-job JSON the control plane stored when it materialized
    -- the run (for an event trigger, whatever the event carried). Handed to the
    -- runner verbatim; the framework never interprets it.
    params         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    summary        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    error          TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Same 4096-byte ceiling admin_activity.details already uses. A summary that
    -- blows the bound fails the REPORT, never the run.
    CONSTRAINT job_runs_summary_bounded CHECK (octet_length(summary::text) <= 4096),
    CONSTRAINT job_runs_params_bounded  CHECK (octet_length(params::text) <= 4096)
);

-- At most one pending-or-running run per (job, target). This is THE single-flight
-- guarantee, and it lives in the database rather than in code so that a second
-- dispatcher instance, a double-clicked "Run now" and a racing event trigger are
-- impossible rather than merely unlikely. The zero UUID stands in for "no host" so
-- that instance-scoped rows collide with each other (a plain partial index on a
-- nullable column would not, since NULL <> NULL).
--
-- The index is the INVARIANT, not the error path: every insert goes through
-- jobs.Store.Materialize, which treats a unique violation as "a run is already
-- open" and returns that run instead of surfacing a constraint error.
CREATE UNIQUE INDEX job_runs_open_per_target
    ON job_runs (job_id, COALESCE(host_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE state IN ('pending', 'running');

-- Dispatcher hot paths: due-run scan, abandoned-claim reap, and the two history
-- reads (per job for the viewer, per host for a host detail page).
CREATE INDEX job_runs_due     ON job_runs (scheduled_for) WHERE state = 'pending';
CREATE INDEX job_runs_claimed ON job_runs (claimed_at)    WHERE state = 'running';
CREATE INDEX job_runs_history ON job_runs (job_id, created_at DESC);
CREATE INDEX job_runs_host    ON job_runs (host_id, created_at DESC) WHERE host_id IS NOT NULL;

COMMIT;
