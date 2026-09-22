package session

// codec_preference_test.go — pure tests for codecPreference (#305): the Auto
// launch's ordering key, computed before placement from the chain and the
// client-side clamps only.

import (
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

func TestCodecPreference(t *testing.T) {
	high := []profile.Profile{
		r("av1-1440", profile.CodecAV1, 1440),
		r("hevc-1440", profile.CodecHEVC, 1440),
		r("h264-1080", profile.CodecH264, 1080),
	}
	cases := []struct {
		name   string
		rungs  []profile.Profile
		probe  *DeviceProbe
		failed map[string]bool
		want   []string
	}{
		{
			name:  "chain order, every codec decodable",
			rungs: high, probe: probe(true, true),
			want: []string{"av1", "h265", "h264"},
		},
		{
			// Order is the chain's, not a codec ranking.
			name: "chain order is kept even when it is not av1-first",
			rungs: []profile.Profile{
				r("hevc", profile.CodecHEVC, 1080), r("av1", profile.CodecAV1, 1080), r("h264", profile.CodecH264, 1080),
			},
			probe: probe(true, true),
			want:  []string{"h265", "av1", "h264"},
		},
		{
			name:  "a device without HEVC drops h265 (hard-gated, needs an explicit true)",
			rungs: high, probe: probe(false, true),
			want: []string{"av1", "h264"},
		},
		{
			// The av1/hevc rungs are 1440; only h264 survives, which orders nothing.
			name:  "decode height drops a rung the device cannot decode",
			rungs: high, probe: probeAt(true, true, 1080),
			want: nil,
		},
		{
			// The first av1 rung is too tall, a later one fits: av1 is still
			// preferred, at the position its surviving rung holds.
			name: "a codec survives through a later, smaller rung",
			rungs: []profile.Profile{
				r("av1-2160", profile.CodecAV1, 2160), r("hevc-1080", profile.CodecHEVC, 1080),
				r("av1-1080", profile.CodecAV1, 1080), r("h264", profile.CodecH264, 1080),
			},
			probe: probeAt(true, true, 1080),
			want:  []string{"h265", "av1", "h264"},
		},
		{
			name:  "decode-failure history drops the failed rung",
			rungs: high, probe: probe(true, true), failed: map[string]bool{"av1-1440": true},
			want: []string{"h265", "h264"},
		},
		{
			name: "duplicates collapse to the first survivor",
			rungs: []profile.Profile{
				r("av1-a", profile.CodecAV1, 1080), r("av1-b", profile.CodecAV1, 720), r("h264", profile.CodecH264, 720),
			},
			probe: probe(true, true),
			want:  []string{"av1", "h264"},
		},
		{
			// Clamp 2/3 admits only h264 without a probe, and h264 alone orders
			// nothing, so an absent probe is an empty preference.
			name:  "no probe is an empty preference",
			rungs: high, probe: nil,
			want: nil,
		},
		{
			name:  "a device that decodes only h264 is an empty preference",
			rungs: high, probe: probe(false, false),
			want: nil,
		},
		{
			name:  "no chain (legacy/tier launch) is an empty preference",
			rungs: nil, probe: probe(true, true),
			want: nil,
		},
		{
			name:  "an unknown codec is skipped, not preferred",
			rungs: []profile.Profile{r("vp9", profile.Codec("vp9"), 1080), r("hevc", profile.CodecHEVC, 1080)},
			probe: probe(true, true),
			want:  []string{"h265"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codecPreference(tc.rungs, tc.probe, tc.failed)
			if !equalStrings(got, tc.want) {
				t.Errorf("codecPreference = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCodecPreferenceAgreesWithTheWalk pins the claim codecPreference's doc
// makes: its first codec is what resolveRung picks when the placed GPU can
// encode everything. One predicate (clientClampReject) serves both.
func TestCodecPreferenceAgreesWithTheWalk(t *testing.T) {
	chain := []profile.Profile{
		r("av1-1440", profile.CodecAV1, 1440),
		r("hevc-1080", profile.CodecHEVC, 1080),
		r("h264-1080", profile.CodecH264, 1080),
	}
	all := []string{"h264", "h265", "av1"}
	probes := []*DeviceProbe{probe(true, true), probe(true, false), probeAt(true, true, 1080), probe(false, true), nil}
	for _, dp := range probes {
		for _, failed := range []map[string]bool{nil, {"av1-1440": true}, {"hevc-1080": true}} {
			pref := codecPreference(chain, dp, failed)
			_, dec, err := resolveRung(chain, all, hostEncoderCaps{}, dp, failed, StreamOverride{})
			if err != nil {
				t.Fatal(err)
			}
			got := wireCodecH264 // an empty preference is the h264-only case
			if len(pref) > 0 {
				got = pref[0]
			}
			if got != dec.Result {
				t.Errorf("probe %+v failed %v: the preference leads with %q, the walk picks %q", dp, failed, got, dec.Result)
			}
		}
	}
}
