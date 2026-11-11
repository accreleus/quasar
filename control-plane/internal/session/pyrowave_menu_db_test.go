package session

// The pyrowave block on GET /v1/me/profiles (amendment 58, "Launch options"), end to end
// against the real schema, plus the pure check that the block and the launch decide alike.
// The DB cases need Postgres (make test-db).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// pwMenuCase is one 1080p60 launch profile, one placed GPU codec set and one device probe.
type pwMenuCase struct {
	mode, transport string
	instanceDefault string // instance_settings.default_transport when set
	gpuCodecs       string
	noProbe         bool
	probe           map[string]any // merged over a probe that passes every check
}

func (c pwMenuCase) caps() string {
	caps := map[string]any{
		"measured_at":       time.Now().UTC().Format(time.RFC3339),
		"bandwidth_kbps":    900000,
		"rtt_ms":            0.6,
		"max_decode_height": 2160,
		"webgpu":            map[string]any{"available": true},
		"webtransport":      map[string]any{"available": true, "datagrams": true},
		"codecs":            map[string]any{"h264": true},
	}
	for k, v := range c.probe {
		if v == nil {
			delete(caps, k)
		} else {
			caps[k] = v
		}
	}
	b, _ := json.Marshal(caps)
	return string(b)
}

// pyrowaveMenu returns the raw "pyrowave" object of the 1080p60 launch profile.
func pyrowaveMenu(t *testing.T, c pwMenuCase) map[string]any {
	t.Helper()
	pool := testDB(t)
	s := seed(t, pool, 4)
	srv, authSvc, _ := newMetricsServer(t, pool)
	u, err := authSvc.Register(t.Context(), "pw-menu@test.local", "pw-menu", "quasar-fixture-pw-08")
	must(t, err)
	tok := loginTok(t, authSvc, "pw-menu@test.local", "quasar-fixture-pw-08")

	if c.transport == "" {
		c.transport = "webtransport_opt_in"
	}
	if c.gpuCodecs == "" {
		c.gpuCodecs = `["h264","pyrowave"]`
	}
	must(t, exec(t, pool, `UPDATE launch_profiles SET pyrowave_mode = $1, transport = $2 WHERE id = '1080p60'`,
		c.mode, c.transport))
	if c.instanceDefault != "" {
		must(t, exec(t, pool, `INSERT INTO instance_settings (id, default_transport) VALUES (true, $1)
			ON CONFLICT (id) DO UPDATE SET default_transport = EXCLUDED.default_transport`, c.instanceDefault))
		t.Cleanup(func() { _, _ = pool.Exec(t.Context(), `UPDATE instance_settings SET default_transport = 'webrtc'`) })
	}
	setGPUCodecsRaw(t, pool, s.hostID, 0, c.gpuCodecs)
	if !c.noProbe {
		must(t, exec(t, pool, `INSERT INTO user_devices (user_id, device_key, capabilities)
			VALUES ($1::uuid, 'pw-menu-device', $2::jsonb)`, u.ID, c.caps()))
	}

	resp := doJSON(t, "GET", srv.URL+"/v1/me/profiles?app_id="+s.appID, tok, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me/profiles = %d", resp.StatusCode)
	}
	var body struct {
		Profiles []map[string]any `json:"profiles"`
	}
	must(t, json.NewDecoder(resp.Body).Decode(&body))
	for _, p := range body.Profiles {
		if p["id"] == "1080p60" {
			pw, ok := p["pyrowave"].(map[string]any)
			if !ok {
				t.Fatalf("1080p60 carries no pyrowave object: %v", p)
			}
			return pw
		}
	}
	t.Fatalf("no 1080p60 launch profile in %v", body.Profiles)
	return nil
}

func TestProfilesPyroWaveBlock(t *testing.T) {
	tests := []struct {
		name       string
		c          pwMenuCase
		available  bool
		reason     any // nil or the code
		auto       bool
		autoReason any
	}{
		{"off", pwMenuCase{mode: "off"}, false, "profile_disallows", false, nil},
		{"allow on a webrtc profile", pwMenuCase{mode: "allow", transport: "webrtc"}, false, "profile_disallows", false, nil},
		{"allow, default transport and the instance default is webrtc",
			pwMenuCase{mode: "allow", transport: "default", instanceDefault: "webrtc"}, false, "profile_disallows", false, nil},
		{"allow, default transport and the instance default is webtransport",
			pwMenuCase{mode: "allow", transport: "default", instanceDefault: "webtransport"}, true, nil, false, nil},
		{"allow, available", pwMenuCase{mode: "allow"}, true, nil, false, nil},
		{"allow, no GPU lists pyrowave", pwMenuCase{mode: "allow", gpuCodecs: `["h264"]`}, false, "host_no_pyrowave", false, nil},
		{"allow, no webgpu", pwMenuCase{mode: "allow", probe: map[string]any{"webgpu": nil}}, false, "device_no_webgpu", false, nil},
		{"allow, no probe at all", pwMenuCase{mode: "allow", noProbe: true}, false, "device_no_webgpu", false, nil},
		{"allow, no webtransport", pwMenuCase{mode: "allow", probe: map[string]any{"webtransport": nil}}, false, "device_no_webtransport", false, nil},
		{"allow, webtransport without datagrams", pwMenuCase{mode: "allow",
			probe: map[string]any{"webtransport": map[string]any{"available": true, "datagrams": false}}}, false, "device_no_webtransport", false, nil},
		{"prefer, Auto picks it", pwMenuCase{mode: "prefer"}, true, nil, true, nil},
		{"prefer, no figures", pwMenuCase{mode: "prefer", probe: map[string]any{"bandwidth_kbps": nil, "rtt_ms": nil}}, true, nil, false, "no_measurement"},
		{"prefer, figures older than the window", pwMenuCase{mode: "prefer",
			probe: map[string]any{"measured_at": time.Now().Add(-11 * time.Minute).UTC().Format(time.RFC3339)}}, true, nil, false, "no_measurement"},
		{"prefer, rtt too high", pwMenuCase{mode: "prefer", probe: map[string]any{"rtt_ms": 20}}, true, nil, false, "rtt_too_high"},
		{"prefer, link too slow", pwMenuCase{mode: "prefer", probe: map[string]any{"bandwidth_kbps": 100000}}, true, nil, false, "link_too_slow"},
		{"prefer, unavailable reports no Auto", pwMenuCase{mode: "prefer", probe: map[string]any{"webgpu": nil}}, false, "device_no_webgpu", false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pyrowaveMenu(t, tc.c)
			want := map[string]any{
				"mode": tc.c.mode, "available": tc.available, "reason": tc.reason,
				"auto": tc.auto, "auto_reason": tc.autoReason,
			}
			if fmt.Sprint(got) != fmt.Sprint(want) || len(got) != len(want) {
				t.Errorf("pyrowave = %v, want %v", got, want)
			}
			for _, k := range []string{"mode", "available", "reason", "auto", "auto_reason"} {
				if _, ok := got[k]; !ok {
					t.Errorf("required key %q missing (a null must be serialized): %v", k, got)
				}
			}
		})
	}
}
