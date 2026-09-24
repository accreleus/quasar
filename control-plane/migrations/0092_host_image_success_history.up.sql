-- RH05 #343: successful managed-image version identity survives inventory loss.
CREATE TABLE host_image_success_history (
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    image_id TEXT NOT NULL REFERENCES image_catalog(id) ON DELETE CASCADE,
    current_version TEXT NOT NULL,
    current_identity JSONB NOT NULL,
    previous_version TEXT,
    previous_identity JSONB,
    verified_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT host_image_history_previous_pair CHECK ((previous_version IS NULL) = (previous_identity IS NULL)),
    PRIMARY KEY (host_id, image_id)
);
