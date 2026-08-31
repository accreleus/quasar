-- 0037_app_launch_profiles.down.sql — reverse of 0037.
--
-- The up migration is purely additive and creates exactly one table, so the down
-- path is a genuine inverse: no snapshot is needed (unlike 0036, whose fan-out is
-- lossy), and no data outside this table is touched. Dropping the table restores
-- the pre-UI-P5 behaviour exactly — every app becomes unrestricted again, which
-- is what an empty allow-list already means.
--
-- The one honest caveat: any allow-list an operator configured while 0037 was
-- applied is DISCARDED, not preserved. There is nowhere to preserve it to — the
-- pre-0037 schema has no representation for it.
BEGIN;

DROP TABLE IF EXISTS app_launch_profiles;

COMMIT;
