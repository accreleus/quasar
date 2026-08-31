-- Reverse of 0001_initial_schema.up.sql. Drops in FK-safe order.

BEGIN;

DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS gpus;
DROP TABLE IF EXISTS hosts;
DROP TABLE IF EXISTS apps;
DROP TABLE IF EXISTS auth_tokens;
DROP TABLE IF EXISTS users;

DROP FUNCTION IF EXISTS set_updated_at();

COMMIT;
