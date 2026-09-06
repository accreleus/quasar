package session

import (
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

func TestProfileCodecUnion(t *testing.T) {
	for _, tc := range []struct {
		name         string
		reports      [][]byte
		unknown, av1 bool
	}{
		{"excluded", [][]byte{[]byte(`["h264","h265"]`)}, false, false},
		{"mixed", [][]byte{[]byte(`["h264","h265"]`), []byte(`["h264","av1"]`)}, false, true},
		{"legacy", [][]byte{[]byte(`["h264","h265"]`), nil}, true, false},
		{"null", [][]byte{[]byte(`null`)}, true, false},
		{"malformed", [][]byte{[]byte(`{}`)}, true, false},
		{"none", nil, true, false},
		{"empty report has h264 floor", [][]byte{[]byte(`[]`)}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := profileCodecUnion(tc.reports)
			if (got == nil) != tc.unknown || got[profile.CodecAV1] != tc.av1 {
				t.Fatalf("union = %#v", got)
			}
			if !tc.unknown && !got[profile.CodecH264] {
				t.Fatal("missing H.264 floor")
			}
			if tc.name == "excluded" && !got[profile.CodecHEVC] {
				t.Fatal("h265 wire report did not map to HEVC catalog")
			}
		})
	}
}
