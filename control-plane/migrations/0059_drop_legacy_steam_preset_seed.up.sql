-- 0059_drop_legacy_steam_preset_seed.up.sql — #457: exactly ONE runtime preset
-- per library provider after enabling library discovery.
--
-- THE DEFECT. Two independent mechanisms create a "Steam" runtime preset:
--
--   (1) migration 0047 SEEDS a static row (stable id 007ef4c6-…, name 'Steam',
--       image 'quasar-steam:latest'), written when a provider app had to be
--       hand-pointed at a hand-written preset;
--   (2) image-management P5 MATERIALIZES a managed preset from the catalog
--       image's manifest runtime block at install
--       (runtime_presets.managed_image_id = 'steam', name 'Steam (steam)'), and
--       links it on installed_images.runtime_preset_id.
--
-- On a fresh box the operator enables the Steam integration and gets BOTH, and
-- the 0047 one is actively BROKEN there: its image is the LOCAL-ONLY tag
-- `quasar-steam:latest`, which exists on no fresh machine (a registry install is
-- digest-pinned, and a template build is tagged quasar-local/…). The P5 row is
-- the authoritative one — it is what installed_images.runtime_preset_id points
-- at and it carries the adopted ref.
--
-- WHAT THIS MIGRATION DOES, AND WHAT IT DELIBERATELY DOES NOT.
-- It deletes the 0047 seed IF AND ONLY IF the row is still exactly as seeded AND
-- nothing references it. Both guards matter, and this must be safe on EVERY live
-- database (the reference deployment has real data and may legitimately be
-- launching Steam through this exact row):
--
--   * UNEDITED — EVERY operator-editable column still matches the seed verbatim:
--     name, image, description, env, args, mounts, managed_home,
--     home_container_path (0035's schema default '/home/quasar', which 0047 does
--     not override), and managed_image_id still NULL (nothing has adopted this
--     row as an image-managed preset). 0047 promised "Edit freely; this seed
--     never overwrites a changed row"; deleting an operator's edited row would
--     break that promise harder than overwriting it would — and checking only
--     name/image/description would delete the row of an operator who had edited
--     any of the OTHER five (Alice review, PR #460). The predicate must cover the
--     whole editable surface of runtime_presets, not a sample of it.
--   * UNREFERENCED — no apps.runtime_preset_id and no
--     installed_images.runtime_preset_id points at it. Those are the ONLY two
--     referencing columns in the schema (verified across every migration:
--     0035 apps.runtime_preset_id ON DELETE RESTRICT, 0058
--     installed_images.runtime_preset_id ON DELETE SET NULL). The apps FK is
--     RESTRICT, so a referenced row would fail the DELETE and crash-loop the
--     control plane on boot — the guard is what keeps this migration from being
--     a deploy-time outage, not a nicety.
--
-- A deployment that fails either guard keeps its row and simply has two Steam
-- presets, which is correct: one is in use, the other is the one P5 owns.
--
-- WHY NO "ADOPT IT INSTEAD" (i.e. set managed_image_id on the seed row and let
-- P5 keep updating it in place). Adoption is only meaningful for a row that is
-- REFERENCED — an unreferenced unedited row is simply deleted here — and
-- adopting a referenced row would hand it to P5's re-materialization, which
-- rewrites env/args/mounts from the manifest. That would silently wipe the
-- seed's PUID/PGID/UNAME env out from under the live app currently launching
-- with it: a launch-path regression on precisely the deployment this migration
-- must not break. Leaving it alone is the safe half of the trade, and after this
-- migration a fresh box never has the duplicate to begin with.
BEGIN;

DELETE FROM runtime_presets rp
 WHERE rp.id = '007ef4c6-28ee-4a6d-9194-25eb56fa862c'::uuid
   AND rp.name = 'Steam'
   AND rp.image = 'quasar-steam:latest'
   AND rp.description =
       'Shipped default for the quasar-steam provider image — the container spec ' ||
       'Steam library discovery''s provider app is expected to launch with. ' ||
       'Edit freely; this seed never overwrites a changed row.'
   -- jsonb equality is key-order-insensitive, so a re-serialized-but-unchanged
   -- env still matches; a changed VALUE does not.
   AND rp.env    = '{"PUID":"99","PGID":"100","UNAME":"quasar"}'::jsonb
   AND rp.args   = '[]'::jsonb
   AND rp.mounts = '[]'::jsonb
   AND rp.managed_home = true
   -- 0047 does not set home_container_path, so the seed carries 0035's column
   -- default. An operator who repointed the home has edited the row.
   AND rp.home_container_path = '/home/quasar'
   -- Nothing has adopted this row as a P5 image-managed preset (0058). If
   -- something had, deleting it would be deleting a managed row out from under
   -- the materializer.
   AND rp.managed_image_id IS NULL
   AND NOT EXISTS (SELECT 1 FROM apps a WHERE a.runtime_preset_id = rp.id)
   AND NOT EXISTS (SELECT 1 FROM installed_images ii WHERE ii.runtime_preset_id = rp.id);

COMMIT;
