package session

// Ultra-low latency (PyroWave) at launch, end to end against the 0114 schema (amendment 58):
// the plan's grant reaches the agent and the session, the per-GPU ceiling refuses the next
// launch without failing it, and the agent's codec.switched moves the session's record.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

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
	if r := switched("h264", "client_decode_error"); r.Active || !r.Used || r.Wanted || !r.Allowed {
		t.Errorf("after a fallback: %+v", r)
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
