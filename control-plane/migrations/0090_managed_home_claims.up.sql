-- RH05-08 (#341): one canonical owner for each managed home. Existing data is
-- evidence even when tombstoned: a pending GC mark does not prove deletion.
BEGIN;

CREATE TABLE managed_home_claims (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    canonical_app_id UUID NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    host_id UUID REFERENCES hosts(id) ON DELETE SET NULL,
    state TEXT NOT NULL CHECK (state IN ('reserved', 'materialized', 'conflict')),
    materialized_at TIMESTAMPTZ,
    legacy_unprotected_dispatch BOOLEAN NOT NULL DEFAULT false,
    pending_home_session_id UUID,
    pending_home_token UUID,
    pending_home_started_at TIMESTAMPTZ,
    conflict_reason TEXT CHECK (conflict_reason IS NULL OR conflict_reason IN
        ('legacy_location_uncertain', 'claim_owner_missing', 'location_mismatch', 'gc_pending')),
    PRIMARY KEY (user_id, canonical_app_id),
    CONSTRAINT managed_home_claims_conflict_reason_ck
        CHECK ((state = 'conflict') = (conflict_reason IS NOT NULL)),
    CONSTRAINT managed_home_claims_owner_ck
        CHECK (state = 'conflict' OR host_id IS NOT NULL),
    CONSTRAINT managed_home_claims_materialized_ck
        CHECK (state <> 'materialized' OR materialized_at IS NOT NULL),
    CONSTRAINT managed_home_claims_pending_home_ck
        CHECK ((pending_home_session_id IS NULL AND pending_home_token IS NULL AND pending_home_started_at IS NULL)
            OR (pending_home_session_id IS NOT NULL AND pending_home_token IS NOT NULL AND pending_home_started_at IS NOT NULL))
);

CREATE INDEX managed_home_claims_pending_home_session_idx
    ON managed_home_claims (pending_home_session_id)
    WHERE pending_home_session_id IS NOT NULL;

CREATE FUNCTION rh05_guard_managed_home_claim_delete_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.pending_home_token IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE='QH001', MESSAGE='managed home operation pending';
    END IF;
    RETURN OLD;
END;
$$;
CREATE TRIGGER rh05_guard_managed_home_claim_delete
    BEFORE DELETE ON managed_home_claims FOR EACH ROW EXECUTE FUNCTION rh05_guard_managed_home_claim_delete_fn();

CREATE FUNCTION rh05_guard_user_delete_home_hold_fn() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE claim RECORD;
BEGIN
    FOR claim IN SELECT pending_home_token FROM managed_home_claims
                 WHERE user_id=OLD.id ORDER BY user_id, canonical_app_id FOR UPDATE LOOP
        IF claim.pending_home_token IS NOT NULL THEN
            RAISE EXCEPTION USING ERRCODE='QH001', MESSAGE='managed home operation pending';
        END IF;
    END LOOP;
    RETURN OLD;
END;
$$;
CREATE TRIGGER rh05_guard_user_delete_home_hold
    BEFORE DELETE ON users FOR EACH ROW EXECUTE FUNCTION rh05_guard_user_delete_home_hold_fn();

CREATE FUNCTION rh05_guard_parent_app_delete_home_hold_fn() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE claim RECORD;
BEGIN
    FOR claim IN SELECT pending_home_token FROM managed_home_claims
                 WHERE canonical_app_id=OLD.id ORDER BY user_id, canonical_app_id FOR UPDATE LOOP
        IF claim.pending_home_token IS NOT NULL THEN
            RAISE EXCEPTION USING ERRCODE='QH001', MESSAGE='managed home operation pending';
        END IF;
    END LOOP;
    RETURN OLD;
END;
$$;
CREATE TRIGGER rh05_guard_parent_app_delete_home_hold
    BEFORE DELETE ON apps FOR EACH ROW EXECUTE FUNCTION rh05_guard_parent_app_delete_home_hold_fn();

-- Host deletion must preserve a repair-required claim before the FK clears
-- its owner. A pre-existing location disagreement outranks a missing owner;
-- historical materialized_at remains untouched.
CREATE FUNCTION managed_home_claim_host_deleted() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE managed_home_claims
       SET host_id = NULL,
           state = 'conflict',
           conflict_reason = CASE
               WHEN conflict_reason IN ('legacy_location_uncertain', 'location_mismatch')
                   THEN conflict_reason
               ELSE 'claim_owner_missing'
           END
     WHERE host_id = OLD.id;
    RETURN OLD;
END;
$$;
CREATE TRIGGER managed_home_claim_host_deleted_before
    BEFORE DELETE ON hosts FOR EACH ROW EXECUTE FUNCTION managed_home_claim_host_deleted();

-- Historical dispatch identity, deliberately without a home FK: confirmed GC
-- may delete the row. Both fields are NULL for pre-RH05 sessions.
ALTER TABLE sessions
    ADD COLUMN managed_home_id UUID,
    ADD COLUMN managed_home_mount_sha256 TEXT
        CHECK (managed_home_mount_sha256 ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT sessions_managed_home_binding_ck
        CHECK ((managed_home_id IS NULL) = (managed_home_mount_sha256 IS NULL));

-- A null host or more than one distinct host is ambiguous. Keep every backing
-- row; the claim only records why launch must wait for operator repair.
WITH locations AS (
    SELECT uh.user_id, COALESCE(a.parent_app_id, uh.app_id) AS canonical_app_id,
           COUNT(DISTINCT uh.host_id) AS hosts,
           BOOL_OR(uh.host_id IS NULL) AS unknown,
           BOOL_OR(uh.gc_after IS NOT NULL) AS tombstoned,
           MIN(uh.host_id::text)::uuid AS sole_host
    FROM user_homes uh
    JOIN apps a ON a.id = uh.app_id
    WHERE uh.user_id IS NOT NULL
    GROUP BY uh.user_id, COALESCE(a.parent_app_id, uh.app_id)
)
INSERT INTO managed_home_claims
    (user_id, canonical_app_id, host_id, state, materialized_at, conflict_reason, legacy_unprotected_dispatch)
SELECT user_id, canonical_app_id,
       CASE WHEN hosts = 1 AND NOT unknown THEN sole_host ELSE NULL END,
       CASE WHEN hosts = 1 AND NOT unknown AND NOT tombstoned THEN 'reserved' ELSE 'conflict' END,
       NULL,
       CASE WHEN hosts <> 1 OR unknown THEN 'legacy_location_uncertain'
            WHEN tombstoned THEN 'gc_pending' ELSE NULL END,
       true
FROM locations;

COMMIT;
