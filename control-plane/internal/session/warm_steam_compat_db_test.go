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
)

const geName = compatSteamNm

func parkedMountedWith(t *testing.T, f *warmFixture, names ...string) string {
	t.Helper()
	k := f.park(t)
	must(t, exec(t, f.pool, `UPDATE sessions SET compat_tool_names = $2::text[] WHERE id = $1::uuid`, k, append([]string{}, names...)))
	return k
}

func TestClaimFailsWhenTheParkedSteamLacksTheMappedGETool(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	tile := seedTile(t, f.pool, f.steam, "Hades", compatGameID)
	seedGETool(t, f.pool)
	hostHasTool(t, f.pool, f.hostID, true) // so check 6, not placement, is what fails
	k := parkedMountedWith(t, f)           // started before the host had the tool

	_, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: tile, DeviceID: f.deviceID, Mic: true})
	var refused *ParkedClaimRefusedError
	if !errors.As(err, &refused) || refused.Warming || refused.Cause != causeCompatTool {
		t.Fatalf("claim = %v, want a superseding refusal with cause %q", err, causeCompatTool)
	}
	if got := readRow(t, f.pool, k); got.State != "stopping" || got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("parked row %+v, want stopping / parked_superseded", got)
	}
}

// A park whose dispatch has not recorded its names yet is not known to lack the tool: the launch
// waits like a claim on any warming park, and the retry decides once it is running.
func TestClaimOnAWarmingParkWithUnrecordedToolNamesWaits(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	tile := seedTile(t, f.pool, f.steam, "Hades", compatGameID)
	seedGETool(t, f.pool)
	hostHasTool(t, f.pool, f.hostID, true)
	f.coord.SteamWarmHeartbeat(ctx, f.userID, false, f.deviceID)
	k := f.parkedID(t)
	f.disp.waitFor(t, "start")
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'starting', compat_tool_names = NULL WHERE id = $1::uuid`, k))

	_, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: tile, DeviceID: f.deviceID, Mic: true})
	var refused *ParkedClaimRefusedError
	if !errors.As(err, &refused) || !refused.Warming {
		t.Fatalf("claim on a warming park with NULL names = %v, want the warming refusal", err)
	}
	if got := readRow(t, f.pool, k); got.State != "starting" || f.disp.stopReason(k) != "" {
		t.Fatalf("parked row %+v stop=%q, want left starting and not stopped", got, f.disp.stopReason(k))
	}

	f.running(t, k)
	must(t, exec(t, f.pool, `UPDATE sessions SET compat_tool_names = '{}' WHERE id = $1::uuid`, k))
	_, err = f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: tile, DeviceID: f.deviceID, Mic: true})
	if !errors.As(err, &refused) || refused.Warming || refused.Cause != causeCompatTool {
		t.Fatalf("retry once running without the tool = %v, want cause %q", err, causeCompatTool)
	}
}

func TestClaimAdoptsAndCopiesTheParkedToolNames(t *testing.T) {
	f := newWarmFixture(t, 4)
	f.disp.compat = true
	ctx := context.Background()
	tile := seedTile(t, f.pool, f.steam, "Hades", compatGameID)
	seedGETool(t, f.pool)
	hostHasTool(t, f.pool, f.hostID, true)
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
	tile := seedTile(t, f.pool, f.steam, "Hades", compatGameID)
	seedGETool(t, f.pool)
	k := parkedMountedWith(t, f, geName)
	must(t, exec(t, f.pool, `DELETE FROM compat_tool_mappings`))
	must(t, exec(t, f.pool, `UPDATE compat_tools SET enabled = false WHERE steam_name = $1`, geName))

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
	tile := seedTile(t, f.pool, f.steam, "Hades", compatGameID)
	seedGETool(t, f.pool)
	k := parkedMountedWith(t, f, geName)

	disable, err := f.pool.Begin(ctx)
	must(t, err)
	defer disable.Rollback(ctx)
	_, err = disable.Exec(ctx, `UPDATE compat_tools SET enabled = false WHERE steam_name = $1`, geName)
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
	tile := seedTile(t, f.pool, f.steam, "Hades", compatGameID)
	seedGETool(t, f.pool)
	must(t, compat.NewStore(f.pool).Reconcile(ctx))
	must(t, exec(t, f.pool, `UPDATE compat_tools SET enabled = true WHERE steam_name = 'proton_hotfix'`))
	must(t, exec(t, f.pool, `UPDATE compat_tool_mappings SET tool_id = (SELECT id FROM compat_tools WHERE steam_name = 'proton_hotfix')`))
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM compat_tool_mappings WHERE external_id = $1`, compatGameID)
		_, _ = f.pool.Exec(context.Background(), `UPDATE compat_tools SET enabled = true WHERE family = 'valve'`)
	})
	f.park(t)
	if _, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: tile, DeviceID: f.deviceID, Mic: true}); err != nil {
		t.Fatalf("claim with a Valve mapping: %v", err)
	}
}
