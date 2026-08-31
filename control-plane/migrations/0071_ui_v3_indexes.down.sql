-- Reverses 0071_ui_v3_indexes.up.sql. Dropping a read-path index loses no data;
-- the two aggregates fall back to a sequential scan.
DROP INDEX IF EXISTS sessions_user_active_idx;
DROP INDEX IF EXISTS sessions_app_created_idx;
