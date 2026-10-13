-- 0103 — give existing apps the direct_display their image's manifest declares (#580).
--
-- Data only, idempotent. A console session starts only a default app whose
-- runtime_spec declares direct_display. Nothing gave the key to an app that
-- existed before its manifest declared it, so a console whose default app was
-- created earlier stopped launching.
--
-- An app that launches an installed catalog image through that image's managed
-- preset takes `direct_display: true` when the stored manifest declares it for
-- the installed version. A catalog already synced past the adoption describes
-- an image this install does not run yet: those apps are stamped by the image
-- update instead (images.stampDirectDisplay, the Go twin of this statement).
--
-- Never touched: an app on another preset or on its own image (the operator
-- repointed it) and a spec that already has the key (an explicit false is the
-- operator's). A derived tile links no preset (apps_derived_shape_ck), so it
-- cannot match.
BEGIN;

UPDATE apps
SET runtime_spec = apps.runtime_spec || '{"direct_display": true}'::jsonb
FROM installed_images ii
JOIN image_catalog ic ON ic.id = ii.image_id
WHERE apps.runtime_preset_id = ii.runtime_preset_id
  AND ii.version = ic.version
  AND ic.runtime -> 'direct_display' = 'true'::jsonb
  AND NOT apps.runtime_spec ? 'direct_display';

COMMIT;
