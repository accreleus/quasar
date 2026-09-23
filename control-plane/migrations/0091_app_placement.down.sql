BEGIN;
DROP TRIGGER IF EXISTS rh05_app_placement_default ON apps;
DROP FUNCTION IF EXISTS rh05_app_placement_default_fn();
DROP TRIGGER IF EXISTS rh05_canonical_app_placement ON app_placement;
DROP FUNCTION IF EXISTS rh05_canonical_app_placement_fn();
DROP TABLE IF EXISTS app_placement_hosts;
DROP TABLE IF EXISTS app_placement;
COMMIT;
