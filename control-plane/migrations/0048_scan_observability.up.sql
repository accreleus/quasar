-- 0048_scan_observability.up.sql — scan observability + backfill amendment
-- (protocol/control-api.md "Amendment — scan observability + backfill
-- (2026-08-01, same-day follow-on)", protocol pin 4dd4691, Michael-signed-off).
--
-- Adds the per-scan OUTCOME COUNTS to library_scans (0045), so a scan's work
-- is visible after the fact rather than living only in the reconcile log
-- line. Eight columns, one per ReconcileResult field plus `backfilled` (the
-- new backfill step in internal/library/store.go's Reconcile): observed,
-- suppressed, created, disabled, granted, revoked, rejected, backfilled.
--
-- INTEGER NOT NULL DEFAULT 0, deliberately. A row from before this migration
-- reads every count as zero once the column exists — there is no way to
-- reconstruct what an already-terminal scan did — and the contract is
-- explicit that a UI must present that as "not recorded", never as "this
-- scan did nothing". Reconcile (successful scans) and MarkFailed (failed
-- scans) both leave a freshly-terminal row with real counts: MarkFailed
-- writes true zeros for a failed walk (nothing was observed, because nothing
-- ran to completion) precisely because the DEFAULT already says that;
-- Reconcile's UPDATE overwrites the default with the counts it computed, in
-- the SAME transaction that marks the row 'reported'.
BEGIN;

ALTER TABLE library_scans
    ADD COLUMN observed   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN suppressed INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN created    INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN disabled   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN granted    INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN revoked    INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN rejected   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN backfilled INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN library_scans.observed IS
    'Per-scan outcome count, stored at reconcile (migration 0048). Rows from before this migration read zero, which a UI presents as "not recorded", not as "nothing happened".';
COMMENT ON COLUMN library_scans.backfilled IS
    'Existing tiles whose blank description this scan filled in (scan-observability amendment). Fill-blanks-only: never counts a field that already had a value.';

COMMIT;
