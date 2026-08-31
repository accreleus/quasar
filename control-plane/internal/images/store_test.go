package images

import (
	"encoding/json"
	"testing"
)

// TestNormalizeJSONObject — MINOR 4 (adversarial review finding 17): a
// literal JSON `null` (not just an empty/absent value) must normalize to
// `{}`, matching every object-typed field in protocol/openapi.yaml
// (CatalogImage.artwork is `type: object`, never nullable).
func TestNormalizeJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{"nil", nil, "{}"},
		{"empty", json.RawMessage(""), "{}"},
		{"literal null", json.RawMessage("null"), "{}"},
		{"literal null with whitespace", json.RawMessage("  null  "), "{}"},
		{"already an object", json.RawMessage(`{"tile":"x.png"}`), `{"tile":"x.png"}`},
		{"empty object", json.RawMessage("{}"), "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeJSONObject(tc.in)
			if string(got) != tc.want {
				t.Errorf("normalizeJSONObject(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
