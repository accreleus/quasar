-- 0099_console_config_trim.up.sql — amendment 19 (#453/#455): console sessions
-- drive the display directly. The console settings that meant something only
-- to the retired local-display path leave every stored console_config row; the
-- six that remain (enabled, output_id, input_devices, auto_start_on_display,
-- default_app, default_user) are untouched. The control plane also ignores
-- unknown keys on read, so a row this migration has not seen never blocks a
-- read or an upgrade. Prose: protocol/schema.md `console_config`.
BEGIN;

UPDATE console_config
   SET config = config - ARRAY[
           'connector', 'mode', 'compositor', 'audio_output', 'stream',
           'stream_audio', 'grab', 'auto_connect_controller', 'fullscreen'
       ]::text[]
 WHERE config ?| ARRAY[
           'connector', 'mode', 'compositor', 'audio_output', 'stream',
           'stream_audio', 'grab', 'auto_connect_controller', 'fullscreen'
       ]::text[];

COMMIT;
