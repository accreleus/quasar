ALTER TABLE sessions ADD COLUMN home_seed JSONB;

ALTER TABLE job_runs ADD COLUMN template_publish_claim_token UUID,
    ADD COLUMN template_publish_connection_id TEXT,
    ADD COLUMN publish_permit_accepted_at TIMESTAMPTZ;

ALTER TABLE sessions ADD CONSTRAINT sessions_home_seed_ck CHECK (
    home_seed IS NULL OR COALESCE((
        jsonb_typeof(home_seed) = 'object'
        AND home_seed - 'mode' - 'reason' = '{}'::jsonb
        AND jsonb_typeof(home_seed->'mode') = 'string'
        AND jsonb_typeof(home_seed->'reason') = 'string'
        AND (
            (home_seed->>'mode' IN ('reflink', 'copy') AND home_seed->>'reason' = 'seeded')
            OR (home_seed->>'mode' = 'existing' AND home_seed->>'reason' = 'existing_home')
            OR (home_seed->>'mode' = 'cold' AND home_seed->>'reason' IN (
                'template_unavailable', 'source_disabled', 'host_templates_disabled',
                'host_setting_invalid', 'policy_unavailable', 'storage_unavailable',
                'clone_failed', 'policy_changed'))
        )
    ), false)
);
