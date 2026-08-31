-- 0067_agent_restart_visibility.up.sql — node-agent restart visibility (#429
-- follow-on). The node-agent container intermittently dies and Docker's
-- `unless-stopped` policy revives it in ~1s, invisibly: the admin UI shows
-- `online`, the heartbeat resumes, and an operator has no way to know the
-- agent died. This makes it visible entirely from signals the control plane
-- already owns (it holds the agent WebSocket connection) — no agent-api.md
-- change, no new agent-reported field (a container's restart count is a
-- Docker-daemon fact the agent, running inside the container, cannot read
-- about itself).
--
-- Four additive columns, all nullable/zero-default — every pre-migration row
-- and every pre-amendment agent behaves exactly as before.
--
--   agent_process_started_at — best-known start of the CURRENT agent process
--     instance. Set on enrollment and on a reconnect classified as a genuine
--     restart (see agent_disconnected_at below); left untouched on a
--     reconnect classified as a mere WebSocket blip, so it reads as
--     continuous process uptime across blips, not connection uptime.
--   agent_restart_count — cumulative count of reconnects classified as a
--     genuine agent-process restart (never incremented for a blip).
--   agent_last_restart_at — timestamp of the most recent classified restart.
--   agent_disconnected_at — INTERNAL bookkeeping, not exposed on the admin
--     API. Stamped by the control plane the instant it detects the agent
--     connection is lost (agentws.markOffline); read and cleared back to
--     NULL by the very next reconnect (agentws.reconnectHost), which uses
--     the elapsed gap to classify blip vs. restart (see that function's
--     comment for the threshold and the empirical basis for it). Consuming
--     it on every reconnect is what keeps a control-plane restart (which
--     never gets to run the graceful-disconnect defer, so agent_disconnected_at
--     stays NULL for every host that was connected) from being misread as
--     N simultaneous agent restarts across the fleet.
BEGIN;

ALTER TABLE hosts
    ADD COLUMN agent_process_started_at TIMESTAMPTZ,
    ADD COLUMN agent_restart_count      INT NOT NULL DEFAULT 0,
    ADD COLUMN agent_last_restart_at    TIMESTAMPTZ,
    ADD COLUMN agent_disconnected_at    TIMESTAMPTZ;

-- Backfill: an already-connected host's current process obviously started at
-- or before its last registration, so last_registered_at is the best
-- available estimate until its next reconnect gives us a real answer.
UPDATE hosts
    SET agent_process_started_at = last_registered_at
    WHERE agent_process_started_at IS NULL;

COMMIT;
