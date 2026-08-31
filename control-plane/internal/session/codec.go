// codec.go — the two codec vocabularies and the capability primitives the rung
// walk (rung.go) calls. One codec per session, chosen server-side at launch and
// sent as session_assign.stream.codec (agent-api.md §3.1).
//
//   - Catalog vocabulary (profile.Codec): h264 | hevc | av1, spoken by the
//     profile catalog and its admin write path.
//   - Wire vocabulary (the *wire* consts here): h264 | h265 | av1, spoken by
//     stream.codec, sessions.codec and the host codec set.
//
// catalogToWire is the ONLY place the hevc↔h265 rename is encoded. Every other
// layer stays in one vocabulary.
package session

import "github.com/accreleus/quasar/control-plane/internal/profile"

// Wire codec strings: the schema.md sessions.codec CHECK set and the agent-api
// stream.codec / host codecs vocabulary.
const (
	wireCodecH264 = "h264"
	wireCodecH265 = "h265"
	wireCodecAV1  = "av1"
)

// catalogToWire maps a catalog profile.Codec to its wire string; ok=false for an
// unknown one.
func catalogToWire(c profile.Codec) (string, bool) {
	switch c {
	case profile.CodecH264:
		return wireCodecH264, true
	case profile.CodecHEVC:
		return wireCodecH265, true
	case profile.CodecAV1:
		return wireCodecAV1, true
	}
	return "", false
}

// codecSet builds a lookup set from a host's reported wire codec list. Empty or
// nil defaults to {h264}: an agent reporting no codecs is h264-only (§3.1.2).
func codecSet(hostCodecs []string) map[string]bool {
	set := make(map[string]bool, len(hostCodecs))
	for _, c := range hostCodecs {
		if c != "" {
			set[c] = true
		}
	}
	if len(set) == 0 {
		set[wireCodecH264] = true
	}
	return set
}

// deviceAccepts is clamps 2+3. H.264 is universal; HEVC and AV1 are hard-gated
// on an explicit probe capability, so a stale or absent probe means no. An
// undecodable codec is a black stream, not a quality drop, so uncertainty
// resolves to the H.264 floor.
func deviceAccepts(wire string, dp *DeviceProbe) bool {
	switch wire {
	case wireCodecH264:
		return true
	case wireCodecH265:
		return dp != nil && dp.HEVC
	case wireCodecAV1:
		return dp != nil && dp.AV1
	}
	return false
}
