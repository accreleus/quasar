BEGIN;

-- A pipeline in `stopping` is still draining consumers and Vulkan image refs.
-- Keep its GPU reservation indexed until the agent's terminal callback proves
-- teardown complete, preventing a replacement pipeline from overlapping it.
DROP INDEX IF EXISTS sessions_active_gpu_idx;
CREATE INDEX sessions_active_gpu_idx ON sessions (gpu_id)
    WHERE state IN ('assigned','starting','running','stopping');

COMMIT;
