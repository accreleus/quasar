package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/compat"
	"github.com/accreleus/quasar/control-plane/internal/storage"
)

// Placement for a game whose per-game mapping names a GE tool (control-api.md §Compatibility
// tools, "Placement"). Integration tests: need Postgres (make test-db).

const (
	compatGameID  = "4242"
	compatTag     = "GE-Proton99-1"
	compatSteamNm = "GE-Proton99-1-x86_64"
)

var compatSHA = strings.Repeat("ab", 64)

// seedGETool inserts an enabled GE tool mapped to compatGameID, and removes both on cleanup
// (testDB does not truncate the compat tables).
func seedGETool(t *testing.T, pool *pgxpool.Pool) (toolID string) {
	t.Helper()
	ctx := context.Background()
	must(t, pool.QueryRow(ctx, `
		INSERT INTO compat_tools (family, source, tag, steam_name, display_name, enabled, first_enabled_at,
		                          asset_name, asset_size_bytes, asset_sha512)
		VALUES ('ge', 'tag', $1, $2, $1, true, now(), $1 || '-x86_64.tar.gz', 1000, $3)
		RETURNING id::text`, compatTag, compatSteamNm, compatSHA).Scan(&toolID))
	_, err := pool.Exec(ctx, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id) VALUES ('steam', $1, $2::uuid)`,
		compatGameID, toolID)
	must(t, err)
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM compat_tool_mappings WHERE tool_id = $1::uuid`, toolID)
		pool.Exec(ctx, `UPDATE instance_settings SET compat_tool_default_id = NULL WHERE compat_tool_default_id = $1::uuid`, toolID)
		pool.Exec(ctx, `DELETE FROM compat_tools WHERE id = $1::uuid`, toolID)
	})
	return toolID
}

// hostHasTool is the host's reported install of the seeded tool (what capacity.compat_tools writes).
func hostHasTool(t *testing.T, pool *pgxpool.Pool, hostID string, has bool) {
	t.Helper()
	q := `DELETE FROM host_compat_tools WHERE host_id = $1::uuid`
	args := []any{hostID}
	if has {
		q = `INSERT INTO host_compat_tools (host_id, name, tag, sha512, bytes) VALUES ($1::uuid, $2, $3, $4, 1000)
			ON CONFLICT DO NOTHING`
		args = append(args, compatSteamNm, compatTag, compatSHA)
	}
	_, err := pool.Exec(context.Background(), q, args...)
	must(t, err)
}

// dropSession frees the slot a finished launch holds.
func dropSession(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `DELETE FROM sessions WHERE id = $1::uuid`, id)
	must(t, err)
}

func compatLaunch(s seedIDs, gameID string) CreateParams {
	p := launchParams(s)
	p.CompatGameID = gameID
	return p
}

// host-1 has far more free slots, so the spread policy picks it unless the gate excludes it.
// Deleting compatGate from the queries fails the first launch.
func TestPlacementWaitsForAHostThatHasTheMappedTool(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	setQuota(t, pool, s.userID, 20)
	host2, _ := addHost(t, pool, "host-2", 2)
	tool := seedGETool(t, pool)
	ctx := context.Background()

	// Installing on host-1, installed on host-2: the launch goes to host-2.
	hostHasTool(t, pool, host2, true)
	sess, err := store.ScheduleAndCreate(ctx, compatLaunch(s, compatGameID))
	if err != nil {
		t.Fatalf("launch with the tool on host-2: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != host2 {
		t.Fatalf("placed on %v, want the host that has the tool (%s)", sess.HostID, host2)
	}

	// The same predicate feeds session_assign.mount: a launch is never placed for a tool it
	// will not be given.
	_, mount, err := compat.NewStore(pool).Entries(ctx, host2)
	must(t, err)
	if !slices.Contains(mount, compatSteamNm) {
		t.Fatalf("host-2 mount = %v, placement and mount disagree", mount)
	}
	dropSession(t, pool, sess.ID)

	// Nobody has it yet: no_host_available naming the tool, nothing persisted.
	hostHasTool(t, pool, host2, false)
	before := countSessions(t, pool)
	_, err = store.ScheduleAndCreate(ctx, compatLaunch(s, compatGameID))
	var rej *CompatToolRejection
	if !errors.Is(err, ErrNoHostAvailable) || !errors.As(err, &rej) {
		t.Fatalf("launch with the tool nowhere: got %v, want ErrNoHostAvailable carrying CompatToolRejection", err)
	}
	if rej.Tool != compatTag || !rej.Usable {
		t.Fatalf("rejection = %+v, want tool %s, usable (still installing)", rej, compatTag)
	}
	if after := countSessions(t, pool); after != before {
		t.Fatalf("refused launch persisted a row: %d → %d", before, after)
	}

	// Eligible once the install lands, with no admin step.
	hostHasTool(t, pool, s.hostID, true)
	sess, err = store.ScheduleAndCreate(ctx, compatLaunch(s, compatGameID))
	if err != nil {
		t.Fatalf("launch after the install: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != s.hostID {
		t.Fatalf("placed on %v, want host-1 %s", sess.HostID, s.hostID)
	}

	// A tool that is no longer usable (marked) is unusable everywhere even where installed,
	// and the message says so.
	dropSession(t, pool, sess.ID)
	_, err = pool.Exec(ctx, `UPDATE compat_tools SET name_conflict = true WHERE id = $1::uuid`, tool)
	must(t, err)
	_, err = store.ScheduleAndCreate(ctx, compatLaunch(s, compatGameID))
	if !errors.As(err, &rej) || rej.Usable {
		t.Fatalf("marked tool: got %v (%+v), want CompatToolRejection with Usable=false", err, rej)
	}
}

// Only a per-game GE mapping constrains: an unmapped game, a Valve build and the fleet default do not.
func TestOnlyAPerGameGEMappingConstrainsPlacement(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	setQuota(t, pool, s.userID, 20)
	tool := seedGETool(t, pool) // installed nowhere
	ctx := context.Background()

	if _, err := store.ScheduleAndCreate(ctx, compatLaunch(s, "")); err != nil {
		t.Fatalf("a launch naming no game: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, compatLaunch(s, "777")); err != nil {
		t.Fatalf("an unmapped game: %v", err)
	}

	// The fleet default is never a placement constraint.
	_, err := pool.Exec(ctx, `UPDATE instance_settings SET compat_tool_default_id = $1::uuid`, tool)
	must(t, err)
	if _, err := store.ScheduleAndCreate(ctx, compatLaunch(s, "778")); err != nil {
		t.Fatalf("a game on the fleet default GE tool: %v", err)
	}

	// A Valve build is usable on every host.
	var valve string
	must(t, pool.QueryRow(ctx, `
		INSERT INTO compat_tools (family, source, steam_name, display_name, enabled, first_enabled_at)
		VALUES ('valve', 'steam', 'proton_experimental', 'Proton Experimental', true, now())
		ON CONFLICT (steam_name) DO UPDATE SET enabled = true RETURNING id::text`).Scan(&valve))
	_, err = pool.Exec(ctx, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id) VALUES ('steam', '779', $1::uuid)`, valve)
	must(t, err)
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM compat_tool_mappings WHERE external_id = '779'`) })
	if _, err := store.ScheduleAndCreate(ctx, compatLaunch(s, "779")); err != nil {
		t.Fatalf("a game on a Valve build: %v", err)
	}
}

// A host that has the tool but is full is capacity_exhausted, not compat_tool_missing; a host
// without it does not rescue the launch.
func TestAFullHostWithTheToolIsCapacityExhausted(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 1)
	setQuota(t, pool, s.userID, 20)
	addHost(t, pool, "host-2", 4) // free, no tool
	seedGETool(t, pool)
	hostHasTool(t, pool, s.hostID, true)
	ctx := context.Background()

	if _, err := store.ScheduleAndCreate(ctx, compatLaunch(s, compatGameID)); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	_, err := store.ScheduleAndCreate(ctx, compatLaunch(s, compatGameID))
	if !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("second launch: got %v, want ErrCapacityExhausted", err)
	}
}

func TestCompatToolMissingIsAReasonedNoHostAvailable(t *testing.T) {
	for _, tc := range []struct {
		usable bool
		want   string
	}{
		{true, "no host can run the compatibility tool"},
		{false, "disabled or marked"},
	} {
		rec := httptest.NewRecorder()
		writeNoHostAvailable(rec, &CompatToolRejection{err: &NoHostRejection{err: ErrNoHostAvailable}, Tool: compatTag, Usable: tc.usable})
		body := rec.Body.String()
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(body, `"reason":"compat_tool_missing"`) ||
			!strings.Contains(body, compatTag) || !strings.Contains(body, tc.want) {
			t.Fatalf("usable=%v: %d %s", tc.usable, rec.Code, body)
		}
	}
}

// The real launch path: a Steam tile is pinned to the host of the user's home, and that host has
// not installed the tool, so the launch waits (compat_tool_missing) instead of mounting nothing.
func TestTileLaunchWaitsForTheToolOnItsHomeHost(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	provisionHome(t, pool, s.userID, parent, s.hostID)
	seedGETool(t, pool)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE compat_tool_mappings SET external_id = '1145360' WHERE external_id = $1`, compatGameID)
	must(t, err)

	coord := newTestCoordinator(t, store, newCapturingDispatcher(), testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	_, err = coord.Launch(ctx, s.userID, hades, StreamOverride{})
	var rej *CompatToolRejection
	if !errors.As(err, &rej) || rej.Tool != compatTag {
		t.Fatalf("launch before the install: got %v, want CompatToolRejection for %s", err, compatTag)
	}

	hostHasTool(t, pool, s.hostID, true)
	if _, err := coord.Launch(ctx, s.userID, hades, StreamOverride{}); err != nil {
		t.Fatalf("launch after the install: %v", err)
	}
}

// The home host being offline outranks the missing tool: only an admin brings it back, and the
// user is told that, not to wait for an install.
func TestOfflineHomeHostWithoutTheToolStaysHomeHostUnavailable(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	hades := seedTile(t, pool, parent, "Hades", "1145360")
	provisionHome(t, pool, s.userID, parent, s.hostID)
	seedGETool(t, pool)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE compat_tool_mappings SET external_id = '1145360' WHERE external_id = $1`, compatGameID)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE hosts SET status = 'offline' WHERE id = $1::uuid`, s.hostID)
	must(t, err)

	coord := newTestCoordinator(t, store, newCapturingDispatcher(), testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	_, err = coord.Launch(ctx, s.userID, hades, StreamOverride{})
	var home *HomeHostOfflineRejection
	var ct *CompatToolRejection
	if !errors.As(err, &home) || errors.As(err, &ct) {
		t.Fatalf("got %v, want HomeHostOfflineRejection and no CompatToolRejection", err)
	}
	rec := httptest.NewRecorder()
	writeNoHostAvailable(rec, err)
	if !strings.Contains(rec.Body.String(), `"reason":"home_host_unavailable"`) {
		t.Fatalf("body %s", rec.Body.String())
	}
}
