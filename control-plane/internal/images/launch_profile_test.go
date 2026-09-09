// launch_profile_test.go — ApplyLaunchProfile (#171). Pure function, no
// TEST_DATABASE_URL needed.
package images

import (
	"encoding/json"
	"testing"
)

func decodeSpec(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m
}

// TestApplyLaunchProfile is the #171 regression: the console wrote gpu:false
// for a KDE app on the managed preset and the image refused to start on a
// software Vulkan renderer. The manifest's declared profile must win.
func TestApplyLaunchProfile(t *testing.T) {
	const kde = `{"gpu":true,"no_new_privileges":false,"systempaths_unconfined":true,"managed_home":true,"network":"bridge"}`
	tests := []struct {
		name    string
		spec    string
		runtime string
		want    map[string]any // keys asserted; "" value ⇒ must be absent
	}{
		{
			name:    "console-created KDE app: explicit gpu:false is overridden",
			spec:    `{"env": {}, "gpu": false, "args": [], "image": "", "mounts": []}`,
			runtime: kde,
			want:    map[string]any{"gpu": true, "no_new_privileges": false, "systempaths_unconfined": true},
		},
		{
			name:    "gpu absent is overridden too (the agent decodes absent as false)",
			spec:    `{"env":{"A":"1"}}`,
			runtime: kde,
			want:    map[string]any{"gpu": true, "no_new_privileges": false, "systempaths_unconfined": true},
		},
		{
			name:    "manifest silent on gpu defaults it TRUE; undeclared keys stay absent",
			spec:    `{"gpu":false}`,
			runtime: `{"managed_home":true}`,
			want:    map[string]any{"gpu": true, "no_new_privileges": "", "systempaths_unconfined": ""},
		},
		{
			name:    "manifest gpu:false (quasar-probe) is honoured",
			spec:    `{"gpu":true}`,
			runtime: `{"gpu":false,"no_new_privileges":true}`,
			want:    map[string]any{"gpu": false, "no_new_privileges": true, "systempaths_unconfined": ""},
		},
		{
			name:    "no runtime block at all: gpu true, nothing else written",
			spec:    `{}`,
			runtime: ``,
			want:    map[string]any{"gpu": true, "no_new_privileges": "", "systempaths_unconfined": ""},
		},
		{
			name:    "empty spec is treated as {}",
			spec:    ``,
			runtime: kde,
			want:    map[string]any{"gpu": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ApplyLaunchProfile(json.RawMessage(tt.spec), []byte(tt.runtime))
			if err != nil {
				t.Fatalf("ApplyLaunchProfile: %v", err)
			}
			got := decodeSpec(t, out)
			for k, want := range tt.want {
				v, present := got[k]
				if want == "" {
					if present {
						t.Errorf("%s = %v, want ABSENT", k, v)
					}
					continue
				}
				if !present || v != want {
					t.Errorf("%s = %v (present=%v), want %v", k, v, present, want)
				}
			}
		})
	}
}

// TestApplyLaunchProfilePreservesOtherKeys: the app's own env/args/mounts and
// any unknown future key survive the stamp — only the three profile keys move.
func TestApplyLaunchProfilePreservesOtherKeys(t *testing.T) {
	spec := `{"image":"x:1","args":["--a"],"env":{"K":"v"},"mounts":["/h:/c"],"gpu":false,"future":{"n":3}}`
	out, err := ApplyLaunchProfile(json.RawMessage(spec), []byte(`{"gpu":true,"systempaths_unconfined":true}`))
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSpec(t, out)
	if got["image"] != "x:1" || got["env"].(map[string]any)["K"] != "v" || got["future"].(map[string]any)["n"] != float64(3) {
		t.Errorf("non-profile keys disturbed: %s", out)
	}
	if got["gpu"] != true || got["systempaths_unconfined"] != true {
		t.Errorf("profile not applied: %s", out)
	}
	if _, present := got["no_new_privileges"]; present {
		t.Errorf("no_new_privileges written though the manifest is silent: %s", out)
	}
}

// TestApplyLaunchProfileRejectsMalformedSpec: a malformed stored spec is an
// error the caller surfaces, never silently replaced with {}.
func TestApplyLaunchProfileRejectsMalformedSpec(t *testing.T) {
	if _, err := ApplyLaunchProfile(json.RawMessage(`{not json`), []byte(`{"gpu":true}`)); err == nil {
		t.Fatal("want error for malformed runtime_spec")
	}
	if _, err := ApplyLaunchProfile(json.RawMessage(`{}`), []byte(`{not json`)); err == nil {
		t.Fatal("want error for malformed runtime block")
	}
}
