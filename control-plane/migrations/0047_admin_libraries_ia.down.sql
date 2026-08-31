-- 0047_admin_libraries_ia.down.sql — reverse of 0047 in symmetric order.
--
-- The two instance_settings columns are dropped unconditionally: they carry no
-- foreign-key-shaped state, so there is nothing else to check before removing
-- them (same posture as 0045's library_discovery_enabled drop).
--
-- The seeded Steam runtime preset is deleted ONLY IF NO APP REFERENCES IT
-- (apps.runtime_preset_id), mirroring 0046's guarded-delete pattern for
-- retired rungs: a still-referenced row is left in place with a RAISE NOTICE
-- rather than breaking the apps.runtime_preset_id foreign key (0035,
-- ON DELETE RESTRICT would fail this migration outright otherwise) or
-- silently stripping the runtime spec out from under a live app. An operator
-- who adopted this preset for their Steam app and then rolls back keeps a
-- working app; an operator who never touched it gets a clean revert.
BEGIN;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM apps
         WHERE runtime_preset_id = '007ef4c6-28ee-4a6d-9194-25eb56fa862c'
    ) THEN
        RAISE NOTICE '0047: default Steam runtime preset 007ef4c6-28ee-4a6d-9194-25eb56fa862c is still referenced by an app; leaving it in place.';
    ELSE
        DELETE FROM runtime_presets WHERE id = '007ef4c6-28ee-4a6d-9194-25eb56fa862c';
    END IF;
END $$;

ALTER TABLE instance_settings
    DROP COLUMN IF EXISTS library_discovery_interval_minutes,
    DROP COLUMN IF EXISTS library_discovery_appdetails_enabled;

COMMIT;
