-- RH05 #345: serialize explicit cleanup with managed-image requirements and launch.
CREATE TABLE host_image_operation_fences (
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    image_id TEXT NOT NULL REFERENCES image_catalog(id) ON DELETE CASCADE,
    generation BIGINT NOT NULL DEFAULT 0 CHECK (generation >= 0),
    state TEXT NOT NULL CHECK (state IN ('idle', 'removing')),
    attempt_id UUID,
    PRIMARY KEY (host_id, image_id)
);
