package console

import "testing"

func okApp(string) (bool, error) { return true, nil }
func noApp(string) (bool, error) { return false, nil }

func okUser(string) (bool, error) { return true, nil }
func noUser(string) (bool, error) { return false, nil }

func TestValidatePatch(t *testing.T) {
	reported := Capabilities{
		Connectors: []string{"DP-4", "HDMI-A-1"},
		Outputs: []DRMOutput{{ID: "card0:DP-4", Connector: "DP-4", Connected: true, Modes: []DRMMode{{
			Width: 3840, Height: 2160, RefreshMillihz: 119879,
		}}}},
		AudioSinks:   []AudioSink{{ID: "hw:1,3", Label: "GPU HDA"}},
		InputDevices: []InputDevicePath{{Path: "/dev/input/event4", Label: "Keyboard"}},
	}
	empty := EmptyCapabilities()

	cases := []struct {
		name    string
		patch   map[string]any
		caps    Capabilities
		app     func(string) (bool, error)
		user    func(string) (bool, error)
		wantErr bool
	}{
		{"unknown key rejected", map[string]any{"bogus": 1}, empty, okApp, okUser, true},
		{"weston compositor ok", map[string]any{"compositor": "weston"}, empty, okApp, okUser, false},
		{"cage compositor rejected", map[string]any{"compositor": "cage"}, empty, okApp, okUser, true},
		{"compositor enum bad", map[string]any{"compositor": "gnome"}, empty, okApp, okUser, true},
		{"bool type checked", map[string]any{"enabled": "yes"}, empty, okApp, okUser, true},
		{"auto connector accepted", map[string]any{"connector": "auto"}, empty, okApp, okUser, false},
		{"explicit connector rejected when caps empty", map[string]any{"connector": "DP-4"}, empty, okApp, okUser, true},
		{"connector rejected when reported and absent", map[string]any{"connector": "DP-9"}, reported, okApp, okUser, true},
		{"reported explicit connector still rejected", map[string]any{"connector": "DP-4"}, reported, okApp, okUser, true},
		{"reported output accepted", map[string]any{"output_id": "card0:DP-4"}, reported, okApp, okUser, false},
		{"unknown output rejected", map[string]any{"output_id": "card9:DP-9"}, reported, okApp, okUser, true},
		{"reported exact mode accepted", map[string]any{"mode": map[string]any{"width": float64(3840), "height": float64(2160), "refresh_millihz": float64(119879)}}, reported, okApp, okUser, false},
		{"unreported mode rejected", map[string]any{"mode": map[string]any{"width": float64(3840), "height": float64(2160), "refresh_millihz": float64(120000)}}, reported, okApp, okUser, true},
		{"dual stream true accepted", map[string]any{"stream": true, "stream_audio": true}, empty, okApp, okUser, false},
		{"local-only stream false accepted", map[string]any{"stream": false}, empty, okApp, okUser, false},
		{"stream audio false accepted", map[string]any{"stream_audio": false}, empty, okApp, okUser, false},
		{"fullscreen true accepted", map[string]any{"fullscreen": true}, empty, okApp, okUser, false},
		{"windowed fullscreen false rejected", map[string]any{"fullscreen": false}, empty, okApp, okUser, true},
		{"null clears unsupported topology overrides", map[string]any{"connector": nil, "compositor": nil, "stream": nil, "stream_audio": nil, "fullscreen": nil}, empty, okApp, okUser, false},
		{"audio_output null allowed (quiet)", map[string]any{"audio_output": nil}, empty, okApp, okUser, false},
		{"audio_output allowed when caps empty", map[string]any{"audio_output": "hw:9,9"}, empty, okApp, okUser, false},
		{"audio_output rejected when reported and absent", map[string]any{"audio_output": "hw:9,9"}, reported, okApp, okUser, true},
		{"audio_output auto always ok", map[string]any{"audio_output": "auto"}, reported, okApp, okUser, false},
		{"input_devices auto ok", map[string]any{"input_devices": "auto"}, reported, okApp, okUser, false},
		{"input_devices list rejected when reported and absent", map[string]any{"input_devices": []any{"/dev/input/event9"}}, reported, okApp, okUser, true},
		{"input_devices list allowed when caps empty", map[string]any{"input_devices": []any{"/dev/input/event9"}}, empty, okApp, okUser, false},
		{"default_app null clears", map[string]any{"default_app": nil}, empty, okApp, okUser, false},
		{"default_app unknown rejected", map[string]any{"default_app": "id"}, empty, noApp, okUser, true},
		{"default_app known ok", map[string]any{"default_app": "id"}, empty, okApp, okUser, false},
		{"default_user null clears", map[string]any{"default_user": nil}, empty, okApp, okUser, false},
		{"default_user unknown rejected", map[string]any{"default_user": "id"}, empty, okApp, noUser, true},
		{"default_user known ok", map[string]any{"default_user": "id"}, empty, okApp, okUser, false},
		{"enabled true + audio null valid (quiet console)", map[string]any{"enabled": true, "audio_output": nil}, empty, okApp, okUser, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePatch(tc.patch, tc.caps, tc.app, tc.user)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidatePatch(%v) err=%v, wantErr=%v", tc.patch, err, tc.wantErr)
			}
		})
	}
}

func TestValidateOutputSelection(t *testing.T) {
	caps := Capabilities{Outputs: []DRMOutput{{
		ID: "card0:DP-4", Connected: true,
		Modes: []DRMMode{{Width: 3840, Height: 2160, RefreshMillihz: 119879}},
	}, {ID: "card1:DP-1", Connected: false}}}
	validMode := map[string]any{"width": float64(3840), "height": float64(2160), "refresh_millihz": float64(119879)}
	if err := ValidateOutputSelection(map[string]any{"output_id": "card0:DP-4", "mode": validMode}, caps); err != nil {
		t.Fatalf("valid selection rejected: %v", err)
	}
	for _, config := range []map[string]any{
		{"output_id": "card0:DP-4"},
		{"mode": validMode},
		{"output_id": "card1:DP-1", "mode": validMode},
		{"output_id": "card0:DP-4", "mode": map[string]any{"width": float64(1920), "height": float64(1080), "refresh_millihz": float64(60000)}},
	} {
		if err := ValidateOutputSelection(config, caps); err == nil {
			t.Fatalf("invalid selection accepted: %#v", config)
		}
	}
}

func TestResolveReportsActualFixedTopologyDespiteStaleOverrides(t *testing.T) {
	got, err := Resolve(map[string]any{
		"connector": "DP-4", "compositor": "cage", "stream": false,
		"stream_audio": false, "fullscreen": false,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Connector != "auto" || got.Compositor != "weston" || got.Stream || got.StreamAudio || !got.Fullscreen {
		t.Fatalf("resolved topology is not runtime truth: %+v", got)
	}
}

func TestResolveNullClearsToLocalOnlyDefaults(t *testing.T) {
	got, err := Resolve(map[string]any{
		"connector": nil, "compositor": nil, "stream": nil,
		"stream_audio": nil, "fullscreen": nil,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Connector != "auto" || got.Compositor != "weston" || got.Stream || got.StreamAudio || !got.Fullscreen {
		t.Fatalf("cleared topology did not resolve to supported values: %+v", got)
	}
}
