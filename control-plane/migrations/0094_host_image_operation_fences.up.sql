-- RH05 #345: serialize explicit cleanup with managed-image requirements and launch.
CREATE TABLE host_image_operation_fences (
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    image_id TEXT NOT NULL,
    generation BIGINT NOT NULL DEFAULT 0 CHECK (generation >= 0),
    state TEXT NOT NULL CHECK (state IN ('idle', 'removing')),
    attempt_id UUID,
    PRIMARY KEY (host_id, image_id)
);

CREATE TABLE host_image_cleanup_attempts (
    id UUID PRIMARY KEY,
    host_id UUID NOT NULL,
    image_id TEXT NOT NULL,
    version TEXT NOT NULL,
    image_ref TEXT NOT NULL,
    runtime_image_id TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation >= 0),
    state TEXT NOT NULL CHECK (state IN ('removing', 'removed', 'failed', 'unknown')),
    reason TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (host_id, image_id)
        REFERENCES host_image_operation_fences(host_id, image_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX host_image_cleanup_one_active
    ON host_image_cleanup_attempts(host_id, image_id)
    WHERE state IN ('removing', 'unknown');

-- Catalog sync may prune and later re-add the same managed ID. Preserve the
-- frozen previous-success pair across that cycle.
ALTER TABLE host_image_success_history
    DROP CONSTRAINT host_image_success_history_image_id_fkey;
