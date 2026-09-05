-- Reverse of 0075. What is lost is the apply history, including the previous
-- digests a revert reads back; nothing running is affected. Attempts first:
-- they reference runs.
DROP TABLE IF EXISTS platform_apply_attempts;
DROP TABLE IF EXISTS platform_apply_runs;
