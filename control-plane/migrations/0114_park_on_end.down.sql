-- Drops the flag; every play session is parkable again. Semantics: schema.md.
BEGIN;

ALTER TABLE sessions DROP COLUMN IF EXISTS park_on_end;

COMMIT;
