-- 0060_provider_app_suspension.up.sql — #456 follow-up (Alice review, PR #460):
-- distinguish "the control plane turned this provider app off because library
-- discovery was switched off" from "the operator turned it off".
--
-- THE DEFECT THIS FIXES. The first cut disabled provider apps on a discovery
-- disable and re-enabled every disabled provider app on a discovery enable.
-- `apps.enabled` alone cannot tell the two intents apart, so:
--
--     operator disables the Steam app in /admin
--       → operator toggles library discovery off, then on again
--       → the Steam app is BACK ON, against the operator's explicit wish.
--
-- A boolean that records WHY the row is off closes it. It is a column and not an
-- inference because the intent exists only at the moment of the write; nothing
-- about the row afterwards can reconstruct it.
--
-- SEMANTICS, and both halves are conditional on purpose:
--   suspend  UPDATE ... SET enabled = false, library_discovery_suspended = true
--            WHERE enabled = true      -- an already-off app is left alone, so an
--                                         operator's off is never relabelled as
--                                         ours and never becomes restorable.
--   restore  UPDATE ... SET enabled = true, library_discovery_suspended = false
--            WHERE library_discovery_suspended = true
--                                      -- ONLY rows we suspended come back.
--
-- Default false, so every existing app (and every hand-created one) reads as
-- "not suspended by us" and is therefore never auto-restored. No backfill: false
-- is the correct value for every pre-existing row, including apps an operator
-- has already disabled by hand.
BEGIN;

ALTER TABLE apps
    ADD COLUMN library_discovery_suspended BOOLEAN NOT NULL DEFAULT false;

-- SUSPENDED IMPLIES DISABLED. The marker only ever means "this app is OFF
-- because discovery is off"; an enabled row carrying it is a contradiction that
-- would make the restore pass a no-op UPDATE on an already-live app and hide a
-- bug in whichever writer left it behind. Expressed as an implication
-- (suspended → NOT enabled) so the ordinary rows — enabled apps and
-- operator-disabled apps, both with the marker false — are unaffected.
--
-- It is also what forces the admin app-update path to clear the marker whenever
-- it writes `enabled` (internal/crud/store.go): an operator re-enabling a
-- suspended app must take ownership of it rather than leave a row the reconciler
-- still believes it owns.
ALTER TABLE apps
    ADD CONSTRAINT apps_suspended_implies_disabled_ck
        CHECK (NOT library_discovery_suspended OR NOT enabled);

COMMENT ON COLUMN apps.library_discovery_suspended IS
    'True only while this app is disabled BECAUSE library discovery is switched off (the control plane suspended it), so re-enabling discovery restores exactly these rows and never an app the operator disabled themselves. Set by the provider reconciler, never by the admin API. Meaningless when enabled = true.';

-- The reconciler''s restore pass filters on this column across the whole apps
-- table; partial (WHERE ... = true) because the column is false for essentially
-- every row and only the true ones are ever selected on.
CREATE INDEX apps_library_discovery_suspended_idx
    ON apps (library_discovery_suspended) WHERE library_discovery_suspended;

COMMIT;
