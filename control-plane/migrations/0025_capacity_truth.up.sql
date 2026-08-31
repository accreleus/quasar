-- Fail-closed GPU capacity reporting. Preserve historical GPU rows referenced
-- by sessions while preventing stale or failed discovery from scheduling work.
BEGIN;

ALTER TABLE hosts
    ADD COLUMN capacity_detection TEXT NOT NULL DEFAULT 'unavailable'
        CHECK (capacity_detection IN ('ok', 'unavailable', 'failed')),
    ADD COLUMN capacity_reason TEXT;

ALTER TABLE gpus
    ADD COLUMN reported BOOLEAN NOT NULL DEFAULT true;

UPDATE hosts h SET capacity_detection='ok'
WHERE EXISTS (SELECT 1 FROM gpus g WHERE g.host_id=h.id);

CREATE INDEX gpus_schedulable_idx ON gpus (host_id, reported) WHERE reported;

COMMIT;
