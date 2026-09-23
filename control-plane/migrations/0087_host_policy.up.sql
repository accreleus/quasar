-- RH05 typed host policy. Legacy overrides remain for older agents.
ALTER TABLE hosts ADD COLUMN deployment_settings JSONB NULL,
    ADD COLUMN deployment_settings_connection UUID NULL,
    ADD COLUMN deployment_settings_reported_at TIMESTAMPTZ NULL,
    ADD COLUMN config_policy_versions JSONB NULL,
    ADD COLUMN config_policy_advertised_groups JSONB NULL,
    ADD COLUMN config_policy_confirmed_groups JSONB NULL,
    ADD COLUMN config_policy_ever_owned_groups JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN config_policy_reported_at TIMESTAMPTZ NULL,
    ADD COLUMN config_policy_gate_connection UUID NULL,
    ADD COLUMN config_policy_delivery_id UUID NULL,
    ADD CONSTRAINT hosts_deployment_settings_shape CHECK (deployment_settings IS NULL OR jsonb_typeof(deployment_settings) = 'object'),
    ADD CONSTRAINT hosts_deployment_settings_identity CHECK ((deployment_settings IS NULL) = (deployment_settings_connection IS NULL) AND (deployment_settings IS NULL) = (deployment_settings_reported_at IS NULL)),
    ADD CONSTRAINT hosts_config_policy_delivery_gate CHECK (config_policy_delivery_id IS NULL OR config_policy_gate_connection IS NOT NULL);

CREATE TABLE host_policy_revisions (
    host_id UUID PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID NULL REFERENCES users(id) ON DELETE SET NULL
);

CREATE TABLE host_setting_choices (
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    source TEXT NOT NULL CHECK (source IN ('automatic', 'deployment', 'explicit')),
    explicit_value JSONB NULL,
    revision BIGINT NOT NULL CHECK (revision >= 0),
    PRIMARY KEY (host_id, key),
    CHECK ((source = 'explicit') = (explicit_value IS NOT NULL)),
    CHECK (explicit_value IS NULL OR jsonb_typeof(explicit_value) <> 'null')
);

CREATE TABLE host_setting_groups (
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    group_key TEXT NOT NULL,
    desired_revision BIGINT NOT NULL CHECK (desired_revision >= 0),
    desired_digest TEXT NULL,
    applied_revision BIGINT NULL CHECK (applied_revision >= 0),
    applied_digest TEXT NULL,
    scope TEXT NOT NULL CHECK (scope IN ('next_session', 'restart')),
    status TEXT NOT NULL CHECK (status IN ('pending', 'applied', 'failed', 'upgrade_required', 'uncertain')),
    evidence_connection UUID NULL,
    evidence_at TIMESTAMPTZ NULL,
    PRIMARY KEY (host_id, group_key),
    CHECK (applied_revision IS NULL OR applied_revision <= desired_revision),
    CHECK (status <> 'applied' OR ((desired_digest IS NOT NULL AND applied_digest = desired_digest AND applied_revision = desired_revision) IS TRUE))
);

CREATE TABLE host_reconcile_obligations (
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    resource_key TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    retry_count INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    PRIMARY KEY (host_id, kind, resource_key)
);

CREATE FUNCTION rh05_seed_host_policy_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO host_policy_revisions (host_id) VALUES (NEW.id);
    RETURN NEW;
END;
$$;
CREATE TRIGGER rh05_seed_host_policy_revision_after_host
    AFTER INSERT ON hosts FOR EACH ROW EXECUTE FUNCTION rh05_seed_host_policy_revision();

INSERT INTO host_policy_revisions (host_id) SELECT id FROM hosts;
INSERT INTO host_setting_choices (host_id, key, source, explicit_value, revision)
SELECT hs.host_id, item.key, 'explicit', item.value, 0
FROM host_settings hs CROSS JOIN LATERAL jsonb_each(hs.overrides) item
WHERE item.value <> 'null'::jsonb;
