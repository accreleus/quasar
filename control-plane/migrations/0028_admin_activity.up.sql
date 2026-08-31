CREATE TABLE admin_activity (
    id            BIGSERIAL PRIMARY KEY,
    actor_user_id UUID,
    action        TEXT NOT NULL CHECK (length(action) BETWEEN 1 AND 80),
    target_type   TEXT NOT NULL CHECK (length(target_type) BETWEEN 1 AND 40),
    target_id     TEXT CHECK (length(target_id) <= 200),
    details       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (octet_length(details::text) <= 4096)
);

CREATE INDEX admin_activity_created_idx ON admin_activity (created_at DESC, id DESC);
