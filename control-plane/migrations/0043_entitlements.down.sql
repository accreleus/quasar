-- 0043_entitlements.down.sql — reverse of 0043.
--
-- Dropping the table drops its indexes, its CHECKs, and every row including the
-- backfill, which is the exact inverse of the up migration: with no entitlements
-- table there is no filter, and every enabled app is visible to everyone again —
-- the pre-0043 behaviour, and exactly what the backfill was chosen to reproduce.
--
-- WHAT IS LOST, stated plainly: every grant an admin made after the migration.
-- Re-applying 0043 re-runs the backfill, so the catalogue comes back FULLY
-- OPEN rather than at whatever narrower state the operator had configured. This
-- is a widening, not a lockout, so it fails safe for availability and unsafe for
-- confidentiality — take a dump of `entitlements` before rolling back a
-- deployment where anything has actually been restricted.
--
-- NOTE the one-way rule in CLAUDE.md: never roll a control-plane binary back
-- BELOW the DB's applied migration version. This file is for a deliberate,
-- operator-run down-migration, not for "deploy main over the phase branch".
BEGIN;

DROP TABLE IF EXISTS entitlements;

COMMIT;
