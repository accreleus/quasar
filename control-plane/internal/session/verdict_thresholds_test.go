package session

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// The golden threshold file is the SPEC; the Go consts are a copy of it. This
// test is the thing that makes that true — change a number in one place and the
// build goes red, instead of two consumers quietly disagreeing about what
// "healthy" means (the ST-09 problem statement).
//
// The web half of the same file is checked by web/src/webrtc/thresholds.test.ts.

type goldenThreshold struct {
	Value    float64 `json:"value"`
	Unit     string  `json:"unit"`
	Why      string  `json:"why"`
	Consumer string  `json:"consumer"`
}

type goldenThresholds struct {
	Version    string                     `json:"version"`
	Thresholds map[string]goldenThreshold `json:"thresholds"`
}

func thresholdsPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..",
		"docs", "session-trace", "thresholds.json")
}

func loadGoldenThresholds(t *testing.T) goldenThresholds {
	t.Helper()
	data, err := os.ReadFile(thresholdsPath(t))
	if err != nil {
		t.Fatalf("read docs/session-trace/thresholds.json: %v", err)
	}
	var g goldenThresholds
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("parse thresholds.json: %v", err)
	}
	if g.Version == "" {
		t.Fatal("thresholds.json has no version")
	}
	if len(g.Thresholds) == 0 {
		t.Fatal("thresholds.json has no thresholds")
	}
	return g
}

func TestThresholdsVersionMatchesGoldenFile(t *testing.T) {
	g := loadGoldenThresholds(t)
	if g.Version != thresholdsVersion {
		t.Fatalf("thresholdsVersion = %q but docs/session-trace/thresholds.json says %q — "+
			"bump both together, or a stored verdict cannot be read against the thresholds "+
			"it was computed under", thresholdsVersion, g.Version)
	}
}

func TestGoThresholdsMatchGoldenFile(t *testing.T) {
	g := loadGoldenThresholds(t)

	want := map[string]float64{
		"classifier.hitch_sd_ms":           hitchSdThresholdMs,
		"classifier.encoder_ceiling_ms":    encoderCeilingMs,
		"classifier.congestion_loss_delta": congestionLossDelta,
		"classifier.congestion_rtt_p95_ms": congestionRttP95Ms,
		"classifier.min_host_fps":          classifierMinHostFps,
		"classifier.hitch_min_samples":     hitchMinSamples,
		"classifier.warmup_exclude_s":      warmupExcludeS,

		"health.network_degrading_after_s": healthNetworkDegradingAfter.Seconds(),
		"health.abr_at_floor_after_s":      healthABRAtFloorAfter.Seconds(),
		"health.unsustainable_after_s":     healthUnsustainableAfter.Seconds(),

		"client_health.degrade_sustain_s": clientDegradeSustainFor.Seconds(),
		"client_health.smooth_sustain_s":  clientSmoothSustainFor.Seconds(),
	}

	for name, code := range want {
		golden, ok := g.Thresholds[name]
		if !ok {
			t.Errorf("threshold %q is used by the control plane but absent from "+
				"docs/session-trace/thresholds.json", name)
			continue
		}
		if math.Abs(golden.Value-code) > 1e-9 {
			t.Errorf("threshold %q: code has %v, thresholds.json has %v — "+
				"the golden file is the spec; change both or neither",
				name, code, golden.Value)
		}
		if golden.Why == "" || golden.Consumer == "" {
			t.Errorf("threshold %q must carry a one-line why and its consumer", name)
		}
	}
}

// Every web-owned entry must still exist under a `web.` prefix, so deleting one
// from the golden file (and quietly re-hardcoding it in a .ts file) fails here
// even when nobody runs the vitest suite.
func TestGoldenFileStillCarriesWebThresholds(t *testing.T) {
	g := loadGoldenThresholds(t)
	required := []string{
		"web.client_health.present_sd_degraded_ms",
		"web.client_health.present_p95_degraded_ms",
		"web.client_health.freeze_degraded_count",
		"web.client_health.decode_budget_fraction",
		"web.client_health.decode_abs_ceiling_ms",
		"web.playout.default_ms",
		"web.playout.floor_ms",
		"web.playout.cap_ms",
		"web.playout.healthy_sd_ms",
		"web.playout.degraded_sd_ms",
		"web.signal.sd_poor_ms",
		"web.signal.sd_fair_ms",
		"web.signal.sd_excellent_ms",
		"web.signal.packets_lost_poor",
		"web.signal.packets_lost_fair",
		"web.clock.repost_interval_s",
		"web.clock.repost_delta_ms",
	}
	for _, name := range required {
		if _, ok := g.Thresholds[name]; !ok {
			t.Errorf("threshold %q missing from docs/session-trace/thresholds.json", name)
		}
	}
}

// Guard the one relationship between the two halves that is load-bearing: the
// browser's connection glyph and the receiver playout controller both act on
// present-interval σ, and they must not disagree about where "degraded" is.
func TestSignalAndPlayoutAgreeOnSigmaBands(t *testing.T) {
	g := loadGoldenThresholds(t)
	pairs := [][2]string{
		{"web.signal.sd_poor_ms", "web.playout.degraded_sd_ms"},
		{"web.signal.sd_fair_ms", "web.playout.healthy_sd_ms"},
	}
	for _, p := range pairs {
		a, b := g.Thresholds[p[0]].Value, g.Thresholds[p[1]].Value
		if a != b {
			t.Errorf("%s = %v but %s = %v — the glyph and the controller read the same "+
				"present σ and must not disagree about it", p[0], a, p[1], b)
		}
	}
}

// Sanity: the duration consts really are durations (a refactor to int seconds
// would silently divide by a billion above).
func TestHealthThresholdsAreDurations(t *testing.T) {
	if healthNetworkDegradingAfter < time.Second {
		t.Fatalf("healthNetworkDegradingAfter = %v, expected a duration", healthNetworkDegradingAfter)
	}
}
