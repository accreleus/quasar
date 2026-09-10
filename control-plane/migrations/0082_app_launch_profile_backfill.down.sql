-- 0082 down — nothing to undo structurally (the up was data only).
--
-- The previous per-row values of `gpu` / `no_new_privileges` /
-- `systempaths_unconfined` are not recoverable, and restoring them would only
-- restore the launch failure the up fixed. A control plane that predates 0082
-- launches these rows exactly as the stamped values say, which is what the
-- image requires.
BEGIN;
COMMIT;
