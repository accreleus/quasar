package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The format-2 asset (control-api.md amendment 14 "Release manifest format 2"), read
// from the fixture the release tooling's validator also passes
// (scripts/release/test-platform-release-manifest.sh), so the producer and this
// reader agree on one document.
func v2Fixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "release", "platform-release-manifest.v2.json"))
	if err != nil {
		t.Fatalf("read the shared format-2 fixture: %v", err)
	}
	return string(raw)
}

func TestParseManifestAcceptsFormat2(t *testing.T) {
	m, err := ParseManifestAsset([]byte(v2Fixture(t)), ManifestFormat2)
	if err != nil {
		t.Fatalf("the documented format-2 manifest was rejected: %v", err)
	}
	if len(m.Components) != 3 || m.Components[2].Name != ComponentRecovery {
		t.Fatalf("components = %+v", m.Components)
	}
	for _, c := range []string{ComponentNodeAgent, ComponentRecovery} {
		if v, ok := m.FloorVersion(c); !ok || v != "0.4.0-0" {
			t.Errorf("floor %s = %q, %v", c, v, ok)
		}
	}
	// A release read from format 2 moves the actor with the agent and the control plane.
	if got := HostComponentsOf(m); len(got) != 2 || got[1].Name != ComponentRecovery {
		t.Errorf("host components = %+v", got)
	}
	if got := ControlPlaneComponents(m); len(got) != 2 || got[0].Name != ComponentControlPlane {
		t.Errorf("control-plane components = %+v", got)
	}
}

// The asset name promises its format: a document under the other name is refused.
func TestParseManifestAssetBindsTheFormatToTheAssetName(t *testing.T) {
	if _, err := ParseManifestAsset([]byte(v2Fixture(t)), ManifestFormat1); err == nil {
		t.Error("a format-2 document published as the format-1 asset was accepted")
	}
	if _, err := ParseManifestAsset([]byte(goodManifest), ManifestFormat2); err == nil {
		t.Error("a format-1 document published as the format-2 asset was accepted")
	}
	if _, err := ParseManifestAsset([]byte(goodManifest), ManifestFormat1); err != nil {
		t.Errorf("the format-1 asset is still read: %v", err)
	}
}

func TestParseManifestFormat2Rejections(t *testing.T) {
	const floorBlock = `"floor": [
    {
      "name": "node-agent",
      "version": "0.4.0-0"
    },
    {
      "name": "recovery-actor",
      "version": "0.4.0-0"
    }
  ]`
	recovery := `,
    {
      "name": "recovery-actor",
      "image": "ghcr.io/accreleus/quasar/quasar-recovery",
      "digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
    }`
	tests := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"no floor", func(s string) string { return strings.Replace(s, ",\n  "+floorBlock, "", 1) }, "has no floor"},
		{"a floor with one entry",
			func(s string) string {
				return strings.Replace(s, `,
    {
      "name": "recovery-actor",
      "version": "0.4.0-0"
    }`, "", 1)
			}, "want exactly 2"},
		{"the floor out of order",
			func(s string) string {
				s = strings.Replace(s, `"name": "node-agent",
      "version"`, `"name": "PLACEHOLDER",
      "version"`, 1)
				s = strings.Replace(s, `"name": "recovery-actor",
      "version"`, `"name": "node-agent",
      "version"`, 1)
				return strings.Replace(s, `"name": "PLACEHOLDER"`, `"name": "recovery-actor"`, 1)
			}, "the order is normative"},
		{"a floor above the release's version",
			func(s string) string { return strings.Replace(s, `"version": "0.4.0-0"`, `"version": "0.4.1"`, 1) },
			"orders above"},
		{"a floor with a leading v",
			func(s string) string { return strings.Replace(s, `"version": "0.4.0-0"`, `"version": "v0.3.0"`, 1) },
			"not semver"},
		{"a floor with build metadata",
			func(s string) string { return strings.Replace(s, `"version": "0.4.0-0"`, `"version": "0.3.0+b1"`, 1) },
			"not semver"},
		{"an unknown key in a floor entry",
			func(s string) string {
				return strings.Replace(s, `"version": "0.4.0-0"`, `"version": "0.4.0-0", "extra": 1`, 1)
			}, "unknown field"},
		{"only the two format-1 components",
			func(s string) string { return strings.Replace(s, recovery, "", 1) },
			"want exactly 3"},
		{"a format-1 manifest carrying a floor",
			func(s string) string { return strings.Replace(s, `"format_version": 2`, `"format_version": 1`, 1) },
			"carries a floor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.mutate(v2Fixture(t))
			if raw == v2Fixture(t) {
				t.Fatal("the mutation did not apply")
			}
			_, err := ParseManifest([]byte(raw))
			if err == nil {
				t.Fatal("manifest accepted, want rejected")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// A floor may equal the release's own version, and a prerelease release may declare a
// floor at or below itself.
func TestParseManifestFloorAtTheReleaseVersion(t *testing.T) {
	raw := strings.ReplaceAll(v2Fixture(t), `"version": "0.4.0-0"`, `"version": "0.4.0"`)
	if _, err := ParseManifest([]byte(raw)); err != nil {
		t.Fatalf("floor equal to the version rejected: %v", err)
	}
	raw = strings.Replace(v2Fixture(t), `"version": "0.4.0",`, `"version": "0.4.0-rc.1",`, 1)
	raw = strings.Replace(raw, `"prerelease": false`, `"prerelease": true`, 1)
	if _, err := ParseManifest([]byte(raw)); err != nil {
		t.Fatalf("an rc with the 0.4.0-0 floor rejected: %v", err)
	}
}
