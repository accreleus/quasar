// rung.go — the launch walks a launch profile's rungs in position order and
// takes the first that survives every clamp. A rung carries its own resolution
// and bitrate, so falling through one changes resolution as well as codec.
//
// Placement is codec-blind; the rung resolves POST-placement. In launcher.go:
//
//	pre-schedule : the session is created from the chain's TOP rung, so admission
//	               is evaluated against the worst case and can never admit a
//	               session it would have refused. The probe envelope is applied to
//	               that bitrate BEFORE insert, so no admission decision is taken
//	               on an inflated number.
//	post-schedule: (1) resolveRung over the selected chain; (2) cert cap, keyed on
//	               THAT rung, which may swap in a lower launch profile via
//	               lowerProfileRung; (3) re-resolve over the capped chain if it
//	               did; (4) ONE UpdateSessionStream write. Both the cap lookup and
//	               the write use launcher.go's rungStream bitrate.
//
// The cap must run second (migration 0041): a certification row is keyed on the
// rung, so it cannot be consulted before the walk names one. The walk reads no
// cert input, so the ordering cannot change which rung a chain produces.
//
// Re-applying the envelope in step 3 is not optional: applyEnvelopeToBitrate
// only lowers, so twice is safe, but skipping it lets a fall-through to a lower
// rung restore an unclamped bitrate.
package session

import (
	"errors"
	"fmt"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// ErrRungCodecNotAvailable: a stream.codec override names a codec no rung of the
// selected launch profile uses. 400, distinct from ErrCodecUnsupportedByHost (409).
var ErrRungCodecNotAvailable = errors.New("launch profile has no rung with the requested codec")

// ErrLaunchProfileEmpty is unreachable via write-time validation and migration
// 0036's fan-out; the resolver refuses rather than dispatching nothing.
var ErrLaunchProfileEmpty = errors.New("launch profile has no rungs")

// Rung-rejection reasons. Wire values: the `codec_decision.considered[].rejected_by`
// enum in control-api.md. Renaming one is a contract change.
const (
	rejectHostEncoder     = "host_encoder"     // clamp 1
	rejectClientDecode    = "client_decode"    // clamp 2/3 (codec)
	rejectDecodeHeight    = "decode_height"    // clamp 2/3 (resolution)
	rejectDecodeHistory   = "decode_history"   // clamp 4
	rejectHardwareEncoder = "hardware_encoder" // clamp 5
	// clamp 6 (#506): host advertises the codec but reported sustained throughput
	// below the rung's pixel rate.
	rejectEncoderThroughput = "encoder_throughput" // clamp 6
	rejectUnknownCodec      = "unknown_codec"      // malformed rung (defensive)
)

// throughputMargin is exactly 1.0, no safety factor: the host's reported rate is
// already an upper bound (idle compositor, fastest silicon in its class), so a
// margin would reject rungs that run and each clamp-6 rejection costs a real
// tier. Under-rejecting is correct — a marginally short codec still streams,
// back-pressured. Raise it only against measured evidence, recorded here.
const throughputMargin = 1.0

// rungPixelRateMpixS is what a launch demands of an encoder, in Mpix/s (the
// agent's unit, agent-api.md `capacity.codec_throughput`).
//
// It measures the launch-EFFECTIVE size, never the rung's nominal one, hence the
// override: judging h265 against a 1440p120 rung's 442 when the session was sized
// down to 720p60 (55) substitutes the codec on a session that never runs that
// big, and an override that GROWS a launch to 2160p60 must clamp even though the
// rung says 124. dispatchDims (stream_plan.go) is the single source of the three
// numbers, shared with rungStream, so the clamp and the dispatch cannot disagree.
func rungPixelRateMpixS(r profile.Profile, ov StreamOverride) float64 {
	w, h, fps := dispatchDims(r, ov)
	if w <= 0 || h <= 0 || fps <= 0 {
		return 0
	}
	return float64(w) * float64(h) * float64(fps) / 1e6
}

// encoderTooSlow is clamp 6. It abstains (false) whenever anything is unknown:
// no hint map, no entry for the codec, a non-positive rate, or a launch with no
// resolution/fps. Never refuse a codec there is no measurement for.
func encoderTooSlow(wire string, r profile.Profile, host hostEncoderCaps, ov StreamOverride) bool {
	rate, ok := host.PixelRates[wire]
	if !ok || rate <= 0 {
		return false
	}
	need := rungPixelRateMpixS(r, ov)
	if need <= 0 {
		return false
	}
	return rate < need*throughputMargin
}

// rungVerdict keeps the three dispatch routes distinguishable: won on merit
// (Selected, no Reject, not Bypassed); clamp-0 override (Selected, no Reject,
// Bypassed); floor (Selected, Bypassed, and Reject STILL carrying the clamp that
// rejected it during the walk — recording that as a clean pass would misreport a
// rung dispatched despite being rejected).
type rungVerdict struct {
	ID       string
	Codec    string // wire vocabulary
	Reject   string // "" when accepted; a floor-selected rung keeps its reason
	Selected bool   // actually dispatched
	Bypassed bool   // dispatched without surviving the chain; never on a walk winner
}

// rungDecision is the inputs and outcome of a resolveRung run. applyPostPlacement
// marshals it (codec_decision.go) into sessions.codec_decision, which the session
// object exposes as `codec_decision`.
type rungDecision struct {
	Override   string        // explicit admin/diagnostic wire codec, if any
	Considered []rungVerdict // every rung walked, in position order
	ResultRung string        // the dispatched rung id
	Result     string        // the dispatched wire codec
	// Floor is true when NO rung survived the clamp chain and the unconditional
	// h264 floor fired (see resolveRung's doc).
	Floor bool
}

// hostEncoderCaps is the placed host's encoder capability so far as it is known.
// Known is false until the agent reports resolved runtime settings, and clamp 5
// is then skipped (unknown allows), matching eligibility's HostCaps.
type hostEncoderCaps struct {
	Known           bool
	HardwareEncoder bool
	// Per-codec sustained throughput in Mpix/s, keyed by wire codec. Absence IS
	// the unknown, per codec: nil map, missing key and non-positive rate all mean
	// do not clamp, which is why Store.HostCodecPixelRates drops non-positive
	// entries.
	PixelRates map[string]float64
}

// resolveRung walks a launch profile's rungs in position order and returns the
// first that survives every clamp.
//
//	clamp 0 : stream.codec override — takes the FIRST rung with that codec; none
//	          ⇒ ErrRungCodecNotAvailable (400); host cannot encode it ⇒
//	          ErrCodecUnsupportedByHost (409, never overridable). It bypasses
//	          clamps 2/3, 4, 5 and 6, honouring only clamp 1 — that is the forced
//	          re-test path for a previously-failed codec on a since-fixed encoder.
//	clamp 1 : host encoder set (an agent reporting nothing is h264-only).
//	clamp 2/3: client decode probe — h265/av1 need an explicit true (stale or
//	          absent probe means no); also rejects a rung whose MinDecodeHeight
//	          exceeds the probe's measured decode height, since a chain may hold
//	          both a 4K and a 1080p rung.
//	clamp 4 : decode-failure history, keyed by rung id (the caller folds in the
//	          legacy launch-profile-level rows — Store.RungFailures).
//	clamp 5 : HardwareEncoderRequired vs a host KNOWN to have no hardware encoder.
//	clamp 6 : encoder throughput (#506) — pixel rate vs what the host reports it
//	          sustains for that codec. Last, because the clamps before it ask
//	          whether the codec can be produced or decoded at all; a rung failing
//	          one of those should say so, not report a throughput shortfall on a
//	          codec it was never going to use. No hint ⇒ no rejection.
//
// Clamp 6 exists because a host advertises h265 without knowing its HEVC
// throughput is a third of its H.264 (measured, RTX 5090: vulkanh265enc ~395
// Mpix/s vs vulkanh264enc ~1400). 1440p120 needs 442 and 2160p60 needs 498, so
// those tiers ran below tier invisibly: a saturated encoder back-pressures the
// compositor instead of dropping frames, so every health signal looked fine.
//
// Floor: with no survivor the LAST h264 rung is dispatched, bypassing every
// clamp including its own HardwareEncoderRequired — eligibility already approved
// the profile, and a session that resolves nothing is a failed launch rather
// than a degraded one. A chain with no h264 rung (hand-edited data) dispatches
// its last rung instead of failing.
//
// ov is carried whole: `ov.Codec` is clamp 0's hatch, `ov.Width/Height/FPS` size
// the launch clamp 6 measures, so a size override changes what clamp 6 asks
// rather than bypassing it.
func resolveRung(
	rungs []profile.Profile,
	hostCodecs []string,
	host hostEncoderCaps,
	dp *DeviceProbe,
	failedRungs map[string]bool,
	ov StreamOverride,
) (profile.Profile, rungDecision, error) {
	if len(rungs) == 0 {
		return profile.Profile{}, rungDecision{}, ErrLaunchProfileEmpty
	}
	override := ov.codecOverride()
	hostSet := codecSet(hostCodecs)
	dec := rungDecision{Override: override, Considered: make([]rungVerdict, 0, len(rungs))}

	// --- clamp 0: explicit admin/diagnostic codec override -------------------
	if override != "" {
		for _, r := range rungs {
			wire, ok := catalogToWire(r.Codec)
			if !ok || wire != override {
				continue
			}
			if !hostSet[wire] {
				dec.Considered = append(dec.Considered, rungVerdict{ID: r.ID, Codec: wire, Reject: rejectHostEncoder})
				return profile.Profile{}, dec, ErrCodecUnsupportedByHost
			}
			// Bypassed, not passed: the override skipped clamps 2/3, 4 and 5.
			dec.Considered = append(dec.Considered, rungVerdict{ID: r.ID, Codec: wire, Selected: true, Bypassed: true})
			dec.ResultRung, dec.Result = r.ID, wire
			return r, dec, nil
		}
		return profile.Profile{}, dec, fmt.Errorf("%w: %s", ErrRungCodecNotAvailable, override)
	}

	// --- the walk ------------------------------------------------------------
	for _, r := range rungs {
		wire, ok := catalogToWire(r.Codec)
		if !ok {
			dec.Considered = append(dec.Considered, rungVerdict{ID: r.ID, Codec: string(r.Codec), Reject: rejectUnknownCodec})
			continue
		}
		v := rungVerdict{ID: r.ID, Codec: wire}
		switch {
		case !hostSet[wire]:
			v.Reject = rejectHostEncoder
		case !deviceAccepts(wire, dp):
			v.Reject = rejectClientDecode
		case dp != nil && dp.MaxDecodeHeight > 0 && r.MinDecodeHeight > dp.MaxDecodeHeight:
			v.Reject = rejectDecodeHeight
		case failedRungs[r.ID]:
			v.Reject = rejectDecodeHistory
		case r.HardwareEncoderRequired && host.Known && !host.HardwareEncoder:
			v.Reject = rejectHardwareEncoder
		case encoderTooSlow(wire, r, host, ov):
			v.Reject = rejectEncoderThroughput
		}
		if v.Reject == "" {
			v.Selected = true
			dec.Considered = append(dec.Considered, v)
			dec.ResultRung, dec.Result = r.ID, wire
			return r, dec, nil
		}
		dec.Considered = append(dec.Considered, v)
	}

	// The unconditional floor. Reaching here means every rung got a verdict (the
	// loop returns early only on an accept, and every branch appends), so
	// Considered[i] is rungs[i] — the index alignment markFloorSelected needs.
	dec.Floor = true
	for i := len(rungs) - 1; i >= 0; i-- {
		if rungs[i].Codec == profile.CodecH264 {
			dec.ResultRung, dec.Result = rungs[i].ID, wireCodecH264
			markFloorSelected(&dec, i)
			return rungs[i], dec, nil
		}
	}
	last := len(rungs) - 1
	wire, ok := catalogToWire(rungs[last].Codec)
	if !ok {
		wire = wireCodecH264
	}
	dec.ResultRung, dec.Result = rungs[last].ID, wire
	markFloorSelected(&dec, last)
	return rungs[last], dec, nil
}

// markFloorSelected stamps the floor-dispatched rung selected-and-bypassed
// without clearing its Reject, so the record still says which clamp rejected it.
// Index-addressed: Considered is position-aligned with rungs after the walk.
func markFloorSelected(dec *rungDecision, i int) {
	if i < 0 || i >= len(dec.Considered) {
		return
	}
	dec.Considered[i].Selected = true
	dec.Considered[i].Bypassed = true
}
