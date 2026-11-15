// pyrowave_plan.go — Ultra-low latency (PyroWave) in the stream plan (amendments 55 and 58):
// pure steps after the rung walk and around the transport choice. They never change the
// standard codec. semantics: control-api.md §Ultra-low latency (PyroWave) ▸ The stream plan
package session

import (
	"encoding/json"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// The launch profile's pyrowave_mode values.
const (
	pyrowaveOff    = "off"
	pyrowaveAllow  = "allow"
	pyrowavePrefer = "prefer"
)

// warnPyroWaveNeedsWebTransport: pyrowave_mode allow or prefer on a webrtc profile.
const warnPyroWaveNeedsWebTransport = "pyrowave_needs_webtransport"

// Session.pyrowave reasons (open strings on the wire).
const (
	reasonHostNoPyroWave  = "host_no_pyrowave"
	reasonDeviceNoWebGPU  = "device_no_webgpu"
	reasonGPUPyroWaveBusy = "gpu_pyrowave_busy"
	reasonNotWebTransport = "not_webtransport"

	autoNoMeasurement = "no_measurement"
	autoRTTTooHigh    = "rtt_too_high"
	autoLinkTooSlow   = "link_too_slow"

	choiceExplicit = "explicit"
	choiceAuto     = "auto"
)

const (
	pyrowaveMaxRTTMs = 5
	// networkFigureWindow is the Auto rule's own freshness window for bandwidth_kbps and rtt_ms.
	networkFigureWindow = 10 * time.Minute
	maxUsableBandwidth  = 10_000_000
)

// PyroWaveRecord is sessions.pyrowave, served as Session.pyrowave.
type PyroWaveRecord struct {
	Allowed      bool    `json:"allowed"`
	Reason       *string `json:"reason"`
	Wanted       bool    `json:"wanted"`
	Choice       *string `json:"choice"`
	AutoReason   *string `json:"auto_reason"`
	Active       bool    `json:"active"`
	Used         bool    `json:"used"`
	TargetKbps   *int32  `json:"target_kbps"`
	SwitchReason *string `json:"switch_reason"`
}

// grant is session_assign.pyrowave for an allowed session, nil otherwise. `wanted` follows
// the record, so a redispatch after the agent cleared it does not turn it back on.
func (r *PyroWaveRecord) grant() *agentws.PyroWaveGrant {
	if r == nil || !r.Allowed || r.TargetKbps == nil {
		return nil
	}
	return &agentws.PyroWaveGrant{
		Wanted:     r.Wanted,
		TargetKbps: *r.TargetKbps,
		FloorKbps:  *r.TargetKbps / 2,
		MaxRTTMs:   pyrowaveMaxRTTMs,
	}
}

func pyrowaveMode(lp profile.LaunchProfile) string {
	if lp.PyroWaveMode == "" {
		return pyrowaveOff
	}
	return lp.PyroWaveMode
}

// pyrowaveProfileAllows is step 1's first row: the mode allows it and the profile is not webrtc.
func pyrowaveProfileAllows(lp profile.LaunchProfile) bool {
	return pyrowaveMode(lp) != pyrowaveOff && lp.Transport != "" && lp.Transport != transportWebRTC
}

// isAutoLaunch: no stream.codec and no pyrowave: true.
func isAutoLaunch(lp LaunchParams, ov StreamOverride) bool {
	return !lp.PyroWave && (ov.Codec == nil || *ov.Codec == "")
}

// pyrowaveCandidate is the scheduler's slot test, made before placement (§The PyroWave
// ceiling): the profile allows PyroWave and the launch asks for it, or is Auto on prefer.
func pyrowaveCandidate(lp profile.LaunchProfile, params LaunchParams) bool {
	if params.parked || !pyrowaveProfileAllows(lp) {
		return false
	}
	return params.PyroWave || (pyrowaveMode(lp) == pyrowavePrefer && isAutoLaunch(params, params.Override))
}

// pyrowaveTargetKbps is codec-pyrowave.md §Rate's formula, the formula of record.
func pyrowaveTargetKbps(height, fps int32, maxKbps *int32) int32 {
	perFrame := int64(400_000)
	switch {
	case height <= 1080:
		perFrame = 200_000
	case height <= 1440:
		perFrame = 300_000
	}
	target := perFrame * int64(fps) / 60
	limit := int64(1_000_000)
	if maxKbps != nil {
		limit = int64(*maxKbps)
	}
	return int32(min(target, limit))
}

// pyrowaveAutoReason is the Auto rule past condition (a); "" when Auto picks PyroWave.
func pyrowaveAutoReason(p *DeviceProbe, target int32, now time.Time) string {
	if p == nil || p.MeasuredAt.IsZero() || now.Sub(p.MeasuredAt) > networkFigureWindow ||
		p.BandwidthKbps <= 0 || p.BandwidthKbps > maxUsableBandwidth || !(p.RTTMsExact > 0) {
		return autoNoMeasurement
	}
	if p.RTTMsExact > pyrowaveMaxRTTMs {
		return autoRTTTooHigh
	}
	if p.BandwidthKbps < target {
		return autoLinkTooSlow
	}
	return ""
}

// pyrowaveIntent is steps 1, 2 and 2b: what the session wants before the transport is chosen.
type pyrowaveIntent struct {
	eligible   bool
	reason     string // the first failed eligibility check, or gpu_pyrowave_busy
	wanted     bool
	choice     string
	autoReason string
	target     int32
}

// pyrowaveEligibility is step 1, shared by the launch and the Launch options preview: "" when
// eligible, else the first failed check. hostLists: the GPU (launch) or some candidate GPU
// (preview) lists pyrowave.
func pyrowaveEligibility(lp profile.LaunchProfile, hostLists bool, probe *DeviceProbe) string {
	switch {
	case !pyrowaveProfileAllows(lp):
		return reasonProfileDisallows
	case !hostLists:
		return reasonHostNoPyroWave
	case probe == nil || !probe.WebGPU:
		return reasonDeviceNoWebGPU
	}
	return ""
}

// PyroWaveOffer is a launch profile's pyrowave block on GET /v1/me/profiles.
// semantics: control-api.md §Ultra-low latency (PyroWave) ▸ Launch options
type PyroWaveOffer struct {
	Mode       string  `json:"mode"`
	Available  bool    `json:"available"`
	Reason     *string `json:"reason"`
	Auto       bool    `json:"auto"`
	AutoReason *string `json:"auto_reason"`
}

// pyrowaveOffer is the preview of an Auto launch: the same steps as pyrowaveIntent, from what is
// known before placement. height and fps are the top rung's; it never reads the per-GPU ceiling.
func pyrowaveOffer(lp profile.LaunchProfile, hostLists bool, probe *DeviceProbe, height, fps int32, now time.Time) PyroWaveOffer {
	offer := PyroWaveOffer{Mode: pyrowaveMode(lp)}
	reason := pyrowaveEligibility(lp, hostLists, probe)
	if reason == "" && !probe.WebTransport {
		reason = reasonDeviceNoWebTransport
	}
	if reason != "" {
		offer.Reason = &reason
		return offer
	}
	offer.Available = true
	if offer.Mode == pyrowavePrefer {
		if ar := pyrowaveAutoReason(probe, pyrowaveTargetKbps(height, fps, lp.PyroWaveKbps), now); ar != "" {
			offer.AutoReason = &ar
		} else {
			offer.Auto = true
		}
	}
	return offer
}

func (in StreamInputs) pyrowaveIntent(height, fps int32) pyrowaveIntent {
	it := pyrowaveIntent{target: pyrowaveTargetKbps(height, fps, in.Chain.PyroWaveKbps)}
	it.reason = pyrowaveEligibility(in.Chain, codecSet(in.GPUCodecs)[wireCodecPyroWave], in.Probe)
	if it.reason != "" {
		return it
	}
	it.eligible = true
	switch {
	case in.Params.PyroWave:
		it.wanted, it.choice = true, choiceExplicit
	case pyrowaveMode(in.Chain) == pyrowavePrefer && isAutoLaunch(in.Params, in.Override):
		it.autoReason = pyrowaveAutoReason(in.Probe, it.target, in.Now)
		it.wanted, it.choice = it.autoReason == "", choiceAuto
	}
	if it.wanted && !in.HoldsPyroWaveSlot {
		it.wanted, it.choice, it.reason = false, "", reasonGPUPyroWaveBusy
	}
	return it
}

// record is step 4 once the transport is known: the session's record, and whether
// it keeps its PyroWave slot (allowed and wanted).
func (it pyrowaveIntent) record(transport string) (PyroWaveRecord, bool) {
	rec := PyroWaveRecord{}
	if it.autoReason != "" {
		rec.AutoReason = &it.autoReason
	}
	reason := it.reason
	if it.eligible && reason == "" && transport != transportWebTransport {
		reason = reasonNotWebTransport
	}
	if reason != "" {
		rec.Reason = &reason
		return rec, false
	}
	target := it.target
	rec.Allowed, rec.TargetKbps, rec.Wanted = true, &target, it.wanted
	if it.wanted {
		choice := it.choice
		rec.Choice = &choice
	}
	return rec, it.wanted
}

func marshalPyroWave(r PyroWaveRecord) []byte {
	b, _ := json.Marshal(r)
	return b
}

// claimWithoutSlot is a claim's plan once its INSERT found the PyroWave slot taken: the
// re-plan, or, when that failed, the first plan made busy on WebRTC. Never a grant without
// a slot.
func claimWithoutSlot(first, again placedStream, err error) placedStream {
	if err == nil {
		return again
	}
	plan := *first.plan
	busy := reasonGPUPyroWaveBusy
	plan.PyroWave = PyroWaveRecord{Reason: &busy}
	plan.Update.PyroWave = marshalPyroWave(plan.PyroWave)
	plan.Update.KeepPyroWaveSlot = false
	plan.Update.Transport, plan.Update.TransportReason, plan.Endpoint = transportWebRTC, "", nil
	return placedStream{plan: &plan}
}
