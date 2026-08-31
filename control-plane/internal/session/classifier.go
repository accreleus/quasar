package session

import (
	"encoding/json"
	"math"
	"sort"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// ST-06 — diagnostic-bundle assembly + classifier v0 (Observability v2,
// contract-amendment.md §B.5, trace-format.md §2/§3/§4). This file is the pure
// diagnostic brain: turns telemetry.Slice into the taxonomy series, the four
// v1 derived windows, and the classifier verdict. No DB, no HTTP —
// diagnostic_handler.go wires it in. Purity keeps it unit-testable; every
// threshold is a named const below. The classifier is observational only —
// emits a verdict and evidence, takes no action.

// taxonomyMapping: the diagnostic view is curated, not exhaustive — a raw
// session_metrics key with no mapping here is still readable via the raw
// metrics surface (GET .../metrics).
type taxonomyMapping struct {
	name   string // e.g. "encoder.encode_ms"
	source string // session_metrics.source
	rawKey string // session_metrics.metrics JSONB key
}

// taxonomyV1 is derived from docs/session-trace/metrics.json, not transcribed,
// so it cannot drift from it. frames_dropped stays source-scoped (appears
// under both encoder.* and client.* with different meaning) since the
// manifest keys on (source, key).
var taxonomyV1 = buildTaxonomyV1()

func buildTaxonomyV1() []taxonomyMapping {
	entries := telemetry.Manifest().TaxonomyEntries()
	out := make([]taxonomyMapping, 0, len(entries))
	for _, e := range entries {
		out = append(out, taxonomyMapping{e.Taxonomy, e.Source, e.Key})
	}
	return out
}

// rvfcQualifiedRawKeys is the manifest's `rvfc_qualified` flag: client keys whose
// value exists only while the browser surfaces an RVFC captureTime.
var rvfcQualifiedRawKeys = telemetry.Manifest().RVFCQualifiedKeys()

type seriesPoint struct {
	TsUnixMs int64   `json:"ts_unix_ms"`
	V        float64 `json:"v"`
}

// normalizeSeries maps raw samples onto the taxonomy v1 series, oldest-first.
// Reads an already clock-aligned set (telemetry/align.go): agent points
// (host wall-clock) and browser points (Date.now()) must not share a raw ts
// axis, or every cross-source rule would silently assume the clocks agreed.
func normalizeSeries(aligned telemetry.AlignedSet, wanted map[string]bool) map[string][]seriesPoint {
	bySource := map[string][]taxonomyMapping{}
	for _, m := range taxonomyV1 {
		if wanted != nil && !wanted[m.name] {
			continue
		}
		bySource[m.source] = append(bySource[m.source], m)
	}

	out := map[string][]seriesPoint{}
	for _, s := range aligned.Samples {
		mappings := bySource[s.Source]
		if len(mappings) == 0 {
			continue
		}
		fields := decodeNumeric(s.Metrics)
		if fields == nil {
			continue
		}
		for _, m := range mappings {
			// Legacy rows lack a method marker; never reinterpret their staged
			// numbers as current RVFC telemetry.
			if rvfcQualifiedRawKeys[m.rawKey] && fields["rvfc_capture_time_available"] != 1 {
				continue
			}
			v, ok := fields[m.rawKey]
			if !ok {
				continue
			}
			out[m.name] = append(out[m.name], seriesPoint{TsUnixMs: s.TsUnixMs, V: v})
		}
	}
	for name := range out {
		pts := out[name]
		sort.Slice(pts, func(i, j int) bool { return pts[i].TsUnixMs < pts[j].TsUnixMs })
		out[name] = pts
	}
	return out
}

// decodeNumeric pulls numeric fields from a metrics JSONB object; a bool is
// coerced (true→1, false→0) so is_hidden still reads. Nil on malformed/empty.
func decodeNumeric(raw json.RawMessage) map[string]float64 {
	if len(raw) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	out := make(map[string]float64, len(obj))
	for k, rv := range obj {
		var f float64
		if err := json.Unmarshal(rv, &f); err == nil {
			out[k] = f
			continue
		}
		var b bool
		if err := json.Unmarshal(rv, &b); err == nil {
			if b {
				out[k] = 1
			} else {
				out[k] = 0
			}
		}
	}
	return out
}

// percentile returns the linear-interpolated p-th percentile (0..100) of vs;
// empty → NaN.
func percentile(vs []float64, p float64) float64 {
	if len(vs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	if len(s) == 1 {
		return s[0]
	}
	rank := (p / 100) * float64(len(s)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return s[lo]
	}
	frac := rank - float64(lo)
	return s[lo] + frac*(s[hi]-s[lo])
}

func values(pts []seriesPoint) []float64 {
	out := make([]float64, len(pts))
	for i, p := range pts {
		out[i] = p.V
	}
	return out
}

func meanOf(vs []float64) float64 {
	if len(vs) == 0 {
		return math.NaN()
	}
	var sum float64
	for _, v := range vs {
		sum += v
	}
	return sum / float64(len(vs))
}

// maxV / minV over a series; NaN for an empty series.
func maxV(pts []seriesPoint) float64 {
	if len(pts) == 0 {
		return math.NaN()
	}
	m := pts[0].V
	for _, p := range pts[1:] {
		if p.V > m {
			m = p.V
		}
	}
	return m
}

func lastV(pts []seriesPoint) float64 {
	if len(pts) == 0 {
		return math.NaN()
	}
	return pts[len(pts)-1].V
}

// derivedWindow shapes match contract-amendment.md §B.5: small lists of
// {from_ms,to_ms, …} markers computed over the aligned series + events.
type hitchWindow struct {
	FromMs              int64   `json:"from_ms"`
	ToMs                int64   `json:"to_ms"`
	PresentIntervalSdMs float64 `json:"present_interval_sd_ms"`
}

type abrDownshiftWindow struct {
	TsUnixMs int64   `json:"ts_unix_ms"`
	FromKbps float64 `json:"from_kbps"`
	ToKbps   float64 `json:"to_kbps"`
}

type encoderSatWindow struct {
	FromMs      int64   `json:"from_ms"`
	ToMs        int64   `json:"to_ms"`
	EncodeMsP95 float64 `json:"encode_ms_p95"`
}

type congestionWindow struct {
	FromMs           int64   `json:"from_ms"`
	ToMs             int64   `json:"to_ms"`
	PacketsLostDelta float64 `json:"packets_lost_delta"`
	RttMsP95         float64 `json:"rtt_ms_p95"`
}

type derivedWindows struct {
	Hitches                 []hitchWindow        `json:"hitches"`
	ABRDownshifts           []abrDownshiftWindow `json:"abr_downshifts"`
	EncoderSaturation       []encoderSatWindow   `json:"encoder_saturation"`
	LikelyNetworkCongestion []congestionWindow   `json:"likely_network_congestion"`
}

// computeDerivedWindows builds the four v1 derived windows. Slices are
// non-nil (empty, not null) so the bundle JSON always has all four keys.
func computeDerivedWindows(series map[string][]seriesPoint, events []traceEventResp) derivedWindows {
	dw := derivedWindows{
		Hitches:                 []hitchWindow{},
		ABRDownshifts:           []abrDownshiftWindow{},
		EncoderSaturation:       []encoderSatWindow{},
		LikelyNetworkCongestion: []congestionWindow{},
	}

	// hitches: each present_interval_sd sample over the threshold is a one-sample
	// window. One sample is not a hitch — on 2026-08-23 a healthy 300 s session
	// flipped to likely_client_presentation_limit on a single σ of 18.058 ms out
	// of 54 under a max-based rule. A hitch now needs hitchMinSamples samples
	// over threshold; below that the samples are still served, just not a window.
	// `series` is already warm-up-trimmed by the caller (aligned.go).
	for _, p := range series["client.present_interval_sd_ms"] {
		if p.V >= hitchSdThresholdMs {
			dw.Hitches = append(dw.Hitches, hitchWindow{
				FromMs: p.TsUnixMs, ToMs: p.TsUnixMs, PresentIntervalSdMs: p.V,
			})
		}
	}
	if len(dw.Hitches) < hitchMinSamples {
		dw.Hitches = []hitchWindow{}
	}

	// abr_downshifts: read straight off abr.retarget events (to_kbps < from_kbps),
	// the source of truth, not a derivative of the setpoint series.
	for _, e := range events {
		if e.Type != "abr.retarget" {
			continue
		}
		var p struct {
			FromKbps float64 `json:"from_kbps"`
			ToKbps   float64 `json:"to_kbps"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.ToKbps < p.FromKbps {
			dw.ABRDownshifts = append(dw.ABRDownshifts, abrDownshiftWindow{
				TsUnixMs: e.TsUnixMs, FromKbps: p.FromKbps, ToKbps: p.ToKbps,
			})
		}
	}

	// encoder_saturation: encode_ms p95 at/over ceiling emits one window
	// spanning the whole encode_ms series (v1 is whole-window, not sub-windowed).
	enc := series["encoder.encode_ms"]
	if len(enc) > 0 {
		p95 := percentile(values(enc), 95)
		if p95 >= encoderCeilingMs {
			dw.EncoderSaturation = append(dw.EncoderSaturation, encoderSatWindow{
				FromMs: enc[0].TsUnixMs, ToMs: enc[len(enc)-1].TsUnixMs, EncodeMsP95: p95,
			})
		}
	}

	// likely_network_congestion: loss rising AND rtt p95 elevated.
	loss := series["transport.packets_lost"]
	rtt := series["transport.rtt_ms"]
	if len(loss) >= 2 && len(rtt) > 0 {
		lossDelta := loss[len(loss)-1].V - loss[0].V
		rttP95 := percentile(values(rtt), 95)
		if lossDelta > congestionLossDelta && rttP95 > congestionRttP95Ms {
			dw.LikelyNetworkCongestion = append(dw.LikelyNetworkCongestion, congestionWindow{
				FromMs:           loss[0].TsUnixMs,
				ToMs:             loss[len(loss)-1].TsUnixMs,
				PacketsLostDelta: lossDelta,
				RttMsP95:         rttP95,
			})
		}
	}

	return dw
}

// Classifier verdict strings, the closed v1 set (ST-07 #324 split the old
// overloaded "unknown" into nominal / indeterminate_client_hidden / unknown).
const (
	verdictEncoderSaturation         = "likely_encoder_saturation"
	verdictNetworkCongestion         = "likely_network_congestion"
	verdictClientPresentingLimit     = "likely_client_presentation_limit"
	verdictNominal                   = "nominal"
	verdictIndeterminateClientHidden = "indeterminate_client_hidden"
	verdictUnknown                   = "unknown"
)

type classifierVerdict struct {
	Verdict  string   `json:"verdict"`
	Evidence []string `json:"evidence"`
}

// classify runs the v1 rules in priority order: (1) likely_network_congestion
// — loss rising + rtt p95 elevated, checked first as the upstream cause; (2)
// likely_encoder_saturation — encode_ms p95 at/over ceiling AND host fps not
// steady; (3) likely_client_presentation_limit — judder with host fps steady
// and no congestion, gated on is_hidden (a backgrounded tab throttles
// presentation and is not a real degradation, trace-format.md §3.3, #108);
// (4) nominal; (5) indeterminate_client_hidden; (6) unknown.
//
// classifyInputs.Series is the assessed (warm-up trimmed) map; Align is what
// the rules may know about the clock.
type classifyInputs struct {
	Series  map[string][]seriesPoint
	Derived derivedWindows
	Events  []traceEventResp
	Align   alignment
}

func classify(in classifyInputs) classifierVerdict {
	series, dw, events, al := in.Series, in.Derived, in.Events, in.Align
	var ev []string

	hostFps := series["encoder.fps"]
	hostFpsSteady := len(hostFps) > 0 && percentile(values(hostFps), 10) >= classifierMinHostFps

	hidden := windowHadHiddenTab(series, events)
	if hidden {
		ev = append(ev, "client tab was hidden/backgrounded during the window (presentation throttling, not a real degradation)")
	} else {
		ev = append(ev, "client tab not hidden during the window")
	}

	// (1) Network congestion.
	if len(dw.LikelyNetworkCongestion) > 0 {
		w := dw.LikelyNetworkCongestion[0]
		ev = append([]string{
			sprintfCongestion(w),
		}, ev...)
		// Downshift is agent-sourced, congestion window browser-sourced; only the
		// aligned clock can check they coincide.
		switch {
		case al.Applied && abrDownshiftInCongestion(dw.ABRDownshifts, w, al.ToleranceMs):
			ev = append(ev, "ABR downshift coincident with the congestion window (governor reacting to loss/rtt)")
		case al.Applied:
			if len(dw.ABRDownshifts) > 0 {
				ev = append(ev, "ABR downshift present but NOT coincident with the congestion window on the aligned clock")
			}
		case len(dw.ABRDownshifts) > 0:
			ev = append(ev, "ABR downshift in the window; "+al.crossSourceNote())
		}
		return classifierVerdict{Verdict: verdictNetworkCongestion, Evidence: ev}
	}

	// (2) Encoder saturation.
	if len(dw.EncoderSaturation) > 0 {
		w := dw.EncoderSaturation[0]
		if !hostFpsSteady {
			ev = append([]string{
				sprintfEncoderSat(w),
				"host encoder fps not steady (encoder cannot sustain the frame rate)",
			}, ev...)
			return classifierVerdict{Verdict: verdictEncoderSaturation, Evidence: ev}
		}
		// At ceiling but coping: record and fall through, not the fault on its own.
		ev = append(ev, sprintfEncoderSat(w)+"; but host fps steady — at capacity, coping")
	}

	// (3) Client presentation limit. Judder is client-sourced, fps guard host-
	// sourced: with the clock applied a hitch on a host fps dip is attributed
	// to the host even if the whole-window p10 reads steady.
	if len(dw.Hitches) > 0 && hostFpsSteady && !hidden {
		onHostDip := al.Applied &&
			hitchCoincidesWithHostDip(dw.Hitches, hostFpsDips(series), al.ToleranceMs)
		if onHostDip {
			ev = append(ev, sprintfHitch(dw.Hitches)+
				"; but the judder coincides with a host encoder fps dip on the aligned clock — not the client present path")
		} else {
			second := "host encoder fps steady and no network-congestion window — symptom is on the client present path"
			if !al.Applied {
				second += " (" + al.crossSourceNote() + ")"
			}
			ev = append([]string{sprintfHitch(dw.Hitches), second}, ev...)
			return classifierVerdict{Verdict: verdictClientPresentingLimit, Evidence: ev}
		}
	}
	if len(dw.Hitches) > 0 && hidden {
		ev = append(ev, "presentation judder present but tab was hidden — attributed to backgrounding, not the stream")
	}

	// (4)/(5) No negative signal fired.
	if hidden {
		ev = append([]string{"no congestion or encoder-saturation signal in the window; client tab was hidden/backgrounded, so presentation cannot be assessed"}, ev...)
		return classifierVerdict{Verdict: verdictIndeterminateClientHidden, Evidence: ev}
	}
	ev = append([]string{"no congestion, encoder-saturation, or presentation-judder signal in the window — healthy"}, ev...)
	return classifierVerdict{Verdict: verdictNominal, Evidence: ev}
}

// windowHadHiddenTab: is_hidden sample set, or a visibility_changed event
// reported hidden=true. Either disqualifies a client-presentation verdict
// (trace-format.md §3.3, #108).
func windowHadHiddenTab(series map[string][]seriesPoint, events []traceEventResp) bool {
	for _, p := range series["client.is_hidden"] {
		if p.V != 0 {
			return true
		}
	}
	for _, e := range events {
		if e.Type != "client.visibility_changed" {
			continue
		}
		// telemetry.HiddenFlag reads both encodings: is_hidden 0/1 or hidden true/false.
		if hidden, present := telemetry.HiddenFlag(e.Payload); present && hidden {
			return true
		}
	}
	return false
}

func sprintfCongestion(w congestionWindow) string {
	return jsonEvidence("network congestion", map[string]any{
		"packets_lost_delta": w.PacketsLostDelta, "rtt_ms_p95": w.RttMsP95,
	})
}

func sprintfEncoderSat(w encoderSatWindow) string {
	return jsonEvidence("encoder at ceiling", map[string]any{"encode_ms_p95": w.EncodeMsP95})
}

func sprintfHitch(hs []hitchWindow) string {
	var worst float64
	for _, h := range hs {
		if h.PresentIntervalSdMs > worst {
			worst = h.PresentIntervalSdMs
		}
	}
	return jsonEvidence("presentation judder", map[string]any{
		"hitch_count": len(hs), "present_interval_sd_ms_max": worst,
	})
}

// jsonEvidence renders a stable "label: {json}" string, readable as prose and
// parseable for its structured tail.
func jsonEvidence(label string, fields map[string]any) string {
	b, _ := json.Marshal(fields)
	return label + ": " + string(b)
}
