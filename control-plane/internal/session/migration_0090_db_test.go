package session

import (
	"context"
	"testing"

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
	var materializedAt *string
	must(t, pool.QueryRow(ctx, `SELECT host_id::text, state, materialized_at::text FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, uniqueUser, appID).Scan(&owner, &state, &materializedAt))
	if owner != host1 || state != "reserved" || materializedAt != nil {
		t.Fatalf("unique legacy claim = (%s,%s,%v), want known owner reserved with no physical proof", owner, state, materializedAt)
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
}
