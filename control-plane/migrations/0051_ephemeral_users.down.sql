BEGIN;
DROP INDEX IF EXISTS users_ephemeral_expires_at_idx;
ALTER TABLE users DROP COLUMN IF EXISTS ephemeral_expires_at;
COMMIT;
