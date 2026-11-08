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
	// A record posted before the WebCodecs keys existed: it proves WebTransport and nothing else.
	beforeWebCodecs := &DeviceProbe{WebTransport: true}

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
		{"wanted but the probe predates the webcodecs keys", wtInputs("webtransport", "", true, beforeWebCodecs), "webrtc", "device_no_webcodecs"},
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

// WebTransport carries h264, hevc and av1 (A18, #1043): the resolved codec no longer sends a
// session to WebRTC, and the device's own WebCodecs probe for that codec decides instead. The
// transport check never changes the codec. codec_unsupported_on_transport stays for a codec
// outside webTransportCodecs.
func TestPlanStreamTransportCarriesH264HevcAndAV1(t *testing.T) {
	tests := []struct {
		name       string
		codec      profile.Codec
		wantCodec  string
		endpoint   bool
		probe      func() *DeviceProbe
		want       string
		wantReason string
	}{
		{"h264", profile.CodecH264, "h264", true, wtProbe, "webtransport", ""},
		{"hevc", profile.CodecHEVC, "h265", true, wtProbe, "webtransport", ""},
		{"av1", profile.CodecAV1, "av1", true, wtProbe, "webtransport", ""},
		{"hevc the device cannot decode stays on webrtc", profile.CodecHEVC, "h265", true, func() *DeviceProbe {
			p := wtProbe()
			p.WebCodecs["hevc"] = false
			return p
		}, "webrtc", "device_no_webcodecs"},
		{"av1 the device cannot decode stays on webrtc", profile.CodecAV1, "av1", true, func() *DeviceProbe {
			p := wtProbe()
			p.WebCodecs["av1"] = false
			return p
		}, "webrtc", "device_no_webcodecs"},
		{"hevc without an endpoint reports the endpoint", profile.CodecHEVC, "h265", false, wtProbe, "webrtc", "host_no_endpoint"},
		{"av1 on a device without webtransport reports the device", profile.CodecAV1, "av1", true, func() *DeviceProbe {
			p := wtProbe()
			p.WebTransport = false
			return p
		}, "webrtc", "device_no_webtransport"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := wtInputs("webtransport", "", tc.endpoint, tc.probe())
			in.Chain.Rungs = []profile.Profile{rung("r", tc.codec, 1920, 1080, 60, 8000)}
			in.Probe.HEVC, in.Probe.AV1 = true, true
			plan, err := planStream(in)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Update.Codec != tc.wantCodec {
				t.Fatalf("codec = %s, want %s: the transport check must never change the codec", plan.Update.Codec, tc.wantCodec)
			}
			if plan.Update.Transport != tc.want || plan.Update.TransportReason != tc.wantReason {
				t.Errorf("transport = %q (%q), want %q (%q)", plan.Update.Transport, plan.Update.TransportReason, tc.want, tc.wantReason)
			}
			if (plan.Endpoint != nil) != (tc.want == "webtransport") {
				t.Errorf("endpoint = %+v for transport %q", plan.Endpoint, plan.Update.Transport)
			}
		})
	}
}

// A codec the transport does not carry keeps its reason; the map is the only place that says which.
func TestPlanStreamTransportKeepsTheReasonForAnUncarriedCodec(t *testing.T) {
	saved := webTransportCodecs[profile.CodecAV1]
	webTransportCodecs[profile.CodecAV1] = false
	defer func() { webTransportCodecs[profile.CodecAV1] = saved }()
	in := wtInputs("webtransport", "", true, wtProbe())
	in.Chain.Rungs = []profile.Profile{rung("r", profile.CodecAV1, 1920, 1080, 60, 8000)}
	in.Probe.HEVC, in.Probe.AV1 = true, true
	plan, err := planStream(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Update.Transport != "webrtc" || plan.Update.TransportReason != "codec_unsupported_on_transport" || plan.Update.Codec != "av1" {
		t.Errorf("got %q (%q) codec %s, want webrtc / codec_unsupported_on_transport / av1",
			plan.Update.Transport, plan.Update.TransportReason, plan.Update.Codec)
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
