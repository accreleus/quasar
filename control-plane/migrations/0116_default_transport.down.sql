-- Never refuses: each 'default' row takes the instance's current default_transport, so no
-- profile's effective transport changes. Then 0111's CHECK and DEFAULT come back.
BEGIN;

UPDATE launch_profiles
   SET transport = COALESCE((SELECT default_transport FROM instance_settings WHERE id = true), 'webrtc')
 WHERE transport = 'default';
ALTER TABLE launch_profiles DROP CONSTRAINT launch_profiles_transport_check;
ALTER TABLE launch_profiles
    ADD CONSTRAINT launch_profiles_transport_check
        CHECK (transport IN ('webrtc','webtransport_opt_in','webtransport')),
    ALTER COLUMN transport SET DEFAULT 'webrtc';
ALTER TABLE instance_settings DROP COLUMN IF EXISTS default_transport;

COMMIT;
