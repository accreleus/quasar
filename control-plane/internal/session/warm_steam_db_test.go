package session

// warm_steam_db_test.go — amendment 39 (#759), warm Steam, at the store and
// coordinator seams: the heartbeat, the claim and its races, park on exit,
// eviction, supersede and suppression, expiry, drains, the user-facing reads
// and shader progress. Semantics: control-api.md §Warm Steam.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/storage"
)

// warmDispatcher records every command and answers parked_sessions_v1.
type warmDispatcher struct {
	mu      sync.Mutex
	parks   bool
	noEpoch bool // the agent's socket is gone: no command epoch to send on
	// reject answers one command kind: "assign" or "start" refused, or
	// "assign-ack" queued with its ack lost.
	reject  string
	assigns []agentws.SessionAssignCmd
	stops   []agentws.SessionStopCmd
	swaps   []agentws.SessionSwapAppCmd
	saw     chan string
}

func newWarmDispatcher(parks bool) *warmDispatcher {
	return &warmDispatcher{parks: parks, saw: make(chan string, 256)}
}

func (d *warmDispatcher) SupportsParkedSessions(string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.parks
}

func (d *warmDispatcher) setParks(v bool) {
	d.mu.Lock()
	d.parks = v
	d.mu.Unlock()
}

func (d *warmDispatcher) record(v any) {
	d.mu.Lock()
	kind := ""
	switch c := v.(type) {
	case agentws.SessionAssignCmd:
		kind = "assign"
		d.assigns = append(d.assigns, c)
	case agentws.SessionStartCmd:
		kind = "start"
	case agentws.SessionStopCmd:
		kind = "stop"
		d.stops = append(d.stops, c)
	case agentws.SessionSwapAppCmd:
		kind = "swap"
		d.swaps = append(d.swaps, c)
	}
	d.mu.Unlock()
	select {
	case d.saw <- kind:
	default:
	}
}

func (d *warmDispatcher) Send(_ string, v any) error { d.record(v); return nil }

// CurrentHomeCommandEpoch makes every managed-home dispatch take the
// cleanup-capable path, so holds are created and moved as in production.
func (d *warmDispatcher) CurrentHomeCommandEpoch(string) (agentws.HomeCommandEpoch, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.noEpoch {
		return nil, false
	}
	return warmEpoch{d}, true
}

func (d *warmDispatcher) dropEpoch() {
	d.mu.Lock()
	d.noEpoch = true
	d.mu.Unlock()
}

type warmEpoch struct{ d *warmDispatcher }

func (warmEpoch) SupportsHomeCleanup() bool      { return true }
func (e warmEpoch) SupportsParkedSessions() bool { return e.d.SupportsParkedSessions("") }
func (e warmEpoch) Send(v any) (bool, error)     { e.d.record(v); return true, nil }
func (e warmEpoch) SendWithAck(_ context.Context, _ string, v any) (agentws.AckResult, bool, error) {
	e.d.record(v)
	if _, ok := v.(agentws.SessionAssignCmd); ok {
		switch e.d.rejecting() {
		case "assign":
			return agentws.AckResult{OK: false, Error: "refused"}, true, nil
		case "assign-ack":
			return agentws.AckResult{}, true, errors.New("ack timeout")
		}
	}
	return agentws.AckResult{OK: true}, true, nil
}

func (d *warmDispatcher) SendWithAck(_ context.Context, _, _ string, v any) (agentws.AckResult, error) {
	d.record(v)
	if _, ok := v.(agentws.SessionStartCmd); ok && d.rejecting() == "start" {
		return agentws.AckResult{OK: false, Error: "refused"}, nil
	}
	return agentws.AckResult{OK: true}, nil
}

func (d *warmDispatcher) rejecting() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reject
}

func (d *warmDispatcher) setReject(kind string) {
	d.mu.Lock()
	d.reject = kind
	d.mu.Unlock()
}

func (d *warmDispatcher) waitFor(t *testing.T, kind string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case k := <-d.saw:
			if k == kind {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s dispatch", kind)
		}
	}
}

func (d *warmDispatcher) assignFor(t *testing.T, sessionID string) agentws.SessionAssignCmd {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		for _, a := range d.assigns {
			if a.SessionID == sessionID {
				d.mu.Unlock()
				return a
			}
		}
		d.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no assign for session %s", sessionID)
	return agentws.SessionAssignCmd{}
}

func (d *warmDispatcher) stopsFor(sessionID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, s := range d.stops {
		if s.SessionID == sessionID {
			n++
		}
	}
	return n
}

func (d *warmDispatcher) stopReason(sessionID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.stops {
		if s.SessionID == sessionID {
			return s.Reason
		}
	}
	return ""
}

type warmFixture struct {
	seedIDs
	steam    string
	deviceID string
	pool     *pgxpool.Pool
	store    *Store
	disp     *warmDispatcher
	coord    *Coordinator
}

// newWarmFixture is the standard seed plus a managed-home Steam parent (two
// encode slots), the user's home for it on the seed host, a bound device, and
// warm Steam switched on.
func newWarmFixture(t *testing.T, slots int) *warmFixture {
	t.Helper()
	pool := testDB(t)
	s := seed(t, pool, slots)
	steam := seedSteamApp(t, pool, `{"image":"steam:1","env":{"STEAM_STARTUP_FLAGS":"-bigpicture"}}`)
	provisionHome(t, pool, s.userID, steam, s.hostID)
	f := &warmFixture{seedIDs: s, steam: steam, pool: pool, store: NewStore(pool), disp: newWarmDispatcher(true)}
	f.deviceID = seedDevice(t, pool, s.userID, "warm-dev")
	clearHoldsAtEnd(t, pool)
	setWarm(t, pool, true, "library_open", 10)
	f.coord = newTestCoordinator(t, f.store, f.disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	return f
}

// clearHoldsAtEnd drops every pending-home hold before the next test's
// truncate, which a held home refuses (QH001).
func clearHoldsAtEnd(t *testing.T, pool *pgxpool.Pool) {
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE managed_home_claims SET pending_home_session_id = NULL,
			pending_home_token = NULL, pending_home_started_at = NULL`)
	})
}

// ended stops a session and clears its hold, as the agent's qualified
// terminal report would.
func (f *warmFixture) ended(t *testing.T, id string) {
	t.Helper()
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'stopped', ended_at = now() WHERE id = $1::uuid`, id))
	must(t, exec(t, f.pool, `UPDATE managed_home_claims SET pending_home_session_id = NULL,
		pending_home_token = NULL, pending_home_started_at = NULL WHERE pending_home_session_id = $1::uuid`, id))
}

func seedDevice(t *testing.T, pool *pgxpool.Pool, userID, key string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO user_devices (user_id, device_key)
		VALUES ($1::uuid, $2) RETURNING id::text`, userID, key).Scan(&id))
	return id
}

// setWarm writes the three instance settings and puts the shared singleton back.
func setWarm(t *testing.T, pool *pgxpool.Pool, enabled bool, rewarm string, idle int) {
	t.Helper()
	ctx := context.Background()
	must(t, exec(t, pool, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT (id) DO NOTHING`))
	must(t, exec(t, pool, `UPDATE instance_settings SET steam_warm_enabled = $1, steam_warm_rewarm = $2,
		steam_warm_idle_minutes = $3 WHERE id = true`, enabled, rewarm, idle))
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE instance_settings SET steam_warm_enabled = false,
			steam_warm_rewarm = 'library_open', steam_warm_idle_minutes = 10 WHERE id = true`)
	})
}

// park runs a heartbeat, waits for the parked session's start and reports it
// running, as the agent would.
func (f *warmFixture) park(t *testing.T) string {
	t.Helper()
	f.coord.SteamWarmHeartbeat(context.Background(), f.userID, false, f.deviceID)
	id := f.parkedID(t)
	if id == "" {
		t.Fatal("heartbeat created no parked session")
	}
	f.disp.waitFor(t, "start")
	f.running(t, id)
	return id
}

func (f *warmFixture) running(t *testing.T, id string) {
	t.Helper()
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'running', started_at = now() WHERE id = $1::uuid`, id))
}

// parkedID is the user's live parked session, "" when none.
func (f *warmFixture) parkedID(t *testing.T) string {
	t.Helper()
	k, err := f.store.liveParked(context.Background(), f.userID)
	if err != nil {
		t.Fatalf("read parked: %v", err)
	}
	return k.ID
}

type rowState struct {
	State, Detail, StopReason, Kind string
	GPUID                           string
	Slots                           int32
}

func readRow(t *testing.T, pool *pgxpool.Pool, id string) rowState {
	t.Helper()
	var r rowState
	var detail, reason, gpu *string
	must(t, pool.QueryRow(context.Background(), `SELECT state, state_detail, stop_reason, kind,
		gpu_id::text, reserved_encode_slots FROM sessions WHERE id = $1::uuid`, id).
		Scan(&r.State, &detail, &reason, &r.Kind, &gpu, &r.Slots))
	r.Detail, r.StopReason, r.GPUID = deref(detail), deref(reason), deref(gpu)
	return r
}

// insertParked writes a live parked row directly, for tests about reading or
// stopping one rather than creating it.
func insertParked(t *testing.T, pool *pgxpool.Pool, userID, appID, hostID, gpuID string, slots int, until string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO sessions
		(user_id, app_id, host_id, gpu_id, state, width, height, fps, bitrate_kbps, h264_profile,
		 reserved_encode_slots, assigned_at, started_at, kind, parked_until)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'running', 1920, 1080, 60, 12000,
		        'constrained-baseline', $5, now(), now(), 'parked', now() + $6::interval)
		RETURNING id::text`, userID, appID, hostID, gpuID, slots, until).Scan(&id))
	return id
}

func warmUser(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	return seedUser(t, pool, name+"@test.local", name)
}

func countParked(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	must(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE kind = 'parked'`).Scan(&n))
	return n
}

// --- the heartbeat --------------------------------------------------------------

func TestHeartbeatCreatesOneParkedSession(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()

	f.coord.SteamWarmHeartbeat(ctx, f.userID, false, f.deviceID)
	id := f.parkedID(t)
	if id == "" {
		t.Fatal("no parked session")
	}
	a := f.disp.assignFor(t, id)
	if a.Kind != KindParked || a.AdoptSessionID != "" {
		t.Errorf("assign kind=%q adopt=%q, want parked and no adoption", a.Kind, a.AdoptSessionID)
	}
	if got := envOf(t, a.App)["STEAM_STARTUP_FLAGS"]; got != "-silent" {
		t.Errorf("STEAM_STARTUP_FLAGS = %q, want -silent", got)
	}
	var app, device string
	var tokens int
	var minutes float64
	must(t, f.pool.QueryRow(ctx, `SELECT app_id::text, device_id::text,
		EXTRACT(EPOCH FROM parked_until - now()) / 60,
		(SELECT count(*) FROM session_tokens WHERE session_id = s.id)
		FROM sessions s WHERE id = $1::uuid`, id).Scan(&app, &device, &minutes, &tokens))
	if app != f.steam || device != f.deviceID || tokens != 0 {
		t.Errorf("parked row app=%s device=%s tokens=%d, want the Steam parent, the token's device, no token", app, device, tokens)
	}
	if minutes < 9 || minutes > 10.1 {
		t.Errorf("parked_until is %.1f min away, want steam_warm_idle_minutes (10)", minutes)
	}

	// A second heartbeat creates nothing and extends the deadline.
	must(t, exec(t, f.pool, `UPDATE sessions SET parked_until = now() + interval '1 minute' WHERE id = $1::uuid`, id))
	f.coord.SteamWarmHeartbeat(ctx, f.userID, false, f.deviceID)
	if n := countParked(t, f.pool); n != 1 {
		t.Fatalf("parked rows = %d after a second heartbeat, want 1", n)
	}
	must(t, f.pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM parked_until - now()) / 60 FROM sessions
		WHERE id = $1::uuid`, id).Scan(&minutes))
	if minutes < 9 {
		t.Errorf("heartbeat left parked_until %.1f min away, want it extended to 10", minutes)
	}
}

func TestHeartbeatCreatesNothingUnlessEveryRuleAllows(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *warmFixture) (deviceID string)
	}{
		{"switch off", func(t *testing.T, f *warmFixture) string {
			setWarm(t, f.pool, false, "library_open", 10)
			return f.deviceID
		}},
		{"no home yet", func(t *testing.T, f *warmFixture) string {
			must(t, exec(t, f.pool, `DELETE FROM managed_home_claims; DELETE FROM user_homes`))
			return f.deviceID
		}},
		{"tombstoned home", func(t *testing.T, f *warmFixture) string {
			must(t, exec(t, f.pool, `UPDATE user_homes SET gc_after = now()`))
			return f.deviceID
		}},
		{"a live session", func(t *testing.T, f *warmFixture) string {
			if _, err := f.store.ScheduleAndCreate(context.Background(), launchParams(f.seedIDs)); err != nil {
				t.Fatalf("live session: %v", err)
			}
			return f.deviceID
		}},
		{"suppressed", func(t *testing.T, f *warmFixture) string {
			must(t, exec(t, f.pool, `UPDATE users SET steam_warm_suppressed = true`))
			return f.deviceID
		}},
		{"zero quota", func(t *testing.T, f *warmFixture) string {
			must(t, exec(t, f.pool, `UPDATE users SET max_concurrent_sessions = 0`))
			return f.deviceID
		}},
		{"unbound token", func(t *testing.T, f *warmFixture) string { return "" }},
		{"host without parked_sessions_v1", func(t *testing.T, f *warmFixture) string {
			f.disp.setParks(false)
			return f.deviceID
		}},
		{"two Steam parents", func(t *testing.T, f *warmFixture) string {
			var other string
			must(t, f.pool.QueryRow(context.Background(), `INSERT INTO apps
				(name, default_encode_slots, runtime_spec, managed_home, home_container_path, kind, library_provider)
				VALUES ('Steam 2', 2, '{}', true, '/home/quasar', 'launcher', 'steam') RETURNING id::text`).Scan(&other))
			entitleAll(t, f.pool, other)
			return f.deviceID
		}},
		{"cordoned host", func(t *testing.T, f *warmFixture) string {
			must(t, exec(t, f.pool, `UPDATE hosts SET status = 'draining'`))
			return f.deviceID
		}},
		{"no room", func(t *testing.T, f *warmFixture) string {
			must(t, exec(t, f.pool, `UPDATE gpus SET encode_slots_total = 1`))
			return f.deviceID
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newWarmFixture(t, 4)
			dev := c.setup(t, f)
			f.coord.SteamWarmHeartbeat(context.Background(), f.userID, false, dev)
			if n := countParked(t, f.pool); n != 0 {
				t.Fatalf("parked rows = %d, want 0", n)
			}
		})
	}
}

// Two heartbeats at once never leave two warm Steams (the user lock plus
// sessions_one_parked_per_user).
func TestConcurrentHeartbeatsCreateOneParkedSession(t *testing.T) {
	f := newWarmFixture(t, 8)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.coord.SteamWarmHeartbeat(context.Background(), f.userID, false, f.deviceID)
		}()
	}
	wg.Wait()
	if n := countParked(t, f.pool); n != 1 {
		t.Fatalf("parked rows = %d, want 1", n)
	}
}

// --- the claim -------------------------------------------------------------------

func TestClaimAdoptsTheParkedSessionInOneTransaction(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	k := f.park(t)
	var holder string
	must(t, f.pool.QueryRow(ctx, `SELECT pending_home_session_id::text FROM managed_home_claims`).Scan(&holder))
	if holder != k {
		t.Fatalf("hold = %s, want the parked session's dispatch hold", holder)
	}
	parked := readRow(t, f.pool, k)

	res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: tile, DeviceID: f.deviceID, Mic: true})
	if err != nil {
		t.Fatalf("claim launch: %v", err)
	}
	play := res.Session
	if play.State != StateAssigned || deref(play.GPUID) != parked.GPUID || play.Kind != KindPlay {
		t.Errorf("play session state=%s gpu=%s kind=%s, want assigned on the parked GPU", play.State, deref(play.GPUID), play.Kind)
	}
	if res.SignalingToken == "" {
		t.Error("a claim still mints the play session's signaling token")
	}
	if got := readRow(t, f.pool, k); got.State != "stopped" || got.Detail != "claimed" || got.StopReason != "" {
		t.Errorf("parked row %+v, want stopped / claimed with no stop reason", got)
	}
	a := f.disp.assignFor(t, play.ID)
	if a.Kind != KindPlay || a.AdoptSessionID != k {
		t.Errorf("assign kind=%q adopt=%q, want play adopting %s", a.Kind, a.AdoptSessionID, k)
	}
	var playHome, parkedHome, adopts string
	must(t, f.pool.QueryRow(ctx, `SELECT COALESCE(p.managed_home_id::text, ''), COALESCE(k.managed_home_id::text, ''),
		COALESCE(p.adopt_session_id::text, '')
		FROM sessions p, sessions k WHERE p.id = $1::uuid AND k.id = $2::uuid`, play.ID, k).Scan(&playHome, &parkedHome, &adopts))
	if got := f.holder(t); got != k || playHome == "" || playHome != parkedHome || adopts != k {
		t.Errorf("hold=%s play home=%q parked home=%q adopts=%q, want the binding copied and the hold left on %s until the agent adopts",
			got, playHome, parkedHome, adopts, k)
	}
	// The play session's first report is the agent's adoption: the hold moves with it.
	f.coord.AgentState(ctx, f.hostID, agentws.SessionStateMsg{SessionID: play.ID, State: "starting"})
	if got := f.holder(t); got != play.ID {
		t.Errorf("hold = %s after the play session's first report, want %s", got, play.ID)
	}
	var reserved int32
	must(t, f.pool.QueryRow(ctx, `SELECT COALESCE(SUM(reserved_encode_slots), 0) FROM sessions
		WHERE gpu_id = $1::uuid AND state IN `+activeStatesSQL, parked.GPUID).Scan(&reserved))
	if reserved != play.ReservedSlots {
		t.Errorf("reserved slots = %d, want only the play session's %d", reserved, play.ReservedSlots)
	}
}

func TestTwoConcurrentClaimsAdoptOnce(t *testing.T) {
	f := newWarmFixture(t, 8)
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	k := f.park(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, app := range []string{tile, f.steam} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.coord.LaunchByProfile(context.Background(), f.userID,
				LaunchParams{AppID: app, DeviceID: f.deviceID, Mic: true})
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("errors = %v / %v, want exactly one claim to succeed", errs[0], errs[1])
	}
	var adopted int
	must(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions
		WHERE kind = 'play' AND state = 'assigned'`).Scan(&adopted))
	if adopted != 1 || readRow(t, f.pool, k).Detail != "claimed" {
		t.Fatalf("play sessions = %d, parked = %+v; want one play and the parked row claimed", adopted, readRow(t, f.pool, k))
	}
}

func TestClaimRacingExpiryHasOneWinner(t *testing.T) {
	for i := 0; i < 5; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) { claimRacingExpiry(t) })
	}
}

func claimRacingExpiry(t *testing.T) {
	f := newWarmFixture(t, 4)
	k := f.park(t)
	setWarm(t, f.pool, false, "library_open", 10) // expiry stops every parked session

	var wg sync.WaitGroup
	var launchErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, launchErr = f.coord.LaunchByProfile(context.Background(), f.userID,
			LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true})
	}()
	go func() {
		defer wg.Done()
		_, _ = f.coord.ExpireParked(context.Background())
	}()
	wg.Wait()
	got := readRow(t, f.pool, k)
	var refused *ParkedClaimRefusedError
	switch {
	case launchErr == nil:
		if got.Detail != "claimed" || got.StopReason != "" {
			t.Fatalf("claim won but parked row is %+v", got)
		}
	case errors.As(launchErr, &refused):
		if got.State != "stopping" || got.StopReason != StopReasonParkedExpired {
			t.Fatalf("expiry won but parked row is %+v", got)
		}
	default:
		t.Fatalf("launch err = %v, want success or a refused claim", launchErr)
	}
}

func TestClaimRacingEvictionHasOneWinner(t *testing.T) {
	for i := 0; i < 5; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) { claimRacingEviction(t) })
	}
}

func claimRacingEviction(t *testing.T) {
	f := newWarmFixture(t, 2) // the parked session fills the GPU
	k := f.park(t)
	other := warmUser(t, f.pool, "evictor")

	var wg sync.WaitGroup
	var claimErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, claimErr = f.coord.LaunchByProfile(context.Background(), f.userID,
			LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true})
	}()
	go func() {
		defer wg.Done()
		_, _ = f.coord.Launch(context.Background(), other, f.appID, StreamOverride{})
	}()
	wg.Wait()
	got := readRow(t, f.pool, k)
	var refused *ParkedClaimRefusedError
	switch {
	case claimErr == nil:
		if got.Detail != "claimed" || got.StopReason != "" {
			t.Fatalf("claim won but parked row is %+v", got)
		}
	case errors.As(claimErr, &refused):
		if got.State != "stopping" || got.StopReason != StopReasonParkedEvicted {
			t.Fatalf("eviction won but parked row is %+v", got)
		}
	default:
		t.Fatalf("claim err = %v, want success or a refused claim", claimErr)
	}
}

func TestFailedClaimStopsTheParkedSessionAndRefusesWithCapacity(t *testing.T) {
	f := newWarmFixture(t, 4)
	k := f.park(t)
	// The parked runtime encodes another frame rate: check 4 fails.
	must(t, exec(t, f.pool, `UPDATE sessions SET fps = 30 WHERE id = $1::uuid`, k))

	_, err := f.coord.LaunchByProfile(context.Background(), f.userID,
		LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true})
	var refused *ParkedClaimRefusedError
	if !errors.As(err, &refused) || !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("err = %v, want a refused claim that reads as capacity_exhausted", err)
	}
	if got := readRow(t, f.pool, k); got.State != "stopping" || got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("parked row %+v, want stopping / parked_superseded", got)
	}
	if r := f.disp.stopReason(k); r != StopReasonParkedSuperseded {
		t.Errorf("session_stop reason = %q, want parked_superseded", r)
	}
	var plays int
	must(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE kind = 'play'`).Scan(&plays))
	if plays != 0 {
		t.Errorf("play rows = %d, want none persisted for a refused launch", plays)
	}
}

// --- park on exit ----------------------------------------------------------------

func (f *warmFixture) runningTile(t *testing.T) string {
	t.Helper()
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	res, err := f.coord.LaunchByProfile(context.Background(), f.userID, LaunchParams{AppID: tile, DeviceID: f.deviceID})
	if err != nil {
		t.Fatalf("launch tile: %v", err)
	}
	f.disp.waitFor(t, "start")
	f.running(t, res.Session.ID)
	return res.Session.ID
}

func heldStop(id string) agentws.SessionStateMsg {
	return agentws.SessionStateMsg{SessionID: id, State: "stopped", Detail: "game exited",
		RuntimeHeld: true, HomeCleanupQualified: true}
}

func TestParkOnExitParksTheHeldRuntime(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	play := f.runningTile(t)
	before := readRow(t, f.pool, play)

	f.coord.AgentState(ctx, f.hostID, heldStop(play))

	if got := readRow(t, f.pool, play); got.State != "stopped" || got.Detail != "game exited" {
		t.Fatalf("play row %+v, want stopped / game exited", got)
	}
	k := f.parkedID(t)
	if k == "" {
		t.Fatal("no parked session")
	}
	got := readRow(t, f.pool, k)
	if got.State != "assigned" || got.GPUID != before.GPUID || got.Slots != before.Slots {
		t.Errorf("parked row %+v, want assigned on gpu %s holding %d slots", got, before.GPUID, before.Slots)
	}
	var app string
	must(t, f.pool.QueryRow(ctx, `SELECT app_id::text FROM sessions WHERE id = $1::uuid`, k).Scan(&app))
	if got := f.holder(t); app != f.steam || got != play {
		t.Errorf("parked app=%s hold=%s, want the Steam parent and the hold kept on %s (not cleared as cleanup proof)", app, got, play)
	}
	a := f.disp.assignFor(t, k)
	if a.Kind != KindParked || a.AdoptSessionID != play || envOf(t, a.App)["STEAM_STARTUP_FLAGS"] != "-silent" {
		t.Errorf("assign kind=%q adopt=%q flags=%q, want parked adopting %s with -silent",
			a.Kind, a.AdoptSessionID, envOf(t, a.App)["STEAM_STARTUP_FLAGS"], play)
	}
	f.disp.waitFor(t, "start")
	f.coord.AgentState(ctx, f.hostID, agentws.SessionStateMsg{SessionID: k, State: "starting"})
	if got := f.holder(t); got != k {
		t.Errorf("hold = %s after the parked session's first report, want %s", got, k)
	}
}

func TestParkOnExitReleasesWhenARuleSaysNo(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *warmFixture)
	}{
		{"switch off", func(t *testing.T, f *warmFixture) { setWarm(t, f.pool, false, "library_open", 10) }},
		{"another live session", func(t *testing.T, f *warmFixture) {
			if _, err := f.store.ScheduleAndCreate(context.Background(), launchParams(f.seedIDs)); err != nil {
				t.Fatalf("second session: %v", err)
			}
		}},
		{"cordoned host", func(t *testing.T, f *warmFixture) {
			must(t, exec(t, f.pool, `UPDATE hosts SET status = 'draining'`))
		}},
		{"host without parked_sessions_v1", func(t *testing.T, f *warmFixture) { f.disp.setParks(false) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newWarmFixture(t, 4)
			play := f.runningTile(t)
			c.setup(t, f)
			f.coord.AgentState(context.Background(), f.hostID, heldStop(play))
			if got := readRow(t, f.pool, play); got.State != "stopped" {
				t.Fatalf("play row %+v, want stopped", got)
			}
			if n := countParked(t, f.pool); n != 0 {
				t.Errorf("parked rows = %d, want a release", n)
			}
			if f.disp.stopReason(play) == "" {
				t.Error("no session_stop for the ended id: the held runtime is never released")
			}
		})
	}
}

// A released runtime keeps the ended row's hold until the agent's qualified
// `stopped` for it (agent-api.md §session_state, runtime_held), and a same-home
// relaunch meanwhile gets the retryable 503, never home_conflict; after the
// proof it is admitted.
func TestReleasedHeldRuntimeAnswers503UntilTheQualifiedStop(t *testing.T) {
	f := newWarmFixture(t, 8)
	ctx := context.Background()
	play := f.runningTile(t)
	setWarm(t, f.pool, false, "library_open", 10) // a release, not a park

	f.coord.AgentState(ctx, f.hostID, heldStop(play))
	holder := func() string {
		var h *string
		must(t, f.pool.QueryRow(ctx, `SELECT pending_home_session_id::text FROM managed_home_claims`).Scan(&h))
		return deref(h)
	}
	if got := holder(); got != play {
		t.Fatalf("hold = %q after the runtime_held stop, want it kept on %s", got, play)
	}
	if got := readRow(t, f.pool, play); got.StopReason != stopReasonRuntimeReleased {
		t.Fatalf("released row %+v, want stop_reason runtime_released", got)
	}

	start := time.Now()
	_, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID})
	var refused *ParkedClaimRefusedError
	if !errors.As(err, &refused) || refused.Warming || refused.HolderID != play {
		t.Fatalf("relaunch while released = %v, want the 503 refusal naming the held row privately", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("relaunch waited %v on the hold, want an immediate 503", time.Since(start))
	}

	f.coord.AgentState(ctx, f.hostID, agentws.SessionStateMsg{SessionID: play, State: "stopped",
		Detail: "stopped", HomeCleanupQualified: true})
	res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID})
	if err != nil {
		t.Fatalf("relaunch after the cleanup proof: %v", err)
	}
	f.disp.assignFor(t, res.Session.ID)
	if got := holder(); got != res.Session.ID {
		t.Errorf("hold = %q, want the relaunched session %s", got, res.Session.ID)
	}
}

// --- eviction, supersede and suppression ------------------------------------------

func TestEvictionStopsOnlyTheOldestEligibleParkedSession(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	a, b, c := warmUser(t, f.pool, "a"), warmUser(t, f.pool, "b"), warmUser(t, f.pool, "c")
	older := insertParked(t, f.pool, a, f.steam, f.hostID, f.gpuID, 2, "10 minutes")
	must(t, exec(t, f.pool, `UPDATE sessions SET created_at = now() - interval '1 hour' WHERE id = $1::uuid`, older))
	newer := insertParked(t, f.pool, b, f.steam, f.hostID, f.gpuID, 2, "10 minutes")

	_, err := f.coord.Launch(ctx, c, f.appID, StreamOverride{})
	if !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("launch onto a full GPU: err = %v, want capacity_exhausted (the attempt is still refused)", err)
	}
	if got := readRow(t, f.pool, older); got.State != "stopping" || got.StopReason != StopReasonParkedEvicted {
		t.Errorf("oldest parked %+v, want stopping / parked_evicted", got)
	}
	if got := readRow(t, f.pool, newer); got.State != "running" {
		t.Errorf("newer parked %+v, want untouched: one eviction per refused attempt", got)
	}
	// The retry gets the room once the evicted session is stopped.
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'stopped', ended_at = now() WHERE id = $1::uuid`, older))
	if _, err := f.coord.Launch(ctx, c, f.appID, StreamOverride{}); err != nil {
		t.Fatalf("retry after the eviction: %v", err)
	}
}

func TestHeartbeatNeverEvicts(t *testing.T) {
	f := newWarmFixture(t, 2)
	other := warmUser(t, f.pool, "other")
	theirs := insertParked(t, f.pool, other, f.steam, f.hostID, f.gpuID, 2, "10 minutes")
	f.coord.SteamWarmHeartbeat(context.Background(), f.userID, false, f.deviceID)
	if got := readRow(t, f.pool, theirs); got.State != "running" {
		t.Errorf("another user's parked session %+v, want untouched by a heartbeat", got)
	}
	if f.parkedID(t) != "" {
		t.Error("heartbeat parked with no room")
	}
}

func TestNonSteamLaunchSupersedesAndSuppresses(t *testing.T) {
	for _, rewarm := range []string{"library_open", "next_steam_launch"} {
		t.Run(rewarm, func(t *testing.T) {
			f := newWarmFixture(t, 4)
			ctx := context.Background()
			setWarm(t, f.pool, true, rewarm, 10)
			k := f.park(t)

			if _, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.appID, DeviceID: f.deviceID}); err != nil {
				t.Fatalf("non-Steam launch: %v", err)
			}
			if got := readRow(t, f.pool, k); got.State != "stopping" || got.StopReason != StopReasonParkedSuperseded {
				t.Errorf("parked row %+v, want stopping / parked_superseded", got)
			}
			var suppressed bool
			must(t, f.pool.QueryRow(ctx, `SELECT steam_warm_suppressed FROM users WHERE id = $1::uuid`, f.userID).Scan(&suppressed))
			if suppressed != (rewarm == "next_steam_launch") {
				t.Errorf("steam_warm_suppressed = %v under %s", suppressed, rewarm)
			}

			// The next Steam launch clears it.
			f.ended(t, k)
			if _, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID}); err != nil {
				t.Fatalf("Steam launch: %v", err)
			}
			must(t, f.pool.QueryRow(ctx, `SELECT steam_warm_suppressed FROM users WHERE id = $1::uuid`, f.userID).Scan(&suppressed))
			if suppressed {
				t.Error("a Steam launch did not clear steam_warm_suppressed")
			}
		})
	}
}

func TestOwnParkedSessionInTheWayIsSupersededNotEvicted(t *testing.T) {
	f := newWarmFixture(t, 2)
	setWarm(t, f.pool, true, "next_steam_launch", 10)
	k := insertParked(t, f.pool, f.userID, f.steam, f.hostID, f.gpuID, 2, "10 minutes")
	must(t, exec(t, f.pool, `UPDATE gpus SET encode_slots_total = 2`))

	// The ordinary app needs a slot the caller's own parked session holds.
	if _, err := f.coord.Launch(context.Background(), f.userID, f.appID, StreamOverride{}); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("err = %v, want capacity_exhausted", err)
	}
	if got := readRow(t, f.pool, k); got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("own parked row %+v, want parked_superseded", got)
	}
	var suppressed bool
	must(t, f.pool.QueryRow(context.Background(), `SELECT steam_warm_suppressed FROM users WHERE id = $1::uuid`, f.userID).Scan(&suppressed))
	if !suppressed {
		t.Error("superseding by eviction did not apply next_steam_launch")
	}
}

func TestSwapOutsideTheFamilySupersedes(t *testing.T) {
	f := newWarmFixture(t, 8)
	ctx := context.Background()
	sess, err := f.store.ScheduleAndCreate(ctx, launchParams(f.seedIDs))
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	f.running(t, sess.ID)
	k := insertParked(t, f.pool, f.userID, f.steam, f.hostID, f.gpuID, 2, "10 minutes")
	var other string
	must(t, f.pool.QueryRow(ctx, `INSERT INTO apps (name, default_encode_slots) VALUES ('other', 1)
		RETURNING id::text`).Scan(&other))
	entitleAll(t, f.pool, other)

	if _, err := f.coord.Swap(ctx, sess.ID, other); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := readRow(t, f.pool, k); got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("parked row %+v, want parked_superseded after an accepted swap", got)
	}
}

// --- expiry and drains -------------------------------------------------------------

func TestExpireParkedStopsPastDeadlineThenEverythingWhenOff(t *testing.T) {
	f := newWarmFixture(t, 8)
	ctx := context.Background()
	u2 := warmUser(t, f.pool, "u2")
	past := insertParked(t, f.pool, f.userID, f.steam, f.hostID, f.gpuID, 2, "-1 minute")
	future := insertParked(t, f.pool, u2, f.steam, f.hostID, f.gpuID, 2, "10 minutes")

	if n, err := f.coord.ExpireParked(ctx); err != nil || n != 1 {
		t.Fatalf("expire = %d, %v; want 1", n, err)
	}
	if got := readRow(t, f.pool, past); got.State != "stopping" || got.StopReason != StopReasonParkedExpired {
		t.Errorf("past-deadline row %+v, want stopping / parked_expired", got)
	}
	if f.disp.stopReason(past) != StopReasonParkedExpired {
		t.Error("no parked_expired session_stop sent")
	}
	if got := readRow(t, f.pool, future); got.State != "running" {
		t.Errorf("in-deadline row %+v, want untouched", got)
	}

	setWarm(t, f.pool, false, "library_open", 10)
	if n, err := f.coord.ExpireParked(ctx); err != nil || n != 1 {
		t.Fatalf("expire with the switch off = %d, %v; want 1", n, err)
	}
	if got := readRow(t, f.pool, future); got.StopReason != StopReasonParkedExpired {
		t.Errorf("row %+v, want stopped while warm Steam is off", got)
	}
}

func TestGracefulDrainStopsOnlyParkedSessions(t *testing.T) {
	f := newWarmFixture(t, 8)
	ctx := context.Background()
	play, err := f.store.ScheduleAndCreate(ctx, launchParams(f.seedIDs))
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	f.running(t, play.ID)
	k := insertParked(t, f.pool, warmUser(t, f.pool, "p"), f.steam, f.hostID, f.gpuID, 2, "10 minutes")

	if _, err := f.coord.DrainHost(ctx, f.hostID, false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := readRow(t, f.pool, k); got.State != "stopping" || got.StopReason != "host_draining" {
		t.Errorf("parked row %+v, want stopping / host_draining", got)
	}
	if got := readRow(t, f.pool, play.ID); got.State != "running" {
		t.Errorf("play row %+v, want left to end on its own", got)
	}
}

// --- what leaves a parked session out ----------------------------------------------

func TestUserListAndQuotaLeaveParkedSessionsOut(t *testing.T) {
	f := newWarmFixture(t, 8)
	ctx := context.Background()
	k := insertParked(t, f.pool, f.userID, f.steam, f.hostID, f.gpuID, 2, "10 minutes")

	list, _, err := f.store.ListByUser(ctx, f.userID, "", 50)
	if err != nil || len(list) != 0 {
		t.Fatalf("ListByUser = %d items, %v; want the parked session left out", len(list), err)
	}
	must(t, exec(t, f.pool, `UPDATE users SET max_concurrent_sessions = 1`))
	if _, err := f.store.ScheduleAndCreate(ctx, launchParams(f.seedIDs)); err != nil {
		t.Fatalf("launch at quota 1 beside a parked session: %v (a parked session is never counted)", err)
	}
	admin, _, err := f.store.ListAll(ctx, "", 50, AdminStateActive)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	found := false
	for _, r := range admin {
		found = found || (r.ID == k && r.Kind == KindParked)
	}
	if !found {
		t.Error("admin list does not show the parked session with kind parked")
	}
}

func TestHomeInUseNeverNamesAParkedHolder(t *testing.T) {
	f := newWarmFixture(t, 8)
	k := insertParked(t, f.pool, f.userID, f.steam, f.hostID, f.gpuID, 2, "10 minutes")
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'stopping' WHERE id = $1::uuid`, k))

	conflict, err := f.store.HasLiveUserAppSession(context.Background(), f.userID, f.steam, "")
	if err != nil || conflict == nil {
		t.Fatalf("conflict = %v, %v; want the parked holder to block the home", conflict, err)
	}
	if conflict.SessionID != "" || !conflict.Stopping {
		t.Errorf("conflict = %+v, want no session id and stopping", conflict)
	}

	// The launch guard names it to LaunchByProfile only, which answers the 503.
	p := launchParams(f.seedIDs)
	p.AppID, p.HomeAppID, p.ManagedHome, p.NeedEncodeSlots = f.steam, f.steam, true, 2
	_, err = f.store.ScheduleAndCreate(context.Background(), p)
	var inUse *HomeInUseError
	if !errors.As(err, &inUse) || inUse.SessionID != "" || inUse.parkedID != k {
		t.Fatalf("launch guard err = %v (%+v), want an unnamed holder carrying the parked id", err, inUse)
	}
}

// --- shader progress -----------------------------------------------------------------

func TestLaunchProgressStoredClearedAndServed(t *testing.T) {
	f := newWarmFixture(t, 4)
	ctx := context.Background()
	sess, err := f.store.ScheduleAndCreate(ctx, launchParams(f.seedIDs))
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	f.running(t, sess.ID)
	report := func(detail, progress string) {
		m := agentws.SessionStateMsg{SessionID: sess.ID, State: "running", Detail: detail}
		if progress != "" {
			m.LaunchProgress = json.RawMessage(progress)
		}
		f.coord.AgentState(ctx, f.hostID, m)
	}
	// Compared decoded: jsonb normalizes the stored text.
	stored := func() string {
		got, err := f.store.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.LaunchProgress == nil {
			return ""
		}
		var v struct {
			Step    string `json:"step"`
			Percent int    `json:"percent"`
		}
		must(t, json.Unmarshal(got.LaunchProgress, &v))
		return fmt.Sprintf("%s:%d", v.Step, v.Percent)
	}

	report(appDetailBooting, `{"step":"shaders","percent":42}`)
	if got := stored(); got != "shaders:42" {
		t.Fatalf("launch_progress = %s, want the accepted value", got)
	}
	for _, bad := range []string{
		`{"step":"compile","percent":1}`, `{"step":"shaders","percent":101}`,
		`{"step":"shaders","percent":4.5}`, `{"step":"shaders","percent":null}`,
		`{"step":"shaders","percent":5,"extra":1}`, `"shaders"`,
	} {
		report(appDetailBooting, bad)
		if got := stored(); got != "shaders:42" {
			t.Errorf("after %s launch_progress = %s, want the bad value dropped", bad, got)
		}
	}
	got, _ := f.store.Get(ctx, sess.ID)
	body, _ := json.Marshal(toSessionResp(got))
	var served map[string]any
	_ = json.Unmarshal(body, &served)
	if p, ok := served["launch_progress"].(map[string]any); !ok || p["percent"] != float64(42) || served["kind"] != "play" {
		t.Errorf("served launch_progress=%v kind=%v", served["launch_progress"], served["kind"])
	}

	report(appDetailPresented, "")
	if got := stored(); got != "" {
		t.Errorf("launch_progress = %s after app presented, want cleared", got)
	}
	report(appDetailPresented, `{"step":"shaders","percent":7}`)
	if got := stored(); got != "" {
		t.Errorf("launch_progress = %s stored outside app booting", got)
	}
	body, _ = json.Marshal(toSessionResp(Session{Kind: KindPlay}))
	_ = json.Unmarshal(body, &served)
	if v, ok := served["launch_progress"]; !ok || v != nil {
		t.Errorf("launch_progress serialized as %v (present=%v), want null", v, ok)
	}
}

// --- Steam dialogs -----------------------------------------------------------------

func TestSteamDialogsReachTheSteamAppOnlyAsTheFixedValue(t *testing.T) {
	f := newWarmFixture(t, 8)
	ctx := context.Background()
	must(t, exec(t, f.pool, `UPDATE apps SET runtime_spec = '{"image":"steam:1","env":{"QUASAR_STEAM_DIALOGS":"anything"}}'
		WHERE id = $1::uuid`, f.steam))

	launch := func() map[string]string {
		res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID})
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
		env := envOf(t, f.disp.assignFor(t, res.Session.ID).App)
		f.ended(t, res.Session.ID)
		return env
	}
	if v, ok := launch()["QUASAR_STEAM_DIALOGS"]; ok {
		t.Errorf("QUASAR_STEAM_DIALOGS = %q with steam_dialogs show, want the key removed", v)
	}
	if _, err := f.store.SetSteamDialogs(ctx, f.userID, "dismiss_safe"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if v := launch()["QUASAR_STEAM_DIALOGS"]; v != "dismiss_safe" {
		t.Errorf("QUASAR_STEAM_DIALOGS = %q, want dismiss_safe", v)
	}
	// A non-Steam app's spec is never touched.
	must(t, exec(t, f.pool, `UPDATE apps SET runtime_spec = '{"env":{"QUASAR_STEAM_DIALOGS":"x"}}' WHERE id = $1::uuid`, f.appID))
	res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.appID, DeviceID: f.deviceID})
	if err != nil {
		t.Fatalf("launch ordinary app: %v", err)
	}
	if v := envOf(t, f.disp.assignFor(t, res.Session.ID).App)["QUASAR_STEAM_DIALOGS"]; v != "x" {
		t.Errorf("ordinary app env = %q, want its own spec untouched", v)
	}
}

// --- the in-place swap -------------------------------------------------------------

func TestTileToTileSwapAsksForInPlace(t *testing.T) {
	cases := []struct {
		name  string
		parks bool
		want  bool
	}{{"capable agent", true, true}, {"older agent", false, false}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newWarmFixture(t, 8)
			ctx := context.Background()
			f.disp.setParks(c.parks)
			hades := seedTile(t, f.pool, f.steam, "Hades", "1145360")
			celeste := seedTile(t, f.pool, f.steam, "Celeste", "504230")
			res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: hades, DeviceID: f.deviceID})
			if err != nil {
				t.Fatalf("launch: %v", err)
			}
			f.disp.waitFor(t, "start")
			f.running(t, res.Session.ID)

			if _, err := f.coord.Swap(ctx, res.Session.ID, celeste); err != nil {
				t.Fatalf("swap: %v", err)
			}
			f.disp.waitFor(t, "swap")
			f.disp.mu.Lock()
			got := f.disp.swaps[len(f.disp.swaps)-1].InPlace
			f.disp.mu.Unlock()
			if got != c.want {
				t.Errorf("in_place = %v, want %v", got, c.want)
			}
		})
	}
}
