-- Refuses while a live session uses WebTransport: an older binary would treat its transport tokens
-- as signaling tokens. Recovery after a refusal: once those sessions have ended, run
-- `migrate ... force 111`, then the down again.
BEGIN;

DO $$
BEGIN
	IF EXISTS (SELECT 1 FROM sessions
	           WHERE transport = 'webtransport' AND state NOT IN ('stopped', 'failed')) THEN
		RAISE EXCEPTION '0111 down: a WebTransport session is still live; end it first';
	END IF;
END $$;

ALTER TABLE session_tokens DROP COLUMN IF EXISTS kind;
ALTER TABLE sessions
    DROP COLUMN IF EXISTS transport_fallback_reason,
    DROP COLUMN IF EXISTS transport_reason,
    DROP COLUMN IF EXISTS transport;
ALTER TABLE hosts
    DROP COLUMN IF EXISTS wt_public_certificate,
    DROP COLUMN IF EXISTS wt_public_url,
    DROP COLUMN IF EXISTS wt_certificates,
    DROP COLUMN IF EXISTS wt_addresses,
    DROP COLUMN IF EXISTS wt_port,
    DROP COLUMN IF EXISTS transports;
ALTER TABLE launch_profiles DROP COLUMN IF EXISTS transport;

COMMIT;
