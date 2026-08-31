package session

// Go half of `docs/session-trace/thresholds.json`; `verdict_thresholds_test.go`
// fails if the two disagree. Web half: `web/src/webrtc/thresholds.ts` +
// `thresholds.test.ts`. thresholdsVersion is reported on every Verdict as
// `thresholds_version` — bump it in BOTH places on any value change below.
const thresholdsVersion = "2026-08-23.4"

// Derived-window detection thresholds (ST-06 v1 hypothesis).
const (
	// hitchSdThresholdMs: present_interval_sd above this is a "hitch" (#108;
	// ≥18 ms is the degraded band from AS-04/AS-05).
	hitchSdThresholdMs = 18.0
	// encoderCeilingMs: 60 fps budget is 16.7 ms; hermes Renoir VCN sits at
	// p50 ~19 ms → a 45 fps ceiling (CLAUDE.md).
	encoderCeilingMs = 16.0
	// congestionLossDelta: packets_lost rise over the window past this is the
	// loss half of congestion (delta = last−first).
	congestionLossDelta = 5.0
	// congestionRttP95Ms: rtt p95 above this is the latency half.
	congestionRttP95Ms = 50.0
)

const (
	// classifierMinHostFps: "host fps steady" guard; 50 fps tolerates the
	// hermes ~45 fps VCN ceiling.
	classifierMinHostFps = 50.0
)

// hitchMinSamples: was a MAX; on 2026-08-23 a healthy 300 s session flipped to
// likely_client_presentation_limit on one 18.058 ms outlier of 54. Two makes
// the claim about the window, not one sample.
const hitchMinSamples = 2

// warmupExcludeS: samples this long after RUNNING are excluded from the
// warm-up-sensitive rules (hitch detection, encoder.fps floor —
// aligned.go warmupSensitiveSeries). 20 s covers the observed ramp without
// swallowing a real early fault, which still shows up after it.
const warmupExcludeS = 20.0
