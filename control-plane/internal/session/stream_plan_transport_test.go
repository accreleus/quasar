package session

// The transport choice of the stream plan (control-api.md §Transport — WebRTC and WebTransport):
// pure table tests, no database.

import (
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/profile"
	"github.com/accreleus/quasar/control-plane/internal/wtendpoint"
)

// wtProbe is a fresh probe that proves everything the WebTransport path needs for h264 + opus.
func wtProbe() *DeviceProbe {
	return &DeviceProbe{
		WebTransport: true,
		WebCodecs:    map[string]bool{"h264": true, "hevc": true, "av1": true, "opus_decode": true},
	}
}

func wtInputs(profileTransport, requested string, endpoint bool, dp *DeviceProbe) StreamInputs {
	c := chain("1080p60", rung("1080p60-h264", profile.CodecH264, 1920, 1080, 60, 8000))
	c.Transport = profileTransport
	var ep *wtendpoint.Endpoint
	if endpoint {
		ep = &wtendpoint.Endpoint{URL: "https://192.0.2.10:4433/wt", Mode: wtendpoint.ModeDirect, CertHashes: []wtendpoint.Hash{}}
	}
	return StreamInputs{
		HostID:     strptr("host-1"),
		GPUIndex:   i32(0),
		Chain:      c,
		GPUCodecs:  []string{"h264", "h265", "av1"},
		Probe:      dp,
		Transport:  TransportInputs{Requested: requested, Endpoint: ep},
		Now:        fixedNow,
		CertMaxAge: CertStaleness,
	}
}

func TestPlanStreamTransport(t *testing.T) {
	noOpus := wtProbe()
	noOpus.WebCodecs["opus_decode"] = false
	noH264 := wtProbe()
	noH264.WebCodecs["h264"] = false
	noWT := wtProbe()
	noWT.WebTransport = false

	tests := []struct {
		name       string
		in         StreamInputs
		want       string
		wantReason string
	}{
		{"webrtc profile, no request", wtInputs("webrtc", "", true, wtProbe()), "webrtc", ""},
		{"empty profile transport reads as webrtc", wtInputs("", "", true, wtProbe()), "webrtc", ""},
		{"webrtc profile refuses a request for webtransport", wtInputs("webrtc", "webtransport", true, wtProbe()), "webrtc", "profile_disallows"},
		{"opt-in profile, no request", wtInputs("webtransport_opt_in", "", true, wtProbe()), "webrtc", ""},
		{"opt-in profile, asked for webrtc", wtInputs("webtransport_opt_in", "webrtc", true, wtProbe()), "webrtc", ""},
		{"opt-in profile, asked for webtransport", wtInputs("webtransport_opt_in", "webtransport", true, wtProbe()), "webtransport", ""},
		{"webtransport profile, no request", wtInputs("webtransport", "", true, wtProbe()), "webtransport", ""},
		{"webtransport profile, asked for webtransport", wtInputs("webtransport", "webtransport", true, wtProbe()), "webtransport", ""},
		{"webtransport profile, asked for webrtc", wtInputs("webtransport", "webrtc", true, wtProbe()), "webrtc", ""},

		{"wanted but the host has no usable endpoint", wtInputs("webtransport", "", false, wtProbe()), "webrtc", "host_no_endpoint"},
		{"wanted but no probe", wtInputs("webtransport", "", true, nil), "webrtc", "device_no_webtransport"},
		{"wanted but the device lacks webtransport", wtInputs("webtransport", "", true, noWT), "webrtc", "device_no_webtransport"},
		{"wanted but the device cannot decode the codec", wtInputs("webtransport", "", true, noH264), "webrtc", "device_no_webcodecs"},
		{"wanted but the device cannot decode opus", wtInputs("webtransport", "", true, noOpus), "webrtc", "device_no_webcodecs"},
		{"endpoint is checked before the device", wtInputs("webtransport", "", false, noWT), "webrtc", "host_no_endpoint"},
		{"a refused request on a webrtc profile reports the profile, not the host", wtInputs("webrtc", "webtransport", false, nil), "webrtc", "profile_disallows"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planStream(tc.in)
			if err != nil {
				t.Fatalf("planStream: %v", err)
			}
			if plan.Update.Transport != tc.want || plan.Update.TransportReason != tc.wantReason {
				t.Errorf("transport = %q (reason %q), want %q (reason %q)",
					plan.Update.Transport, plan.Update.TransportReason, tc.want, tc.wantReason)
			}
			if plan.RungID != "1080p60-h264" || plan.Update.Codec != "h264" {
				t.Errorf("transport changed the rung or codec: %s / %s", plan.RungID, plan.Update.Codec)
			}
		})
	}
}

// The probe is checked for the session's codec, in the catalog spelling (hevc, not h265).
func TestPlanStreamTransportChecksTheSessionsCodec(t *testing.T) {
	in := wtInputs("webtransport", "", true, wtProbe())
	in.Chain.Rungs = []profile.Profile{rung("hevc", profile.CodecHEVC, 1920, 1080, 60, 8000), in.Chain.Rungs[0]}
	in.Probe.HEVC = true

	plan, err := planStream(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Update.Codec != "h265" || plan.Update.Transport != "webtransport" {
		t.Fatalf("codec/transport = %s/%s, want h265/webtransport", plan.Update.Codec, plan.Update.Transport)
	}

	in.Probe.WebCodecs["hevc"] = false
	plan, err = planStream(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Update.Codec != "h265" {
		t.Fatalf("codec = %s: the transport check must never change the codec", plan.Update.Codec)
	}
	if plan.Update.Transport != "webrtc" || plan.Update.TransportReason != "device_no_webcodecs" {
		t.Errorf("transport = %q (%q), want webrtc (device_no_webcodecs)", plan.Update.Transport, plan.Update.TransportReason)
	}
}

// A parked session has no viewer: always webrtc, with no reason.
func TestPlanStreamTransportParkedIsAlwaysWebRTC(t *testing.T) {
	in := wtInputs("webtransport", "webtransport", true, wtProbe())
	in.Params.parked = true
	plan, err := planStream(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Update.Transport != "webrtc" || plan.Update.TransportReason != "" {
		t.Errorf("parked: transport = %q (%q), want webrtc with no reason", plan.Update.Transport, plan.Update.TransportReason)
	}
}

func TestStreamPlanApplyToCarriesTheTransport(t *testing.T) {
	plan, err := planStream(wtInputs("webtransport", "", true, wtProbe()))
	if err != nil {
		t.Fatal(err)
	}
	var sess Session
	plan.applyTo(&sess)
	if sess.Transport != "webtransport" || sess.TransportReason != nil {
		t.Errorf("session transport = %q (%v), want webtransport", sess.Transport, sess.TransportReason)
	}
	plan, _ = planStream(wtInputs("webtransport", "", false, wtProbe()))
	plan.applyTo(&sess)
	if sess.Transport != "webrtc" || sess.TransportReason == nil || *sess.TransportReason != "host_no_endpoint" {
		t.Errorf("session transport = %q (%v), want webrtc / host_no_endpoint", sess.Transport, sess.TransportReason)
	}
}

func TestPlanStreamTransportCarriesTheEndpointOnlyWhenChosen(t *testing.T) {
	chosen, _ := planStream(wtInputs("webtransport", "", true, wtProbe()))
	if chosen.Endpoint == nil || chosen.Endpoint.URL == "" {
		t.Errorf("chosen plan endpoint = %+v, want the resolved endpoint", chosen.Endpoint)
	}
	refused, _ := planStream(wtInputs("webtransport_opt_in", "", true, wtProbe()))
	if refused.Endpoint != nil {
		t.Errorf("a webrtc plan carries endpoint %+v, want none", refused.Endpoint)
	}
}

func TestParseDeviceProbeWebTransportAndWebCodecs(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	full := parseDeviceProbe([]byte(`{"measured_at":"` + now + `",
		"webtransport":{"available":true,"datagrams":true},
		"webcodecs":{"h264":true,"hevc":false,"opus_decode":true}}`))
	if full == nil || !full.WebTransport || !full.WebCodecs["h264"] || full.WebCodecs["hevc"] || !full.WebCodecs["opus_decode"] {
		t.Fatalf("probe = %+v", full)
	}
	for name, caps := range map[string]string{
		"no datagrams":   `"webtransport":{"available":true,"datagrams":false}`,
		"not available":  `"webtransport":{"available":false,"datagrams":true}`,
		"wrong shape":    `"webtransport":"yes"`,
		"absent":         `"x":1`,
		"truthy strings": `"webtransport":{"available":"true","datagrams":"true"}`,
	} {
		p := parseDeviceProbe([]byte(`{"measured_at":"` + now + `",` + caps + `}`))
		if p == nil || p.WebTransport {
			t.Errorf("%s: probe = %+v, want a probe with WebTransport false", name, p)
		}
	}
	if p := parseDeviceProbe([]byte(`{"measured_at":"` + now + `","webcodecs":[1]}`)); p == nil || p.WebCodecs["h264"] {
		t.Errorf("wrong-shaped webcodecs must lose only itself, got %+v", p)
	}
}
