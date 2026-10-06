package console

import (
	"strings"
	"testing"
)

func okApp(string) (bool, error) { return true, nil }
func noApp(string) (bool, error) { return false, nil }

func okUser(string) (bool, error) { return true, nil }
func noUser(string) (bool, error) { return false, nil }

// retiredKeyNames is amendment 19's list (#455): the console settings that
// meant something only to the retired local-display path.
var retiredKeyNames = []string{
	"connector", "mode", "compositor", "audio_output", "stream",
	"stream_audio", "grab", "auto_connect_controller", "fullscreen",
}

func TestValidatePatch(t *testing.T) {
	reported := Capabilities{
		Connectors: []string{"DP-4"},
		Outputs: []DRMOutput{
			{ID: "card0:DP-4", Connector: "DP-4", Connected: true},
			{ID: "card1:HDMI-A-1", Connector: "HDMI-A-1", Connected: false},
		},
		InputDevices: []InputDevicePath{{Path: "/dev/input/event4", Label: "Keyboard"}},
	}
	empty := EmptyCapabilities()

	cases := []struct {
		name    string
		patch   map[string]any
		caps    Capabilities
		app     func(string) (bool, error)
		user    func(string) (bool, error)
		wantErr string // "" = accepted; otherwise a substring of the error
	}{
		// Accept: the six trimmed settings.
		{"enabled bool accepted", map[string]any{"enabled": true}, empty, okApp, okUser, ""},
		{"auto start bool accepted", map[string]any{"auto_start_on_display": true}, empty, okApp, okUser, ""},
		{"connected output accepted", map[string]any{"output_id": "card0:DP-4"}, reported, okApp, okUser, ""},
		{"unplugged output accepted (launch when a monitor appears)", map[string]any{"output_id": "card1:HDMI-A-1"}, reported, okApp, okUser, ""},
		{"output null clears to automatic", map[string]any{"output_id": nil}, empty, okApp, okUser, ""},
		{"input_devices auto accepted", map[string]any{"input_devices": "auto"}, reported, okApp, okUser, ""},
		{"input_devices reported list accepted", map[string]any{"input_devices": []any{"/dev/input/event4"}}, reported, okApp, okUser, ""},
		{"input_devices list allowed when caps empty", map[string]any{"input_devices": []any{"/dev/input/event9"}}, empty, okApp, okUser, ""},
		{"default_app known accepted", map[string]any{"default_app": "id"}, empty, okApp, okUser, ""},
		{"default_app null clears", map[string]any{"default_app": nil}, empty, okApp, okUser, ""},
		{"default_user known accepted", map[string]any{"default_user": "id"}, empty, okApp, okUser, ""},
		{"default_user null clears", map[string]any{"default_user": nil}, empty, okApp, okUser, ""},

		// Reject: bad values and keys the trimmed shape does not have.
		{"unknown key rejected", map[string]any{"bogus": 1}, empty, okApp, okUser, "unknown console-config key"},
		{"enabled type checked", map[string]any{"enabled": "yes"}, empty, okApp, okUser, "boolean"},
		{"auto start type checked", map[string]any{"auto_start_on_display": 1}, empty, okApp, okUser, "boolean"},
		{"unreported output rejected", map[string]any{"output_id": "card9:DP-9"}, reported, okApp, okUser, "not a reported DRM output"},
		{"output rejected when nothing reported", map[string]any{"output_id": "card0:DP-4"}, empty, okApp, okUser, "not a reported DRM output"},
		{"empty output rejected", map[string]any{"output_id": ""}, reported, okApp, okUser, "non-empty string"},
		{"input_devices bad string rejected", map[string]any{"input_devices": "all"}, reported, okApp, okUser, "auto"},
		{"input_devices unreported entry rejected", map[string]any{"input_devices": []any{"/dev/input/event9"}}, reported, okApp, okUser, "unreported device"},
		{"default_app unknown rejected", map[string]any{"default_app": "id"}, empty, noApp, okUser, "unknown app"},
		{"default_user unknown rejected", map[string]any{"default_user": "id"}, empty, okApp, noUser, "unknown user"},
	}
	for _, key := range retiredKeyNames {
		cases = append(cases, struct {
			name    string
			patch   map[string]any
			caps    Capabilities
			app     func(string) (bool, error)
			user    func(string) (bool, error)
			wantErr string
		}{"retired " + key + " rejected", map[string]any{key: true}, empty, okApp, okUser, "retired"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePatch(tc.patch, tc.caps, tc.app, tc.user)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidatePatch(%v) = %v, want accepted", tc.patch, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidatePatch(%v) = %v, want an error containing %q", tc.patch, err, tc.wantErr)
			}
		})
	}
}

func TestDefaultsAreTheSixSettings(t *testing.T) {
	got := Defaults()
	want := []string{"enabled", "output_id", "input_devices", "auto_start_on_display", "default_app", "default_user"}
	if len(got) != len(want) {
		t.Fatalf("Defaults() has %d keys, want the six: %v", len(got), got)
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Fatalf("Defaults() lacks %q: %v", key, got)
		}
	}
}

// Ignore: a stored row written before amendment 19 (or by a later release)
// carries keys this reader does not know. They never block a read.
func TestResolveIgnoresRetiredAndUnknownKeys(t *testing.T) {
	stored := map[string]any{
		"enabled": true, "output_id": "card0:DP-4", "auto_start_on_display": true,
		"default_app": "app-1", "default_user": "user-1",
		"input_devices": []any{"/dev/input/event4"},
		// Retired keys, including values the old validator would refuse today.
		"connector": "DP-4", "mode": map[string]any{"width": "wide"}, "compositor": "cage",
		"audio_output": 7, "stream": "yes", "stream_audio": true, "grab": false,
		"auto_connect_controller": true, "fullscreen": false,
		// A key some later release might add.
		"future_setting": map[string]any{"nested": true},
	}
	got, err := Resolve(stored)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Enabled || !got.AutoStartOnDisplay || got.OutputID == nil || *got.OutputID != "card0:DP-4" ||
		got.DefaultApp == nil || *got.DefaultApp != "app-1" || got.DefaultUser == nil || *got.DefaultUser != "user-1" ||
		got.InputDevices.Auto || len(got.InputDevices.Paths) != 1 {
		t.Fatalf("Resolve lost a kept setting: %+v", got)
	}
}

func TestResolveDefaults(t *testing.T) {
	got, err := Resolve(map[string]any{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Enabled || got.AutoStartOnDisplay || got.OutputID != nil || got.DefaultApp != nil ||
		got.DefaultUser != nil || !got.InputDevices.Auto {
		t.Fatalf("defaults = %+v, want off, automatic output, every input device, no app or owner", got)
	}
}

// KnownOnly is what a PATCH merge writes back: a stored retired key does not
// survive the next write.
func TestKnownOnlyDropsRetiredKeys(t *testing.T) {
	got := KnownOnly(map[string]any{"enabled": true, "grab": true, "stream": true, "default_app": "a"})
	if len(got) != 2 || got["enabled"] != true || got["default_app"] != "a" {
		t.Fatalf("KnownOnly = %v, want enabled and default_app only", got)
	}
}
