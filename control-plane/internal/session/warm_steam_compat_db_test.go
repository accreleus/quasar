package session

// warm_steam_compat_db_test.go — claim check 6 (amendment 56, #1174): a parked Steam is adopted
// for a game mapped to a GE tool only when that tool was mounted when it started.
// Semantics: control-api.md §Compatibility tools, "Hot Loading".

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/compat"
	"github.com/jackc/pgx/v5/pgxpool"
)

const geName = "GE-Proton9-99"

// seedGETool inserts an enabled GE tool bound to geName and maps game 1145360 to it.
func seedGETool(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	must(t, exec(t, pool, `DELETE FROM compat_tool_mappings`))
	must(t, exec(t, pool, `INSERT INTO compat_tools (family, source, tag, steam_name, display_name, enabled,
		first_enabled_at, asset_name, asset_size_bytes, asset_sha512)
		VALUES ('ge', 'tag', $1, $1, $1, true, now(), $1 || '.tar.gz', 1, repeat('a', 128))`, geName))
	must(t, exec(t, pool, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id, forced)
		SELECT 'steam', '1145360', id, false FROM compat_tools WHERE tag = $1`, geName))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM compat_tool_mappings`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM compat_tools WHERE tag = $1`, geName)
	})
}

func parkedMountedWith(t *testing.T, f *warmFixture, names ...string) string {
	t.Helper()
	k := f.park(t)
	must(t, exec(t, f.pool, `UPDATE sessions SET compat_tool_names = $2::text[] WHERE id = $1::uuid`, k, names))
	return k
}

func TestClaimFailsWhenTheParkedSteamLacksTheMappedGETool(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	seedGETool(t, f.pool)
	k := parkedMountedWith(t, f) // started before the host had the tool

	_, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: tile, DeviceID: f.deviceID, Mic: true})
	var refused *ParkedClaimRefusedError
	if !errors.As(err, &refused) || refused.Warming || refused.Cause != causeCompatTool {
		t.Fatalf("claim = %v, want a superseding refusal with cause %q", err, causeCompatTool)
	}
	if got := readRow(t, f.pool, k); got.State != "stopping" || got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("parked row %+v, want stopping / parked_superseded", got)
	}
}

func TestClaimAdoptsAndCopiesTheParkedToolNames(t *testing.T) {
	f := newWarmFixture(t, 4)
	f.disp.compat = true
	ctx := context.Background()
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	seedGETool(t, f.pool)
	k := parkedMountedWith(t, f, geName)

	res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: tile, DeviceID: f.deviceID, Mic: true})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if a := f.disp.assignFor(t, res.Session.ID); a.AdoptSessionID != k {
		t.Fatalf("assign adopts %q, want %s", a.AdoptSessionID, k)
	}
	// The assign carries the host's current mount, which the running Steam never saw.
	var names []string
	must(t, f.pool.QueryRow(ctx, `SELECT compat_tool_names FROM sessions WHERE id = $1::uuid`, res.Session.ID).Scan(&names))
	if strings.Join(names, ",") != geName {
		t.Errorf("adopting session compat_tool_names = %v, want [%s]", names, geName)
	}
}

func TestClaimFailsWhenAToolTheParkedSteamHoldsIsNoLongerUsable(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	seedGETool(t, f.pool)
	k := parkedMountedWith(t, f, geName)
	must(t, exec(t, f.pool, `DELETE FROM compat_tool_mappings`))
	must(t, exec(t, f.pool, `UPDATE compat_tools SET enabled = false WHERE tag = $1`, geName))

	_, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: tile, DeviceID: f.deviceID, Mic: true})
	var refused *ParkedClaimRefusedError
	if !errors.As(err, &refused) || refused.Cause != causeCompatTool {
		t.Fatalf("claim = %v, want a refusal with cause %q", err, causeCompatTool)
	}
	if got := readRow(t, f.pool, k); got.State != "stopping" {
		t.Errorf("parked row %+v, want stopping", got)
	}
}

// A disable holds the tool row FOR UPDATE; a claim queues behind it on the tool row without
// having locked the parked session (control-api.md §Usable, "Lock order"), then sees it unusable.
func TestClaimTakesTheToolShareLockBeforeAnySessionLock(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	seedGETool(t, f.pool)
	k := parkedMountedWith(t, f, geName)

	disable, err := f.pool.Begin(ctx)
	must(t, err)
	defer disable.Rollback(ctx)
	_, err = disable.Exec(ctx, `UPDATE compat_tools SET enabled = false WHERE tag = $1`, geName)
	must(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: tile, DeviceID: f.deviceID, Mic: true})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("claim finished (%v) while the tool row was locked FOR UPDATE", err)
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := f.pool.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1::uuid FOR UPDATE NOWAIT`, k); err != nil {
		t.Fatalf("parked session is locked while the claim waits on the tool row: %v", err)
	}
	must(t, disable.Commit(ctx))
	var refused *ParkedClaimRefusedError
	if err := <-done; !errors.As(err, &refused) || refused.Cause != causeCompatTool {
		t.Fatalf("claim after the disable = %v, want cause %q", err, causeCompatTool)
	}
}

func TestClaimIgnoresAMappingToAValveBuild(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	seedGETool(t, f.pool)
	must(t, compat.NewStore(f.pool).Reconcile(ctx))
	must(t, exec(t, f.pool, `UPDATE compat_tools SET enabled = true WHERE steam_name = 'proton_hotfix'`))
	must(t, exec(t, f.pool, `UPDATE compat_tool_mappings SET tool_id = (SELECT id FROM compat_tools WHERE steam_name = 'proton_hotfix')`))
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `UPDATE compat_tools SET enabled = true WHERE family = 'valve'`)
	})
	f.park(t)
	if _, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: tile, DeviceID: f.deviceID, Mic: true}); err != nil {
		t.Fatalf("claim with a Valve mapping: %v", err)
	}
}
