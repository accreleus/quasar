-- 0114 — Ultra-low latency (PyroWave), amendments 55 and 58 (#1050, #1059).
-- Additive: defaulted or nullable columns and three CHECKs; no data change. Semantics: schema.md.
BEGIN;

ALTER TABLE launch_profiles
    ADD COLUMN pyrowave_mode TEXT NOT NULL DEFAULT 'off'
        CHECK (pyrowave_mode IN ('off','allow','prefer')),
    ADD COLUMN pyrowave_kbps INT NULL
        CHECK (pyrowave_kbps IS NULL OR pyrowave_kbps BETWEEN 50000 AND 1000000);
ALTER TABLE sessions
    ADD COLUMN pyrowave JSONB NULL,
    ADD COLUMN pyrowave_slot BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE instance_settings
    ADD COLUMN pyrowave_max_per_gpu INT NOT NULL DEFAULT 1
        CHECK (pyrowave_max_per_gpu BETWEEN 1 AND 4);

COMMIT;
