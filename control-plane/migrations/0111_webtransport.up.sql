-- 0111 — WebTransport, transport #2 (amendment 54, #1026).
--
-- Additive: defaulted or nullable columns and their CHECKs. Every existing profile, host, session
-- and token reads as WebRTC. Semantics: schema.md.
BEGIN;

ALTER TABLE launch_profiles
    ADD COLUMN transport TEXT NOT NULL DEFAULT 'webrtc'
        CHECK (transport IN ('webrtc','webtransport_opt_in','webtransport'));

ALTER TABLE hosts
    ADD COLUMN transports JSONB NULL,
    ADD COLUMN wt_port INT NULL CHECK (wt_port IS NULL OR wt_port BETWEEN 1 AND 65535),
    ADD COLUMN wt_addresses JSONB NULL,
    ADD COLUMN wt_certificates JSONB NULL,
    ADD COLUMN wt_public_url TEXT NULL,
    ADD COLUMN wt_public_certificate TEXT NOT NULL DEFAULT 'public'
        CHECK (wt_public_certificate IN ('public','agent'));

ALTER TABLE sessions
    ADD COLUMN transport TEXT NOT NULL DEFAULT 'webrtc' CHECK (transport IN ('webrtc','webtransport')),
    ADD COLUMN transport_reason TEXT NULL,
    ADD COLUMN transport_fallback_reason TEXT NULL;

ALTER TABLE session_tokens
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'signaling' CHECK (kind IN ('signaling','transport'));

COMMIT;
