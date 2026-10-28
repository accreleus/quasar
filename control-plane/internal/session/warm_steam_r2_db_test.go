package session

// warm_steam_r2_db_test.go — amendment 39 review round 1 behaviour: kind only
// while the switch is on, the agent's real held-exit order, the stale-read
// supersede, a warming claim, the live-claim heartbeat skip, a codec the parked
// GPU cannot encode, console auto-start, the heartbeat's qualification stop,
// the post-eviction cooldown, a stripped adoption, a claimed id never stopped,
// and the home hold that waits for the agent's adoption.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/storage"
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
	// The heartbeat's own dispatch goroutine may reach the agent handshake after
	// the supersede: it then refuses to bind the no-longer-assigned row, and the
	// failure on a stopping row is coerced to a clean stop. Both are a superseded
	// session; what must hold is that the supersede's reason survives and the row
	// is not failed.
	got := readRow(t, f.pool, k)
	if (got.State != "stopping" && got.State != "stopped") || got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("parked row %+v, want stopping or stopped / parked_superseded", got)
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

// The agent lists a claimed parked id until the claim's start adopts it; the
// heartbeat reconcile must not stop it while the claim lives, and does once the
// claim has ended unadopted.
func TestAgentHeartbeatSparesAParkedSessionALiveClaimAdopts(t *testing.T) {
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
		t.Fatalf("heartbeat stopped the claimed id (%q); the play session would cold-start", r)
	}
	f.coord.failSession(res.Session.ID, "test: the claim ended unadopted")
	f.coord.AgentHeartbeat(ctx, f.hostID, []string{k})
	if f.disp.stopReason(k) == "" {
		t.Error("with the claim ended the still-listed claimed id is an orphan and must be stopped")
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

// --- round 3: an assign the agent never took --------------------------------------

// waitState polls until id reaches state.
func waitState(t *testing.T, f *warmFixture, id, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if readRow(t, f.pool, id).State == state {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s never reached %s (%+v)", id, state, readRow(t, f.pool, id))
}

func (f *warmFixture) holder(t *testing.T) string {
	t.Helper()
	var h *string
	must(t, f.pool.QueryRow(context.Background(), `SELECT pending_home_session_id::text FROM managed_home_claims`).Scan(&h))
	return deref(h)
}

// unadoptedClaim parks a running Steam, claims it, and ends the claim before
// the agent adopts, in each way a row can. The agent proves the play id at
// once, as RH05 does for an id it never ran.
func unadoptedClaim(t *testing.T, how string) (f *warmFixture, k, q string) {
	t.Helper()
	f = newWarmFixture(t, 4)
	ctx := context.Background()
	k = f.park(t)
	switch how {
	case "lost assign":
		f.disp.dropEpoch()
	case "rejected assign":
		f.disp.setReject("assign")
	case "queued assign, ack lost":
		f.disp.setReject("assign-ack")
	case "refused start":
		f.disp.setReject("start")
	}
	res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	q = res.Session.ID
	switch how {
	case "user stop":
		f.disp.waitFor(t, "start")
		if _, err := f.coord.Stop(ctx, q, "user_requested"); err != nil {
			t.Fatalf("stop the assigned claim: %v", err)
		}
	case "reconnect reap":
		f.disp.waitFor(t, "start")
		f.coord.AgentReconnected(ctx, f.hostID)
	case "control-plane restart":
		f.disp.waitFor(t, "start")
		fresh := newTestCoordinator(t, f.store, f.disp, testLogger(), WithHomeProvider(storage.NewLocal(f.pool, testHomeRoot)))
		fresh.AgentReconnected(ctx, f.hostID)
		f.coord = fresh
	default:
		waitState(t, f, q, "failed")
	}
	f.coord.AgentState(ctx, f.hostID, agentws.SessionStateMsg{SessionID: q, State: "stopped", HomeCleanupQualified: true})
	return f, k, q
}

// A claim that ends before the agent adopts leaves the hold on the parked
// runtime, which the agent may still run on the home: the play id's proof and
// the reconcile clear nothing, a relaunch waits with the 503, and only the
// parked runtime's own proof clears it (control-api.md §Warm Steam, Claim).
func TestAClaimEndedBeforeAdoptionKeepsTheHoldOnTheParkedRuntime(t *testing.T) {
	for _, how := range []string{"lost assign", "rejected assign", "queued assign, ack lost",
		"refused start", "user stop", "reconnect reap", "control-plane restart"} {
		t.Run(how, func(t *testing.T) {
			f, k, _ := unadoptedClaim(t, how)
			ctx := context.Background()
			if got := f.holder(t); got != k {
				t.Fatalf("hold = %q after the play id's proof, want it kept on the parked runtime %s", got, k)
			}
			before := f.disp.stopsFor(k)
			f.coord.ReconcilePendingHomes(ctx, f.hostID, warmEpoch{f.disp})
			if got := f.holder(t); got != k {
				t.Fatalf("hold = %q after the reconcile, want it kept on %s", got, k)
			}
			if f.disp.stopsFor(k) != before+1 {
				t.Error("the reconcile sent no session_stop for the parked runtime nobody adopts")
			}
			_, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID})
			var refused *ParkedClaimRefusedError
			if !errors.As(err, &refused) || refused.Warming || refused.HolderID != k {
				t.Fatalf("relaunch while the parked runtime stops = %v, want the 503 refusal", err)
			}
			f.coord.AgentState(ctx, f.hostID, agentws.SessionStateMsg{SessionID: k, State: "stopped", HomeCleanupQualified: true})
			if got := f.holder(t); got != "" {
				t.Errorf("hold = %q after the parked runtime's proof, want cleared", got)
			}
		})
	}
}

// The agent's declined or fresh path: the parked runtime exits first and its
// proof arrives while the claim waits to build fresh. The hold passes to the
// claim and is never free in between.
func TestTheParkedRuntimesProofHandsTheHoldToALiveClaim(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	k := f.park(t)
	res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	q := res.Session.ID
	f.disp.waitFor(t, "start")
	f.coord.ReconcilePendingHomes(ctx, f.hostID, warmEpoch{f.disp})
	if r := f.disp.stopReason(k); r != "" {
		t.Fatalf("reconcile stopped the runtime a live claim adopts (%q)", r)
	}
	f.coord.AgentState(ctx, f.hostID, agentws.SessionStateMsg{SessionID: k, State: "stopped", HomeCleanupQualified: true})
	if got := f.holder(t); got != q {
		t.Fatalf("hold = %q after the parked runtime's proof, want it passed to the live claim %s", got, q)
	}
	f.coord.AgentState(ctx, f.hostID, agentws.SessionStateMsg{SessionID: q, State: "starting"})
	if got := f.holder(t); got != q {
		t.Errorf("hold = %q once the claim starts, want %s", got, q)
	}
}

// The park side: a park the agent never took leaves the hold on the ended play
// row whose runtime the agent still holds, and releases that runtime.
func TestAParkEndedBeforeAdoptionKeepsTheHoldOnTheHeldRuntime(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	play := f.runningTile(t)
	f.disp.dropEpoch()

	f.coord.AgentState(ctx, f.hostID, heldStop(play))
	k := ""
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM sessions WHERE kind = 'parked'`).Scan(&k))
	waitState(t, f, k, "failed")
	if f.disp.stopReason(play) == "" {
		t.Error("the held runtime was not released")
	}
	f.coord.AgentState(ctx, f.hostID, agentws.SessionStateMsg{SessionID: k, State: "stopped", HomeCleanupQualified: true})
	f.coord.ReconcilePendingHomes(ctx, f.hostID, warmEpoch{f.disp})
	if got := f.holder(t); got != play {
		t.Fatalf("hold = %q after the parked id's proof and the reconcile, want it kept on %s", got, play)
	}
	_, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID})
	var refused *ParkedClaimRefusedError
	if !errors.As(err, &refused) || refused.HolderID != play {
		t.Fatalf("relaunch while the held runtime stops = %v, want the 503 refusal", err)
	}
	f.coord.AgentState(ctx, f.hostID, agentws.SessionStateMsg{SessionID: play, State: "stopped", HomeCleanupQualified: true})
	if got := f.holder(t); got != "" {
		t.Errorf("hold = %q after the held runtime's proof, want cleared", got)
	}
}

// countingPreparer counts lazy image preparations.
type countingPreparer struct {
	mu sync.Mutex
	n  int
}

func (p *countingPreparer) PrepareLazyImage(context.Context, string, string) error {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
	return nil
}

// A claim adopts a runtime whose image its parked session already prepared, so
// no image preparation can sit between the claim and its assign.
func TestAClaimSkipsLazyImagePreparation(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	f.park(t)
	prep := &countingPreparer{}
	f.coord.SetLazyImagePreparer(prep)
	res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	f.disp.assignFor(t, res.Session.ID)
	prep.mu.Lock()
	defer prep.mu.Unlock()
	if prep.n != 0 {
		t.Errorf("lazy preparations = %d for a claim, want 0", prep.n)
	}
}

// The released marker overwrites any earlier reason but the served one.
func TestMarkReleasedKeepsOnlyEntitlementRevoked(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	for reason, want := range map[string]string{
		"user_requested":             stopReasonRuntimeReleased,
		StopReasonEntitlementRevoked: StopReasonEntitlementRevoked,
		"":                           stopReasonRuntimeReleased,
	} {
		sess, err := f.store.ScheduleAndCreate(ctx, launchParams(f.seedIDs))
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		must(t, exec(t, f.pool, `UPDATE sessions SET state = 'stopped', stop_reason = NULLIF($2, '') WHERE id = $1::uuid`, sess.ID, reason))
		tx, err := f.pool.Begin(ctx)
		must(t, err)
		must(t, markReleased(ctx, tx, sess.ID))
		must(t, tx.Commit(ctx))
		if got := readRow(t, f.pool, sess.ID).StopReason; got != want {
			t.Errorf("after %q: stop_reason = %q, want %q", reason, got, want)
		}
	}
}
