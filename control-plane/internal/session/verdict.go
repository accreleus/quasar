package session

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// Verdict is the stream-health judgement as one checkable value (ST-09): a
// verdict string and prose evidence plus reason/window/clock/evidence_tier/
// falsifiers, so a reader can check the claim without a psql session (case in
// point: 2026-08-22, a healthy 1440p120 h265 session — recv 120 fps, encode
// 7.5 ms, zero drops, present σ 1.9 ms — got investigated over present_fps, a
// MEAN, reading 88-108). Falsifiers are the numbers it rests on; overturn one
// to overturn the verdict. buildVerdict is a pure function of the classifier's
// decision plus the series it looked at; a verdict itself has no session
// authority.

// Estimator names; a falsifier is meaningless without one (mean vs. p95 vs. max).
const (
	estimatorP10   = "p10"
	estimatorP95   = "p95"
	estimatorMax   = "max"
	estimatorDelta = "delta"
	estimatorMean  = "mean"
	estimatorAny   = "any"
	// estimatorCountGE counts samples in the window at/above a threshold, not one
	// sample: on 2026-08-23 a single σ of 18.058 out of 54 samples carried a
	// five-minute verdict under a max-based rule.
	estimatorCountGE = "count_ge_threshold"
)

const (
	opGTE = ">="
	opLTE = "<="
	opGT  = ">"
	opLT  = "<"
	opEQ  = "=="
)

const (
	unitFPS      = "fps"
	unitMS       = "ms"
	unitCount    = "count"
	unitBool     = "bool"
	unitFraction = "fraction"
)

const (
	tierFull         = "full"
	tierHostOnly     = "host_only"
	tierClientOnly   = "client_only"
	tierInsufficient = "insufficient"
)

// minSamplesForTier: below this, a side has not really reported.
const minSamplesForTier = 3

const (
	clockMeasured   = "measured"
	clockUnmeasured = "unmeasured"
)

// VerdictWindow: sample counts are load-bearing (4 client samples vs. 400 is
// not the same claim).
type VerdictWindow struct {
	FromMs  int64 `json:"from_ms"`
	ToMs    int64 `json:"to_ms"`
	NHost   int   `json:"n_host"`
	NClient int   `json:"n_client"`
	// WarmupExcludedMs: samples in this span were served but not judged by the
	// warm-up-sensitive rules (hitch detection, encoder.fps floor).
	WarmupExcludedMs int64 `json:"warmup_excluded_ms"`
}

// VerdictClock: whether host/client timelines align. Unmeasured (the common
// case) is stated rather than papered over.
type VerdictClock struct {
	Quality       string   `json:"quality"`
	OffsetMs      *float64 `json:"offset_ms,omitempty"`
	UncertaintyMs *float64 `json:"uncertainty_ms,omitempty"`
	// Applied: whether the offset was actually used to shift client points, not
	// merely reported.
	Applied bool `json:"applied"`
	// AgeMs: now − measured_at; large age means the client stopped refreshing.
	AgeMs *int64 `json:"age_ms,omitempty"`
}

// Falsifier is one named, estimator-qualified number the verdict relies on.
// Holds answers "does the data satisfy the condition", not "is this good".
// Value is nil when the series had no samples; N is then 0 and Holds false —
// a missing measurement must never read as a passing one.
type Falsifier struct {
	Name      string   `json:"name"`
	Estimator string   `json:"estimator"`
	Value     *float64 `json:"value"`
	Op        string   `json:"op"`
	Threshold float64  `json:"threshold"`
	Unit      string   `json:"unit"`
	N         int      `json:"n"`
	Holds     bool     `json:"holds"`
	Note      string   `json:"note,omitempty"`
}

// Verdict: `verdict`/`evidence` are byte-for-byte what the ST-01 classifier
// always produced; everything else is additive (ST-09).
type Verdict struct {
	Verdict           string        `json:"verdict"`
	Evidence          []string      `json:"evidence"`
	Reason            string        `json:"reason"`
	Window            VerdictWindow `json:"window"`
	Clock             VerdictClock  `json:"clock"`
	EvidenceTier      string        `json:"evidence_tier"`
	Falsifiers        []Falsifier   `json:"falsifiers"`
	ThresholdsVersion string        `json:"thresholds_version"`
}

// verdictInputs is everything buildVerdict reads; keeps it pure and testable
// without a database.
type verdictInputs struct {
	Classifier classifierVerdict
	Series     map[string][]seriesPoint
	Derived    derivedWindows
	Events     []traceEventResp
	FromMs     int64
	ToMs       int64
	NHost      int
	NClient    int
	Clock      *telemetry.Clock
	// Align/Warmup: what the rules knew about the clock and declined to judge.
	Align  alignment
	Warmup warmupExclusion
	// FullSeries is the untrimmed series map, for telling "no samples" apart
	// from "every sample was inside warm-up".
	FullSeries map[string][]seriesPoint
}

// countSamplesBySource splits raw samples into host (agent) / client counts.
func countSamplesBySource(samples []telemetry.Sample) (nHost, nClient int) {
	for _, s := range samples {
		if s.Source == telemetry.SourceAgent {
			nHost++
			continue
		}
		nClient++
	}
	return nHost, nClient
}

// estimate applies one estimator, returning the value and samples consumed.
// ok is false when the series can't support it (no samples, or <2 for delta).
func estimate(pts []seriesPoint, estimator string) (v float64, n int, ok bool) {
	n = len(pts)
	if n == 0 {
		return 0, 0, false
	}
	switch estimator {
	case estimatorP10:
		return percentile(values(pts), 10), n, true
	case estimatorP95:
		return percentile(values(pts), 95), n, true
	case estimatorMax:
		return maxV(pts), n, true
	case estimatorMean:
		return meanOf(values(pts)), n, true
	case estimatorDelta:
		// A cumulative counter's delta needs two ends to subtract.
		if n < 2 {
			return 0, n, false
		}
		return pts[n-1].V - pts[0].V, n, true
	case estimatorAny:
		// "any" over a boolean-ish series: 1 if any sample is non-zero.
		for _, p := range pts {
			if p.V != 0 {
				return 1, n, true
			}
		}
		return 0, n, true
	}
	return 0, n, false
}

func satisfies(v float64, op string, threshold float64) bool {
	switch op {
	case opGTE:
		return v >= threshold
	case opLTE:
		return v <= threshold
	case opGT:
		return v > threshold
	case opLT:
		return v < threshold
	case opEQ:
		return v == threshold
	}
	return false
}

// falsifierFor: a series that can't support the estimator must never silently
// pass or produce a fabricated zero — value null / holds false / a note instead.
func falsifierFor(series map[string][]seriesPoint, name, estimator, op string, threshold float64, unit string) Falsifier {
	f := Falsifier{
		Name: name, Estimator: estimator, Op: op, Threshold: threshold, Unit: unit,
	}
	pts := series[name]
	v, n, ok := estimate(pts, estimator)
	f.N = n
	if !ok {
		if n == 0 {
			f.Note = "no samples"
		} else {
			f.Note = fmt.Sprintf("%s needs at least 2 samples, got %d", estimator, n)
		}
		return f
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		f.Note = "estimator produced a non-finite value"
		return f
	}
	rounded := math.Round(v*1000) / 1000
	f.Value = &rounded
	f.Holds = satisfies(v, op, threshold)
	return f
}

// Each constructor is named for the condition it states.

func fHostFpsSteady(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "encoder.fps", estimatorP10, opGTE, classifierMinHostFps, unitFPS)
}
func fHostFpsNotSteady(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "encoder.fps", estimatorP10, opLT, classifierMinHostFps, unitFPS)
}
func fEncoderHasHeadroom(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "encoder.encode_ms", estimatorP95, opLT, encoderCeilingMs, unitMS)
}
func fEncoderAtCeiling(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "encoder.encode_ms", estimatorP95, opGTE, encoderCeilingMs, unitMS)
}
func fLossQuiet(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "transport.packets_lost", estimatorDelta, opLTE, congestionLossDelta, unitCount)
}
func fLossClimbing(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "transport.packets_lost", estimatorDelta, opGT, congestionLossDelta, unitCount)
}
func fRttQuiet(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "transport.rtt_ms", estimatorP95, opLTE, congestionRttP95Ms, unitMS)
}
func fRttElevated(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "transport.rtt_ms", estimatorP95, opGT, congestionRttP95Ms, unitMS)
}
func fNoJudder(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "client.present_interval_sd_ms", estimatorMax, opLT, hitchSdThresholdMs, unitMS)
}
func fJudder(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "client.present_interval_sd_ms", estimatorMax, opGTE, hitchSdThresholdMs, unitMS)
}

// Present σ is a good judder detector and a terrible explanation: on 2026-08-22
// a 2560x1440@120 h265 session with σ 1.9 ms and zero drops was investigated as
// an encoder fault because the mean present fps read 88-108 (a missed vsync at
// source fps == display Hz doubles one interval and drags the mean). The next
// two falsifiers are informative and additive — they change no classification.

// fNoLongPresentFrames asserts no interval exceeded 2.5x the median (above the
// doubled vsync-beat band, so a beat can never trip it) — the falsifier that
// separates "120 fps with a beat" from "120 fps with a stall", which the mean
// cannot since both drag it to ~107. Max over the window: one is enough.
func fNoLongPresentFrames(s map[string][]seriesPoint) Falsifier {
	f := falsifierFor(s, "client.present_long_frames", estimatorMax, opEQ, 0, unitCount)
	if f.Note == "" && f.Value != nil && *f.Value > 0 {
		f.Note = "at least one presentation interval exceeded 2.5x the window median — a stall, not the vsync beat"
	}
	return f
}

// fPresentBeat carries the doubled-interval share; a fraction cannot exceed 1,
// so this never fails on its own. Read beside client.present_long_frames: a
// beat with no long frames is a healthy stream at source fps == display Hz.
func fPresentBeat(s map[string][]seriesPoint) Falsifier {
	f := falsifierFor(s, "client.present_beat_fraction", estimatorMax, opLTE, 1, unitFraction)
	if f.Note == "" && f.Value != nil && *f.Value > 0 {
		f.Note = "vsync beat is inherent at source fps == display Hz; read it with client.present_long_frames"
	}
	return f
}

func fTabVisible(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "client.is_hidden", estimatorAny, opEQ, 0, unitBool)
}
func fTabHidden(s map[string][]seriesPoint) Falsifier {
	return falsifierFor(s, "client.is_hidden", estimatorAny, opEQ, 1, unitBool)
}

// falsifiersFor returns the falsifier set for a verdict: for a likely_* verdict,
// the conditions that fired followed by the guard conditions ruled out; for
// nominal, the full set a healthy window must satisfy. An unrecognised verdict
// string falls through to the nominal set rather than an empty argument.
func falsifiersFor(verdict string, s map[string][]seriesPoint) []Falsifier {
	switch verdict {
	case verdictNetworkCongestion:
		return []Falsifier{
			fLossClimbing(s), fRttElevated(s),
			// Ruled out: encoder had headroom, tab visible.
			fEncoderHasHeadroom(s), fTabVisible(s),
		}
	case verdictEncoderSaturation:
		return []Falsifier{
			fEncoderAtCeiling(s), fHostFpsNotSteady(s),
			// Ruled out: no congestion window.
			fLossQuiet(s), fRttQuiet(s),
		}
	case verdictClientPresentingLimit:
		return []Falsifier{
			fJudder(s), fHitchCount(s),
			// Ruled out: host fps steady, wire quiet, tab visible — leaves the
			// client present path as the explanation.
			fHostFpsSteady(s), fLossQuiet(s), fRttQuiet(s), fTabVisible(s),
			// Informative: stall vs. vsync beat.
			fNoLongPresentFrames(s), fPresentBeat(s),
		}
	case verdictIndeterminateClientHidden:
		f := []Falsifier{fTabHidden(s)}
		// Stated as an unassessable falsifier, not omitted.
		judder := fNoJudder(s)
		judder.Holds = false
		if judder.Note == "" {
			judder.Note = "not assessable: the tab was hidden, so presentation pacing reflects browser throttling"
		}
		return append(f, judder, fHostFpsSteady(s), fLossQuiet(s), fRttQuiet(s))
	default:
		// nominal, unknown, and future vocabulary.
		return []Falsifier{
			fHostFpsSteady(s), fEncoderHasHeadroom(s), fLossQuiet(s), fRttQuiet(s),
			fHitchCountBelow(s), fWorstPresentSd(s), fTabVisible(s),
			// Informative: explains a low panel headline on an otherwise-healthy
			// session (2026-08-22).
			fNoLongPresentFrames(s), fPresentBeat(s),
		}
	}
}

func clockOf(c *telemetry.Clock, al alignment) VerdictClock {
	if c == nil {
		return VerdictClock{Quality: clockUnmeasured, Applied: false}
	}
	offset, unc := c.ClientOffsetMs, c.UncertaintyMs
	return VerdictClock{
		Quality:       clockMeasured,
		OffsetMs:      &offset,
		UncertaintyMs: &unc,
		Applied:       al.Applied,
		AgeMs:         al.AgeMs,
	}
}

// evidenceTier: "full" requires both host and client AND an aligned clock; an
// unmeasured clock caps it at host_only even with client samples present.
func evidenceTier(nHost, nClient int, clock VerdictClock) string {
	hostOK := nHost >= minSamplesForTier
	clientOK := nClient >= minSamplesForTier
	switch {
	case hostOK && clientOK:
		if clock.Quality != clockMeasured {
			return tierHostOnly
		}
		return tierFull
	case hostOK:
		return tierHostOnly
	case clientOK:
		return tierClientOnly
	default:
		return tierInsufficient
	}
}

// reasonFor: one-sentence finding, window, sample counts, then caveats.
func reasonFor(v Verdict) string {
	spanS := float64(v.Window.ToMs-v.Window.FromMs) / 1000
	var head string
	switch v.Verdict {
	case verdictNetworkCongestion:
		head = "Packet loss and round-trip time both crossed their congestion thresholds"
	case verdictEncoderSaturation:
		head = "Encode time reached the per-frame ceiling while host frame rate fell below steady"
	case verdictClientPresentingLimit:
		head = "Presentation pacing juddered on a visible tab while the host held its frame rate and the wire stayed quiet"
	case verdictNominal:
		head = "No congestion, encoder-saturation, or presentation-judder signal"
	case verdictIndeterminateClientHidden:
		head = "Nothing negative fired, but the client tab was hidden, so presentation cannot be assessed"
	case verdictUnknown:
		head = "No usable signal in the window"
	default:
		head = "Verdict " + v.Verdict
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s over a %.0f s window (%d host, %d client samples).",
		head, spanS, v.Window.NHost, v.Window.NClient)

	if v.Clock.Quality != clockMeasured {
		b.WriteString(" The client clock is unmeasured, so host and client series cannot be aligned;" +
			" evidence tier is capped below full, and any cross-source timing claim is downgraded" +
			" to what one source can support on its own.")
	} else if v.Clock.Applied {
		fmt.Fprintf(&b, " Client series were aligned onto the host clock (offset %.1f ms ± %.1f ms).",
			deref64(v.Clock.OffsetMs), deref64(v.Clock.UncertaintyMs))
	}
	if v.Window.WarmupExcludedMs > 0 {
		fmt.Fprintf(&b, " The first %.0f s of the window is session warm-up and was excluded from"+
			" hitch detection and the host frame-rate floor.",
			float64(v.Window.WarmupExcludedMs)/1000)
	}
	switch v.EvidenceTier {
	case tierInsufficient:
		b.WriteString(" Neither side reported enough samples to support a conclusion.")
	case tierClientOnly:
		b.WriteString(" No host samples in this window — this rests on client telemetry alone.")
	case tierHostOnly:
		if v.Window.NClient < minSamplesForTier {
			b.WriteString(" No client samples in this window — this rests on host telemetry alone.")
		}
	}

	// Name what's unsatisfied — the weakest points of the verdict.
	var missing []string
	for _, f := range v.Falsifiers {
		if !f.Holds {
			missing = append(missing, f.Name)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, " Unsatisfied: %s.", strings.Join(missing, ", "))
	}
	return b.String()
}

// eventSuffix names discrete facts a series-based verdict can't see (never
// flips the classification): on 2026-08-22 a peer-rejected HEVC m-line was
// investigated as an encoder stall for lack of exactly this.
func eventSuffix(events []traceEventResp, fromMs, toMs int64) string {
	var stalls, rejects, xids int
	for _, e := range events {
		if e.TsUnixMs < fromMs || e.TsUnixMs > toMs {
			continue
		}
		switch e.Type {
		case "encoder.stall":
			// Detection half only; a recovery in-window shouldn't double-count.
			var p struct {
				Phase string `json:"phase"`
			}
			if json.Unmarshal(e.Payload, &p) == nil && p.Phase == "recovered" {
				continue
			}
			stalls++
		case "sdp.answer_applied":
			var p struct {
				RejectedCount int `json:"rejected_count"`
			}
			if json.Unmarshal(e.Payload, &p) == nil && p.RejectedCount > 0 {
				rejects++
			}
		case "host.xid", "host.gpu_fault":
			xids++
		}
	}
	var parts []string
	if stalls > 0 {
		parts = append(parts, pluralise(stalls, "an encoder stall", "encoder stalls"))
	}
	if rejects > 0 {
		parts = append(parts, pluralise(rejects, "a rejected m-line", "rejected m-lines"))
	}
	if xids > 0 {
		parts = append(parts, pluralise(xids, "a GPU fault", "GPU faults"))
	}
	if len(parts) == 0 {
		return ""
	}
	return " Also in this window: " + joinWithAnd(parts) + " — see events."
}

func pluralise(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func joinWithAnd(parts []string) string {
	switch len(parts) {
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

// buildVerdict: pure, same inputs same value, no I/O.
func buildVerdict(in verdictInputs) Verdict {
	evidence := in.Classifier.Evidence
	if evidence == nil {
		evidence = []string{}
	}
	clock := clockOf(in.Clock, in.Align)
	fs := falsifiersFor(in.Classifier.Verdict, in.Series)
	fs = annotateCrossSource(in.Classifier.Verdict, fs, in.Align)
	fs = annotateWarmup(fs, in.FullSeries, in.Warmup)
	v := Verdict{
		Verdict:  in.Classifier.Verdict,
		Evidence: evidence,
		Window: VerdictWindow{
			FromMs: in.FromMs, ToMs: in.ToMs, NHost: in.NHost, NClient: in.NClient,
			WarmupExcludedMs: in.Warmup.ExcludedMs,
		},
		Clock:             clock,
		EvidenceTier:      evidenceTier(in.NHost, in.NClient, clock),
		Falsifiers:        fs,
		ThresholdsVersion: thresholdsVersion,
	}
	v.Reason = reasonFor(v) + eventSuffix(in.Events, in.FromMs, in.ToMs)
	return v
}

// A hitch is a property of the window, not one sample: on 2026-08-23 a healthy
// 300 s session flipped to likely_client_presentation_limit on a single σ of
// 18.058 ms out of 54 samples under a max-based rule. The count row below is
// now load-bearing; the max row stays informative and cannot fail on its own.

const hitchSeriesName = "client.present_interval_sd_ms"

func countAtOrAbove(pts []seriesPoint, threshold float64) float64 {
	var n float64
	for _, p := range pts {
		if p.V >= threshold {
			n++
		}
	}
	return n
}

func hitchCountFalsifier(s map[string][]seriesPoint, op string) Falsifier {
	pts := s[hitchSeriesName]
	f := Falsifier{
		Name:      hitchSeriesName,
		Estimator: estimatorCountGE,
		Op:        op,
		Threshold: hitchMinSamples,
		Unit:      unitCount,
		N:         len(pts),
		Note: fmt.Sprintf("samples whose present-interval σ reached %.0f ms; a hitch window needs at least %d of them, so one outlier no longer decides the verdict",
			hitchSdThresholdMs, int(hitchMinSamples)),
	}
	if len(pts) == 0 {
		f.Note = "no samples"
		return f
	}
	c := countAtOrAbove(pts, hitchSdThresholdMs)
	f.Value = &c
	f.Holds = satisfies(c, op, hitchMinSamples)
	return f
}

func fHitchCount(s map[string][]seriesPoint) Falsifier {
	return hitchCountFalsifier(s, opGTE)
}

// fHitchCountBelow: too few over-threshold samples to be a hitch (nominal).
func fHitchCountBelow(s map[string][]seriesPoint) Falsifier {
	return hitchCountFalsifier(s, opLT)
}

// fWorstPresentSd is informative (compared against its own floor, can't fail):
// the worst single σ next to the count that actually decides.
func fWorstPresentSd(s map[string][]seriesPoint) Falsifier {
	f := falsifierFor(s, hitchSeriesName, estimatorMax, opGTE, 0, unitMS)
	if f.N > 0 && f.Note == "" {
		f.Note = fmt.Sprintf("worst single-sample present-interval σ in the window; informative — the hitch condition is the %s count row beside it",
			estimatorCountGE)
	}
	return f
}

// seriesSource maps a taxonomy series to its reporter (host- or client-sourced).
func seriesSource(name string) string {
	for _, m := range taxonomyV1 {
		if m.name == name {
			return m.source
		}
	}
	return ""
}

// primarySourceFor is the reporter whose numbers fired the verdict; a falsifier
// from the other side gets a cross-source note. nominal has no primary source.
func primarySourceFor(verdict string) string {
	switch verdict {
	case verdictNetworkCongestion, verdictClientPresentingLimit, verdictIndeterminateClientHidden:
		return telemetry.SourceBrowser
	case verdictEncoderSaturation:
		return telemetry.SourceAgent
	default:
		return ""
	}
}

func annotateCrossSource(verdict string, fs []Falsifier, al alignment) []Falsifier {
	primary := primarySourceFor(verdict)
	if primary == "" {
		return fs
	}
	note := al.crossSourceNote()
	out := make([]Falsifier, 0, len(fs))
	for _, f := range fs {
		src := seriesSource(f.Name)
		if src != "" && src != primary {
			if f.Note == "" {
				f.Note = note
			} else {
				f.Note = f.Note + "; " + note
			}
		}
		out = append(out, f)
	}
	return out
}

// annotateWarmup tells "series was empty" apart from "every sample fell inside
// warm-up" — both give n=0/holds=false, but only one is worth a re-check.
func annotateWarmup(fs []Falsifier, full map[string][]seriesPoint, w warmupExclusion) []Falsifier {
	if w.ExcludedMs == 0 || full == nil {
		return fs
	}
	out := make([]Falsifier, 0, len(fs))
	for _, f := range fs {
		if f.N == 0 && warmupSensitiveSeries[f.Name] && len(full[f.Name]) > 0 {
			f.Note = fmt.Sprintf("no samples outside warm-up: all %d fell within %.0f s of the session reaching running, and warm-up is not evidence about the session",
				len(full[f.Name]), warmupExcludeS)
		}
		out = append(out, f)
	}
	return out
}

func deref64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
