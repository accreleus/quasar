package session

import (
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// kind goes only to a parked_sessions_v1 connection and only while warm Steam
// is on; a parked session or an adoption to a connection that cannot take it
// fails closed.
func TestWireAssign(t *testing.T) {
	for _, c := range []struct {
		name          string
		parks, warmOn bool
		kind, adopt   string
		wantKind      string
		wantAdopt     string
		wantOK        bool
	}{
		{"capable, on: kind defaults to play", true, true, "", "", KindPlay, "", true},
		{"capable, on: parked", true, true, KindParked, "", KindParked, "", true},
		{"capable, on: claim", true, true, "", "k", KindPlay, "k", true},
		{"capable, off: no kind", true, false, "", "", "", "", true},
		{"capable, off: a claim still adopts, with no kind", true, false, "", "k", "", "k", true},
		{"capable, off: no parked session", true, false, KindParked, "", "", "", false},
		{"older agent: no kind", false, true, "", "", "", "", true},
		{"older agent: no parked session", false, true, KindParked, "", "", "", false},
		{"older agent: an adoption fails closed", false, true, "", "k", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := wireAssign(agentws.SessionAssignCmd{Kind: c.kind, AdoptSessionID: c.adopt}, c.parks, c.warmOn)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && (got.Kind != c.wantKind || got.AdoptSessionID != c.wantAdopt) {
				t.Errorf("kind=%q adopt=%q, want %q / %q", got.Kind, got.AdoptSessionID, c.wantKind, c.wantAdopt)
			}
		})
	}
}

func TestValidLaunchProgress(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"step":"shaders","percent":0}`:       true,
		`{"step":"shaders","percent":100}`:     true,
		`{"step":"shaders","percent":101}`:     false,
		`{"step":"shaders","percent":-1}`:      false,
		`{"step":"shaders","percent":4.5}`:     false,
		`{"step":"shaders","percent":null}`:    false,
		`{"step":"compile","percent":5}`:       false,
		`{"step":"shaders"}`:                   false,
		`{"step":"shaders","percent":5,"x":1}`: false,
		`"shaders"`:                            false,
		`{"step":"shaders","percent":"5"}`:     false,
		`{"percent":7,"step":"shaders"}`:       true,
	} {
		if _, ok := validLaunchProgress([]byte(raw)); ok != want {
			t.Errorf("validLaunchProgress(%s) = %v, want %v", raw, ok, want)
		}
	}
}

func TestDecideClaim(t *testing.T) {
	park := claimStream{Codec: "av1", Width: 2560, Height: 1440, FPS: 60, BitrateKbps: 7000, H264Profile: "main", Mic: true}
	with := func(f func(*claimStream)) claimStream { s := park; f(&s); return s }
	p1080 := with(func(s *claimStream) { s.Width, s.Height, s.BitrateKbps = 1920, 1080, 5000 })
	running := parkedFacts{Family: true, State: StateRunning, InDeadline: true, HostUp: true, Restream: true, Stream: park}
	old := running
	old.Restream = false
	at := func(f func(*parkedFacts)) parkedFacts { k := running; f(&k); return k }

	cases := []struct {
		name     string
		k        parkedFacts
		launch   claimStream
		outcome  claimOutcome
		cause    string
		restream string
	}{
		{"same settings", running, park, claimAdopt, "", ""},
		{"same settings, old agent", old, park, claimAdopt, "", ""},
		{"resolution only", running, with(func(s *claimStream) { s.Width, s.Height = 1920, 1080 }), claimAdopt, "", "size"},
		{"resolution only, old agent", old, with(func(s *claimStream) { s.Width, s.Height = 1920, 1080 }), claimAdopt, "", "size"},
		{"1080p rung over a 1440p park", running, p1080, claimAdopt, "", "size,bitrate"},
		{"1080p rung, old agent", old, p1080, claimSupersede, causeStreamDiffers, ""},
		{"fps lower", running, with(func(s *claimStream) { s.FPS = 30 }), claimAdopt, "", "fps"},
		{"fps lower, old agent", old, with(func(s *claimStream) { s.FPS = 30 }), claimSupersede, causeStreamDiffers, ""},
		{"fps higher", running, with(func(s *claimStream) { s.FPS = 120 }), claimSupersede, causeFPSHigher, ""},
		{"size larger", running, with(func(s *claimStream) { s.Width, s.Height = 3840, 2160 }), claimSupersede, causeSizeLarger, ""},
		{"codec", running, with(func(s *claimStream) { s.Codec, s.H264Profile = "h264", "constrained-baseline" }), claimAdopt, "", "codec,h264_profile"},
		{"codec, old agent", old, with(func(s *claimStream) { s.Codec = "h264" }), claimSupersede, causeStreamDiffers, ""},
		{"mic first", running, with(func(s *claimStream) { s.Mic, s.Width = false, 3840 }), claimSupersede, causeMicDiffers, ""},
		{"warming waits", at(func(k *parkedFacts) { k.State = StateStarting }), park, claimWarming, "", ""},
		{"warming with a check-4 failure", at(func(k *parkedFacts) { k.State = StateStarting }), with(func(s *claimStream) { s.FPS = 120 }), claimSupersede, causeNotRunning, ""},
		{"stopping", at(func(k *parkedFacts) { k.State = StateStopping }), park, claimSupersede, causeNotRunning, ""},
		{"expired before host", at(func(k *parkedFacts) { k.InDeadline, k.HostUp = false, false }), park, claimSupersede, causeExpired, ""},
		{"host down", at(func(k *parkedFacts) { k.HostUp = false }), park, claimSupersede, causeHostUnavailable, ""},
		{"check 1 has no cause", at(func(k *parkedFacts) { k.Family = false }), park, claimSupersede, "", ""},
		{"claimed by a racing launch", at(func(k *parkedFacts) { k.State, k.Claimed = StateStopped, true }), park, claimLost, "", ""},
		{"stopped by expiry or eviction", at(func(k *parkedFacts) { k.State = StateStopped }), park, claimSupersede, causeNotRunning, ""},
		{"codec gated", at(func(k *parkedFacts) { k.CodecGated = true }), park, claimSupersede, causeCodecUnsupported, ""},
		{"codec gated after fps", at(func(k *parkedFacts) { k.CodecGated = true }), with(func(s *claimStream) { s.FPS = 120 }), claimSupersede, causeFPSHigher, ""},
		{"codec gated after expiry", at(func(k *parkedFacts) { k.CodecGated, k.InDeadline = true, false }), park, claimSupersede, causeExpired, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := decideClaim(tc.k, tc.launch)
			if v.Outcome != tc.outcome || v.Cause != tc.cause || strings.Join(v.Restream, ",") != tc.restream {
				t.Errorf("decideClaim = %+v, want outcome %d cause %q restream %q", v, tc.outcome, tc.cause, tc.restream)
			}
		})
	}
}
