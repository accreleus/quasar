-- 0067_agent_restart_visibility.down.sql — reverse of 0067.
--
-- Drops all four columns. What is lost: the derived agent-process-uptime
-- estimate and the cumulative restart count/last-restart timestamp. Nothing
-- else in the schema depends on them (purely additive, admin-API-only
-- surface), and a roll-forward re-derives them fresh from the next
-- enroll/reconnect cycle per host — same posture as every other additive
-- amendment's down migration in this ledger.
BEGIN;

ALTER TABLE hosts
    DROP COLUMN IF EXISTS agent_disconnected_at,
    DROP COLUMN IF EXISTS agent_last_restart_at,
    DROP COLUMN IF EXISTS agent_restart_count,
    DROP COLUMN IF EXISTS agent_process_started_at;

COMMIT;
