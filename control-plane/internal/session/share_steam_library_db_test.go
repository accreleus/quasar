package session

// share_steam_library_db_test.go — #1001: a desktop app with share_steam_library
// mounts the user's Steam data dir. The invariant under test: no two sessions
// ever mount one Steam data dir at once. Semantics: control-api.md §Storage.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/storage"
)

// seedDesktop adds a managed-home desktop app, with sharing on or off.
func seedDesktop(t *testing.T, f *warmFixture, share bool) string {
	t.Helper()
	var id string
	must(t, f.pool.QueryRow(context.Background(), `INSERT INTO apps
		(name, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps,
		 runtime_spec, managed_home, home_container_path, kind, share_steam_library)
		VALUES ('KDE', 1, 1280, 720, 60, 6000, '{"image":"kde:1"}', true, '/home/quasar', 'desktop', $1)
		RETURNING id::text`, share).Scan(&id))
	entitleAll(t, f.pool, id)
	return id
}

func (f *warmFixture) launch(t *testing.T, appID string) (LaunchResult, error) {
	t.Helper()
	return f.coord.LaunchByProfile(context.Background(), f.userID, LaunchParams{AppID: appID, DeviceID: f.deviceID})
}

func (f *warmFixture) mustLaunch(t *testing.T, appID string) LaunchResult {
	t.Helper()
	res, err := f.launch(t, appID)
	if err != nil {
		t.Fatalf("launch %s: %v", appID, err)
	}
	return res
}

// mountsOf is the dispatched spec's mounts for a session.
func (f *warmFixture) mountsOf(t *testing.T, sessionID string) []string {
	t.Helper()
	var spec struct {
		Mounts []string `json:"mounts"`
	}
	if err := json.Unmarshal(f.disp.assignFor(t, sessionID).App, &spec); err != nil {
		t.Fatalf("parse dispatched spec: %v", err)
	}
	return spec.Mounts
}

func (f *warmFixture) homeRef(t *testing.T, appID, hostID string) string {
	t.Helper()
	var ref string
	must(t, f.pool.QueryRow(context.Background(), `SELECT ref FROM user_homes
		WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid`, f.userID, appID, hostID).Scan(&ref))
	return ref
}

func (f *warmFixture) claimHolder(t *testing.T, appID string) (holder, token string) {
	t.Helper()
	var h, tok *string
	must(t, f.pool.QueryRow(context.Background(), `SELECT
		(SELECT pending_home_session_id::text FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid),
		(SELECT pending_home_token::text FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid)`,
		f.userID, appID).Scan(&h, &tok))
	return deref(h), deref(tok)
}

func TestSharingDesktopMountsTheSteamLibraryAndLocksTheSteamFamily(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")

	res := f.mustLaunch(t, desktop)
	if res.Notice != "" || res.Session.SharedHomeAppID == nil || *res.Session.SharedHomeAppID != f.steam {
		t.Fatalf("launch notice=%q shared=%v, want the Steam app shared and no notice", res.Notice, res.Session.SharedHomeAppID)
	}
	want := []string{
		f.homeRef(t, desktop, f.hostID) + ":/home/quasar:rw",
		f.homeRef(t, f.steam, f.hostID) + "/.local/share/Steam:/home/quasar/.local/share/Steam:rw",
	}
	if got := f.mountsOf(t, res.Session.ID); !slices.Equal(got[len(got)-2:], want) {
		t.Fatalf("mounts = %v, want the own home then the Steam data dir last: %v", got, want)
	}
	ownHolder, ownToken := f.claimHolder(t, desktop)
	steamHolder, steamToken := f.claimHolder(t, f.steam)
	if ownHolder != res.Session.ID || steamHolder != res.Session.ID || ownToken == "" || steamToken != ownToken {
		t.Fatalf("holds own=%s/%s steam=%s/%s, want both held by %s under one token",
			ownHolder, ownToken, steamHolder, steamToken, res.Session.ID)
	}

	// The Steam parent and a tile are refused while the desktop runs, naming it.
	for _, app := range []string{f.steam, tile} {
		_, err := f.launch(t, app)
		var inUse *HomeInUseError
		if !errors.As(err, &inUse) || inUse.SessionID != res.Session.ID {
			t.Fatalf("Steam family launch %s = %v, want home_in_use naming the desktop", app, err)
		}
		if msg := homeInUseMessage(err, "live"); msg != "Your Steam library is open in the desktop. Close the desktop first." {
			t.Errorf("message = %q", msg)
		}
	}
	// So is the swap guard.
	if conflict, err := f.store.HasLiveUserAppSession(context.Background(), f.userID, f.steam, ""); err != nil || conflict == nil {
		t.Fatalf("swap guard = %v, %v; want the sharing desktop to hold the Steam home", conflict, err)
	}
	// A second sharing desktop is refused too.
	other := seedDesktop(t, f, true)
	if _, err := f.launch(t, other); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("second sharing desktop = %v, want home_in_use", err)
	}

	// The tombstone guard protects the library while the desktop mounts it.
	var steamHome string
	must(t, f.pool.QueryRow(context.Background(), `SELECT id::text FROM user_homes WHERE app_id=$1::uuid`, f.steam).Scan(&steamHome))
	if _, err := storage.NewLocal(f.pool, testHomeRoot).TombstoneHome(context.Background(), steamHome); !errors.Is(err, storage.ErrHomeInUse) {
		t.Fatalf("tombstone of the shared Steam home = %v, want home in use", err)
	}

	// One proved-unstarted release clears both holds.
	if err := f.store.ClearNewHomeHold(context.Background(), &HomeHoldDecision{Token: ownToken, Created: true,
		UserID: f.userID, AppID: desktop, SessionID: res.Session.ID}); err != nil {
		t.Fatal(err)
	}
	if h, _ := f.claimHolder(t, f.steam); h != "" {
		t.Errorf("Steam hold = %q after ClearNewHomeHold, want cleared with the own hold", h)
	}

	// After the desktop ends, Steam launches again.
	f.ended(t, res.Session.ID)
	f.mustLaunch(t, tile)
}

func TestRunningSteamGameRefusesTheSharingDesktop(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	game := f.runningTile(t)

	_, err := f.launch(t, desktop)
	var inUse *HomeInUseError
	if !errors.As(err, &inUse) || inUse.SessionID != game {
		t.Fatalf("sharing desktop with a Steam game running = %v, want home_in_use naming the game", err)
	}
	if msg := homeInUseMessage(err, "live"); msg != "Close your Steam game first." {
		t.Errorf("message = %q", msg)
	}
	var n int
	must(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE app_id=$1::uuid`, desktop).Scan(&n))
	if n != 0 {
		t.Errorf("refused launch left %d desktop rows", n)
	}

	f.ended(t, game)
	f.mustLaunch(t, desktop)
}

func TestSharingDesktopSupersedesParkedSteamAndWaitsForItToStop(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	k := f.park(t)

	// Refused with the retryable 503 while the parked Steam stops.
	for i := 0; i < 2; i++ {
		_, err := f.launch(t, desktop)
		var refused *ParkedClaimRefusedError
		if !errors.As(err, &refused) || !errors.Is(err, ErrCapacityExhausted) {
			t.Fatalf("attempt %d = %v, want the 503 parked refusal", i, err)
		}
		if got := readRow(t, f.pool, k); got.State != "stopping" || got.StopReason != StopReasonParkedSuperseded {
			t.Fatalf("parked row %+v, want stopping / parked_superseded", got)
		}
	}

	// Stopped but not yet proved gone: the launch waits for the proof.
	must(t, exec(t, f.pool, `UPDATE sessions SET state='stopped', ended_at=now() WHERE id=$1::uuid`, k))
	if h, _ := f.claimHolder(t, f.steam); h != k {
		t.Fatalf("Steam hold = %q, want the parked session %s until its qualified stop", h, k)
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		_, _ = f.pool.Exec(context.Background(), `UPDATE managed_home_claims SET pending_home_session_id=NULL,
			pending_home_token=NULL, pending_home_started_at=NULL WHERE pending_home_session_id=$1::uuid`, k)
	}()
	start := time.Now()
	res := f.mustLaunch(t, desktop)
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Errorf("launch took %v, want it to wait for the parked Steam's proof", waited)
	}
	if res.Session.SharedHomeAppID == nil {
		t.Error("the desktop launched without its Steam library")
	}
}

func TestSharingDesktopOnAnotherHostThanSteamOpensWithoutIt(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	host2, _ := addHost(t, f.pool, "host-2", 8)
	provisionHome(t, f.pool, f.userID, desktop, host2)

	res := f.mustLaunch(t, desktop)
	if res.Notice != noticeSharedOtherHost || res.Session.SharedHomeAppID != nil {
		t.Fatalf("notice=%q shared=%v, want no sharing and the other-host notice", res.Notice, res.Session.SharedHomeAppID)
	}
	if deref(res.Session.HostID) != host2 {
		t.Errorf("desktop placed on %s, want its own home's host %s", deref(res.Session.HostID), host2)
	}
	for _, m := range f.mountsOf(t, res.Session.ID) {
		if strings.Contains(m, ".local/share/Steam") {
			t.Errorf("mount %q dispatched without sharing", m)
		}
	}
	if h, _ := f.claimHolder(t, f.steam); h != "" {
		t.Errorf("Steam hold = %q, want untouched", h)
	}
	// Steam stays launchable: nothing holds its home.
	f.mustLaunch(t, f.steam)
}

func TestSharingDesktopWithNoHomeIsPlacedOnTheSteamHost(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	host2, gpu2 := addHost(t, f.pool, "host-2", 8)
	// Move the Steam home to host-2 and load it, so spread placement alone would
	// pick the seed host.
	must(t, exec(t, f.pool, `DELETE FROM managed_home_claims; DELETE FROM user_homes`))
	provisionHome(t, f.pool, f.userID, f.steam, host2)
	insertParked(t, f.pool, warmUser(t, f.pool, "load"), f.steam, host2, gpu2, 4, "10 minutes")

	res := f.mustLaunch(t, desktop)
	if deref(res.Session.HostID) != host2 || res.Session.SharedHomeAppID == nil {
		t.Fatalf("placed on %s shared=%v, want the Steam home's host %s with sharing", deref(res.Session.HostID), res.Session.SharedHomeAppID, host2)
	}
}

func TestSharingDesktopCreatesTheSteamHomeWhenThereIsNone(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	must(t, exec(t, f.pool, `DELETE FROM managed_home_claims; DELETE FROM user_homes`))

	res := f.mustLaunch(t, desktop)
	if res.Session.SharedHomeAppID == nil || res.Notice != "" {
		t.Fatalf("shared=%v notice=%q, want the Steam library shared", res.Session.SharedHomeAppID, res.Notice)
	}
	mounts := f.mountsOf(t, res.Session.ID)
	steamRef := f.homeRef(t, f.steam, f.hostID)
	if !strings.HasPrefix(mounts[len(mounts)-1], steamRef+"/.local/share/Steam:") {
		t.Errorf("mounts = %v, want the new Steam home %s mounted", mounts, steamRef)
	}
	var claimHost string
	must(t, f.pool.QueryRow(context.Background(), `SELECT host_id::text FROM managed_home_claims
		WHERE canonical_app_id=$1::uuid`, f.steam).Scan(&claimHost))
	if claimHost != f.hostID {
		t.Errorf("Steam claim on %s, want the desktop's host %s", claimHost, f.hostID)
	}
}

func TestDesktopWithSharingOffIsUnchanged(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, false)
	game := f.runningTile(t)

	res := f.mustLaunch(t, desktop)
	if res.Notice != "" || res.Session.SharedHomeAppID != nil {
		t.Fatalf("notice=%q shared=%v, want neither", res.Notice, res.Session.SharedHomeAppID)
	}
	mounts := f.mountsOf(t, res.Session.ID)
	if len(mounts) != 1 || mounts[0] != f.homeRef(t, desktop, f.hostID)+":/home/quasar:rw" {
		t.Errorf("mounts = %v, want only the desktop's own home", mounts)
	}
	if h, _ := f.claimHolder(t, f.steam); h != game {
		t.Errorf("Steam hold = %q, want the game's own %s", h, game)
	}
}

func TestShareSteamLibraryIsRefusedOnATileOrAProvider(t *testing.T) {
	f := newWarmFixture(t, 8)
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	for _, id := range []string{tile, f.steam} {
		if err := exec(t, f.pool, `UPDATE apps SET share_steam_library=true WHERE id=$1::uuid`, id); err == nil ||
			!strings.Contains(err.Error(), "apps_share_steam_library_ck") {
			t.Errorf("share_steam_library on %s: err = %v, want apps_share_steam_library_ck", id, err)
		}
	}
}

// The guard alone, with no hold: rows written directly, as a legacy agent that
// takes no holds would leave them.
func TestSingleWriterGuardCountsASharedHome(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	ctx := context.Background()
	var live string
	must(t, f.pool.QueryRow(ctx, `INSERT INTO sessions (user_id, app_id, host_id, gpu_id, state, width, height,
		fps, bitrate_kbps, h264_profile, reserved_encode_slots, assigned_at, started_at, shared_home_app_id)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'running', 1280, 720, 60, 6000, 'constrained-baseline',
		        1, now(), now(), $5::uuid) RETURNING id::text`,
		f.userID, desktop, f.hostID, f.gpuID, f.steam).Scan(&live))

	p := launchParams(f.seedIDs)
	p.AppID, p.HomeAppID, p.ManagedHome = f.steam, f.steam, true
	_, err := f.store.ScheduleAndCreate(ctx, p)
	var inUse *HomeInUseError
	if !errors.As(err, &inUse) || inUse.SessionID != live || !inUse.holderShares {
		t.Fatalf("Steam launch beside a sharing desktop = %v, want home_in_use naming it", err)
	}
}

func TestSharingLaunchIsRefusedByALiveSteamSessionWithoutAHold(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	ctx := context.Background()
	var game string
	must(t, f.pool.QueryRow(ctx, `INSERT INTO sessions (user_id, app_id, host_id, gpu_id, state, width, height,
		fps, bitrate_kbps, h264_profile, reserved_encode_slots, assigned_at, started_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'running', 1920, 1080, 60, 12000, 'constrained-baseline',
		        2, now(), now()) RETURNING id::text`, f.userID, f.steam, f.hostID, f.gpuID).Scan(&game))

	p := launchParams(f.seedIDs)
	p.AppID, p.HomeAppID, p.ManagedHome, p.SharedHomeAppID = desktop, desktop, true, f.steam
	_, err := f.store.ScheduleAndCreate(ctx, p)
	var inUse *HomeInUseError
	if !errors.As(err, &inUse) || inUse.SessionID != game || !inUse.forShared {
		t.Fatalf("sharing launch beside a Steam game = %v, want home_in_use naming the game", err)
	}
}

// --- round 2: swaps, placement, pre-agent failure, hold-free storage guards ----

func (f *warmFixture) startedLaunch(t *testing.T, appID string) string {
	t.Helper()
	res := f.mustLaunch(t, appID)
	f.disp.waitFor(t, "start")
	f.running(t, res.Session.ID)
	return res.Session.ID
}

func TestSwapFromASharingDesktopIntoSteamIsRefused(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	must(t, exec(t, f.pool, `UPDATE apps SET default_encode_slots=2 WHERE id=$1::uuid`, desktop))
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	sess := f.startedLaunch(t, desktop)

	for _, target := range []string{f.steam, tile} {
		_, err := f.coord.Swap(context.Background(), sess, target)
		var inUse *HomeInUseError
		if !errors.As(err, &inUse) || inUse.SessionID != sess || homeInUseReason(err) != "steam_library_open_in_desktop" {
			t.Fatalf("swap of a sharing desktop into %s = %v, want home_in_use steam_library_open_in_desktop naming itself", target, err)
		}
	}
	if got := readRow(t, f.pool, sess); got.Detail == swapDetailInProgress {
		t.Error("a refused swap left the session marked swapping")
	}
}

func TestSwapIntoASharingDesktopRunsWithoutTheLibrary(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	game := f.runningTile(t)

	if _, err := f.coord.Swap(context.Background(), game, desktop); err != nil {
		t.Fatalf("swap into a sharing desktop: %v", err)
	}
	f.disp.waitFor(t, "swap")
	f.disp.mu.Lock()
	cmd := f.disp.swaps[len(f.disp.swaps)-1]
	f.disp.mu.Unlock()
	var spec struct {
		Mounts []string `json:"mounts"`
	}
	if err := json.Unmarshal(cmd.App, &spec); err != nil {
		t.Fatal(err)
	}
	for _, m := range spec.Mounts {
		if strings.Contains(m, ".local/share/Steam") {
			t.Errorf("swap target mount %q: a swap never shares", m)
		}
	}
	var shared *string
	must(t, f.pool.QueryRow(context.Background(), `SELECT shared_home_app_id::text FROM sessions WHERE id=$1::uuid`, game).Scan(&shared))
	if shared != nil {
		t.Errorf("shared_home_app_id = %s after a swap, want NULL", *shared)
	}
}

func TestSharingDesktopNeverCreatesASteamHomeWhereSteamCannotRun(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	host2, _ := addHost(t, f.pool, "host-2", 8)
	must(t, exec(t, f.pool, `DELETE FROM managed_home_claims; DELETE FROM user_homes`))
	must(t, exec(t, f.pool, `INSERT INTO app_placement (app_id, mode) VALUES ($1::uuid, 'fixed')
		ON CONFLICT (app_id) DO UPDATE SET mode='fixed'`, f.steam))
	must(t, exec(t, f.pool, `INSERT INTO app_placement_hosts (app_id, host_id) VALUES ($1::uuid, $2::uuid)`, f.steam, host2))
	provisionHome(t, f.pool, f.userID, desktop, f.hostID)

	res := f.mustLaunch(t, desktop)
	if res.Notice != noticeSharedUnavailable || res.Session.SharedHomeAppID != nil {
		t.Fatalf("notice=%q shared=%v, want no sharing and the notice: Steam may not run on this host", res.Notice, res.Session.SharedHomeAppID)
	}
	var claims, homes int
	must(t, f.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM managed_home_claims WHERE canonical_app_id=$1::uuid),
		(SELECT count(*) FROM user_homes WHERE app_id=$1::uuid)`, f.steam).Scan(&claims, &homes))
	if claims != 0 || homes != 0 {
		t.Errorf("Steam claims=%d homes=%d, want none created on a host Steam's placement excludes", claims, homes)
	}
}

func TestFailedSharingLaunchReleasesOnlyAnUnusedSteamClaim(t *testing.T) {
	steamClaims := func(t *testing.T, f *warmFixture) int {
		var n int
		must(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM managed_home_claims
			WHERE canonical_app_id=$1::uuid`, f.steam).Scan(&n))
		return n
	}
	for _, tc := range []struct {
		name       string
		steamHome  bool
		wantClaims int
	}{{"created by this launch", false, 0}, {"over an existing Steam home", true, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWarmFixture(t, 8)
			desktop := seedDesktop(t, f, true)
			if !tc.steamHome {
				must(t, exec(t, f.pool, `DELETE FROM managed_home_claims; DELETE FROM user_homes`))
			}
			// The desktop's own home mount fails after placement committed both claims.
			must(t, exec(t, f.pool, `UPDATE apps SET home_container_path='home' WHERE id=$1::uuid`, desktop))
			if _, err := f.launch(t, desktop); err == nil {
				t.Fatal("launch with a relative home path succeeded")
			}
			var state string
			must(t, f.pool.QueryRow(context.Background(), `SELECT state FROM sessions WHERE app_id=$1::uuid`, desktop).Scan(&state))
			if state != string(StateFailed) {
				t.Fatalf("session state %s, want failed before the agent", state)
			}
			if n := steamClaims(t, f); n != tc.wantClaims {
				t.Errorf("Steam claims = %d, want %d", n, tc.wantClaims)
			}
		})
	}
}

// insertSharingSession writes a live sharing desktop session with no hold, as a
// dispatch that was not cleanup-capable leaves it.
func (f *warmFixture) insertSharingSession(t *testing.T, desktop string) string {
	t.Helper()
	var id string
	must(t, f.pool.QueryRow(context.Background(), `INSERT INTO sessions (user_id, app_id, host_id, gpu_id, state,
		width, height, fps, bitrate_kbps, h264_profile, reserved_encode_slots, assigned_at, started_at, shared_home_app_id)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'running', 1280, 720, 60, 6000, 'constrained-baseline',
		        1, now(), now(), $5::uuid) RETURNING id::text`,
		f.userID, desktop, f.hostID, f.gpuID, f.steam).Scan(&id))
	return id
}

func (f *warmFixture) steamHomeID(t *testing.T) string {
	t.Helper()
	var id string
	must(t, f.pool.QueryRow(context.Background(), `SELECT id::text FROM user_homes WHERE app_id=$1::uuid`, f.steam).Scan(&id))
	return id
}

func TestTombstoneRefusesASharedHomeWithoutAHold(t *testing.T) {
	f := newWarmFixture(t, 8)
	f.insertSharingSession(t, seedDesktop(t, f, true))
	_, err := storage.NewLocal(f.pool, testHomeRoot).TombstoneHome(context.Background(), f.steamHomeID(t))
	if !errors.Is(err, storage.ErrHomeInUse) {
		t.Fatalf("tombstone of a shared Steam home = %v, want home in use", err)
	}
}

func TestClaimReleaseRefusesASharedHomeWithoutAHold(t *testing.T) {
	f := newWarmFixture(t, 8)
	must(t, exec(t, f.pool, `INSERT INTO managed_home_claims (user_id, canonical_app_id, host_id, state, conflict_reason)
		VALUES ($1::uuid, $2::uuid, NULL, 'conflict', 'claim_owner_missing')`, f.userID, f.steam))
	f.insertSharingSession(t, seedDesktop(t, f, true))
	_, err := storage.NewLocal(f.pool, testHomeRoot).ReleaseHomeClaim(context.Background(), storage.ReleaseHomeClaimReq{
		UserID: f.userID, AppID: f.steam, ExpectedState: "conflict", ExpectedConflictReason: "claim_owner_missing"})
	if !errors.Is(err, storage.ErrClaimInUse) {
		t.Fatalf("release of a shared Steam claim = %v, want claim in use", err)
	}
}

func TestGCConfirmKeepsTheClaimOfASharedHome(t *testing.T) {
	f := newWarmFixture(t, 8)
	home := f.steamHomeID(t)
	must(t, exec(t, f.pool, `UPDATE user_homes SET gc_after = now() - interval '2 days' WHERE id=$1::uuid`, home))
	must(t, exec(t, f.pool, `INSERT INTO managed_home_claims (user_id, canonical_app_id, host_id, state, conflict_reason)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'conflict', 'gc_pending')`, f.userID, f.steam, f.hostID))
	f.insertSharingSession(t, seedDesktop(t, f, true))
	if _, err := storage.NewLocal(f.pool, testHomeRoot).GCConfirm(context.Background(), f.hostID, []string{home}); err != nil {
		t.Fatalf("gc confirm: %v", err)
	}
	var n int
	must(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM managed_home_claims WHERE canonical_app_id=$1::uuid`, f.steam).Scan(&n))
	if n != 1 {
		t.Errorf("Steam claims = %d after GC confirm, want the claim kept while a session shares the home", n)
	}
}
