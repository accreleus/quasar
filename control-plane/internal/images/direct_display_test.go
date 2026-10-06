package images

import (
	"encoding/json"
	"testing"
)

// TestProviderRuntimeSpecDirectDisplay: amendment 19 (#455). A manifest that
// states direct_display reaches the app's runtime_spec (it gates the console's
// default-app list); a silent manifest leaves the key absent, which reads as
// "cannot run direct".
func TestProviderRuntimeSpecDirectDisplay(t *testing.T) {
	for _, tt := range []struct {
		runtimeRaw string
		want       any // nil = absent
	}{
		{`{"gpu":true,"direct_display":true}`, true},
		{`{"gpu":true,"direct_display":false}`, false},
		{`{"gpu":true}`, nil},
	} {
		out, _, err := providerRuntimeSpec([]byte(tt.runtimeRaw), "ghcr.io/example/kde@sha256:deadbeef", false)
		if err != nil {
			t.Fatalf("providerRuntimeSpec(%s): %v", tt.runtimeRaw, err)
		}
		var spec map[string]any
		if err := json.Unmarshal(out, &spec); err != nil {
			t.Fatalf("decode runtime_spec %s: %v", out, err)
		}
		if got := spec["direct_display"]; got != tt.want {
			t.Errorf("manifest %s: runtime_spec.direct_display = %v, want %v", tt.runtimeRaw, got, tt.want)
		}
	}
}

// The console-created-app path (#171) shares the rule.
func TestApplyLaunchProfileDirectDisplay(t *testing.T) {
	out, err := ApplyLaunchProfile(json.RawMessage(`{"image":"kde:1"}`), []byte(`{"direct_display":true}`))
	if err != nil {
		t.Fatalf("ApplyLaunchProfile: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(out, &spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if spec["direct_display"] != true || spec["image"] != "kde:1" {
		t.Fatalf("spec = %v, want direct_display true beside the app's own image", spec)
	}
}
