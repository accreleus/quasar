BEGIN;

-- AS-02: add playout0_ms to sessions so the probe-consumer can stamp the
-- tier-selected initial jitter-buffer playout delay. The client reads this
-- from the session response to initialise its jitter buffer before any
-- in-session adaptive controller (AS-05) takes over.
--
-- Additive-only migration: new column with a NOT NULL default, no change to
-- existing columns, types, or constraints.
-- Amendment type: additive per the schema.md amendment rules (AS-00 design
-- approved, PR #123).

ALTER TABLE sessions
    ADD COLUMN playout0_ms INT NOT NULL DEFAULT 100;

COMMIT;
