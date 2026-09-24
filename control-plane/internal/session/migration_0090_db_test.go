package session

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration0090PreservesLegacyLocations(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 86)
	pool, err := pgxpool.New(context.Background(), url)
	must(t, err)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	var host1, host2, appID, uniqueUser, conflictUser string
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name) VALUES ('legacy-home-1') RETURNING id::text`).Scan(&host1))
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name) VALUES ('legacy-home-2') RETURNING id::text`).Scan(&host2))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name, managed_home) VALUES ('legacy-managed', true) RETURNING id::text`).Scan(&appID))
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash) VALUES ('unique@test.local','unique-home','x') RETURNING id::text`).Scan(&uniqueUser))
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash) VALUES ('conflict@test.local','conflict-home','x') RETURNING id::text`).Scan(&conflictUser))
	_, err = pool.Exec(ctx, `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref, gc_after)
		VALUES ($1::uuid, $3::uuid, $4::uuid, 'local', '/homes/unique', now()),
		       ($2::uuid, $3::uuid, $4::uuid, 'local', '/homes/conflict-a', NULL),
		       ($2::uuid, $3::uuid, $5::uuid, 'local', '/homes/conflict-b', NULL)
	`, uniqueUser, conflictUser, appID, host1, host2)
	must(t, err)

	migrateTo(t, m, 90)
	var owner, state string
	var uniqueReason *string
	var materializedAt *string
	must(t, pool.QueryRow(ctx, `SELECT host_id::text, state, materialized_at::text, conflict_reason FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, uniqueUser, appID).Scan(&owner, &state, &materializedAt, &uniqueReason))
	if owner != host1 || state != "conflict" || uniqueReason == nil || *uniqueReason != "gc_pending" || materializedAt != nil {
		t.Fatalf("unique tombstoned legacy claim = (%s,%s,%v,%v), want known owner conflict/gc_pending with no physical proof", owner, state, uniqueReason, materializedAt)
	}
	var legacyUnprotected bool
	must(t, pool.QueryRow(ctx, `SELECT legacy_unprotected_dispatch FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, uniqueUser, appID).Scan(&legacyUnprotected))
	if !legacyUnprotected {
		t.Fatal("legacy backfill must disclose absence of RH05 hold coverage")
	}
	var conflictOwner *string
	var reason string
	must(t, pool.QueryRow(ctx, `SELECT host_id::text, state, conflict_reason FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, conflictUser, appID).Scan(&conflictOwner, &state, &reason))
	if conflictOwner != nil || state != "conflict" || reason != "legacy_location_uncertain" {
		t.Fatalf("divergent legacy claim = (%v,%s,%s), want repair-required conflict", conflictOwner, state, reason)
	}
	var homes int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes WHERE app_id=$1::uuid`, appID).Scan(&homes))
	if homes != 3 {
		t.Fatalf("migration preserved %d home rows, want all three", homes)
	}
	var unbound int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE managed_home_id IS NULL AND managed_home_mount_sha256 IS NULL`).Scan(&unbound))
	if unbound != 0 {
		t.Fatalf("pre-RH05 bindings: %d unexpected sessions", unbound)
	}
}

func TestMigration0090HeldClaimCannotCascadeAway(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	appID := seedManagedApp(t, pool, `{}`)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO managed_home_claims
		(user_id,canonical_app_id,host_id,state,pending_home_session_id,pending_home_token,pending_home_started_at)
		VALUES ($1::uuid,$2::uuid,$3::uuid,'reserved',$4::uuid,$5::uuid,now())`,
		s.userID, appID, s.hostID, "00000000-0000-4000-8000-000000000041", "00000000-0000-4000-8000-000000000042")
	must(t, err)
	t.Cleanup(func() {
		_, cleanupErr := pool.Exec(context.Background(), `UPDATE managed_home_claims
			SET pending_home_session_id=NULL,pending_home_token=NULL,pending_home_started_at=NULL
			WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, appID)
		if cleanupErr != nil {
			t.Errorf("clear test hold: %v", cleanupErr)
		}
	})
	for _, tc := range []struct{ name, sql, id string }{
		{"claim", `DELETE FROM managed_home_claims WHERE user_id=$1::uuid`, s.userID},
		{"user", `DELETE FROM users WHERE id=$1::uuid`, s.userID},
		{"app", `DELETE FROM apps WHERE id=$1::uuid`, appID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql, tc.id)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "QH001" {
				t.Fatalf("held %s delete error = %v, want QH001", tc.name, err)
			}
			var n int
			must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, appID).Scan(&n))
			if n != 1 {
				t.Fatalf("held claim count after refused %s delete = %d", tc.name, n)
			}
		})
	}
}

func TestMigration0090HostDeleteRetainsConflict(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	appID := seedManagedApp(t, pool, `{}`)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO managed_home_claims (user_id,canonical_app_id,host_id,state,materialized_at) VALUES ($1::uuid,$2::uuid,$3::uuid,'materialized',now())`, s.userID, appID, s.hostID)
	must(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM hosts WHERE id=$1::uuid`, s.hostID)
	must(t, err)
	var owner *string
	var state string
	var reason *string
	var used bool
	must(t, pool.QueryRow(ctx, `SELECT host_id::text,state,conflict_reason,materialized_at IS NOT NULL FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, appID).Scan(&owner, &state, &reason, &used))
	if owner != nil || state != "conflict" || reason == nil || *reason != "claim_owner_missing" || !used {
		t.Fatalf("deleted owner claim = (%v,%s,%v,used=%t), want conflict with historical use", owner, state, reason, used)
	}
}
