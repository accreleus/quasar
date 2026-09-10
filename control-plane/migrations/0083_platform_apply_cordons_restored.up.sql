-- 0083 — a fleet run's scheduling cleanup is a recorded fact, not an inference
-- (#176).
--
-- PURELY ADDITIVE: one nullable column, no existing column touched, and nothing
-- on the wire — `PlatformApplyRun` gains no field, so this is invisible to
-- control-api.md and to every client.
--
-- `finish` writes the terminal state and only THEN restores the cordons the run
-- imposed (migration 0076). If that restore fails — or the process dies in the
-- window — the run is already terminal, `ActiveRun` selects only non-terminal
-- runs, and so nothing on the next boot knows the cleanup is unfinished. The
-- host stays `draining` with one ERROR line as the only evidence.
--
-- This column is what carries the unfinished cleanup across that boundary: unset
-- on a terminal run whose `cordoned_hosts` is non-empty means "this run's
-- scheduling changes have not been proven undone", and the boot sweep retries it.
ALTER TABLE platform_apply_runs
    ADD COLUMN cordons_restored_at TIMESTAMPTZ;

COMMENT ON COLUMN platform_apply_runs.cordons_restored_at IS
    'When this run''s scheduling changes were proven undone (every host it cordoned back in scheduling, every admin cordon it found put back). NULL on a terminal run with a non-empty cordoned_hosts is a recovery requirement the next boot retries. Not served.';

-- Runs that are ALREADY terminal predate this column, so nothing recorded
-- whether their cleanup finished. Stamp them rather than sweeping them: their
-- `cordoned_hosts` may carry the pre-#170 mis-recording that read an
-- already-offline host as one the run had cordoned, and acting on that would
-- lift an operator's own cordon. #170 fixed the ordinary case for live runs;
-- this migration deliberately does not reinterpret history.
UPDATE platform_apply_runs
   SET cordons_restored_at = COALESCE(finished_at, created_at)
 WHERE state IN ('succeeded', 'failed', 'cancelled');
