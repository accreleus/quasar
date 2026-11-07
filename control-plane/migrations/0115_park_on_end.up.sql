-- 0115_park_on_end.up.sql — a mapping change must reach every Steam started before it (#1070).
-- A play session's runtime is parked when its game exits; the supersede clears this flag on every
-- live play session so that park never keeps a Steam that loaded the old mapping.
-- Semantics: control-api.md §Compatibility tools, "Hot Loading"; schema.md `sessions`.
BEGIN;

ALTER TABLE sessions ADD COLUMN park_on_end BOOLEAN NOT NULL DEFAULT true;

COMMIT;
