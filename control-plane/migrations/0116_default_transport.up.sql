-- 0116 — instance default transport (amendment 61, #1217). Semantics: schema.md.
-- One data change: every launch profile on 'webrtc' becomes 'default'. With default_transport at
-- 'webrtc' that is the same transport, so no launch changes on upgrade.
BEGIN;

ALTER TABLE instance_settings
    ADD COLUMN default_transport TEXT NOT NULL DEFAULT 'webrtc'
        CHECK (default_transport IN ('webrtc','webtransport'));

ALTER TABLE launch_profiles DROP CONSTRAINT launch_profiles_transport_check;
ALTER TABLE launch_profiles
    ADD CONSTRAINT launch_profiles_transport_check
        CHECK (transport IN ('default','webrtc','webtransport_opt_in','webtransport')),
    ALTER COLUMN transport SET DEFAULT 'default';
UPDATE launch_profiles SET transport = 'default' WHERE transport = 'webrtc';

COMMIT;
