-- RH05-08 (#341): one canonical owner for each managed home. Existing data is
-- evidence even when tombstoned: a pending GC mark does not prove deletion.
BEGIN;

CREATE TABLE managed_home_claims (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    canonical_app_id UUID NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    host_id UUID REFERENCES hosts(id) ON DELETE SET NULL,
    state TEXT NOT NULL CHECK (state IN ('reserved', 'materialized', 'conflict')),
    materialized_at TIMESTAMPTZ,
    conflict_reason TEXT,
    PRIMARY KEY (user_id, canonical_app_id),
    CONSTRAINT managed_home_claims_conflict_reason_ck
        CHECK (state <> 'conflict' OR conflict_reason IS NOT NULL)
);

-- A null host or more than one distinct host is ambiguous. Keep every backing
-- row; the claim only records why launch must wait for operator repair.
WITH locations AS (
    SELECT uh.user_id, COALESCE(a.parent_app_id, uh.app_id) AS canonical_app_id,
           COUNT(DISTINCT uh.host_id) AS hosts,
           BOOL_OR(uh.host_id IS NULL) AS unknown,
           MIN(uh.host_id::text)::uuid AS sole_host
    FROM user_homes uh
    JOIN apps a ON a.id = uh.app_id
    WHERE uh.user_id IS NOT NULL
    GROUP BY uh.user_id, COALESCE(a.parent_app_id, uh.app_id)
)
INSERT INTO managed_home_claims
    (user_id, canonical_app_id, host_id, state, materialized_at, conflict_reason)
SELECT user_id, canonical_app_id,
       CASE WHEN hosts = 1 AND NOT unknown THEN sole_host ELSE NULL END,
       CASE WHEN hosts = 1 AND NOT unknown THEN 'reserved' ELSE 'conflict' END,
       NULL,
       CASE WHEN hosts = 1 AND NOT unknown THEN NULL ELSE 'legacy_location_uncertain' END
FROM locations;

COMMIT;
