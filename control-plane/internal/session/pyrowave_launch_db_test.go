package session

// Ultra-low latency (PyroWave) at launch, end to end against the 0115 schema (amendment 58):
// the plan's grant reaches the agent and the session, the per-GPU ceiling refuses the next
// launch without failing it, and the agent's codec.switched moves the session's record.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// newPyroWaveFixture: the WebTransport fixture on an opt-in profile that allows PyroWave, a GPU
// that passed its PyroWave probe, and a device that reports WebGPU.
func newPyroWaveFixture(t *testing.T) *wtFixture {
	t.Helper()
	f := newWTFixture(t, "webtransport_opt_in")
	must(t, exec(t, f.pool, `UPDATE launch_profiles SET pyrowave_mode = 'allow' WHERE id = '1080p60'`))
	must(t, exec(t, f.pool, `UPDATE gpus SET codecs = '["h264","pyrowave"]' WHERE host_id = $1::uuid`, f.hostID))
	must(t, exec(t, f.pool, `UPDATE user_devices SET capabilities = capabilities || '{"webgpu":{"available":true}}'
		WHERE user_id = $1::uuid`, f.userID))
	return f
}

func pyrowaveOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	p, ok := body["session"].(map[string]any)["pyrowave"].(map[string]any)
	if !ok {
		t.Fatalf("session.pyrowave missing: %v", body["session"])
	}
	return p
}

func TestLaunchGrantsPyroWaveAndTheCeilingRefusesTheNext(t *testing.T) {
	f := newPyroWaveFixture(t)
	ctx := context.Background()

	status, body := f.launch(t, map[string]any{"pyrowave": true})
	if status != http.StatusCreated {
		t.Fatalf("launch: %d %v", status, body)
	}
	f.waitDispatched(t)
	if body["session"].(map[string]any)["transport"] != "webtransport" {
		t.Errorf("a session that wants PyroWave wants WebTransport: %v", body["session"])
	}
	pw := pyrowaveOf(t, body)
	if pw["allowed"] != true || pw["wanted"] != true || pw["choice"] != "explicit" || pw["reason"] != nil ||
		pw["target_kbps"] != float64(200_000) || pw["active"] != false || pw["used"] != false {
		t.Errorf("session.pyrowave = %v", pw)
	}
	if got := f.disp.lastAssignCmd().PyroWave; got == nil ||
		*got != (agentws.PyroWaveGrant{Wanted: true, TargetKbps: 200_000, FloorKbps: 100_000, MaxRTTMs: 5}) {
		t.Errorf("session_assign.pyrowave = %+v", got)
	}
	first := body["session"].(map[string]any)["id"].(string)
	var slot bool
	must(t, f.pool.QueryRow(ctx, `SELECT pyrowave_slot FROM sessions WHERE id = $1::uuid`, first).Scan(&slot))
	if !slot {
		t.Fatal("the granted session holds no PyroWave slot")
	}

	// pyrowave_max_per_gpu is 1: the next pick on the same GPU gets its standard codec.
	status, body = f.launch(t, map[string]any{"pyrowave": true})
	if status != http.StatusCreated {
		t.Fatalf("second launch: %d %v", status, body)
	}
	pw = pyrowaveOf(t, body)
	if pw["allowed"] != false || pw["reason"] != "gpu_pyrowave_busy" || pw["choice"] != nil || pw["target_kbps"] != nil {
		t.Errorf("second session.pyrowave = %v", pw)
	}
	second := body["session"].(map[string]any)["id"].(string)
	must(t, f.pool.QueryRow(ctx, `SELECT pyrowave_slot FROM sessions WHERE id = $1::uuid`, second).Scan(&slot))
	if slot {
		t.Error("a refused session holds a slot")
	}
	f.waitDispatched(t)

	// The agent's switches move active, used, switch_reason and wanted.
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'running' WHERE id = $1::uuid`, first))
	switched := func(to, reason string) PyroWaveRecord {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"from": nil, "to": to, "reason": reason, "config_id": 1})
		f.coord.AgentTraceEvent(ctx, f.hostID, agentws.SessionTraceEventMsg{
			Type: "session_trace_event", SessionID: first, Event: "codec.switched", Payload: payload,
		})
		got, err := f.store.Get(ctx, first)
		if err != nil || got.PyroWave == nil {
			t.Fatalf("read back: %+v (%v)", got.PyroWave, err)
		}
		return *got.PyroWave
	}
	if r := switched("pyrowave", "launch"); !r.Active || !r.Used || !r.Wanted || r.SwitchReason == nil || *r.SwitchReason != "launch" {
		t.Errorf("after launch: %+v", r)
	}
	slotOf := func(id string) bool {
		t.Helper()
		var held bool
		must(t, f.pool.QueryRow(ctx, `SELECT pyrowave_slot FROM sessions WHERE id = $1::uuid`, id).Scan(&held))
		return held
	}
	if r := switched("h264", "client_no_decoder"); r.Active || !r.Wanted || !slotOf(first) {
		t.Errorf("a connection that cannot decode keeps the session wanting, and its slot: %+v", r)
	}
	if r := switched("h264", "client_decode_error"); r.Active || !r.Used || r.Wanted || !r.Allowed {
		t.Errorf("after a fallback: %+v", r)
	}
	// Seat A: a step-down never switches on again, so it gives the GPU's slot back.
	if slotOf(first) {
		t.Fatal("the stepped-down session still holds the GPU's only PyroWave slot")
	}
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'stopped', ended_at = now() WHERE id = $1::uuid`, second))
	status, body = f.launch(t, map[string]any{"pyrowave": true})
	if status != http.StatusCreated {
		t.Fatalf("third launch: %d %v", status, body)
	}
	if pw := pyrowaveOf(t, body); pw["allowed"] != true || pw["wanted"] != true {
		t.Errorf("the next launch after a step-down: %v, want the freed slot", pw)
	}
}

// N3: a session that ends gives its slot back by leaving the active set.
func TestAStoppedSessionFreesItsPyroWaveSlot(t *testing.T) {
	f := newPyroWaveFixture(t)
	_, body := f.launch(t, map[string]any{"pyrowave": true})
	first := body["session"].(map[string]any)["id"].(string)
	if pw := pyrowaveOf(t, body); pw["allowed"] != true {
		t.Fatalf("first launch: %v", pw)
	}
	f.waitDispatched(t)
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'stopped', ended_at = now() WHERE id = $1::uuid`, first))
	_, body = f.launch(t, map[string]any{"pyrowave": true})
	if pw := pyrowaveOf(t, body); pw["allowed"] != true || pw["wanted"] != true {
		t.Errorf("after the first stopped: %v, want the slot", pw)
	}
}

func TestLaunchOnAnOffProfileTakesNoPyroWaveSlot(t *testing.T) {
	f := newPyroWaveFixture(t)
	must(t, exec(t, f.pool, `UPDATE launch_profiles SET pyrowave_mode = 'off' WHERE id = '1080p60'`))
	status, body := f.launch(t, map[string]any{"pyrowave": true})
	if status != http.StatusCreated {
		t.Fatalf("launch: %d %v", status, body)
	}
	f.waitDispatched(t)
	if pw := pyrowaveOf(t, body); pw["allowed"] != false || pw["reason"] != "profile_disallows" {
		t.Errorf("session.pyrowave = %v", pw)
	}
	if f.disp.lastAssignCmd().PyroWave != nil {
		t.Error("an off profile sent a grant")
	}
	var slot bool
	must(t, f.pool.QueryRow(context.Background(), `SELECT pyrowave_slot FROM sessions WHERE id = $1::uuid`,
		body["session"].(map[string]any)["id"]).Scan(&slot))
	if slot {
		t.Error("an off profile took a slot")
	}
}

// prepareWarmForPyroWave makes the warm fixture's every launch profile an opt-in WebTransport
// profile that allows PyroWave, its host a WebTransport endpoint with a PyroWave GPU, and its
// device one that proves WebTransport, WebCodecs and WebGPU. The park and the play resolve the
// same chain, so the play claims the park.
func prepareWarmForPyroWave(t *testing.T, f *warmFixture) {
	t.Helper()
	must(t, exec(t, f.pool, `UPDATE launch_profiles SET transport = 'webtransport_opt_in', pyrowave_mode = 'allow'`))
	must(t, exec(t, f.pool, `UPDATE hosts SET transports = '["webrtc","webtransport"]', wt_port = 4433,
		wt_addresses = '["192.0.2.10"]',
		wt_certificates = jsonb_build_array(jsonb_build_object('sha256', $2::text,
			'not_before', now() - interval '1 day', 'not_after', now() + interval '10 days'))
		WHERE id = $1::uuid`, f.hostID, wtCertA))
	must(t, exec(t, f.pool, `UPDATE gpus SET codecs = '["h264","pyrowave"]' WHERE host_id = $1::uuid`, f.hostID))
	must(t, exec(t, f.pool, `UPDATE user_devices SET capabilities = $2::jsonb WHERE id = $1::uuid`, f.deviceID,
		`{"measured_at":"`+time.Now().UTC().Format(time.RFC3339)+`","codecs":{"h264":true},
		  "webtransport":{"available":true,"datagrams":true},"webgpu":{"available":true},
		  "webcodecs":{"h264":true,"opus_decode":true}}`))
}

// Seat B S1: a Hot Loading claim plans like any launch and takes the parked GPU's slot.
func TestAHotLoadingClaimGetsPyroWaveWithTheSlot(t *testing.T) {
	f := newWarmFixture(t, 4)
	prepareWarmForPyroWave(t, f)
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	k := f.park(t)

	res, err := f.coord.LaunchByProfile(context.Background(), f.userID,
		LaunchParams{AppID: tile, DeviceID: f.deviceID, PyroWave: true})
	if err != nil {
		t.Fatalf("claim launch: %v", err)
	}
	play := res.Session
	if play.AdoptSessionID != k {
		t.Fatalf("the play session adopts %q, want the park %s", play.AdoptSessionID, k)
	}
	got, err := f.store.Get(context.Background(), play.ID)
	if err != nil || got.PyroWave == nil || !got.PyroWave.Allowed || !got.PyroWave.Wanted || !got.PyroWaveSlot ||
		got.Transport != "webtransport" {
		t.Fatalf("claimed session: transport %q slot %v record %+v (%v)", got.Transport, got.PyroWaveSlot, got.PyroWave, err)
	}
	if a := f.disp.assignFor(t, play.ID); a.PyroWave == nil || !a.PyroWave.Wanted {
		t.Errorf("assign.pyrowave = %+v", a.PyroWave)
	}
}

// A claim whose INSERT finds the GPU's only slot taken is planned again as busy, onto WebRTC.
func TestAHotLoadingClaimWithTheSlotTakenIsBusy(t *testing.T) {
	f := newWarmFixture(t, 4)
	prepareWarmForPyroWave(t, f)
	tile := seedTile(t, f.pool, f.steam, "Hades", "1145360")
	k := f.park(t)
	var other string
	must(t, f.pool.QueryRow(context.Background(), `INSERT INTO users (email, username, password_hash)
		VALUES ('pw-other@test.local','pwother','x') RETURNING id::text`).Scan(&other))
	must(t, exec(t, f.pool, `INSERT INTO sessions (user_id, app_id, host_id, gpu_id, state, width, height, fps,
		bitrate_kbps, pyrowave_slot)
		SELECT $1::uuid, $2::uuid, g.host_id, g.id, 'running', 1280, 720, 60, 6000, true
		FROM gpus g WHERE g.host_id = $3::uuid`, other, f.steam, f.hostID))

	res, err := f.coord.LaunchByProfile(context.Background(), f.userID,
		LaunchParams{AppID: tile, DeviceID: f.deviceID, PyroWave: true})
	if err != nil {
		t.Fatalf("claim launch: %v", err)
	}
	if res.Session.AdoptSessionID != k {
		t.Fatalf("the play session adopts %q, want the park %s", res.Session.AdoptSessionID, k)
	}
	got, err := f.store.Get(context.Background(), res.Session.ID)
	if err != nil || got.PyroWave == nil || got.PyroWave.Allowed || got.PyroWave.Reason == nil ||
		*got.PyroWave.Reason != "gpu_pyrowave_busy" || got.PyroWaveSlot || got.Transport != "webrtc" {
		t.Fatalf("busy claim: transport %q slot %v record %+v (%v)", got.Transport, got.PyroWaveSlot, got.PyroWave, err)
	}
	if a := f.disp.assignFor(t, res.Session.ID); a.PyroWave != nil {
		t.Errorf("a busy claim was granted: %+v", a.PyroWave)
	}
}
