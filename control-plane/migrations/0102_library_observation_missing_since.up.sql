-- 0102_library_observation_missing_since.up.sql — schema.md amendment 25 (#521):
-- when a successful scan first did not list this observation; NULL while it is
-- seen. Prose: protocol/schema.md `library_observations.missing_since`.
ALTER TABLE library_observations ADD COLUMN IF NOT EXISTS missing_since TIMESTAMPTZ;
