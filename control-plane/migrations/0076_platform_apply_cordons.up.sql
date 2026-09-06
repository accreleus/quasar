-- 0076 — a fleet run remembers the cordons it imposed
-- (platform-release amendment 2, #117; schema.md `platform_apply_runs`).
--
-- PURELY ADDITIVE: one column with a default, no existing column touched, and
-- nothing on the wire — `PlatformApplyRun` gains no field.
--
-- A fleet run cordons every host for its control-plane step, and its FIRST
-- TARGET RESTARTS THE CONTROL PLANE. Holding the pre-run scheduling state in
-- memory therefore loses it exactly when the run needs it, and the fleet is
-- left `draining` with nothing left that knows to lift it. Observed live on a
-- registry install: a failed run left both hosts out of scheduling.
ALTER TABLE platform_apply_runs
    ADD COLUMN cordoned_hosts JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN platform_apply_runs.cordoned_hosts IS
    'What the run found before it cordoned: [{host_id, was_cordoned}]. Restored on every terminal path, including after the restart the run itself causes. Not served.';
