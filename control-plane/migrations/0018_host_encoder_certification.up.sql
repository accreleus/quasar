-- 0018_host_encoder_certification.up.sql — Stream Perf Tuning Phase C (SPT-06).
-- Records the measured sustainable encode envelope per (host, GPU, encoder,
-- profile, bench-bitrate) so the scheduler can avoid default-starting a profile
-- a host cannot hold in real time (e.g. Renoir 1080p60 → unsafe).
-- Scheduling input, not telemetry: upsert-latest (one current verdict per
-- configuration), NOT append-only, NOT on the session-metrics retention prune.
-- Never access control, never a session-state authority (schema.md).
-- Prose companion: docs/stream-perf/contract-amendment.md §A.
BEGIN;

CREATE TABLE host_encoder_certification (
    id                UUID             PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id           UUID             NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    gpu_index         INT              NOT NULL,
    encoder           TEXT             NOT NULL CHECK (encoder IN ('va','nvenc','openh264')),
    profile_id        TEXT             NOT NULL,
    width             INT              NOT NULL,
    height            INT              NOT NULL,
    fps               INT              NOT NULL,
    bitrate_kbps      INT              NOT NULL,
    verdict           TEXT             NOT NULL CHECK (verdict IN ('ok','capped','unsafe')),
    encode_ms_p50     DOUBLE PRECISION NOT NULL,
    encode_ms_p95     DOUBLE PRECISION NOT NULL,
    encode_ms_max     DOUBLE PRECISION NOT NULL,
    output_fps        DOUBLE PRECISION NOT NULL,
    drop_rate         DOUBLE PRECISION NOT NULL,
    live_write_stable BOOLEAN          NOT NULL,
    sample_window_ms  INT              NOT NULL,
    sample_count      INT              NOT NULL,
    agent_version     TEXT,
    measured_at       TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ      NOT NULL DEFAULT now(),
    UNIQUE (host_id, gpu_index, encoder, profile_id, bitrate_kbps)
);

CREATE INDEX host_encoder_certification_lookup_idx
    ON host_encoder_certification (host_id, gpu_index, encoder, profile_id);

COMMIT;
