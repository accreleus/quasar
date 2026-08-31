BEGIN;

-- AS10-06: server-side stream-health state machine. The control plane computes a
-- per-session health classification from telemetry the agent already reports
-- (abr_setpoint_kbps in session_metrics) against the session's profile ABR floor
-- (AS10-01). Health is observational — the only action it takes is failing a
-- session that stays unsustainable for too long (state_detail = "unsustainable: ...").
--
-- All seven states are defined now for forward-compat with AS10-07 (#207, which
-- populates the client_* states from browser telemetry); this ticket drives only
-- the server/network states. Additive-only: three new columns, no change to
-- existing columns/types/constraints. Amendment type: additive per schema.md.

ALTER TABLE sessions
    ADD COLUMN health_state TEXT NOT NULL DEFAULT 'healthy'
        CHECK (health_state IN (
            'healthy',
            'network_degrading',
            'abr_at_floor',
            'client_decode_degrading',
            'client_presentation_degrading',
            'unsustainable',
            'failed'
        )),
    ADD COLUMN health_state_reason TEXT,
    ADD COLUMN health_state_changed_at TIMESTAMPTZ NOT NULL DEFAULT now();

COMMIT;
