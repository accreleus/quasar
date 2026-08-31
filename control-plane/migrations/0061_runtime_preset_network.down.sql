-- Down: drop the additive column (its CHECK constraint goes with it). Any
-- preset that stated a network reverts to the agent's host default.
BEGIN;

ALTER TABLE runtime_presets DROP COLUMN network;

COMMIT;
