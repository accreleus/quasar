package platform

import (
	"strings"
	"testing"
)

// A valid manifest, as the publish workflow emits it. Each case below breaks
// exactly one rule of control-api.md §"The release manifest asset".
const goodManifest = `{
  "format_version": 1,
  "version": "0.2.0",
  "prerelease": false,
  "source_commit": "1f0c1e0e0c5a9d1b7a2f3e4d5c6b7a8901234567",
  "built_at": "2026-09-04T12:00:00Z",
  "schema_version": 74,
  "components": [
    { "name": "control-plane", "image": "ghcr.io/accreleus/quasar/quasar-control-plane", "digest": "sha256:` + hex64 + `" },
    { "name": "node-agent",    "image": "ghcr.io/accreleus/quasar/quasar-node-agent",    "digest": "sha256:` + hex64 + `" }
  ]
}`

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseManifestAcceptsTheDocumentedShape(t *testing.T) {
	m, err := ParseManifest([]byte(goodManifest))
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if m.SchemaVersion != 74 || m.Version != "0.2.0" {
		t.Fatalf("decoded wrong: %+v", m)
	}
	if got := m.BuiltAtTime().Format("2006-01-02T15:04:05Z"); got != "2026-09-04T12:00:00Z" {
		t.Fatalf("built_at = %s", got)
	}
}

func TestParseManifestRejections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"an unknown format_version is not guessed at",
			func(s string) string { return strings.Replace(s, `"format_version": 1`, `"format_version": 2`, 1) },
			"format_version"},
		{"an unknown key at the object level",
			func(s string) string {
				return strings.Replace(s, `"version": "0.2.0"`, `"version": "0.2.0", "extra": 1`, 1)
			},
			"unknown field"},
		{"an unknown key inside a component",
			func(s string) string {
				return strings.Replace(s, `"name": "node-agent"`, `"name": "node-agent", "extra": 1`, 1)
			},
			"unknown field"},
		{"a missing component",
			func(s string) string {
				i := strings.Index(s, `,\n    { "name": "node-agent"`)
				_ = i
				return strings.Replace(s, `,
    { "name": "node-agent",    "image": "ghcr.io/accreleus/quasar/quasar-node-agent",    "digest": "sha256:`+hex64+`" }`, "", 1)
			},
			"want exactly 2"},
		{"the two components out of order",
			func(s string) string {
				s = strings.Replace(s, `"name": "control-plane"`, `"name": "PLACEHOLDER"`, 1)
				s = strings.Replace(s, `"name": "node-agent"`, `"name": "control-plane"`, 1)
				return strings.Replace(s, `"name": "PLACEHOLDER"`, `"name": "node-agent"`, 1)
			},
			"the order is normative"},
		{"an image carrying a tag",
			func(s string) string {
				return strings.Replace(s, `quasar-control-plane"`, `quasar-control-plane:0.2.0"`, 1)
			},
			"carries a tag"},
		{"an image carrying a digest",
			func(s string) string {
				return strings.Replace(s, `quasar-control-plane"`, `quasar-control-plane@sha256:`+hex64+`"`, 1)
			},
			"carries a digest"},
		{"a malformed digest",
			func(s string) string { return strings.Replace(s, "sha256:"+hex64, "sha256:deadbeef", 1) },
			"digest"},
		{"a non-positive schema_version",
			func(s string) string { return strings.Replace(s, `"schema_version": 74`, `"schema_version": 0`, 1) },
			"schema_version"},
		{"a short source_commit",
			func(s string) string {
				return strings.Replace(s, "1f0c1e0e0c5a9d1b7a2f3e4d5c6b7a8901234567", "1f0c1e0", 1)
			},
			"source_commit"},
		{"a built_at that is not RFC3339",
			func(s string) string { return strings.Replace(s, "2026-09-04T12:00:00Z", "yesterday", 1) },
			"built_at"},
		{"a version with a leading v",
			func(s string) string { return strings.Replace(s, `"version": "0.2.0"`, `"version": "v0.2.0"`, 1) },
			"leading v"},
		{"trailing content after the object",
			func(s string) string { return s + "\n{}" },
			"trailing content"},
		{"not JSON at all",
			func(string) string { return "<html>404</html>" },
			"not valid JSON"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tc.mutate(goodManifest)))
			if err == nil {
				t.Fatal("manifest accepted, want rejected")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}
