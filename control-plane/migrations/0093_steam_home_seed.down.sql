ALTER TABLE sessions DROP COLUMN home_seed;
ALTER TABLE job_runs DROP COLUMN template_publish_claim_token,
    DROP COLUMN template_publish_connection_id,
    DROP COLUMN publish_permit_accepted_at;
