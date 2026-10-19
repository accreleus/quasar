package session

// warm_steam_r2_db_test.go — amendment 39 review round 1 behaviour: kind only
// while the switch is on, the agent's real held-exit order, the stale-read
// supersede, a warming claim, the claimed-row grace, a codec the parked GPU
// cannot encode, console auto-start, the heartbeat's qualification stop, the
// post-eviction cooldown, a stripped adoption, and a claimed id never stopped.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

func TestKindIsSentOnlyWhileWarmSteamIsOn(t *testing.T) {
	f := newWarmFixture(t, 8)
	ctx := context.Background()
	on, err := f.coord.Launch(ctx, f.userID, f.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch with the switch on: %v", err)
	}
	if k := f.disp.assignFor(t, on.Session.ID).Kind; k != KindPlay {
		t.Errorf("kind = %q with the switch on, want play", k)
	}
	setWarm(t, f.pool, false, "library_open", 10)
	off, err := f.coord.Launch(ctx, f.userID, f.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch with the switch off: %v", err)
	}
	if k := f.disp.assignFor(t, off.Session.ID).Kind; k != "" {
		t.Errorf("kind = %q with the switch off, want none: no runtime may be held", k)
	}
}

// The agent's real held-exit order (agent-api.md §session_state): `running`,
// then the old viewer's `bye`, then `stopped{runtime_held}`, with no `stopping`.
// A bye that cannot reach a stuck browser socket must not end the row first.
func TestParkOnExitReplaysTheAgentsRealOrder(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	play := f.runningTile(t)
	f.coord.AgentState(ctx, f.hostID, agentws.SessionStateMsg{SessionID: play, State: "running", Detail: appDetailPresented})

	f.coord.AgentSignalingFailure(ctx, f.hostID, play, true, "relay browser queue blocked")
	if got := readRow(t, f.pool, play); got.State != "running" || f.disp.stopReason(play) != "" {
		t.Fatalf("after the stuck bye: %+v stop=%q, want still running and no stop", got, f.disp.stopReason(play))
	}

	f.coord.AgentState(ctx, f.hostID, heldStop(play))
	if got := readRow(t, f.pool, play); got.State != "stopped" || got.Detail != "game exited" {
		t.Fatalf("play row %+v, want stopped / game exited", got)
	}
	k := f.parkedID(t)
	if k == "" {
		t.Fatal("the real order did not park")
	}
	if a := f.disp.assignFor(t, k); a.Kind != KindParked || a.AdoptSessionID != play {
		t.Errorf("assign kind=%q adopt=%q, want parked adopting %s", a.Kind, a.AdoptSessionID, play)
	}
}

// A `stopping` before the held stop means the control plane asked for the stop:
// that session is never parked.
func TestAStoppingSessionIsNeverParked(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	play := f.runningTile(t)
	if _, err := f.coord.Stop(ctx, play, "user_requested"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	f.coord.AgentState(ctx, f.hostID, heldStop(play))
	if got := readRow(t, f.pool, play); got.State != "stopped" {
		t.Fatalf("play row %+v, want stopped", got)
	}
	if n := countParked(t, f.pool); n != 0 {
		t.Errorf("parked rows = %d, want a release", n)
	}
}

// The launch's parked-session read is outside the user lock. A heartbeat that
// parks after it must still be superseded by the admitted launch.
func TestAdmittedLaunchSupersedesAParkedSessionTheReadMissed(t *testing.T) {
	f := newWarmFixture(t, 8)
	ctx := context.Background()
	afterParkedRead = func() {
		afterParkedRead = nil
		f.coord.SteamWarmHeartbeat(ctx, f.userID, false, f.deviceID)
	}
	t.Cleanup(func() { afterParkedRead = nil })

	if _, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.appID, DeviceID: f.deviceID}); err != nil {
		t.Fatalf("non-Steam launch: %v", err)
	}
	var k string
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM sessions WHERE kind = 'parked'`).Scan(&k))
	if got := readRow(t, f.pool, k); got.State != "stopping" || got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("parked row %+v, want stopping / parked_superseded", got)
	}
}

// A click while the parked Steam is still starting waits for it rather than
// killing it; once it is running the retry claims it.
func TestClaimOnAWarmingParkedSessionWaitsWithoutStoppingIt(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	f.coord.SteamWarmHeartbeat(ctx, f.userID, false, f.deviceID)
	k := f.parkedID(t)
	f.disp.waitFor(t, "start")
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'starting' WHERE id = $1::uuid`, k))

	_, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true})
	var refused *ParkedClaimRefusedError
	if !errors.As(err, &refused) || !refused.Warming {
		t.Fatalf("claim while starting = %v, want a warming refusal", err)
	}
	if got := readRow(t, f.pool, k); got.State != "starting" || f.disp.stopReason(k) != "" {
		t.Fatalf("parked row %+v stop=%q, want left starting and not stopped", got, f.disp.stopReason(k))
	}

	f.running(t, k)
	res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true})
	if err != nil {
		t.Fatalf("retry once running: %v", err)
	}
	if a := f.disp.assignFor(t, res.Session.ID); a.AdoptSessionID != k {
		t.Errorf("retry adopts %q, want the parked session %s", a.AdoptSessionID, k)
	}
}

// The agent lists a just-claimed parked id until the adopting assign lands; the
// heartbeat reconcile must not stop it inside the grace, and does after.
func TestAgentHeartbeatSparesAJustClaimedParkedSession(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	k := f.park(t)
	res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	f.disp.assignFor(t, res.Session.ID)

	f.coord.AgentHeartbeat(ctx, f.hostID, []string{k})
	if r := f.disp.stopReason(k); r != "" {
		t.Fatalf("heartbeat stopped the just-claimed id (%q); the play session would cold-start", r)
	}
	must(t, exec(t, f.pool, `UPDATE sessions SET ended_at = now() - interval '1 minute' WHERE id = $1::uuid`, k))
	f.coord.AgentHeartbeat(ctx, f.hostID, []string{k})
	if f.disp.stopReason(k) == "" {
		t.Error("past the grace the still-listed claimed id is an orphan and must be stopped")
	}
}

// A codec the parked GPU cannot encode is a failed check 4: the parked session
// is superseded and the launch answers the retryable 503.
func TestClaimWithACodecTheParkedGPUCannotEncode(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	k := f.park(t)
	// An explicit stream override takes the legacy path, where the GPU's codec set
	// is the clamp (ErrCodecUnsupportedByHost); a profile with no such rung is a 400.
	av1, w, h := wireCodecAV1, int32(1920), int32(1080)
	_, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true,
		Override: StreamOverride{Codec: &av1, Width: &w, Height: &h}})
	var refused *ParkedClaimRefusedError
	if !errors.As(err, &refused) || refused.Warming {
		t.Fatalf("claim with an unencodable codec = %v, want the 503 refusal", err)
	}
	if got := readRow(t, f.pool, k); got.State != "stopping" || got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("parked row %+v, want stopping / parked_superseded", got)
	}
}

func TestConsoleAutoStartSupersedesTheParkedHolder(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	must(t, exec(t, f.pool, `UPDATE apps SET runtime_spec = runtime_spec || '{"direct_display":true}'::jsonb
		WHERE id = $1::uuid`, f.steam))
	k := f.park(t)

	_, err := f.coord.LaunchConsoleSession(ctx, f.hostID, f.userID, f.steam, "local_only", 0, 0, 0)
	if !errors.Is(err, ErrConsoleHomeSettling) {
		t.Fatalf("console launch beside a parked holder = %v, want the settling refusal it re-runs after", err)
	}
	if got := readRow(t, f.pool, k); got.State != "stopping" || got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("parked row %+v, want stopping / parked_superseded", got)
	}
}

func TestConsoleLaunchOutsideTheFamilySupersedes(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	must(t, exec(t, f.pool, `UPDATE apps SET runtime_spec = '{"direct_display":true}'::jsonb WHERE id = $1::uuid`, f.appID))
	k := insertParked(t, f.pool, f.userID, f.steam, f.hostID, f.gpuID, 2, "10 minutes")

	if _, err := f.coord.LaunchConsoleSession(ctx, f.hostID, f.userID, f.appID, "local_only", 0, 0, 0); err != nil {
		t.Fatalf("console launch: %v", err)
	}
	if got := readRow(t, f.pool, k); got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("parked row %+v, want parked_superseded after an admitted console launch", got)
	}
}

func TestHeartbeatStopsAParkedSessionThatNoLongerQualifies(t *testing.T) {
	for name, change := range map[string]string{
		"parent disabled": `UPDATE apps SET enabled = false WHERE library_provider = 'steam'`,
		"quota zero":      `UPDATE users SET max_concurrent_sessions = 0`,
		"entitlement gone": `DELETE FROM entitlements WHERE app_id IN
			(SELECT id FROM apps WHERE library_provider = 'steam')`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newWarmFixture(t, 4)
			k := f.park(t)
			must(t, exec(t, f.pool, change))
			f.coord.SteamWarmHeartbeat(context.Background(), f.userID, false, f.deviceID)
			if got := readRow(t, f.pool, k); got.State != "stopping" || got.StopReason != StopReasonParkedSuperseded {
				t.Errorf("parked row %+v, want stopping / parked_superseded", got)
			}
		})
	}
}

func TestExpireParkedStopsAParkedSessionWhoseParentIsDisabled(t *testing.T) {
	f := newWarmFixture(t, 4)
	k := insertParked(t, f.pool, f.userID, f.steam, f.hostID, f.gpuID, 2, "10 minutes")
	must(t, exec(t, f.pool, `UPDATE apps SET enabled = false WHERE id = $1::uuid`, f.steam))
	if n, err := f.coord.ExpireParked(context.Background()); err != nil || n != 1 {
		t.Fatalf("expire = %d, %v; want 1", n, err)
	}
	if got := readRow(t, f.pool, k); got.StopReason != StopReasonParkedExpired {
		t.Errorf("parked row %+v, want parked_expired", got)
	}
}

// An evicted user's heartbeat does not re-park into the room the eviction freed
// for the refused launch's retry, for 5 minutes.
func TestEvictionStartsAParkCooldown(t *testing.T) {
	f := newWarmFixture(t, 2) // the parked session fills the GPU
	ctx := context.Background()
	k := f.park(t)
	if _, err := f.coord.Launch(ctx, warmUser(t, f.pool, "other"), f.appID, StreamOverride{}); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("other user's launch = %v, want capacity_exhausted with an eviction", err)
	}
	if got := readRow(t, f.pool, k); got.StopReason != StopReasonParkedEvicted {
		t.Fatalf("parked row %+v, want parked_evicted", got)
	}
	f.ended(t, k)

	f.coord.SteamWarmHeartbeat(ctx, f.userID, false, f.deviceID)
	if id := f.parkedID(t); id != "" {
		t.Fatalf("heartbeat re-parked %s inside the eviction cooldown", id)
	}
	must(t, exec(t, f.pool, `UPDATE sessions SET ended_at = now() - interval '6 minutes' WHERE id = $1::uuid`, k))
	f.coord.SteamWarmHeartbeat(ctx, f.userID, false, f.deviceID)
	if f.parkedID(t) == "" {
		t.Error("heartbeat after the cooldown parked nothing")
	}
}

// An adoption dispatched to a connection without parked_sessions_v1 fails the
// session and stops the runtime it would have adopted, rather than stripping
// the field and leaving that runtime on the home.
func TestAnAdoptionToAConnectionThatCannotTakeItFailsClosed(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	sess, err := f.store.ScheduleAndCreate(ctx, launchParams(f.seedIDs))
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	k := insertParked(t, f.pool, f.userID, f.steam, f.hostID, f.gpuID, 1, "10 minutes")
	f.disp.setParks(false)
	sess.AdoptSessionID = k
	f.coord.dispatchAssignStart(sess, []byte(`{}`), nil)

	if got := readRow(t, f.pool, sess.ID); got.State != "failed" {
		t.Errorf("session %+v, want failed", got)
	}
	if f.disp.stopReason(k) == "" {
		t.Error("the runtime it would have adopted was not stopped")
	}
}

// A claimed session is already stopped, so no later expiry, supersede or drain
// sends a stop for its id (the adopted runtime is the play session's now).
func TestAClaimedSessionIsNeverStoppedLater(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	k := f.park(t)
	if _, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	setWarm(t, f.pool, false, "library_open", 10)
	if _, err := f.coord.ExpireParked(ctx); err != nil {
		t.Fatalf("expire: %v", err)
	}
	f.coord.supersedeParked(ctx, f.userID, "", "")
	if err := f.coord.StopParkedOnHost(ctx, f.hostID); err != nil {
		t.Fatalf("drain stop: %v", err)
	}
	if r := f.disp.stopReason(k); r != "" {
		t.Errorf("a stop (%q) was sent for the claimed id", r)
	}
	if got := readRow(t, f.pool, k); got.Detail != claimedDetail || got.StopReason != "" {
		t.Errorf("claimed row %+v, want untouched", got)
	}
}

// A heartbeat never waits on a settling hold: it parks nothing and returns.
func TestHeartbeatNeverWaitsOnASettlingHold(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	play := f.runningTile(t)
	// An unqualified terminal leaves the hold settling on a recent, connected holder.
	f.coord.AgentState(ctx, f.hostID, agentws.SessionStateMsg{SessionID: play, State: "stopped"})

	start := time.Now()
	f.coord.SteamWarmHeartbeat(ctx, f.userID, false, f.deviceID)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("heartbeat took %v, want no settle wait", d)
	}
	if f.parkedID(t) != "" {
		t.Error("heartbeat parked onto a settling hold")
	}
}
