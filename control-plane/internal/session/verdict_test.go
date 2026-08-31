package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// ST-09 — the Verdict value. These tests are about the part a prose evidence
// string could never carry: which numbers the verdict rests on, computed with
// which estimator, over how many samples, and whether they hold.

func falsifierNamed(t *testing.T, v Verdict, name string) Falsifier {
	t.Helper()
	for _, f := range v.Falsifiers {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("verdict %q carries no falsifier named %q (has %v)", v.Verdict, name, falsifierNames(v))
	return Falsifier{}
}

func falsifierNames(v Verdict) []string {
	out := make([]string, 0, len(v.Falsifiers))
	for _, f := range v.Falsifiers {
		out = append(out, f.Name)
	}
	return out
}

func buildFrom(series map[string][]seriesPoint, events []traceEventResp, nHost, nClient int, clock *telemetry.Clock) Verdict {
	al := alignmentOf(telemetry.AlignSeries(nil, nil, clock))
	dw := computeDerivedWindows(series, events)
	return buildVerdict(verdictInputs{
		Classifier: classify(classifyInputs{Series: series, Derived: dw, Events: events, Align: al}),
		Series:     series,
		FullSeries: series,
		Align:      al,
		Derived:    dw,
		Events:     events,
		FromMs:     1_000_000,
		ToMs:       1_300_000,
		NHost:      nHost,
		NClient:    nClient,
		Clock:      clock,
	})
}

func measuredClock() *telemetry.Clock {
	return &telemetry.Clock{ClientOffsetMs: -3.2, UncertaintyMs: 1.8, MeasuredAt: time.Unix(0, 0)}
}

func healthySeries(base int64) map[string][]seriesPoint {
	return map[string][]seriesPoint{
		"encoder.fps":                   pt(base, 60, 60, 59, 60, 60),
		"encoder.encode_ms":             pt(base, 3, 4, 3, 4, 3),
		"transport.packets_lost":        pt(base, 0, 0, 0, 0, 0),
		"transport.rtt_ms":              pt(base, 10, 11, 12, 11, 10),
		"client.present_interval_sd_ms": pt(base, 4, 5, 4, 5, 4),
		"client.is_hidden":              pt(base, 0, 0, 0, 0, 0),
		// Present cadence: a 120 fps client on a 120 Hz panel, beating gently
		// against its own display and stalling never — the 2026-08-22 shape.
		"client.present_long_frames":   pt(base, 0, 0, 0, 0, 0),
		"client.present_beat_fraction": pt(base, 0.1, 0.12, 0.08, 0.11, 0.09),
	}
}

// A nominal verdict must state every condition a healthy window satisfies, and
// every one of them must hold. "Nothing is wrong" is only checkable if the
// verdict says what "wrong" would have looked like.
func TestVerdictNominalFalsifiersAllHold(t *testing.T) {
	v := buildFrom(healthySeries(1_000_000), nil, 300, 290, measuredClock())

	if v.Verdict != verdictNominal {
		t.Fatalf("verdict = %q want nominal", v.Verdict)
	}
	// Ordered, because client.present_interval_sd_ms appears TWICE: the
	// count_ge_threshold row is the condition nominal rests on, and the max row
	// beside it carries the worst single sample informatively.
	want := []struct{ name, estimator, op string }{
		{"encoder.fps", estimatorP10, opGTE},
		{"encoder.encode_ms", estimatorP95, opLT},
		{"transport.packets_lost", estimatorDelta, opLTE},
		{"transport.rtt_ms", estimatorP95, opLTE},
		{"client.present_interval_sd_ms", estimatorCountGE, opLT},
		{"client.present_interval_sd_ms", estimatorMax, opGTE},
		{"client.is_hidden", estimatorAny, opEQ},
		{"client.present_long_frames", estimatorMax, opEQ},
		{"client.present_beat_fraction", estimatorMax, opLTE},
	}
	if len(v.Falsifiers) != len(want) {
		t.Fatalf("nominal falsifiers = %v, want exactly %d", falsifierNames(v), len(want))
	}
	for i, spec := range want {
		f := v.Falsifiers[i]
		name := spec.name
		if f.Name != spec.name {
			t.Errorf("falsifier %d: name = %s, want %s", i, f.Name, spec.name)
			continue
		}
		if f.Estimator != spec.estimator || f.Op != spec.op {
			t.Errorf("%s: estimator/op = %s %s, want %s %s", name, f.Estimator, f.Op, spec.estimator, spec.op)
		}
		if !f.Holds {
			t.Errorf("%s: holds=false on a healthy window (value=%v threshold=%v)", name, f.Value, f.Threshold)
		}
		if f.Value == nil {
			t.Errorf("%s: value is null on a healthy window", name)
		}
		if f.N != 5 {
			t.Errorf("%s: n = %d want 5", name, f.N)
		}
		if f.Unit == "" {
			t.Errorf("%s: no unit", name)
		}
	}
	if v.ThresholdsVersion != thresholdsVersion {
		t.Errorf("thresholds_version = %q want %q", v.ThresholdsVersion, thresholdsVersion)
	}
}

// A congestion verdict must state the two conditions that FIRED as holding —
// the verdict relies on them being true — plus the alternatives it ruled out.
func TestVerdictCongestionFalsifiers(t *testing.T) {
	base := int64(2_000_000)
	series := map[string][]seriesPoint{
		"transport.packets_lost": pt(base, 0, 4, 12, 30, 55),
		"transport.rtt_ms":       pt(base, 40, 70, 95, 110, 120),
		"encoder.encode_ms":      pt(base, 3, 4, 4, 3, 4),
		"encoder.fps":            pt(base, 60, 59, 60, 60, 59),
		"client.is_hidden":       pt(base, 0, 0, 0, 0, 0),
	}
	events := []traceEventResp{{Source: "agent", TsUnixMs: base + 2000, Type: "abr.retarget",
		Payload: json.RawMessage(`{"from_kbps":14000,"to_kbps":9000}`)}}

	v := buildFrom(series, events, 120, 118, measuredClock())
	if v.Verdict != verdictNetworkCongestion {
		t.Fatalf("verdict = %q want %q", v.Verdict, verdictNetworkCongestion)
	}

	loss := falsifierNamed(t, v, "transport.packets_lost")
	if loss.Estimator != estimatorDelta || loss.Op != opGT || !loss.Holds {
		t.Errorf("loss falsifier = %+v; want a delta > threshold that holds", loss)
	}
	if loss.Value == nil || *loss.Value != 55 {
		t.Errorf("loss delta = %v want 55 (last-first)", loss.Value)
	}
	rtt := falsifierNamed(t, v, "transport.rtt_ms")
	if rtt.Estimator != estimatorP95 || rtt.Op != opGT || !rtt.Holds {
		t.Errorf("rtt falsifier = %+v; want a p95 > threshold that holds", rtt)
	}
	// The ruled-out alternative: the encoder had headroom, so it is not the cause.
	enc := falsifierNamed(t, v, "encoder.encode_ms")
	if !enc.Holds {
		t.Errorf("encoder falsifier should hold (encoder had headroom): %+v", enc)
	}
}

func TestVerdictEncoderSaturationFalsifiers(t *testing.T) {
	base := int64(3_000_000)
	series := map[string][]seriesPoint{
		"encoder.encode_ms":      pt(base, 18, 19, 20, 21, 22),
		"encoder.fps":            pt(base, 44, 43, 41, 40, 42),
		"transport.packets_lost": pt(base, 0, 0, 1, 1, 1),
		"transport.rtt_ms":       pt(base, 12, 14, 13, 12, 15),
		"client.is_hidden":       pt(base, 0, 0, 0, 0, 0),
	}
	v := buildFrom(series, nil, 90, 88, measuredClock())
	if v.Verdict != verdictEncoderSaturation {
		t.Fatalf("verdict = %q want %q", v.Verdict, verdictEncoderSaturation)
	}
	enc := falsifierNamed(t, v, "encoder.encode_ms")
	if enc.Op != opGTE || !enc.Holds {
		t.Errorf("encode falsifier = %+v; want >= ceiling and holding", enc)
	}
	fps := falsifierNamed(t, v, "encoder.fps")
	if fps.Op != opLT || !fps.Holds {
		t.Errorf("fps falsifier = %+v; want < steady and holding (fps collapsed)", fps)
	}
	// Ruled out: the wire was quiet.
	if !falsifierNamed(t, v, "transport.packets_lost").Holds {
		t.Errorf("loss falsifier should hold (quiet wire)")
	}
	if !falsifierNamed(t, v, "transport.rtt_ms").Holds {
		t.Errorf("rtt falsifier should hold (quiet wire)")
	}
}

func TestVerdictClientPresentationLimitFalsifiers(t *testing.T) {
	base := int64(4_000_000)
	series := map[string][]seriesPoint{
		"client.present_interval_sd_ms": pt(base, 20, 24, 22, 26, 21),
		"client.is_hidden":              pt(base, 0, 0, 0, 0, 0),
		"encoder.fps":                   pt(base, 60, 60, 59, 60, 60),
		"encoder.encode_ms":             pt(base, 3, 4, 3, 4, 3),
		"transport.packets_lost":        pt(base, 0, 0, 0, 1, 1),
		"transport.rtt_ms":              pt(base, 10, 11, 12, 11, 10),
	}
	v := buildFrom(series, nil, 150, 150, measuredClock())
	if v.Verdict != verdictClientPresentingLimit {
		t.Fatalf("verdict = %q want %q", v.Verdict, verdictClientPresentingLimit)
	}
	sd := falsifierNamed(t, v, "client.present_interval_sd_ms")
	if sd.Estimator != estimatorMax || sd.Op != opGTE || !sd.Holds {
		t.Errorf("judder falsifier = %+v; want max >= threshold and holding", sd)
	}
	if sd.Value == nil || *sd.Value != 26 {
		t.Errorf("judder max = %v want 26", sd.Value)
	}
	for _, name := range []string{"encoder.fps", "transport.packets_lost", "transport.rtt_ms", "client.is_hidden"} {
		if !falsifierNamed(t, v, name).Holds {
			t.Errorf("guard %s should hold — it is what leaves the client as the explanation", name)
		}
	}
}

// The hidden-tab verdict must say what could NOT be assessed, as a falsifier
// with a note, rather than omit it. An absent number is what misleads.
func TestVerdictIndeterminateClientHiddenNamesWhatIsMissing(t *testing.T) {
	base := int64(5_000_000)
	series := map[string][]seriesPoint{
		"encoder.fps":            pt(base, 60, 60, 59, 60, 60),
		"encoder.encode_ms":      pt(base, 3, 4, 3, 4, 3),
		"transport.packets_lost": pt(base, 0, 0, 0, 0, 0),
		"transport.rtt_ms":       pt(base, 10, 11, 12, 11, 10),
		"client.is_hidden":       pt(base, 1, 1, 1, 1, 1),
	}
	v := buildFrom(series, nil, 100, 100, measuredClock())
	if v.Verdict != verdictIndeterminateClientHidden {
		t.Fatalf("verdict = %q want %q", v.Verdict, verdictIndeterminateClientHidden)
	}
	hidden := falsifierNamed(t, v, "client.is_hidden")
	if !hidden.Holds || hidden.Value == nil || *hidden.Value != 1 {
		t.Errorf("hidden falsifier = %+v; want value 1 and holding", hidden)
	}
	sd := falsifierNamed(t, v, "client.present_interval_sd_ms")
	if sd.Holds {
		t.Errorf("presentation must not read as satisfied while the tab was hidden: %+v", sd)
	}
	if sd.Note == "" {
		t.Errorf("presentation falsifier must carry a note saying why it is unassessable")
	}
}

// A series with no samples reports value null / n 0 / holds false / a note. It
// must never read as a silent pass, which is the failure mode that lets a
// verdict rest on a measurement nobody took.
func TestVerdictMissingSeriesIsNullNotPassing(t *testing.T) {
	base := int64(6_000_000)
	// Host only: no client telemetry at all.
	series := map[string][]seriesPoint{
		"encoder.fps":       pt(base, 60, 60, 60, 60, 60),
		"encoder.encode_ms": pt(base, 3, 3, 3, 3, 3),
	}
	v := buildFrom(series, nil, 200, 0, measuredClock())

	for _, name := range []string{"transport.rtt_ms", "client.present_interval_sd_ms", "client.is_hidden"} {
		f := falsifierNamed(t, v, name)
		if f.Value != nil {
			t.Errorf("%s: value = %v, want null with no samples", name, *f.Value)
		}
		if f.N != 0 {
			t.Errorf("%s: n = %d want 0", name, f.N)
		}
		if f.Holds {
			t.Errorf("%s: holds=true with zero samples — a missing measurement is not a passing one", name)
		}
		if f.Note != "no samples" {
			t.Errorf("%s: note = %q want %q", name, f.Note, "no samples")
		}
	}
	// A one-sample cumulative counter cannot produce a delta either.
	single := map[string][]seriesPoint{"transport.packets_lost": pt(base, 7)}
	f := falsifierFor(single, "transport.packets_lost", estimatorDelta, opLTE, congestionLossDelta, unitCount)
	if f.Value != nil || f.Holds || f.N != 1 || f.Note == "" {
		t.Errorf("single-sample delta = %+v; want null/holds-false/n=1 with a note", f)
	}
}

func TestEvidenceTier(t *testing.T) {
	measured := VerdictClock{Quality: clockMeasured}
	unmeasured := VerdictClock{Quality: clockUnmeasured}

	cases := []struct {
		name           string
		nHost, nClient int
		clock          VerdictClock
		want           string
	}{
		{"both sides, measured clock", 100, 100, measured, tierFull},
		{"both sides at the boundary", 3, 3, measured, tierFull},
		{"both sides, unmeasured clock is never full", 100, 100, unmeasured, tierHostOnly},
		{"client silent", 100, 0, measured, tierHostOnly},
		{"client under the floor", 100, 2, measured, tierHostOnly},
		{"host silent", 0, 100, measured, tierClientOnly},
		{"neither side", 2, 2, measured, tierInsufficient},
		{"nothing at all", 0, 0, unmeasured, tierInsufficient},
	}
	for _, c := range cases {
		if got := evidenceTier(c.nHost, c.nClient, c.clock); got != c.want {
			t.Errorf("%s: tier = %q want %q", c.name, got, c.want)
		}
	}
}

// An unmeasured clock is the common case, so it must be reported as a fact —
// in clock.quality, in the tier cap, and in the sentence a human reads.
func TestVerdictUnmeasuredClockIsStated(t *testing.T) {
	v := buildFrom(healthySeries(7_000_000), nil, 300, 300, nil)

	if v.Clock.Quality != clockUnmeasured {
		t.Fatalf("clock.quality = %q want %q", v.Clock.Quality, clockUnmeasured)
	}
	if v.Clock.OffsetMs != nil || v.Clock.UncertaintyMs != nil {
		t.Errorf("unmeasured clock must not carry a zero-default offset: %+v", v.Clock)
	}
	if v.EvidenceTier == tierFull {
		t.Errorf("evidence_tier must never be full with an unmeasured clock")
	}
	if !contains(v.Reason, "unmeasured") {
		t.Errorf("reason does not mention the unmeasured clock: %q", v.Reason)
	}
}

func TestVerdictMeasuredClockCarriesOffsetAndUncertainty(t *testing.T) {
	v := buildFrom(healthySeries(8_000_000), nil, 300, 300, measuredClock())
	if v.Clock.Quality != clockMeasured {
		t.Fatalf("clock.quality = %q want measured", v.Clock.Quality)
	}
	if v.Clock.OffsetMs == nil || *v.Clock.OffsetMs != -3.2 {
		t.Errorf("offset_ms = %v want -3.2", v.Clock.OffsetMs)
	}
	if v.Clock.UncertaintyMs == nil || *v.Clock.UncertaintyMs != 1.8 {
		t.Errorf("uncertainty_ms = %v want 1.8", v.Clock.UncertaintyMs)
	}
	if v.EvidenceTier != tierFull {
		t.Errorf("evidence_tier = %q want full", v.EvidenceTier)
	}
}

// The window is not just a span: the per-source sample counts are what separate
// "healthy over 300 samples" from "healthy over 4".
func TestVerdictWindowCarriesSampleCounts(t *testing.T) {
	v := buildFrom(healthySeries(9_000_000), nil, 312, 298, measuredClock())
	if v.Window.NHost != 312 || v.Window.NClient != 298 {
		t.Fatalf("window = %+v want n_host 312 / n_client 298", v.Window)
	}
	if v.Window.FromMs != 1_000_000 || v.Window.ToMs != 1_300_000 {
		t.Fatalf("window span = %+v", v.Window)
	}
	if !contains(v.Reason, "312 host") || !contains(v.Reason, "298 client") {
		t.Errorf("reason omits the sample counts: %q", v.Reason)
	}
}

func TestCountSamplesBySource(t *testing.T) {
	samples := []telemetry.Sample{
		{Source: telemetry.SourceAgent, TsUnixMs: 1},
		{Source: telemetry.SourceBrowser, TsUnixMs: 2},
		{Source: telemetry.SourceBrowser, TsUnixMs: 3},
		{Source: "native", TsUnixMs: 4}, // a receiver is a receiver
	}
	nHost, nClient := countSamplesBySource(samples)
	if nHost != 1 || nClient != 3 {
		t.Fatalf("counts = %d host / %d client, want 1 / 3", nHost, nClient)
	}
}

// A verdict string this build has never seen must still come back with the
// standard numbers, not an empty argument. The control plane grows this
// vocabulary; a consumer (and this function) must treat an unknown state as
// data. This is the 2026-08-22 regression, as a unit test.
func TestVerdictUnknownStateStillCarriesFalsifiers(t *testing.T) {
	series := healthySeries(10_000_000)
	v := buildVerdict(verdictInputs{
		Classifier: classifierVerdict{Verdict: "a_verdict_from_the_future", Evidence: nil},
		Series:     series,
		FromMs:     1_000_000, ToMs: 1_300_000,
		NHost: 50, NClient: 50, Clock: measuredClock(),
	})
	if len(v.Falsifiers) == 0 {
		t.Fatal("an unrecognised verdict came back with no falsifiers")
	}
	if v.Evidence == nil {
		t.Error("evidence must serialise as [] not null")
	}
	if !contains(v.Reason, "a_verdict_from_the_future") {
		t.Errorf("reason does not name the verdict: %q", v.Reason)
	}
}

// The verdict + evidence pair is the pre-ST-09 contract and must survive intact.
func TestVerdictPassesClassifierOutputThrough(t *testing.T) {
	series := healthySeries(11_000_000)
	dw := computeDerivedWindows(series, nil)
	cl := classify(classifyInputs{Series: series, Derived: dw, Events: nil})
	v := buildFrom(series, nil, 10, 10, measuredClock())

	if v.Verdict != cl.Verdict {
		t.Fatalf("verdict = %q, classifier said %q", v.Verdict, cl.Verdict)
	}
	if len(v.Evidence) != len(cl.Evidence) {
		t.Fatalf("evidence = %v, classifier said %v", v.Evidence, cl.Evidence)
	}
	for i := range cl.Evidence {
		if v.Evidence[i] != cl.Evidence[i] {
			t.Fatalf("evidence[%d] = %q, classifier said %q", i, v.Evidence[i], cl.Evidence[i])
		}
	}
}

// The wire shape: every additive key present, with the JSON names the contract
// documents. A consumer reads these names, not the Go field names.
func TestVerdictJSONShape(t *testing.T) {
	v := buildFrom(healthySeries(12_000_000), nil, 20, 20, measuredClock())
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"verdict", "evidence", "reason", "window", "clock",
		"evidence_tier", "falsifiers", "thresholds_version"} {
		if _, ok := got[key]; !ok {
			t.Errorf("verdict JSON is missing %q (keys: %v)", key, keysOf(got))
		}
	}
	var falsifiers []map[string]json.RawMessage
	if err := json.Unmarshal(got["falsifiers"], &falsifiers); err != nil {
		t.Fatalf("falsifiers: %v", err)
	}
	if len(falsifiers) == 0 {
		t.Fatal("no falsifiers on the wire")
	}
	for _, key := range []string{"name", "estimator", "value", "op", "threshold", "unit", "n", "holds"} {
		if _, ok := falsifiers[0][key]; !ok {
			t.Errorf("falsifier JSON is missing %q", key)
		}
	}
	// note is omitted when empty — a falsifier that holds should not carry noise.
	if _, ok := falsifiers[0]["note"]; ok {
		t.Errorf("a holding falsifier should omit `note`")
	}
}

func TestEstimators(t *testing.T) {
	pts := pt(0, 10, 20, 30, 40, 50)
	cases := []struct {
		estimator string
		want      float64
	}{
		{estimatorMax, 50},
		{estimatorMean, 30},
		{estimatorDelta, 40},
		{estimatorAny, 1},
	}
	for _, c := range cases {
		got, n, ok := estimate(pts, c.estimator)
		if !ok || n != 5 || got != c.want {
			t.Errorf("%s = %v (n=%d ok=%v), want %v", c.estimator, got, n, ok, c.want)
		}
	}
	if v, _, _ := estimate(pt(0, 0, 0, 0), estimatorAny); v != 0 {
		t.Errorf("any over all-zero = %v want 0", v)
	}
	if _, _, ok := estimate(nil, estimatorMax); ok {
		t.Error("an empty series must not produce an estimate")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ── present cadence (2026-08-22) ─────────────────────────────────────────────

// A gentle vsync beat with no long frames is what a healthy stream at source
// fps == display Hz looks like. Both cadence falsifiers must hold, and the beat
// one must SAY so — the verdict that reads "nominal" beside a panel headline of
// 88-108 fps is precisely the one that got a healthy session investigated.
func TestVerdictExplainsTheVsyncBeat(t *testing.T) {
	v := buildFrom(healthySeries(1_000_000), nil, 300, 290, measuredClock())

	long := falsifierNamed(t, v, "client.present_long_frames")
	if !long.Holds || long.Value == nil || *long.Value != 0 {
		t.Errorf("present_long_frames = %v holds=%v, want 0 holding", long.Value, long.Holds)
	}
	if long.Note != "" {
		t.Errorf("present_long_frames carries a note on a clean window: %q", long.Note)
	}

	beat := falsifierNamed(t, v, "client.present_beat_fraction")
	if !beat.Holds {
		t.Error("present_beat_fraction must not fail: it is informative, not a test")
	}
	if beat.Unit != unitFraction {
		t.Errorf("present_beat_fraction unit = %q want %q", beat.Unit, unitFraction)
	}
	if beat.Note == "" {
		t.Error("a non-zero beat must be explained, or the number invites the same misread again")
	}
}

// A long frame is a stall, and the beat explanation must not cover for it.
func TestVerdictNamesALongPresentFrame(t *testing.T) {
	s := healthySeries(1_000_000)
	s["client.present_long_frames"] = pt(1_000_000, 0, 0, 3, 0, 0)
	v := buildFrom(s, nil, 300, 290, measuredClock())

	f := falsifierNamed(t, v, "client.present_long_frames")
	if f.Holds {
		t.Error("present_long_frames holds with 3 long frames in the window")
	}
	if f.Note == "" {
		t.Error("a failing long-frame falsifier must say what it means")
	}
}

// An older client sends neither key. The falsifiers must report that honestly —
// value null, n 0, a note — and never a fabricated passing zero.
func TestVerdictCadenceFalsifiersOnAnOlderClient(t *testing.T) {
	s := healthySeries(1_000_000)
	delete(s, "client.present_long_frames")
	delete(s, "client.present_beat_fraction")
	v := buildFrom(s, nil, 300, 290, measuredClock())

	for _, name := range []string{"client.present_long_frames", "client.present_beat_fraction"} {
		f := falsifierNamed(t, v, name)
		if f.Value != nil {
			t.Errorf("%s: value = %v on a client that never sent it", name, *f.Value)
		}
		if f.N != 0 || f.Holds {
			t.Errorf("%s: n=%d holds=%v, want 0/false", name, f.N, f.Holds)
		}
		if f.Note != "no samples" {
			t.Errorf("%s: note = %q want %q", name, f.Note, "no samples")
		}
	}
}

// ── the event suffix: facts a sample series cannot see ───────────────────────
//
// A verdict computed from series alone cannot see a rejected m-line, a stalled
// encoder or a GPU fault. Each of those explains a symptom outright, and on
// 2026-08-22 the first of them was investigated for hours as the second. The
// suffix names the fact and points at the events table; it changes no
// falsifier and cannot flip the classification.

func TestVerdictReasonNamesTheEventsInTheWindow(t *testing.T) {
	base := int64(1_000_000)
	events := []traceEventResp{
		{Source: "agent", TsUnixMs: base + 1000, Type: "sdp.answer_applied",
			Payload: json.RawMessage(`{"pc":"video","rejected_count":1}`)},
		{Source: "agent", TsUnixMs: base + 2000, Type: "encoder.stall",
			Payload: json.RawMessage(`{"phase":"detected","reason":"input_starved","since_ms":1200}`)},
		{Source: "agent", TsUnixMs: base + 9000, Type: "encoder.stall",
			Payload: json.RawMessage(`{"phase":"recovered","reason":"input_starved","stalled_ms":7000}`)},
		{Source: "agent", TsUnixMs: base + 3000, Type: "host.xid",
			Payload: json.RawMessage(`{"code":31,"pci":"0000:01:00"}`)},
	}
	v := buildFrom(healthySeries(base), events, 120, 118, measuredClock())

	for _, want := range []string{"an encoder stall", "a rejected m-line", "a GPU fault", "see events"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("reason does not mention %q:\n%s", want, v.Reason)
		}
	}
	// The recovery half must not be counted as a second stall.
	if strings.Contains(v.Reason, "2 encoder stalls") {
		t.Errorf("a Detected/Recovered pair was counted as two stalls:\n%s", v.Reason)
	}
}

func TestVerdictReasonIsUnchangedWithoutThoseEvents(t *testing.T) {
	base := int64(1_000_000)
	// An abr.retarget is an ordinary event and must add nothing.
	events := []traceEventResp{{Source: "agent", TsUnixMs: base + 2000, Type: "abr.retarget",
		Payload: json.RawMessage(`{"from_kbps":14000,"to_kbps":9000}`)}}
	v := buildFrom(healthySeries(base), events, 120, 118, measuredClock())
	if strings.Contains(v.Reason, "Also in this window") {
		t.Errorf("an ordinary event produced an event suffix:\n%s", v.Reason)
	}
}

func TestVerdictReasonIgnoresEventsOutsideTheWindow(t *testing.T) {
	base := int64(1_000_000)
	events := []traceEventResp{
		// buildFrom's window is [1_000_000, 1_300_000].
		{Source: "agent", TsUnixMs: 999_000, Type: "host.xid",
			Payload: json.RawMessage(`{"code":31}`)},
		{Source: "agent", TsUnixMs: 1_400_000, Type: "encoder.stall",
			Payload: json.RawMessage(`{"phase":"detected","reason":"no_output"}`)},
	}
	v := buildFrom(healthySeries(base), events, 120, 118, measuredClock())
	if strings.Contains(v.Reason, "Also in this window") {
		t.Errorf("an out-of-window event produced an event suffix:\n%s", v.Reason)
	}
}

func TestVerdictEventSuffixDoesNotAddAFalsifier(t *testing.T) {
	base := int64(1_000_000)
	withEvents := buildFrom(healthySeries(base), []traceEventResp{
		{Source: "agent", TsUnixMs: base + 1000, Type: "host.xid",
			Payload: json.RawMessage(`{"code":31}`)},
	}, 120, 118, measuredClock())
	without := buildFrom(healthySeries(base), nil, 120, 118, measuredClock())
	if withEvents.Verdict != without.Verdict {
		t.Errorf("the event suffix changed the verdict: %q vs %q", withEvents.Verdict, without.Verdict)
	}
	if len(withEvents.Falsifiers) != len(without.Falsifiers) {
		t.Errorf("the event suffix changed the falsifier set: %d vs %d",
			len(withEvents.Falsifiers), len(without.Falsifiers))
	}
}
