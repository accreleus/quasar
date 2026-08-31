package session

// discovered_tile_launch_db_test.go — steam-library-discovery Phase 4, the one
// acceptance line that cannot be proved inside internal/library (spec §13
// "Phase 4"): "The tile is visible ONLY to users with an observation. A second
// user without that game installed gets 403 on a direct launch."
//
// It lives here rather than with the reconciler because the claim is about the
// LAUNCH GATE, not about the rows the reconciler writes. A test in
// internal/library could only assert that user B holds no entitlement; this one
// asserts that a client which ignores the filtered library and POSTs the tile's
// app id directly is refused — which is the property that actually matters.
//
// The tiles here are created by the REAL reconciler (library.Store.Reconcile),
// not hand-inserted, so the test also pins that what discovery writes is exactly
// what the launch gate reads.

import (
	"context"
	"net/http"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/library"
)

// TestDiscoveredTileIsEntitledOnlyToObservers.
func TestDiscoveredTileIsEntitledOnlyToObservers(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	f := newEntLaunchFixture(t, pool)

	// The Steam provider app, and a home for each user on the same host.
	var parent string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, library_provider, managed_home, default_vram_mb, default_encode_slots,
		 default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('Steam', 'steam', true, 1024, 1, 1280, 720, 60, 6000)
		RETURNING id::text`).Scan(&parent))
	for _, u := range []string{f.userID, f.adminID} {
		must(t, execEnt(ctx, pool, `INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'local', '/homes/' || $1)`, u, parent, f.hostID))
	}

	// ONLY the plain user is observed to have Redout installed. The admin's scan
	// sees a different game entirely — and both see Proton, which the denylist
	// suppresses (§8.4: every Steam user has Proton, which is why entitlements
	// alone do not bound a denylist miss).
	store := library.NewStore(pool)
	scanFor := func(user string) string {
		var id string
		must(t, pool.QueryRow(ctx, `INSERT INTO library_scans (user_id, app_id, host_id, state, claimed_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'claimed', now()) RETURNING id::text`,
			user, parent, f.hostID).Scan(&id))
		return id
	}
	if _, err := store.Reconcile(ctx, scanFor(f.userID), f.hostID, []library.ReportEntry{
		{ExternalID: "517710", Name: "Redout: Enhanced Edition"},
		{ExternalID: "1493710", Name: "Proton Experimental"},
	}, nil); err != nil {
		t.Fatalf("reconcile (user): %v", err)
	}
	if _, err := store.Reconcile(ctx, scanFor(f.adminID), f.hostID, []library.ReportEntry{
		{ExternalID: "3179810", Name: "Tiny Dangerous Dungeons Remake"},
		{ExternalID: "1493710", Name: "Proton Experimental"},
	}, nil); err != nil {
		t.Fatalf("reconcile (admin): %v", err)
	}

	var redout string
	must(t, pool.QueryRow(ctx, `SELECT id::text FROM apps
		WHERE parent_app_id = $1::uuid AND external_id = '517710'`, parent).Scan(&redout))

	// The admin has a home on the same host, a valid token, and a fleet with
	// capacity. What they do not have is an observation for this game.
	//
	// NOTE THIS IS THE ADMIN. §6.5: the launch gate has no admin arm, so "admin"
	// here is the strongest possible form of the assertion.
	if status, _ := launchStatus(t, f.base, f.adminTok, redout); status != http.StatusForbidden {
		t.Fatalf("a user with no observation for a discovered tile launched it: got %d, want 403", status)
	}
	var sessions int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE app_id::text = $1`, redout).Scan(&sessions))
	if sessions != 0 {
		t.Errorf("a refused launch of a discovered tile persisted %d session row(s)", sessions)
	}

	// POSITIVE CONTROL, and it is what makes the 403 above mean something: the
	// observed user is NOT refused. The launch may still fail further down (home
	// provisioning, placement) — what it must not be is 403.
	if status, code := launchStatus(t, f.base, f.userTok, redout); status == http.StatusForbidden {
		t.Fatalf("the user who actually has the game installed was refused: %d (%s)", status, code)
	}

	// And the suppressed Valve tool produced no tile at all, so there is nothing
	// for anyone to launch.
	var protonTiles int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM apps
		WHERE parent_app_id = $1::uuid AND external_id = '1493710'`, parent).Scan(&protonTiles))
	if protonTiles != 0 {
		t.Errorf("Proton Experimental produced %d tile(s); the denylist must suppress it for everyone", protonTiles)
	}
}
