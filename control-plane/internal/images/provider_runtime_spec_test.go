// provider_runtime_spec_test.go — unit coverage for providerRuntimeSpec's
// systempaths_unconfined threading (2026-08-14 amendment draft: KDE desktop's
// flatpak run needs an unmasked /proc for bwrap's own fresh /proc mount).
// Pure function, no TEST_DATABASE_URL needed.
package images

import (
	"encoding/json"
	"testing"
)

// TestProviderRuntimeSpecSystempathsUnconfined asserts the field is threaded
// exactly like no_new_privileges: written when the manifest states it (either
// true or false), and left ABSENT — not defaulted to false — when the
// manifest is silent, so an image predating this knob dispatches a
// byte-identical runtime_spec to what it dispatched before this change.
func TestProviderRuntimeSpecSystempathsUnconfined(t *testing.T) {
	tests := []struct {
		name       string
		runtimeRaw string
		wantAbsent bool
		want       bool
	}{
		{
			name:       "manifest states true (kde-desktop)",
			runtimeRaw: `{"gpu":true,"no_new_privileges":false,"systempaths_unconfined":true,"managed_home":true}`,
			want:       true,
		},
		{
			name:       "manifest states false explicitly",
			runtimeRaw: `{"gpu":true,"systempaths_unconfined":false,"managed_home":true}`,
			want:       false,
		},
		{
			name:       "manifest silent on the key (steam, pre-existing images)",
			runtimeRaw: `{"gpu":true,"no_new_privileges":false,"managed_home":true}`,
			wantAbsent: true,
		},
		{
			name:       "no runtime block at all",
			runtimeRaw: `{}`,
			wantAbsent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := providerRuntimeSpec([]byte(tt.runtimeRaw), "ghcr.io/example/img@sha256:deadbeef", false)
			if err != nil {
				t.Fatalf("providerRuntimeSpec: %v", err)
			}
			var spec map[string]any
			if err := json.Unmarshal(out, &spec); err != nil {
				t.Fatalf("decode runtime_spec %s: %v", out, err)
			}
			v, present := spec["systempaths_unconfined"]
			if tt.wantAbsent {
				if present {
					t.Errorf("runtime_spec.systempaths_unconfined = %v, want ABSENT (byte-identical to before this knob existed)", v)
				}
				return
			}
			if !present {
				t.Fatalf("runtime_spec.systempaths_unconfined is absent, want %v", tt.want)
			}
			if v != tt.want {
				t.Errorf("runtime_spec.systempaths_unconfined = %v, want %v", v, tt.want)
			}
		})
	}
}

// TestProviderRuntimeSpecSystempathsUnconfinedDoesNotDisturbOtherKeys pins the
// sibling fields (gpu, no_new_privileges, image) so this addition cannot
// regress the existing #432 threading it sits next to.
func TestProviderRuntimeSpecSystempathsUnconfinedDoesNotDisturbOtherKeys(t *testing.T) {
	out, managedHome, err := providerRuntimeSpec(
		[]byte(`{"gpu":true,"no_new_privileges":false,"systempaths_unconfined":true,"managed_home":true}`),
		"ghcr.io/example/kde@sha256:deadbeef",
		true, // needsImage
	)
	if err != nil {
		t.Fatalf("providerRuntimeSpec: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(out, &spec); err != nil {
		t.Fatalf("decode runtime_spec %s: %v", out, err)
	}
	if spec["gpu"] != true {
		t.Errorf("gpu = %v, want true", spec["gpu"])
	}
	if spec["no_new_privileges"] != false {
		t.Errorf("no_new_privileges = %v, want false", spec["no_new_privileges"])
	}
	if spec["systempaths_unconfined"] != true {
		t.Errorf("systempaths_unconfined = %v, want true", spec["systempaths_unconfined"])
	}
	if spec["image"] != "ghcr.io/example/kde@sha256:deadbeef" {
		t.Errorf("image = %v, want the adopted ref (needsImage=true)", spec["image"])
	}
	if !managedHome {
		t.Errorf("managedHome = false, want true from the manifest")
	}
}
