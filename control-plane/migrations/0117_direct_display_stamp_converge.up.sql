-- 0117 — converge the direct_display stamp for apps 0103 and the adoption moves missed (#583).
--
-- Data only, idempotent. Same statement as 0103 (images.stampDirectDisplay is
-- its Go twin) with one more guard: an app that sets its own runtime_spec.image
-- launches that image, not the managed preset's, so it is left alone. Catches
-- an app linked back to the managed preset after 0103 ran, which no adoption
-- move would stamp. Rows 0103 already stamped stay stamped: they cannot be told
-- from an operator's explicit true.
BEGIN;

UPDATE apps
SET runtime_spec = apps.runtime_spec || '{"direct_display": true}'::jsonb
FROM installed_images ii
JOIN image_catalog ic ON ic.id = ii.image_id
WHERE apps.runtime_preset_id = ii.runtime_preset_id
  AND ii.version = ic.version
  AND ic.runtime -> 'direct_display' = 'true'::jsonb
  AND NOT apps.runtime_spec ? 'direct_display'
  AND COALESCE(apps.runtime_spec->>'image', '') = '';

COMMIT;
