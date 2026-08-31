package session

// steam-library-discovery Phase 3 — derived launch tiles.
//
// This file carries the SESSION-LAYER half of spec §2's homeAppID rule, one test
// per site, named so a reviewer can match them to §2's table:
//
//	site 1  scheduler.go guard 2b      → TestHomeRuleSite1LaunchGuardKeysOnTheParentHome
//	site 2  store.HasLiveUserAppSession → TestHomeRuleSite2SwapGuardKeysOnTheParentHome
//	site 4  placement.policyOrderSQL    → TestHomeRuleSite4PlacementKeysOnTheParentHome
//	site 5  home.resolveHomeSpec        → TestHomeRuleSite5DerivedTileRequiresAnExistingHome
//
// Sites 3, 6 and 7 are SQL in internal/storage; see derived_home_db_test.go
// there. Together the two files are the seven.
//
// Plus the Phase 3 acceptance criteria that are not one of the seven: the launch
// composition (§1.2/§3), admission resolving from the parent (§1.2), the two 409s
// (§2.1, §5) and the byte-identical guarantee (§3).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/storage"
)

// --- fixtures ----------------------------------------------------------------

// seedSteamApp inserts a managed-home provider app on the standard seed's fleet:
// the "Steam" client a tile borrows everything executable from.
//
// vram/slots/stream defaults are DISTINCTIVE (2 encode slots, 1920x1080) so a
// test can tell "resolved from the parent" from "read off the tile" by value
// rather than by absence.
func seedSteamApp(t *testing.T, pool *pgxpool.Pool, spec string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height,
		 default_fps, default_bitrate_kbps, runtime_spec, managed_home, home_container_path,
		 kind, library_provider)
		VALUES ('Steam', 4096, 2, 1920, 1080, 60, 12000, $1, true, '/home/quasar',
		        'launcher', 'steam')
		RETURNING id::text`, spec).Scan(&id))
	entitleAll(t, pool, id)
	return id
}

// seedTile inserts a derived tile of parentID. Every column
// apps_derived_shape_ck constrains is left at its tile-shaped value — in
// particular the resource columns are ZERO, which is the cb97bfb shape and is
// what TestDerivedTileAdmissionUsesTheParentsResources relies on.
func seedTile(t *testing.T, pool *pgxpool.Pool, parentID, name, appid string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO apps
		(name, parent_app_id, external_source, external_id, origin, kind,
		 default_vram_mb, default_encode_slots)
		VALUES ($1, $2::uuid, 'steam', $3, 'discovered', 'game', 0, 0)
		RETURNING id::text`, name, parentID, appid).Scan(&id))
	entitleAll(t, pool, id)
	return id
}

// provisionHome creates the (user, parent, host) home a derived tile borrows —
// i.e. simulates "the user launched Steam once on this host".
func provisionHome(t *testing.T, pool *pgxpool.Pool, userID, appID, hostID string) {
	t.Helper()
	mgr := storage.NewLocal(pool, testHomeRoot)
	if _, err := mgr.EnsureHome(context.Background(), userID, appID, hostID, "/home/quasar"); err != nil {
		t.Fatalf("provision home: %v", err)
	}
}

func envOf(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	var spec struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse dispatched spec: %v", err)
	}
	return spec.Env
}

func homeRowCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	must(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM user_homes`).Scan(&n))
	return n
}

// --- §2 site 1: the launch single-writer guard --------------------------------

// TestHomeRuleSite1LaunchGuardKeysOnTheParentHome covers scheduler.go guard 2b.
//
// TWO SUBSTITUTIONS ARE UNDER TEST HERE, and a naive fix gets only one:
//
//  1. THE GATE. Guard 2b is gated on ManagedHome, and a derived tile's OWN
//     managed_home is false by apps_derived_shape_ck. Read off the tile, the
//     guard does not fire AT ALL for exactly the apps it exists to protect — the
//     exact inverse of its purpose. GetLaunchApp must resolve it from the parent.
//  2. THE KEY. It must compare the home-owning app FAMILY, not the app id. The
//     test asserts parent→tile AND tile→sibling-tile, because keying on
//     `s.app_id = homeAppID` alone catches the first and misses the second — the
//     same corruption reached one click differently.
//
// The 409 body must also NAME the live session (§2.1): with the Launcher tile
// staying visible in the library, a user clicks a game and is told a different
// app is in the way, and without the id the client can only render a generic
// failure that reads as a bug.
func TestHomeRuleSite1LaunchGuardKeysOnTheParentHome(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8) // plenty of encode slots: capacity must not be the reason
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	celeste := seedTile(t, pool, parent, "Celeste", "504230")
	provisionHome(t, pool, s.userID, parent, s.hostID)

	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	// The Launcher tile itself goes live.
	res, err := coord.Launch(ctx, s.userID, parent, StreamOverride{})
	if err != nil {
		t.Fatalf("launch the parent: %v", err)
	}
	disp.waitFor(t, "assign")

	// (a) parent live → a derived tile is refused, and the body names the session.
	_, err = coord.Launch(ctx, s.userID, hades, StreamOverride{})
	if !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("launching a tile with the parent live: err = %v, want ErrHomeInUse.\n"+
			"This is §2 site 1. Check BOTH substitutions: the gate must read the PARENT's "+
			"managed_home (a tile's own is false by CHECK) and the key must be the home app family.", err)
	}
	if got := conflictingSessionID(err); got != res.Session.ID {
		t.Errorf("409 body session_id = %q, want the live session %q (§2.1: the client links to it)",
			got, res.Session.ID)
	}

	// Stop the parent's session and put a TILE live instead.
	must(t, exec(t, pool, `UPDATE sessions SET state = 'stopped' WHERE id::text = $1`, res.Session.ID))
	tileRes, err := coord.Launch(ctx, s.userID, hades, StreamOverride{})
	if err != nil {
		t.Fatalf("launch the tile once the parent's session ended: %v", err)
	}
	disp.waitFor(t, "assign")

	// (b) tile live → a SIBLING tile is refused. This is the half a key of
	// `s.app_id = homeAppID` would miss.
	if _, err := coord.Launch(ctx, s.userID, celeste, StreamOverride{}); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("launching a SIBLING tile with another tile live: err = %v, want ErrHomeInUse.\n"+
			"Two tiles of one Steam install run two Steam clients on one steamapps tree.", err)
	}

	// (c) tile live → the PARENT (the Launcher tile) is refused. §2.2: it is one
	// lock and the Launcher tile is simply its most visible holder.
	err = func() error { _, e := coord.Launch(ctx, s.userID, parent, StreamOverride{}); return e }()
	if !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("launching the parent with a tile live: err = %v, want ErrHomeInUse", err)
	}
	if got := conflictingSessionID(err); got != tileRes.Session.ID {
		t.Errorf("409 body session_id = %q, want the live tile session %q", got, tileRes.Session.ID)
	}
}

// TestHomeRuleSite1OrdinaryAppsAreUnaffected pins the pre-existing behaviour: two
// UNRELATED managed-home apps still launch concurrently. A guard rewritten to
// compare families must not start treating every managed-home app as one family.
func TestHomeRuleSite1OrdinaryAppsAreUnaffected(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	appA := seedManagedApp(t, pool, `{"image":"a:1"}`)
	var appB string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height,
		 default_fps, default_bitrate_kbps, runtime_spec, managed_home, home_container_path)
		VALUES ('managed-b', 512, 1, 1280, 720, 30, 2000, '{"image":"b:1"}', true, '/home/quasar')
		RETURNING id::text`).Scan(&appB))
	entitleAll(t, pool, appB)

	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	if _, err := coord.Launch(ctx, s.userID, appA, StreamOverride{}); err != nil {
		t.Fatalf("launch A: %v", err)
	}
	disp.waitFor(t, "assign")
	if _, err := coord.Launch(ctx, s.userID, appB, StreamOverride{}); err != nil {
		t.Fatalf("launch B with A live: %v — two unrelated managed-home apps are not one family", err)
	}
}

// --- §2 site 2: the swap-path single-writer guard -----------------------------

// TestHomeRuleSite2SwapGuardKeysOnTheParentHome covers
// Store.HasLiveUserAppSession, reached through swapper.Swap.
//
// WITHOUT IT THE LAUNCH GUARD IS DEFEATABLE IN TWO REQUESTS: launch an app you
// are allowed to run, then swap into a derived tile of a family whose home is
// already live. The launch guard never sees the second app, and this guard —
// keyed on the target's own app id — would have compared two different ids and
// passed. Same corruption, different door, which is why §2 lists it separately.
func TestHomeRuleSite2SwapGuardKeysOnTheParentHome(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	provisionHome(t, pool, s.userID, parent, s.hostID)

	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	// Session 1: an ordinary app, running. This is the session we will swap.
	//
	// It reserves TWO encode slots deliberately — the parent Steam app's
	// default_encode_slots, which the tile resolves to. With the default one-slot
	// fixture the swap is refused by ErrSwapExceedsReservation first and this test
	// would go green against a completely unguarded home, proving nothing.
	swapParams := launchParams(s)
	swapParams.NeedEncodeSlots = 2
	swapMe, err := store.ScheduleAndCreate(ctx, swapParams)
	if err != nil {
		t.Fatalf("schedule the session to be swapped: %v", err)
	}
	if swapMe, err = store.Transition(ctx, swapMe.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("→ running: %v", err)
	}

	// Session 2: the parent Steam app, live — it holds the home lock.
	steamSess, err := coord.Launch(ctx, s.userID, parent, StreamOverride{})
	if err != nil {
		t.Fatalf("launch the parent: %v", err)
	}
	disp.waitFor(t, "assign")

	// Swapping session 1 INTO a tile of that family must be refused.
	_, err = coord.Swap(ctx, swapMe.ID, hades)
	if !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("swap into a tile whose parent home is live: err = %v, want ErrHomeInUse.\n"+
			"This is §2 site 2: keyed on the tile's own id the guard compares two different "+
			"app ids and lets a second Steam client onto one steamapps tree.", err)
	}
	if got := conflictingSessionID(err); got != steamSess.Session.ID {
		t.Errorf("swap 409 session_id = %q, want %q", got, steamSess.Session.ID)
	}
}

// --- §2 site 4: placement -----------------------------------------------------

// TestHomeRuleSite4PlacementKeysOnTheParentHome covers both halves of §5.
//
// PolicySpread is the DEFAULT and locality is only a sort preference, so without
// a hard pin a tile lands wherever there is room. For an ordinary app that is a
// degraded outcome; for a tile it is a guaranteed failure — the tile provisions
// nothing, so a host with no home for (user, parent) has nothing to mount.
func TestHomeRuleSite4PlacementKeysOnTheParentHome(t *testing.T) {
	t.Run("hard pin beats spread", func(t *testing.T) {
		pool := testDB(t)
		store := NewStore(pool) // DEFAULT policy = spread, deliberately
		// Host 1 has exactly the parent Steam app's two encode slots: enough to fit,
		// and nothing spare. That is the point — spread ranks on FREE slots, so a
		// host with room to spare must lose to the pin, not merely tie with it.
		s := seed(t, pool, 2)
		ctx := context.Background()

		// A second, much roomier host. Under spread, "most free encode slots" wins,
		// so this is the host the scheduler WANTS to pick.
		var host2 string
		must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
			VALUES ('host-2','online','ok') RETURNING id::text`).Scan(&host2))
		must(t, exec(t, pool, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
			VALUES ($1::uuid, 0, 16384, 32)`, host2))

		parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
		hades := seedTile(t, pool, parent, "Hades", "1145360")
		// The install lives on host 1 — the CRAMPED one.
		provisionHome(t, pool, s.userID, parent, s.hostID)

		disp := newCapturingDispatcher()
		coord := newTestCoordinator(t, store, disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))

		res, err := coord.Launch(ctx, s.userID, hades, StreamOverride{})
		if err != nil {
			t.Fatalf("launch the tile: %v", err)
		}
		if res.Session.HostID == nil || *res.Session.HostID != s.hostID {
			t.Fatalf("tile placed on host %v, want the HOME host %s.\n"+
				"§5: a derived tile is placed with a HARD PinHostID, not an affinity — spread "+
				"would otherwise send it to the roomier host, where the install does not exist.",
				res.Session.HostID, s.hostID)
		}
	})

	t.Run("locality ordering keys on the parent", func(t *testing.T) {
		// Unit-level, because the hard pin above makes the two indistinguishable at
		// the integration level. The ordering is fixed anyway: a preference that is
		// silently wrong is worse than one that is absent, and an operator running
		// QUASAR_PLACEMENT_POLICY=locality would otherwise watch tiles ignore an
		// affinity the setting promises.
		cp := CreateParams{UserID: "u-1", AppID: "tile-1", HomeAppID: "parent-1"}
		_, args := PolicyLocality.policyOrderSQL(cp, 2, 3)
		if len(args) != 2 || args[0] != "u-1" || args[1] != "parent-1" {
			t.Errorf("locality subquery args = %v, want [u-1 parent-1].\n"+
				"Keyed on the tile the subquery returns NULL, the CASE collapses to ELSE 1 for "+
				"every candidate, and locality degrades silently to spread.", args)
		}

		// And an ordinary app is unchanged: HomeAppID empty falls back to AppID.
		_, args = PolicyLocality.policyOrderSQL(CreateParams{UserID: "u-1", AppID: "app-1"}, 2, 3)
		if len(args) != 2 || args[1] != "app-1" {
			t.Errorf("ordinary-app locality args = %v, want [u-1 app-1]", args)
		}
	})
}

// --- §2 site 5: resolveHomeSpec → RequireHome ---------------------------------

// TestHomeRuleSite5ResolveHomeSpecRefusesWithoutAHome IS THE SITE-5 TEST.
//
// It calls resolveHomeSpec DIRECTLY, and that is the whole point. The two
// launch-level tests below assert the same outcome but do NOT prove site 5:
// LaunchByProfile's §5 pre-placement pin (launcher.go) refuses first, because
// HomeHostForApp's SQL already requires gc_after IS NULL, so swapping
// RequireHome→EnsureHome in home.go leaves them green. They measure site 4's
// guard standing in front of site 5's.
//
// That shadowing is not academic: LaunchConsoleSession has NO pin resolution and
// calls resolveHomeSpec directly (see the console test below), so RequireHome is
// the only thing between a console-configured derived tile and a silently
// provisioned empty Steam home — the exact failure §3 says the method exists for.
//
// MUTATION CHECK: replace RequireHome with EnsureHome at the derived branch in
// home.go and this test must go RED on the row count.
func TestHomeRuleSite5ResolveHomeSpecRefusesWithoutAHome(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	ctx := context.Background()

	coord := newTestCoordinator(t, store, newCapturingDispatcher(), testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))

	app, err := store.GetLaunchApp(ctx, hades)
	if err != nil {
		t.Fatalf("get the tile: %v", err)
	}

	t.Run("no home row at all", func(t *testing.T) {
		before := homeRowCount(t, pool)
		_, err := coord.resolveHomeSpec(ctx, app, s.userID, s.hostID)
		if !errors.Is(err, ErrHomeNotProvisioned) {
			t.Fatalf("resolveHomeSpec for a tile with no home: err = %v, want ErrHomeNotProvisioned", err)
		}
		if after := homeRowCount(t, pool); after != before {
			t.Fatalf("user_homes rows %d → %d: resolveHomeSpec PROVISIONED a home for a derived tile.\n"+
				"This is §2 site 5 and the mutation this test exists to catch — EnsureHome creates on "+
				"a miss, so the tile would mount an empty directory and the session would reach "+
				"`running` looking perfectly healthy with the game absent.", before, after)
		}
	})

	t.Run("a tombstoned home is treated as absent and left tombstoned", func(t *testing.T) {
		provisionHome(t, pool, s.userID, parent, s.hostID)
		must(t, exec(t, pool, `UPDATE user_homes SET gc_after = now()
			WHERE user_id::text = $1 AND app_id::text = $2`, s.userID, parent))
		before := homeRowCount(t, pool)

		_, err := coord.resolveHomeSpec(ctx, app, s.userID, s.hostID)
		if !errors.Is(err, ErrHomeNotProvisioned) {
			t.Fatalf("resolveHomeSpec onto a TOMBSTONED home: err = %v, want ErrHomeNotProvisioned", err)
		}
		if after := homeRowCount(t, pool); after != before {
			t.Errorf("user_homes rows %d → %d on the tombstoned path", before, after)
		}
		// EnsureHome's upsert sets gc_after = NULL, so this is the resurrection half
		// of the same mutation: it would un-tombstone a home an admin just marked
		// for reaping, and the reaper's queue would quietly lose an entry.
		var stillTombstoned bool
		must(t, pool.QueryRow(ctx, `SELECT gc_after IS NOT NULL FROM user_homes
			WHERE user_id::text = $1 AND app_id::text = $2`, s.userID, parent).Scan(&stillTombstoned))
		if !stillTombstoned {
			t.Error("resolveHomeSpec CLEARED gc_after — EnsureHome's resurrection behaviour must be unreachable from a tile")
		}
	})

	t.Run("a live home resolves to the parent's mount", func(t *testing.T) {
		must(t, exec(t, pool, `UPDATE user_homes SET gc_after = NULL
			WHERE user_id::text = $1 AND app_id::text = $2`, s.userID, parent))
		spec, err := coord.resolveHomeSpec(ctx, app, s.userID, s.hostID)
		if err != nil {
			t.Fatalf("resolveHomeSpec with a live home: %v", err)
		}
		want := testHomeRoot + "/u/steam:/home/quasar:rw"
		mounts := mountsOf(t, spec)
		if len(mounts) != 1 || mounts[0] != want {
			t.Errorf("mounts = %v, want [%s] — the PARENT's home", mounts, want)
		}
	})
}

// TestHomeRuleSite5ConsoleLaunchIsGuardedByRequireHome drives the one production
// caller with no §5 pin in front of it, so site 5 is load-bearing end to end
// rather than only at the seam.
//
// MUTATION CHECK: RequireHome→EnsureHome makes this go red too — the launch
// SUCCEEDS and a user_homes row appears.
func TestHomeRuleSite5ConsoleLaunchIsGuardedByRequireHome(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	// NO home: nobody has launched Steam on this host.
	ctx := context.Background()

	coord := newTestCoordinator(t, store, newCapturingDispatcher(), testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))

	before := homeRowCount(t, pool)
	_, err := coord.LaunchConsoleSession(ctx, s.hostID, s.userID, hades, "local_only", 1920, 1080, 60)
	if !errors.Is(err, ErrHomeNotProvisioned) {
		t.Fatalf("console-launching a tile with no home: err = %v, want ErrHomeNotProvisioned", err)
	}
	if after := homeRowCount(t, pool); after != before {
		t.Fatalf("user_homes rows %d → %d: the console path PROVISIONED an empty Steam home.\n"+
			"LaunchConsoleSession has no §5 pin, so RequireHome is the only guard here.", before, after)
	}
	// The session it scheduled must be failed, not left assigned holding a slot.
	var state string
	must(t, pool.QueryRow(ctx, `SELECT state FROM sessions
		WHERE user_id::text = $1 ORDER BY created_at DESC LIMIT 1`, s.userID).Scan(&state))
	if state != "failed" {
		t.Errorf("session state = %s, want failed (the reservation must be released)", state)
	}
}

// TestDerivedTileKeepsItsFlagsWhenTheParentIsNotManagedHome is review finding 2:
// the one defect in this phase that produced a WRONG STREAM rather than an error.
//
// The flag injection lives inside resolveHomeSpec, so a caller gating on
// ManagedHome alone skipped it entirely for a tile whose parent is not
// managed-home — Steam launched the CLIENT instead of the game, with nothing on
// any surface to say so. §16.1 already records that a failed direct launch is
// undetectable, so this would have arrived as user complaints.
//
// Driven through LaunchConsoleSession because that is where it is reachable:
// LaunchByProfile's §5 pin refuses a non-managed-home tile first.
func TestDerivedTileKeepsItsFlagsWhenTheParentIsNotManagedHome(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	ctx := context.Background()

	// A provider app that is NOT managed-home.
	var parent string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height,
		 default_fps, default_bitrate_kbps, runtime_spec, managed_home, kind, library_provider)
		VALUES ('Steam (unmanaged)', 4096, 1, 1920, 1080, 60, 12000,
		        '{"image":"steam:1","env":{"DISPLAY":":0"}}', false, 'launcher', 'steam')
		RETURNING id::text`).Scan(&parent))
	entitleAll(t, pool, parent)
	hades := seedTile(t, pool, parent, "Hades", "1145360")

	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))

	if _, err := coord.LaunchConsoleSession(ctx, s.hostID, s.userID, hades, "local_only", 1920, 1080, 60); err != nil {
		t.Fatalf("console-launch a tile of an unmanaged parent: %v", err)
	}
	disp.waitFor(t, "assign")

	disp.mu.Lock()
	got := disp.assigns[0]
	disp.mu.Unlock()

	env := envOf(t, got)
	if env["STEAM_STARTUP_FLAGS"] != "-bigpicture -applaunch 1145360" {
		t.Fatalf("STEAM_STARTUP_FLAGS = %q, want %q.\n"+
			"The dispatch site gated on ManagedHome alone, so resolveHomeSpec — where the flag "+
			"injection lives — was never entered. Steam launches the CLIENT, not the game, and "+
			"nothing reports it (§16.1).",
			env["STEAM_STARTUP_FLAGS"], "-bigpicture -applaunch 1145360")
	}
	if env["DISPLAY"] != ":0" {
		t.Errorf("DISPLAY = %q, want :0 — the parent's other env must survive", env["DISPLAY"])
	}
	// No home was invented for an unmanaged parent.
	if n := homeRowCount(t, pool); n != 0 {
		t.Errorf("user_homes rows = %d, want 0 — the parent is not managed-home", n)
	}
}

// TestDisabledParentBlocksItsTiles is the review ruling on judgement call 2, and
// a deliberate deviation from §1.2's table (which lists `enabled` under "lives on
// the tile" and reads the other way).
//
// A tile borrows the parent's image, runtime, mounts and home, so launching a
// tile IS running the parent. Before this, an operator taking Steam out of
// service — mid-upgrade, or over a bad image tag — had every game tile keep
// launching that exact image against that exact home, with no UI surface warning
// them, because the tiles are separate rows and still look enabled.
func TestDisabledParentBlocksItsTiles(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	provisionHome(t, pool, s.userID, parent, s.hostID)
	ctx := context.Background()

	coord := newTestCoordinator(t, store, newCapturingDispatcher(), testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))

	// Baseline: it launches while the parent is enabled, so the refusal below is
	// the disable doing it and not something else.
	if _, err := coord.Launch(ctx, s.userID, hades, StreamOverride{}); err != nil {
		t.Fatalf("baseline launch: %v", err)
	}
	must(t, exec(t, pool, `UPDATE sessions SET state = 'stopped' WHERE user_id::text = $1`, s.userID))

	// The operator takes Steam out of service.
	must(t, exec(t, pool, `UPDATE apps SET enabled = false WHERE id::text = $1`, parent))

	_, err := coord.Launch(ctx, s.userID, hades, StreamOverride{})
	if !errors.Is(err, ErrParentDisabled) {
		t.Fatalf("launching a tile whose parent is DISABLED: err = %v, want ErrParentDisabled.\n"+
			"Disabling an app means \"stop this from running\"; a tile that keeps launching the "+
			"disabled parent's image against its home is the inverse of a kill switch.", err)
	}
	// It NAMES the parent: "Steam" is what an operator sees in the admin list, and
	// a uuid is not actionable.
	if got := disabledParentName(err); got != "Steam" {
		t.Errorf("parent name in the error = %q, want %q", got, "Steam")
	}
	// Nothing was scheduled — the refusal is at app resolution, before placement.
	var live int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions
		WHERE user_id::text = $1 AND state NOT IN ('stopped','failed')`, s.userID).Scan(&live))
	if live != 0 {
		t.Errorf("live sessions = %d, want 0", live)
	}

	// Re-enabling restores it, so this is a switch and not a one-way door.
	must(t, exec(t, pool, `UPDATE apps SET enabled = true WHERE id::text = $1`, parent))
	if _, err := coord.Launch(ctx, s.userID, hades, StreamOverride{}); err != nil {
		t.Fatalf("launch after re-enabling the parent: %v", err)
	}

	// An ORDINARY app is unaffected: its own `enabled` still governs, and a
	// disabled one is still the pre-existing ErrNotFound (404), not the new 409.
	must(t, exec(t, pool, `UPDATE apps SET enabled = false WHERE id::text = $1`, s.appID))
	if _, err := store.GetLaunchApp(ctx, s.appID); !errors.Is(err, ErrNotFound) {
		t.Errorf("disabled ORDINARY app: err = %v, want the pre-existing ErrNotFound", err)
	}
}

// TestLaunchByProfileRefusesATileWithNoHome asserts the LAUNCH-LEVEL outcome of
// §5: the pre-placement pin refuses, nothing is reserved, and no home appears.
//
// IT DOES NOT PROVE SITE 5, and it is named accordingly after the review caught
// the overclaim. LaunchByProfile's §5 pin (launcher.go) runs first and
// HomeHostForApp's SQL already requires gc_after IS NULL, so this stays GREEN
// with RequireHome swapped for EnsureHome — it measures site 4's guard standing
// in front of site 5's. Site 5 itself is
// TestHomeRuleSite5ResolveHomeSpecRefusesWithoutAHome, which calls the seam
// directly, plus the console test beside it.
//
// It is kept because the launch-level contract is worth pinning on its own: the
// refusal must happen BEFORE placement, so no reservation is taken for a launch
// that cannot work.
func TestLaunchByProfileRefusesATileWithNoHome(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	// NO provisionHome: this user has never launched Steam.

	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	before := homeRowCount(t, pool)

	_, err := coord.Launch(ctx, s.userID, hades, StreamOverride{})
	if !errors.Is(err, ErrHomeNotProvisioned) {
		t.Fatalf("launching a tile with no home: err = %v, want ErrHomeNotProvisioned", err)
	}

	if after := homeRowCount(t, pool); after != before {
		t.Fatalf("user_homes rows %d → %d: the failed launch CREATED a home.\n"+
			"This is §2 site 5. EnsureHome must not be reachable from a derived tile — it would "+
			"mount an empty directory and the session would reach `running` looking healthy.",
			before, after)
	}

	// And nothing was reserved: the refusal happens BEFORE placement (§5), so
	// there is no session row at all to release.
	var sessions int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id::text = $1`, s.userID).Scan(&sessions))
	if sessions != 0 {
		t.Errorf("sessions = %d, want 0 — home_not_provisioned is refused before placement", sessions)
	}
}

// TestLaunchByProfileRefusesATileOntoATombstonedHome is the launch-level
// tombstone case. Like the test above it is SHADOWED by the §5 pin — HomeHostForApp
// requires gc_after IS NULL, so the pin refuses before resolveHomeSpec is reached
// — and it therefore does not prove site 5 either. The seam-level tombstone
// assertion lives in TestHomeRuleSite5ResolveHomeSpecRefusesWithoutAHome.
//
// Kept for the same reason: "an admin tombstones a home and launches still stop"
// is a contract worth pinning at the launch level regardless of which guard
// enforces it.
func TestLaunchByProfileRefusesATileOntoATombstonedHome(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	provisionHome(t, pool, s.userID, parent, s.hostID)

	ctx := context.Background()
	must(t, exec(t, pool, `UPDATE user_homes SET gc_after = now()
		WHERE user_id::text = $1 AND app_id::text = $2`, s.userID, parent))

	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))

	if _, err := coord.Launch(ctx, s.userID, hades, StreamOverride{}); !errors.Is(err, ErrHomeNotProvisioned) {
		t.Fatalf("launching a tile onto a TOMBSTONED home: err = %v, want ErrHomeNotProvisioned", err)
	}
	var stillTombstoned bool
	must(t, pool.QueryRow(ctx, `SELECT gc_after IS NOT NULL FROM user_homes
		WHERE user_id::text = $1 AND app_id::text = $2`, s.userID, parent).Scan(&stillTombstoned))
	if !stillTombstoned {
		t.Error("the tile launch cleared gc_after — EnsureHome's resurrection behaviour must be unreachable")
	}
}

// --- launch composition (§1.2, §3) -------------------------------------------

// TestDerivedTileLaunchesWithParentRuntimeAndTileFlags is the headline
// acceptance criterion: a hand-created tile launches the GAME, using the
// parent's image and mounting the user's real library.
func TestDerivedTileLaunchesWithParentRuntimeAndTileFlags(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	// The parent carries a default STEAM_STARTUP_FLAGS (plain Big Picture) plus an
	// unrelated env key and an unrelated mount — all three must survive.
	parent := seedSteamApp(t, pool, `{"image":"steam:1",
		"mounts":["/opt/shared:/opt/shared:ro"],
		"env":{"STEAM_STARTUP_FLAGS":"-bigpicture","DISPLAY":":0"}}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	provisionHome(t, pool, s.userID, parent, s.hostID)

	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	sess, err := coord.Launch(ctx, s.userID, hades, StreamOverride{})
	if err != nil {
		t.Fatalf("launch the tile: %v", err)
	}
	disp.waitFor(t, "assign")

	disp.mu.Lock()
	got := disp.assigns[0]
	disp.mu.Unlock()

	// The parent's image.
	var spec map[string]any
	if err := json.Unmarshal(got, &spec); err != nil {
		t.Fatalf("parse dispatched spec: %v", err)
	}
	if spec["image"] != "steam:1" {
		t.Errorf("image = %v, want steam:1 (resolved from the parent)", spec["image"])
	}

	// The PARENT's home, mounted — not a home of the tile's own.
	mounts := mountsOf(t, got)
	wantMount := testHomeRoot + "/u/steam:/home/quasar:rw"
	if len(mounts) != 2 || mounts[0] != "/opt/shared:/opt/shared:ro" || mounts[1] != wantMount {
		t.Errorf("mounts = %v, want [/opt/shared:/opt/shared:ro %s]", mounts, wantMount)
	}

	// The tile's flags OVER the parent's env: parent first, tile second, tile wins,
	// and env-only (DISPLAY survives untouched).
	env := envOf(t, got)
	if env["STEAM_STARTUP_FLAGS"] != "-bigpicture -applaunch 1145360" {
		t.Errorf("STEAM_STARTUP_FLAGS = %q, want %q",
			env["STEAM_STARTUP_FLAGS"], "-bigpicture -applaunch 1145360")
	}
	if env["DISPLAY"] != ":0" {
		t.Errorf("DISPLAY = %q, want :0 — the flag merge touches env's STEAM_STARTUP_FLAGS key only", env["DISPLAY"])
	}

	// The session records the TILE, not the parent: that is what entitlements,
	// favourites and the library are keyed on.
	if sess.Session.AppID != hades {
		t.Errorf("sessions.app_id = %s, want the tile %s", sess.Session.AppID, hades)
	}
	// And exactly one home row exists — the parent's.
	if n := homeRowCount(t, pool); n != 1 {
		t.Errorf("user_homes rows = %d, want 1 (the parent's; a tile never owns one)", n)
	}
}

// TestEditingTheParentChangesTheTilesNextLaunch is the reason the merge happens
// at launch instead of being flattened into the tile on save (the UI-P3
// runtime-preset lesson): an image bump on the Steam app must reach every tile
// with no tile edit and no re-sync.
func TestEditingTheParentChangesTheTilesNextLaunch(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	provisionHome(t, pool, s.userID, parent, s.hostID)
	ctx := context.Background()

	// The admin bumps the image on the PARENT only.
	must(t, exec(t, pool, `UPDATE apps SET runtime_spec = '{"image":"steam:2"}' WHERE id::text = $1`, parent))

	app, err := store.GetLaunchApp(ctx, hades)
	if err != nil {
		t.Fatalf("get the tile: %v", err)
	}
	if !strings.Contains(string(app.RuntimeSpec), `"steam:2"`) {
		t.Errorf("tile runtime_spec = %s, want the parent's bumped image steam:2 "+
			"(with NO tile edit — this is why the merge is at launch, not at save)", app.RuntimeSpec)
	}
	// The tile's own stored runtime_spec is still '{}' — it never gained a copy.
	var stored string
	must(t, pool.QueryRow(ctx, `SELECT runtime_spec::text FROM apps WHERE id::text = $1`, hades).Scan(&stored))
	if stored != "{}" {
		t.Errorf("tile's STORED runtime_spec = %s, want {} — apps_derived_shape_ck exists so no "+
			"flattened copy can ever be written", stored)
	}
}

// TestNonDerivedRuntimeSpecIsByteIdentical re-runs the §3 guarantee under Phase 3:
// an app with no preset AND no parent still dispatches its stored JSONB bytes
// with no decode/re-encode round trip. Phase 3 added a self-join and two new
// merge branches to the resolution path; this is the assertion that says none of
// them touch an ordinary app.
func TestNonDerivedRuntimeSpecIsByteIdentical(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Key ordering and an unknown field are what a re-marshal would disturb.
	const raw = `{"zeta": 1, "image": "plain:1", "alpha": {"nested": [1,2,3]}, "extra_unknown": true}`
	must(t, exec(t, pool, `UPDATE apps SET runtime_spec = $1 WHERE id::text = $2`, raw, s.appID))

	// The stored bytes, straight from Postgres…
	var stored string
	must(t, pool.QueryRow(ctx, `SELECT runtime_spec::text FROM apps WHERE id::text = $1`, s.appID).Scan(&stored))

	// …must be exactly what GetLaunchApp hands the dispatch path.
	app, err := store.GetLaunchApp(ctx, s.appID)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if string(app.RuntimeSpec) != stored {
		t.Errorf("runtime_spec was re-encoded for a non-derived app:\n got: %s\nwant: %s",
			app.RuntimeSpec, stored)
	}
	if app.ParentAppID != "" {
		t.Errorf("ParentAppID = %q, want empty for an ordinary app", app.ParentAppID)
	}

	// And the dispatch path passes those same bytes through untouched.
	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	if _, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	disp.waitFor(t, "assign")
	disp.mu.Lock()
	dispatched := string(disp.assigns[0])
	disp.mu.Unlock()
	if dispatched != stored {
		t.Errorf("dispatched spec differs from the stored bytes:\n got: %s\nwant: %s", dispatched, stored)
	}
}

// --- admission (§1.2) ---------------------------------------------------------

// TestDerivedTileAdmissionUsesTheParentsResources is the cb97bfb assertion, and
// it is deliberately the STRONG form: the tile's own default_encode_slots is 0,
// which under the incident's failure mode makes an app admissible on a GPU with
// no free encode slot at all.
//
// A discovered tile is created by a background job (Phase 4), which is precisely
// the shape that incident took — omitted create fields zeroing the column. So
// the test does not check "the number came from the parent", it checks the
// consequence: a FULL host must still refuse the tile.
func TestDerivedTileAdmissionUsesTheParentsResources(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 2) // exactly the parent's default_encode_slots
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	provisionHome(t, pool, s.userID, parent, s.hostID)
	ctx := context.Background()

	// Fill the GPU: one other user's session holding both encode slots.
	var other string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('filler@test.local','filler','x') RETURNING id::text`).Scan(&other))
	must(t, exec(t, pool, `INSERT INTO sessions
		(user_id, app_id, host_id, gpu_id, state, width, height, fps, bitrate_kbps,
		 h264_profile, reserved_vram_mb, reserved_encode_slots)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'running', 1280, 720, 30, 2000,
		        'constrained-baseline', 0, 2)`, other, s.appID, s.hostID, s.gpuID))

	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))

	_, err := coord.Launch(ctx, s.userID, hades, StreamOverride{})
	if err == nil {
		t.Fatal("a derived tile with default_encode_slots = 0 was ADMITTED onto a full GPU.\n" +
			"§1.2: the tile's own resource columns are never read; admission resolves from the parent. " +
			"cb97bfb is the live incident of exactly this.")
	}
	if !errors.Is(err, ErrCapacityExhausted) && !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("launch on a full GPU: err = %v, want a capacity rejection", err)
	}

	// And the positive control: the SAME tile is admitted once the GPU is free, so
	// the rejection above is admission working, not the tile being unlaunchable.
	must(t, exec(t, pool, `UPDATE sessions SET state = 'stopped' WHERE user_id::text = $1`, other))
	if _, err := coord.Launch(ctx, s.userID, hades, StreamOverride{}); err != nil {
		t.Fatalf("launch the tile on a free GPU: %v", err)
	}
}

// TestDerivedTileResolvesEveryBorrowedField is the field-by-field companion to
// the admission test: §1.2's right-hand column, asserted directly, so a reviewer
// can see which values come from where.
func TestDerivedTileResolvesEveryBorrowedField(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	seed(t, pool, 4)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	ctx := context.Background()

	app, err := store.GetLaunchApp(ctx, hades)
	if err != nil {
		t.Fatalf("get the tile: %v", err)
	}

	// Identity stays the tile's.
	if app.ID != hades {
		t.Errorf("ID = %s, want the tile %s", app.ID, hades)
	}
	if app.ParentAppID != parent {
		t.Errorf("ParentAppID = %s, want %s", app.ParentAppID, parent)
	}
	if homeAppID(app) != parent {
		t.Errorf("homeAppID = %s, want the parent %s", homeAppID(app), parent)
	}
	if app.ExternalID != "1145360" {
		t.Errorf("ExternalID = %q, want 1145360", app.ExternalID)
	}

	// Everything executable is the parent's (seedSteamApp's distinctive values).
	for _, c := range []struct {
		field string
		got   int32
		want  int32
	}{
		{"DefaultVramMB", app.DefaultVramMB, 4096},
		{"DefaultEncodeSlots", app.DefaultEncodeSlots, 2},
		{"DefaultWidth", app.DefaultWidth, 1920},
		{"DefaultHeight", app.DefaultHeight, 1080},
		{"DefaultFPS", app.DefaultFPS, 60},
		{"DefaultBitrateKbps", app.DefaultBitrateKbps, 12000},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d (resolved from the parent, not read off the tile)", c.field, c.got, c.want)
		}
	}
	if !app.ManagedHome {
		t.Error("ManagedHome = false — a tile's own column is false by CHECK; it MUST resolve from " +
			"the parent or scheduler guard 2b never fires for derived tiles at all")
	}
	if app.HomeContainerPath != "/home/quasar" {
		t.Errorf("HomeContainerPath = %q, want /home/quasar (the parent's)", app.HomeContainerPath)
	}
	if !strings.Contains(string(app.RuntimeSpec), "steam:1") {
		t.Errorf("RuntimeSpec = %s, want the parent's", app.RuntimeSpec)
	}
}

// exec is a terse pool.Exec that discards the command tag, for fixtures.
func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) error {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, args...)
	return err
}
