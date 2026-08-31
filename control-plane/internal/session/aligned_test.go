package session

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// ── the cross-source claim ───────────────────────────────────────────────────
//
// A hitch is CLIENT-sourced; a host encoder fps dip is AGENT-sourced. Whether
// they coincide is the difference between "the client's present path is the
// limit" and "the host stuttered and the client faithfully showed it". Before
// alignment those two timestamps were compared as if the clocks agreed — which
// is to say the comparison was never actually made.

// skewedJudderSeries builds a window where the client reports two hitches and the
// host dips once. clientSkewMs is how far the client's clock runs BEHIND the
// host: the hitches are reported that much early, and only adding the measured
// offset puts them back on top of the dip.
func skewedJudderSeries(base int64, clientSkewMs int64) map[string][]seriesPoint {
	dipAt := base + 60_000
	hostFps := []seriesPoint{}
	for i := int64(0); i < 15; i++ {
		v := 60.0
		ts := base + i*5_000
		if ts == dipAt {
			v = 20.0 // one dip; too brief to move the whole-window p10
		}
		hostFps = append(hostFps, seriesPoint{TsUnixMs: ts, V: v})
	}
	return map[string][]seriesPoint{
		"encoder.fps":       hostFps,
		"encoder.encode_ms": pt(base, 6, 6, 6, 6, 6),
		"transport.packets_lost": {
			{TsUnixMs: base, V: 10}, {TsUnixMs: base + 70_000, V: 11},
		},
		"transport.rtt_ms": pt(base, 8, 9, 8, 9, 8),
		"client.is_hidden": pt(base, 0, 0, 0),
		"client.present_interval_sd_ms": {
			{TsUnixMs: dipAt - clientSkewMs, V: 30},
			{TsUnixMs: dipAt - clientSkewMs + 1_000, V: 26},
		},
	}
}

// alignAndClassify runs the real read-side path over a series map whose client
// points are already expressed on the client clock: it applies the offset to the
// client-sourced series exactly as normalizeSeries would, then classifies.
func alignAndClassify(t *testing.T, series map[string][]seriesPoint, offsetMs float64, measured bool) classifierVerdict {
	t.Helper()
	var clock *telemetry.Clock
	if measured {
		clock = &telemetry.Clock{ClientOffsetMs: offsetMs, UncertaintyMs: 2, MeasuredAt: time.Now()}
	}
	al := alignmentOf(telemetry.AlignSeries(nil, nil, clock))

	shifted := map[string][]seriesPoint{}
	for name, pts := range series {
		out := make([]seriesPoint, 0, len(pts))
		for _, p := range pts {
			if al.Applied && seriesSource(name) != telemetry.SourceAgent {
				p.TsUnixMs += int64(offsetMs)
			}
			out = append(out, p)
		}
		shifted[name] = out
	}
	dw := computeDerivedWindows(shifted, nil)
	return classify(classifyInputs{Series: shifted, Derived: dw, Align: al})
}

// The load-bearing test: the same data, the same rule, and the claim flips —
// but ONLY because the clock was applied. Unaligned, the classifier blames the
// client's present path; aligned, the hitches land on the host's fps dip and it
// refuses to.
func TestCrossSourceClaimFlipsOnlyWhenTheClockIsApplied(t *testing.T) {
	const base int64 = 1_000_000
	const skew = 2_500 // larger than the ±1 s coincidence tolerance

	unaligned := alignAndClassify(t, skewedJudderSeries(base, skew), skew, false)
	if unaligned.Verdict != verdictClientPresentingLimit {
		t.Fatalf("unaligned verdict = %q, want %q — with no clock the classifier cannot see the coincidence and falls back to the single-source argument",
			unaligned.Verdict, verdictClientPresentingLimit)
	}
	if !evidenceMentions(unaligned, "cross-source timing unverified (clock unmeasured)") {
		t.Errorf("unaligned evidence must SAY the timing could not be verified, got %v", unaligned.Evidence)
	}

	aligned := alignAndClassify(t, skewedJudderSeries(base, skew), skew, true)
	if aligned.Verdict == verdictClientPresentingLimit {
		t.Fatalf("aligned verdict = %q — on the aligned clock the judder sits on a host fps dip and is not the client's present path",
			aligned.Verdict)
	}
	if !evidenceMentions(aligned, "coincides with a host encoder fps dip") {
		t.Errorf("aligned evidence must name the coincidence it found, got %v", aligned.Evidence)
	}
}

// Sub-cadence skew must NOT be able to flip a claim. Both sides report about once
// a second, so 300 ms of skew is inside the noise the tolerance exists to absorb:
// aligning changes nothing, which is the correct answer, not a missed detection.
func TestSubCadenceSkewCannotFlipAClaim(t *testing.T) {
	const base int64 = 1_000_000
	const skew = 300

	unaligned := alignAndClassify(t, skewedJudderSeries(base, skew), skew, false)
	aligned := alignAndClassify(t, skewedJudderSeries(base, skew), skew, true)

	// The hitch is within the tolerance of the dip either way, so BOTH refuse the
	// client-presentation verdict — the tolerance, not the offset, decides here.
	if unaligned.Verdict != verdictClientPresentingLimit {
		t.Errorf("unaligned verdict = %q, want %q (no clock ⇒ no coincidence check at all)",
			unaligned.Verdict, verdictClientPresentingLimit)
	}
	if aligned.Verdict == verdictClientPresentingLimit {
		t.Errorf("aligned verdict = %q: a 300 ms skew is inside the ±%.0f ms tolerance, so the hitch still sits on the dip",
			aligned.Verdict, telemetry.CoincidenceWindowMs)
	}
}

func evidenceMentions(v classifierVerdict, substr string) bool {
	for _, e := range v.Evidence {
		if contains(e, substr) {
			return true
		}
	}
	return false
}

// ── warm-up exclusion ────────────────────────────────────────────────────────

func TestWarmupForNeedsAnAnchor(t *testing.T) {
	if got := warmupFor(nil, 1_000_000, 1_300_000); got.UntilMs != 0 || got.ExcludedMs != 0 {
		t.Errorf("warmupFor(nil) = %+v, want nothing excluded — there is no anchor to measure from", got)
	}
}

func TestWarmupExcludesOnlyTheHeadOfTheWindow(t *testing.T) {
	running := time.UnixMilli(1_000_000)
	w := warmupFor(&running, 1_000_000, 1_300_000)
	if w.UntilMs != 1_020_000 {
		t.Errorf("UntilMs = %d, want running+20s", w.UntilMs)
	}
	if w.ExcludedMs != 20_000 {
		t.Errorf("ExcludedMs = %d, want 20000", w.ExcludedMs)
	}

	// A window that starts long after warm-up excludes nothing.
	late := warmupFor(&running, 1_100_000, 1_400_000)
	if late.ExcludedMs != 0 {
		t.Errorf("ExcludedMs = %d on a post-warm-up window, want 0", late.ExcludedMs)
	}
}

func TestWarmupTrimsOnlyTheSensitiveSeries(t *testing.T) {
	running := time.UnixMilli(1_000_000)
	w := warmupFor(&running, 1_000_000, 1_300_000)
	series := map[string][]seriesPoint{
		"encoder.fps":                   {{TsUnixMs: 1_005_000, V: 30}, {TsUnixMs: 1_100_000, V: 60}},
		"client.present_interval_sd_ms": {{TsUnixMs: 1_005_000, V: 40}, {TsUnixMs: 1_100_000, V: 3}},
		"transport.rtt_ms":              {{TsUnixMs: 1_005_000, V: 9}, {TsUnixMs: 1_100_000, V: 9}},
	}
	got := w.assessed(series)
	if len(got["encoder.fps"]) != 1 || got["encoder.fps"][0].V != 60 {
		t.Errorf("encoder.fps = %v, want the warm-up sample dropped", got["encoder.fps"])
	}
	if len(got["client.present_interval_sd_ms"]) != 1 {
		t.Errorf("present σ = %v, want the warm-up sample dropped", got["client.present_interval_sd_ms"])
	}
	if len(got["transport.rtt_ms"]) != 2 {
		t.Errorf("transport.rtt_ms = %v — rtt is NOT warm-up sensitive and must not be trimmed", got["transport.rtt_ms"])
	}
	if len(series["encoder.fps"]) != 2 {
		t.Error("assessed() mutated its input; the bundle must still serve every sample")
	}
}

// ── the 2026-08-23 live case, as a fixture ───────────────────────────────────
//
// A healthy 300 s session was reported as likely_client_presentation_limit
// because ONE present-interval σ sample of 18.058 ms out of 54 crossed the hitch
// threshold and the rule was a max; and its encoder.fps p10 read 42 over n=9
// because the first samples were the session still ramping. Both are now handled
// — the hitch needs two samples, warm-up is excluded — and the fixture is here so
// neither can quietly come back.

func agentSample(ts int64, fps, encodeMs float64) telemetry.Sample {
	return telemetry.Sample{
		Source: telemetry.SourceAgent, TsUnixMs: ts,
		Metrics: json.RawMessage(fmt.Sprintf(`{"fps":%g,"encode_ms":%g}`, fps, encodeMs)),
	}
}

func browserSample(ts int64, sd float64, packetsLost int) telemetry.Sample {
	return telemetry.Sample{
		Source: telemetry.SourceBrowser, TsUnixMs: ts,
		Metrics: json.RawMessage(fmt.Sprintf(
			`{"present_interval_sd_ms":%g,"present_long_frames":0,"present_beat_fraction":0.1,`+
				`"rtt_ms":9,"packets_lost":%d,"is_hidden":0,"fps":60}`, sd, packetsLost)),
	}
}

func live20260823Slice(base int64) telemetry.Slice {
	var samples []telemetry.Sample

	// 9 agent samples; the first three are the ramp (fps 40/42/45 inside the first
	// 20 s), which is what dragged the whole-window p10 to 42.
	ramp := []struct {
		offset int64
		fps    float64
	}{{0, 40}, {5_000, 42}, {15_000, 45}}
	for _, r := range ramp {
		samples = append(samples, agentSample(base+r.offset, r.fps, 7))
	}
	for i := int64(0); i < 6; i++ {
		samples = append(samples, agentSample(base+40_000+i*40_000, 60, 7.5))
	}

	// 54 client samples across the window; exactly one reaches the hitch threshold.
	for i := int64(0); i < 54; i++ {
		sd := 3.0 + float64(i%4)
		if i == 30 {
			sd = 18.058
		}
		samples = append(samples, browserSample(base+i*5_500, sd, 2))
	}

	return telemetry.Slice{
		Samples: samples,
		Clock:   &telemetry.Clock{ClientOffsetMs: -3.2, UncertaintyMs: 1.8, MeasuredAt: time.Now()},
	}
}

func TestLive20260823HealthySessionReadsNominal(t *testing.T) {
	const base int64 = 1_700_000_000_000
	running := time.UnixMilli(base)
	a := assess(live20260823Slice(base), &running, base, base+300_000)
	v := a.Verdict

	if v.Verdict != verdictNominal {
		t.Fatalf("verdict = %q, want nominal\nreason: %s\nevidence: %v",
			v.Verdict, v.Reason, v.Evidence)
	}
	for _, f := range v.Falsifiers {
		if !f.Holds {
			t.Errorf("falsifier %s/%s does not hold (value=%v %s %v, n=%d, note=%q) — "+
				"a healthy session must satisfy every condition nominal claims",
				f.Name, f.Estimator, f.Value, f.Op, f.Threshold, f.N, f.Note)
		}
	}
	if v.Window.WarmupExcludedMs != 20_000 {
		t.Errorf("warmup_excluded_ms = %d, want 20000 — the exclusion must be visible, not silent",
			v.Window.WarmupExcludedMs)
	}
	if !v.Clock.Applied {
		t.Error("clock.applied = false on a measured clock; the offset must actually be used")
	}
	if v.Clock.AgeMs == nil {
		t.Error("clock.age_ms is nil on a measured clock; staleness must be visible")
	}

	// The two numbers that used to decide, still reported — just no longer able to
	// carry the verdict on their own.
	count := falsifierWith(t, v, "client.present_interval_sd_ms", estimatorCountGE)
	if count.Value == nil || *count.Value != 1 {
		t.Errorf("hitch count = %v, want 1 (the single 18.058 sample)", count.Value)
	}
	worst := falsifierWith(t, v, "client.present_interval_sd_ms", estimatorMax)
	if worst.Value == nil || *worst.Value != 18.058 {
		t.Errorf("worst σ = %v, want 18.058 — the number must still be shown", worst.Value)
	}
	fps := falsifierWith(t, v, "encoder.fps", estimatorP10)
	if fps.Value == nil || *fps.Value != 60 {
		t.Errorf("encoder.fps p10 = %v, want 60 — the 42 was the warm-up ramp", fps.Value)
	}
}

func falsifierWith(t *testing.T, v Verdict, name, estimator string) Falsifier {
	t.Helper()
	for _, f := range v.Falsifiers {
		if f.Name == name && f.Estimator == estimator {
			return f
		}
	}
	t.Fatalf("verdict carries no %s/%s falsifier (has %v)", name, estimator, falsifierNames(v))
	return Falsifier{}
}
