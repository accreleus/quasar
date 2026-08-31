-- First-run experience S1 + S5 (docs/design/plans/2026-08-09-first-run-experience-spec.md).
--
-- S1 — host readiness. The node agent probes its own view of the host (NVIDIA
-- EGL vendor config, libnvidia-eglcore, 32-bit GL, render node, /dev/uinput,
-- user namespaces) and reports the result on the EXISTING host-observability
-- channel (`capacity.readiness`). Stored opaquely as JSONB, exactly like
-- hosts.storage and hosts.effective_settings before it: the check set is
-- agent-owned, so adding, renaming or removing a check must never need a
-- migration or an API change.
--
-- NULL means "no amendment-aware agent has reported yet" and is distinct from
-- '[]' ("reported, nothing to say"). The control plane keeps the last stored
-- value when the field is absent from a report (keep-if-absent), so a stale
-- readiness set is possible; readiness_reported_at is what makes that visible
-- rather than silently presenting old evidence as current.
--
-- ADVISORY ONLY. Nothing in the scheduler, the admission path or session launch
-- reads these columns. A host with every check failing still registers and still
-- runs sessions.
ALTER TABLE hosts
    ADD COLUMN IF NOT EXISTS readiness             JSONB,
    ADD COLUMN IF NOT EXISTS readiness_reported_at TIMESTAMPTZ;

-- S5 — app-exit-before-frames capture.
--
-- failure_code is the machine-readable classification of a terminal failure
-- ('app_exited_early' today). It sits BESIDE error_message rather than
-- replacing it: error_message is operator prose that may be rewritten freely,
-- while this is the stable key the UI branches on. NULL for every failure that
-- carries no classification, and for every non-failed session.
--
-- app_log_tail holds the app container's own last ~100 log lines, captured
-- while it ran. It needs its own column because it cannot share error_message:
-- that field is rendered inline as a one-line reason everywhere it appears, and
-- a hundred lines of Steam output pasted into it would wreck every existing
-- surface. App containers run with `--rm`, so these lines are unrecoverable
-- once the daemon reaps the container — this column is the only copy.
--
-- No index on either: both are read only when a specific session row is already
-- being fetched by id.
ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS failure_code  TEXT,
    ADD COLUMN IF NOT EXISTS app_log_tail  TEXT;
