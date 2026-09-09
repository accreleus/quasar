-- 0081 — unattended automatic apply of platform releases (#122).
--
-- PURELY ADDITIVE: two defaulted booleans, no new table, no existing column
-- changed. Both default false, so the upgrade is a behavioural no-op — an
-- instance that never opts in behaves exactly as it does today.

-- ── instance_settings: the opt-in ───────────────────────────────────────────
--
-- Beside release_channel and release_webhook_enabled, and for the same reason:
-- one instance-wide operator setting, one control plane, one value, set through
-- PATCH /v1/admin/settings — which already carries the admin gate, the
-- one-transaction apply and the changed-keys audit record.
--
-- THERE IS NO WINDOW COLUMN, DELIBERATELY. Unattended apply fires on a
-- successful `platform.release_detect` pass, so the detection schedule IS the
-- window: it is already a cron in the Jobs tab and already editable there, and
-- an admin moves the update hour by moving the job they already own. A second
-- time model here could disagree with that one, and would need its own
-- validation, rendering and documentation to say the same thing twice.
BEGIN;

ALTER TABLE instance_settings
    ADD COLUMN IF NOT EXISTS platform_auto_apply BOOLEAN NOT NULL DEFAULT false;

-- ── platform_apply_runs: who started it ─────────────────────────────────────
--
-- THIS COLUMN IS WHAT MAKES THE FAILURE POLICY EXPRESSIBLE. An unattended run
-- that fails must not be retried on the next detection pass — a genuinely bad
-- release would otherwise be re-attempted once a week for ever — but "has
-- unattended apply already failed on this release" cannot be answered from a run
-- history that does not record who started the run. requested_by is NULL for an
-- unattended run and also NULL for a run whose requesting admin has since been
-- deleted (it is ON DELETE SET NULL), so it cannot stand in for this.
--
-- The suppression is per RELEASE, not global: a newer release is tried, and an
-- admin applying the failed one by hand clears it. One flaky host must not end
-- automatic updates for the whole instance.
ALTER TABLE platform_apply_runs
    ADD COLUMN IF NOT EXISTS unattended BOOLEAN NOT NULL DEFAULT false;

-- The suppression read asks "what was the MOST RECENT run on this release, and
-- was it a failed unattended one" (DISTINCT ON (release_id) ORDER BY
-- created_at DESC), so the index is the ordering that query walks. Not a partial
-- index on `unattended`: the query has to see an ADMIN's later run to know the
-- suppression is cleared, so it cannot filter unattended rows out at the index.
CREATE INDEX IF NOT EXISTS platform_apply_runs_release_recent_idx
    ON platform_apply_runs (release_id, created_at DESC, id DESC);

COMMIT;
