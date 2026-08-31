package session

// rung_test.go — pure unit tests for UI-P4 rung resolution and the single
// catalog↔wire mapping point. No database required; these run on every
// `go test`.
//
// One case PER CLAMP, plus the two that carry the most risk: the terminal
// (floor) rung bypassing ALL clamps, and the decode-HEIGHT half of clamp 2/3,
// which is new because a rung now carries its own resolution.

import (
	"errors"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

func probe(hevc, av1 bool) *DeviceProbe { return &DeviceProbe{HEVC: hevc, AV1: av1} }

// codecOv is the clamp-0 codec override as a StreamOverride, which is what
// resolveRung takes since #506 gave clamp 6 the launch-effective frame size. An
// empty string is "no override" and must stay the zero value, not a pointer to "".
func codecOv(codec string) StreamOverride {
	if codec == "" {
		return StreamOverride{}
	}
	return StreamOverride{Codec: &codec}
}

// probeAt is a probe with a measured decode ceiling.
func probeAt(hevc, av1 bool, maxDecodeHeight int32) *DeviceProbe {
	return &DeviceProbe{HEVC: hevc, AV1: av1, MaxDecodeHeight: maxDecodeHeight}
}

// r builds a rung: id, codec, height (min decode height = height).
func r(id string, codec profile.Codec, height int32) profile.Profile {
	return profile.Profile{
		ID: id, DisplayName: id, Codec: codec,
		Width: height * 16 / 9, Height: height, FPS: 60,
		NominalBitrateKbps: height * 10, MinDecodeHeight: height,
		ABRFloorKbps: height * 3, Playout0Ms: 50,
		H264Profile: "high", Visibility: profile.VisibilityInternal,
	}
}

// hwRung is r with hardware_encoder_required set.
func hwRung(id string, codec profile.Codec, height int32) profile.Profile {
	p := r(id, codec, height)
	p.HardwareEncoderRequired = true
	return p
}

func TestCatalogToWire(t *testing.T) {
	cases := []struct {
		in   profile.Codec
		want string
		ok   bool
	}{
		{profile.CodecH264, "h264", true},
		{profile.CodecHEVC, "h265", true}, // the load-bearing rename
		{profile.CodecAV1, "av1", true},
		{profile.Codec("vp9"), "", false},
		{profile.Codec(""), "", false},
	}
	for _, c := range cases {
		got, ok := catalogToWire(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("catalogToWire(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestValidCodec(t *testing.T) {
	// Wire vocabulary: h265, NOT the catalog's hevc.
	for _, ok := range []string{"h264", "h265", "av1"} {
		if !ValidCodec(ok) {
			t.Errorf("ValidCodec(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"hevc", "vp9", "H264", "", "h.264"} {
		if ValidCodec(bad) {
			t.Errorf("ValidCodec(%q) = true, want false", bad)
		}
	}
}

func TestCodecSetDefaultsToH264(t *testing.T) {
	for _, in := range [][]string{nil, {}, {""}} {
		set := codecSet(in)
		if !set["h264"] || len(set) != 1 {
			t.Errorf("codecSet(%v) = %v, want {h264}", in, set)
		}
	}
}

func TestResolveRungMatrix(t *testing.T) {
	// The shipped (post-0036) shape: a single h264 rung.
	single := []profile.Profile{r("1080p60-h264", profile.CodecH264, 1080)}
	// "High": AV1 1440p, HEVC 1440p, H.264 1080p floor. Resolution RISES with
	// codec efficiency — that is the entire point of the restructure.
	high := []profile.Profile{
		r("1440p60-av1", profile.CodecAV1, 1440),
		r("1440p60-hevc", profile.CodecHEVC, 1440),
		r("1080p60-h264", profile.CodecH264, 1080),
	}

	cases := []struct {
		name     string
		rungs    []profile.Profile
		host     []string
		hostCaps hostEncoderCaps
		probe    *DeviceProbe
		failed   map[string]bool
		override string
		wantRung string
		wantWire string
		// wantWalk is the per-rung verdict trail, "id!reason" for a rejection.
		wantWalk  []string
		wantFloor bool
		wantErr   error
	}{
		{
			name:  "shipped single-rung chain resolves its only rung",
			rungs: single, host: []string{"h264", "h265", "av1"}, probe: probe(true, true),
			wantRung: "1080p60-h264", wantWire: "h264", wantWalk: []string{"1080p60-h264:h264"},
		},
		{
			name:  "everything supported ⇒ the top rung (order is preference)",
			rungs: high, host: []string{"h264", "h265", "av1"}, probe: probeAt(true, true, 2160),
			wantRung: "1440p60-av1", wantWire: "av1", wantWalk: []string{"1440p60-av1:av1"},
		},
		{
			name:  "clamp 1 host encoder: no av1 on the host ⇒ hevc",
			rungs: high, host: []string{"h264", "h265"}, probe: probeAt(true, true, 2160),
			wantRung: "1440p60-hevc", wantWire: "h265",
			wantWalk: []string{"1440p60-av1:av1!host_encoder", "1440p60-hevc:h265"},
		},
		{
			name:  "clamp 1: an agent reporting NO codecs is h264-only",
			rungs: high, host: nil, probe: probeAt(true, true, 2160),
			wantRung: "1080p60-h264", wantWire: "h264",
			wantWalk: []string{"1440p60-av1:av1!host_encoder", "1440p60-hevc:h265!host_encoder", "1080p60-h264:h264"},
		},
		{
			name:  "clamp 2/3 codec: device cannot decode av1 ⇒ hevc",
			rungs: high, host: []string{"h264", "h265", "av1"}, probe: probeAt(true, false, 2160),
			wantRung: "1440p60-hevc", wantWire: "h265",
			wantWalk: []string{"1440p60-av1:av1!client_decode", "1440p60-hevc:h265"},
		},
		{
			name:  "clamp 2/3 codec: a stale/absent probe hard-gates hevc AND av1",
			rungs: high, host: []string{"h264", "h265", "av1"}, probe: nil,
			wantRung: "1080p60-h264", wantWire: "h264",
			wantWalk: []string{"1440p60-av1:av1!client_decode", "1440p60-hevc:h265!client_decode", "1080p60-h264:h264"},
		},
		{
			name: "clamp 2/3 DECODE HEIGHT (new): a 1080-capped decoder skips both 1440p rungs",
			// The codecs are fully supported — only the RESOLUTION disqualifies them.
			// A codec-only clamp chain cannot express this, which is why the rung
			// model needs it.
			rungs: high, host: []string{"h264", "h265", "av1"}, probe: probeAt(true, true, 1080),
			wantRung: "1080p60-h264", wantWire: "h264",
			wantWalk: []string{"1440p60-av1:av1!decode_height", "1440p60-hevc:h265!decode_height", "1080p60-h264:h264"},
		},
		{
			name:  "clamp 2/3 decode height: an UNMEASURED ceiling never clamps (unknown → allow)",
			rungs: high, host: []string{"h264", "h265", "av1"}, probe: probe(true, true),
			wantRung: "1440p60-av1", wantWire: "av1", wantWalk: []string{"1440p60-av1:av1"},
		},
		{
			name:  "clamp 4 decode history: the failed rung is skipped, the chain survives",
			rungs: high, host: []string{"h264", "h265", "av1"}, probe: probeAt(true, true, 2160),
			failed:   map[string]bool{"1440p60-av1": true},
			wantRung: "1440p60-hevc", wantWire: "h265",
			wantWalk: []string{"1440p60-av1:av1!decode_history", "1440p60-hevc:h265"},
		},
		{
			name:  "clamp 5 hardware encoder: a software host skips a hw-required rung",
			rungs: []profile.Profile{hwRung("1440p60-av1", profile.CodecAV1, 1440), r("1080p60-h264", profile.CodecH264, 1080)},
			host:  []string{"h264", "av1"}, hostCaps: hostEncoderCaps{Known: true, HardwareEncoder: false},
			probe:    probeAt(true, true, 2160),
			wantRung: "1080p60-h264", wantWire: "h264",
			wantWalk: []string{"1440p60-av1:av1!hardware_encoder", "1080p60-h264:h264"},
		},
		{
			name:  "clamp 5: UNKNOWN host encoder capability never clamps (unknown → allow)",
			rungs: []profile.Profile{hwRung("1440p60-av1", profile.CodecAV1, 1440), r("1080p60-h264", profile.CodecH264, 1080)},
			host:  []string{"h264", "av1"}, hostCaps: hostEncoderCaps{}, probe: probeAt(true, true, 2160),
			wantRung: "1440p60-av1", wantWire: "av1", wantWalk: []string{"1440p60-av1:av1"},
		},
		{
			name:  "clamp 0 override: selects the first rung with that codec, bypassing decode + history",
			rungs: high, host: []string{"h264", "h265", "av1"}, probe: nil,
			failed: map[string]bool{"1440p60-hevc": true}, override: "h265",
			wantRung: "1440p60-hevc", wantWire: "h265", wantWalk: []string{"1440p60-hevc:h265"},
		},
		{
			name:  "clamp 0 override: a codec no rung uses ⇒ 400",
			rungs: single, host: []string{"h264", "av1"}, probe: probe(true, true), override: "av1",
			wantErr: ErrRungCodecNotAvailable,
		},
		{
			name:  "clamp 0 override: the host cannot encode it ⇒ 409 (encoder capability is physics)",
			rungs: high, host: []string{"h264"}, probe: probe(true, true), override: "av1",
			wantErr: ErrCodecUnsupportedByHost,
		},
		{
			name:  "a rung with an unmappable codec is skipped, never fatal",
			rungs: []profile.Profile{r("weird", profile.Codec("vp9"), 1080), r("1080p60-h264", profile.CodecH264, 1080)},
			host:  []string{"h264"}, probe: probe(false, false),
			wantRung: "1080p60-h264", wantWire: "h264",
			wantWalk: []string{"weird:vp9!unknown_codec", "1080p60-h264:h264"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, dec, err := resolveRung(c.rungs, c.host, c.hostCaps, c.probe, c.failed, codecOv(c.override))
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if got.ID != c.wantRung {
				t.Errorf("resolved rung = %q, want %q", got.ID, c.wantRung)
			}
			if dec.ResultRung != c.wantRung || dec.Result != c.wantWire {
				t.Errorf("decision = (%q,%q), want (%q,%q)", dec.ResultRung, dec.Result, c.wantRung, c.wantWire)
			}
			if dec.Floor != c.wantFloor {
				t.Errorf("decision.Floor = %v, want %v", dec.Floor, c.wantFloor)
			}
			if walk := formatRungVerdicts(dec.Considered); !equalStrings(walk, c.wantWalk) {
				t.Errorf("rung walk = %v, want %v", walk, c.wantWalk)
			}
		})
	}
}

// TestTerminalRungBypassesEveryClamp is the single most important resolver
// behaviour, and the one a naive implementation gets wrong.
//
// When NO rung survives, the LAST h264 rung dispatches bypassing EVERY clamp:
// the host encoder set, the client decode probe, the decode-height ceiling, the
// failure history, and its own hardware_encoder_required. The justification is
// that the user picked the launch profile and eligibility already approved it —
// a session that cannot resolve anything is a FAILED launch rather than a
// degraded one. Same silent-downgrade-to-H.264 posture as Sunshine/Moonlight.
func TestTerminalRungBypassesEveryClamp(t *testing.T) {
	floor := hwRung("4k60-h264", profile.CodecH264, 2160) // hw-required AND 4K
	rungs := []profile.Profile{r("4k60-av1", profile.CodecAV1, 2160), floor}

	got, dec, err := resolveRung(
		rungs,
		[]string{"h265"}, // clamp 1: the host cannot even encode h264
		hostEncoderCaps{Known: true, HardwareEncoder: false}, // clamp 5: no hardware encoder
		probeAt(false, false, 720),                           // clamp 2/3: no av1, 720p decode ceiling
		map[string]bool{"4k60-av1": true, "4k60-h264": true}, // clamp 4: both rungs previously failed
		StreamOverride{},
	)
	if err != nil {
		t.Fatalf("err = %v; the floor must never fail to resolve", err)
	}
	if got.ID != "4k60-h264" {
		t.Fatalf("resolved %q, want the terminal h264 rung 4k60-h264", got.ID)
	}
	if dec.Result != wireCodecH264 {
		t.Errorf("result codec = %q, want h264", dec.Result)
	}
	if !dec.Floor {
		t.Error("decision.Floor = false; the floor fired and must be recorded as such")
	}
}

// TestFloorPicksTheLASTH264Rung: with several h264 rungs, the floor is the last
// (least demanding by convention), not the first.
func TestFloorPicksTheLastH264Rung(t *testing.T) {
	rungs := []profile.Profile{
		r("4k60-h264", profile.CodecH264, 2160),
		r("1080p60-h264", profile.CodecH264, 1080),
	}
	// A 480p decode ceiling rejects both.
	got, dec, err := resolveRung(rungs, []string{"h264"}, hostEncoderCaps{}, probeAt(false, false, 480), nil, StreamOverride{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.ID != "1080p60-h264" {
		t.Errorf("floor = %q, want the LAST h264 rung 1080p60-h264", got.ID)
	}
	if !dec.Floor {
		t.Error("decision.Floor = false, want true")
	}
}

// TestFloorWithNoH264RungDispatchesTheLastRung: write-time validation rejects a
// chain with no h264 rung, so this only happens with hand-edited data. The
// resolver still dispatches SOMETHING rather than failing the launch.
func TestFloorWithNoH264RungDispatchesTheLastRung(t *testing.T) {
	rungs := []profile.Profile{
		r("4k60-av1", profile.CodecAV1, 2160),
		r("1440p60-hevc", profile.CodecHEVC, 1440),
	}
	got, dec, err := resolveRung(rungs, []string{"h264"}, hostEncoderCaps{}, nil, nil, StreamOverride{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.ID != "1440p60-hevc" || dec.Result != wireCodecH265 {
		t.Errorf("got (%q,%q), want (1440p60-hevc,h265)", got.ID, dec.Result)
	}
}

// TestResolveRungEmptyChain: a rung-less chain is a refusal, not a silent
// dispatch of nothing.
func TestResolveRungEmptyChain(t *testing.T) {
	if _, _, err := resolveRung(nil, []string{"h264"}, hostEncoderCaps{}, nil, nil, StreamOverride{}); !errors.Is(err, ErrLaunchProfileEmpty) {
		t.Fatalf("err = %v, want ErrLaunchProfileEmpty", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
