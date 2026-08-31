CREATE TABLE session_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id  UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX session_tokens_session_created_idx
    ON session_tokens (session_id, created_at DESC);

INSERT INTO session_tokens (session_id, token_hash, expires_at, consumed_at, created_at)
SELECT id, signaling_token_hash, signaling_token_expires_at,
       signaling_token_consumed_at, created_at
FROM sessions
WHERE signaling_token_hash IS NOT NULL
ON CONFLICT (token_hash) DO NOTHING;
