// probe_envelope.go — SPT-07: the stored login-time capability probe
// (user_devices.capabilities) turned into a conservative launch envelope. It
// only ever LOWERS from the resolved tier/rung ceiling, and is never applied to
// an admin or explicit-override launch. A missing, stale or unmeasured probe
// leaves every field at its resolved default.
//
// Staleness uses probeMaxAgeDays (store.go), the same cut the tier selector and
// probe reader use. Unmeasured means BandwidthKbps == 0 (absent, or sanitized
// from a bogus sub-100 kbps probe by the #146 guard) or RTTMs == 0.
//
// The max_decode_height and codec bounds are the eligibility gate's job upstream,
// not duplicated here.
package session

// safe_ceiling = measured_bandwidth * safeCeilingFactor. 0.70 is mid-range of
// §A.4 (0.65-0.75) and leaves ABR a 30% upward probe window before the physical
// pipe ceiling.
const safeCeilingFactor = 0.70

// RTT class thresholds, ms.
const (
	rttLANMs   = 15 // <= 15 ms: LAN, no playout0 bump
	rttMetroMs = 50 // 16-50 ms: metro/regional WAN; above that, hostile WAN
)

// playout0 bumps on top of the rung's AS-04-measured baseline. Metro adds 25 ms
// (mid-point between LAN and the hostile hop); hostile adds 50 ms, one
// jitter-buffer standard deviation of headroom per the AS-04 loss findings.
const (
	rttMetroPlayout0Bump   = int32(25)
	rttHostilePlayout0Bump = int32(50)
)

// ProbeEnvelope is the constraints derived from the login probe. A zero field is
// no constraint: the caller's resolved value stands.
type ProbeEnvelope struct {
	// SafeCeilingKbps > 0 is the bitrate this link sustains conservatively; the
	// caller clamps down to it, never up.
	SafeCeilingKbps int32
	// Playout0BumpMs is added to the initial playout delay for the RTT class.
	Playout0BumpMs int32
}

// buildProbeEnvelope derives the envelope. A nil or fully unmeasured probe gives
// a zero envelope. The caller must skip it for admin or explicit-override launches.
func buildProbeEnvelope(dp *DeviceProbe) ProbeEnvelope {
	if dp == nil {
		return ProbeEnvelope{}
	}
	var env ProbeEnvelope

	if dp.BandwidthKbps > 0 {
		env.SafeCeilingKbps = int32(float64(dp.BandwidthKbps) * safeCeilingFactor)
	}

	if dp.RTTMs > 0 {
		switch {
		case dp.RTTMs <= rttLANMs:
			// LAN: no bump, keep the rung's measured value.
		case dp.RTTMs <= rttMetroMs:
			env.Playout0BumpMs = rttMetroPlayout0Bump
		default:
			env.Playout0BumpMs = rttHostilePlayout0Bump
		}
	}

	return env
}

// applyEnvelopeToBitrate clamps by the safe ceiling; it only ever lowers.
func applyEnvelopeToBitrate(bitrateKbps int32, env ProbeEnvelope) int32 {
	if env.SafeCeilingKbps <= 0 {
		return bitrateKbps
	}
	if bitrateKbps > env.SafeCeilingKbps {
		return env.SafeCeilingKbps
	}
	return bitrateKbps
}

// applyEnvelopeToPlayout0 adds the RTT-class bump to the baseline.
func applyEnvelopeToPlayout0(playout0Ms int32, env ProbeEnvelope) int32 {
	return playout0Ms + env.Playout0BumpMs
}
