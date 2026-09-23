DROP TRIGGER IF EXISTS rh05_seed_host_policy_revision_after_host ON hosts;
DROP FUNCTION IF EXISTS rh05_seed_host_policy_revision();
DROP TABLE IF EXISTS host_reconcile_obligations;
DROP TABLE IF EXISTS host_setting_groups;
DROP TABLE IF EXISTS host_setting_choices;
DROP TABLE IF EXISTS host_policy_revisions;
ALTER TABLE hosts DROP CONSTRAINT IF EXISTS hosts_config_policy_delivery_gate,
    DROP CONSTRAINT IF EXISTS hosts_deployment_settings_identity,
    DROP CONSTRAINT IF EXISTS hosts_deployment_settings_shape,
    DROP COLUMN IF EXISTS config_policy_delivery_id,
    DROP COLUMN IF EXISTS config_policy_gate_connection,
    DROP COLUMN IF EXISTS config_policy_reported_at,
    DROP COLUMN IF EXISTS config_policy_ever_owned_groups,
    DROP COLUMN IF EXISTS config_policy_confirmed_groups,
    DROP COLUMN IF EXISTS config_policy_advertised_groups,
    DROP COLUMN IF EXISTS config_policy_versions,
    DROP COLUMN IF EXISTS deployment_settings_reported_at,
    DROP COLUMN IF EXISTS deployment_settings_connection,
    DROP COLUMN IF EXISTS deployment_settings;
