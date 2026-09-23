-- RH05 #337: independently owned admission restrictions.
CREATE TABLE host_admission_restrictions (
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    owner_kind TEXT NOT NULL CHECK (owner_kind IN ('manual','platform','idle_apply','recovery','legacy')),
    owner_id UUID NOT NULL,
    reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (owner_kind <> 'manual' OR owner_id = '00000000-0000-0000-0000-000000000000'::uuid),
    CHECK (owner_kind <> 'legacy' OR owner_id = '00000000-0000-0000-0000-000000000001'::uuid),
    PRIMARY KEY (host_id, owner_kind, owner_id)
);

-- Preserve active fleet runs whose own cordons were recorded. A terminal run
-- still awaiting restoration retains its protection across the migration.
INSERT INTO host_admission_restrictions (host_id, owner_kind, owner_id, reason)
SELECT DISTINCT (item->>'host_id')::uuid, 'platform', r.id, 'Platform apply'
FROM platform_apply_runs r
CROSS JOIN LATERAL jsonb_array_elements(r.cordoned_hosts) item
JOIN hosts h ON h.id = (item->>'host_id')::uuid
WHERE item->>'was_cordoned' = 'false'
  AND (r.state IN ('pending','running') OR r.cordons_restored_at IS NULL)
ON CONFLICT DO NOTHING;

-- A draining row could also contain a manual drain added after an active
-- platform operation started. The old single status cannot prove otherwise.
-- Preserve every such row as legacy; the operator may explicitly uncordon it
-- after upgrade. This can require one extra resume, but never reopens work.
INSERT INTO host_admission_restrictions (host_id, owner_kind, owner_id, reason)
SELECT h.id, 'legacy', '00000000-0000-0000-0000-000000000001'::uuid,
       'Existing drain; review before resuming'
FROM hosts h
WHERE h.status = 'draining'
ON CONFLICT DO NOTHING;

-- An in-flight platform row can exist while the host is online because of an
-- older agent registration. Re-project it closed before accepting new work.
UPDATE hosts h SET status = 'draining'
WHERE h.status = 'online'
  AND EXISTS (SELECT 1 FROM host_admission_restrictions r WHERE r.host_id = h.id);
