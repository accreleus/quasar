BEGIN;

DROP INDEX IF EXISTS sessions_active_gpu_idx;
CREATE INDEX sessions_active_gpu_idx ON sessions (gpu_id)
    WHERE state IN ('assigned','starting','running');

COMMIT;
