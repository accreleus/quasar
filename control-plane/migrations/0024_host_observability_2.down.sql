BEGIN;

ALTER TABLE gpus
    DROP COLUMN IF EXISTS render_node;

ALTER TABLE hosts
    DROP COLUMN IF EXISTS pending_restart,
    DROP COLUMN IF EXISTS cpu_model;

COMMIT;
