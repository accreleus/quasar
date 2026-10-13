-- 0103 down — nothing to undo structurally (the up was data only).
--
-- Which rows carried `direct_display` before the up is not recoverable. A
-- control plane that predates 0103 reads the key the same way, and removing it
-- would only restore the console that does not launch.
BEGIN;
COMMIT;
