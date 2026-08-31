// Package tier implements the Adaptive Streaming tier ladder (AS-01).
//
// The ladder is a static, versioned, in-code table (not in the database) —
// it changes with releases, not at runtime. Playout0Ms values are derived
// from the AS-04 playout sweep (docs/completed/adaptive-streaming/AS-04-playout-sweep.md).
//
// Design: docs/completed/adaptive-streaming/AS-00-design.md §Component 1.
package tier

// Probe captures the device/network measurements from a user_devices
// capabilities probe. Zero values indicate the field was not measured.
type Probe struct {
	BandwidthKbps   int32 // measured available downstream bandwidth
	RTTMs           int32 // measured round-trip time in milliseconds
	MaxDecodeHeight int32 // maximum decode resolution height the device supports
}

// Tier describes one rung of the streaming quality ladder.
type Tier struct {
	// Name is the canonical identifier used in logs and admin drill-downs.
	Name string
	// Width and Height are the compositor/encoder output dimensions.
	Width  int32
	Height int32
	// FPS is the target frame rate.
	FPS int32
	// BitrateKbps is the nominal CBR encoder bitrate.
	BitrateKbps int32
	// ABRFloorKbps is the minimum bitrate the ABR governor (AS-03) will descend to
	// within this tier.
	ABRFloorKbps int32
	// Playout0Ms is the initial jitter-buffer playout delay applied at session
	// start. Derived from AS-04 playout sweep; see AS-04-playout-sweep.md.
	Playout0Ms int32
}

// Ladder is the ordered tier table, highest quality first.
// Select() walks this slice top-down; the first eligible tier wins.
//
// Playout0Ms values are measured (AS-04 sweep); bitrate values remain
// provisional pending a dedicated bitrate sweep.
//
// The design doc table (AS-00 §Component 1) is the authoritative source;
// this slice must stay in sync with it.
var Ladder = []Tier{
	{
		// Confirmed low-latency LAN only. Playout0Ms=50, not AS-04's 30: the T2
		// playout-knee sweep found the smoothness/drop knee at 50ms — 4.7x fewer
		// client-present drops for +24ms g2g, and it removes a bimodal ~9% drop
		// mode (docs/reports/2026-08-18-overnight-optimisation/t2-playout-knee.md).
		Name: "1080p60-lan", Width: 1920, Height: 1080, FPS: 60,
		BitrateKbps: 12000, ABRFloorKbps: 4000, Playout0Ms: 50,
	},
	{
		// The default tier, and the no-probe fallback. Playout0Ms=50: AS-04 shows
		// σ<12ms at 30ms on netem30; 50ms adds headroom for real-browser vsync
		// quantization headless RVFC never captures.
		Name: "1080p60", Width: 1920, Height: 1080, FPS: 60,
		BitrateKbps: 8000, ABRFloorKbps: 2500, Playout0Ms: 50,
	},
	{
		// Playout0Ms=75: AS-04 σ<12ms at 30ms on netem60; headroom for
		// real-browser vsync on higher-RTT paths.
		Name: "720p60", Width: 1280, Height: 720, FPS: 60,
		BitrateKbps: 5000, ABRFloorKbps: 1500, Playout0Ms: 75,
	},
	{
		// Floor tier, always eligible. Playout0Ms=100: AS-04 loss condition makes
		// σ 27-32ms regardless of playout; 100ms is the conservative floor.
		Name: "720p30", Width: 1280, Height: 720, FPS: 30,
		BitrateKbps: 3000, ABRFloorKbps: 1000, Playout0Ms: 100,
	},
}

// defaultTier is the tier used when no probe is available (or all probes are
// stale). It must match today's hardcoded session behavior (1080p60/8000)
// so AS-02 is a no-op until a probe is present.
var defaultTier = Ladder[1] // "1080p60"

// headroomFactor is the multiplier applied to a tier's bitrate to compute the
// minimum probe bandwidth for eligibility.
// Congestion control needs headroom to probe upward; a pipe sized exactly to
// the bitrate oscillates. Value: 1.5× (AS-00 §Component 1 headroom rule).
const headroomFactor = 1.5

// eligible reports whether p satisfies t's requirements (AS-00 §Component 1):
// the 1.5x headroom rule plus the per-tier thresholds below. A zero-value
// probe field means "not measured" and is not checked; 720p30 is the floor and
// always eligible.
func eligible(p Probe, t Tier) bool {
	if t.Name == "720p30" {
		return true
	}

	if p.BandwidthKbps > 0 {
		required := int32(float64(t.BitrateKbps) * headroomFactor)
		if p.BandwidthKbps < required {
			return false
		}
	}

	switch t.Name {
	case "1080p60-lan":
		// Requires confirmed LAN: both measurements must be present.
		if p.BandwidthKbps == 0 || p.RTTMs == 0 {
			return false
		}
		if p.BandwidthKbps < 25000 || p.RTTMs > 10 {
			return false
		}
	case "1080p60":
		if p.BandwidthKbps > 0 && p.BandwidthKbps < 12000 {
			return false
		}
		if p.MaxDecodeHeight > 0 && p.MaxDecodeHeight < 1080 {
			return false
		}
	case "720p60":
		if p.BandwidthKbps > 0 && p.BandwidthKbps < 8000 {
			return false
		}
	}
	return true
}

// Select intersects probe p against the ladder top-down and returns the first
// eligible tier. The floor tier (720p30) is always eligible, so Select never
// returns a zero-value Tier.
//
// Zero-value probe fields ("not measured") skip the corresponding requirement
// checks — an unmeasured bandwidth field does not disqualify a tier.
func Select(p Probe) Tier {
	for _, t := range Ladder {
		if eligible(p, t) {
			return t
		}
	}
	// Should never be reached: 720p30 is always eligible.
	return Ladder[len(Ladder)-1]
}

// Default returns the tier that applies when no probe is available (or all
// probes are stale). This is the "today's behavior" tier: 1080p60 / 8000 kbps /
// 100 ms playout. Behavior is unchanged from before AS-02 until a fresh probe
// is present.
func Default() Tier {
	return defaultTier
}
