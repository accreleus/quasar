package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// ST-06 — diagnostic-bundle + classifier v0 tests (contract-amendment.md §B.5).
//
// Two layers:
//   1. Pure-classifier unit tests over synthetic series (no DB): a congestion trace →
//      likely_network_congestion; an encoder-ceiling trace → likely_encoder_saturation;
//      a present-jitter-with-steady-host trace → likely_client_presentation_limit; an
//      is_hidden window is NOT flagged.
//   2. HTTP/DB tests: admin-only gate (non-admin bearer → 403); unmeasured clock surfaces
//      as {"unmeasured": true} in the bundle; the assembled bundle's verdict over real
//      session_metrics + session_trace_events rows matches the synthetic shape.

// --- pure classifier unit tests ----------------------------------------------

// pt builds a series of points at 1-second spacing starting at base, one per value.
func pt(base int64, vals ...float64) []seriesPoint {
	out := make([]seriesPoint, len(vals))
	for i, v := range vals {
		out[i] = seriesPoint{TsUnixMs: base + int64(i)*1000, V: v}
	}
	return out
}

func TestClassifyNetworkCongestion(t *testing.T) {
	base := int64(1_000_000)
	series := map[string][]seriesPoint{
		// Loss climbing hard and rtt p95 well over 50 ms → congestion.
		"transport.packets_lost": pt(base, 0, 4, 12, 30, 55),
		"transport.rtt_ms":       pt(base, 40, 70, 95, 110, 120),
		// Encoder is fine and host fps steady — so it must NOT be blamed.
		"encoder.encode_ms": pt(base, 3, 4, 4, 3, 4),
		"encoder.fps":       pt(base, 60, 59, 60, 60, 59),
	}
	events := []traceEventResp{
		{Source: "agent", TsUnixMs: base + 2000, Type: "abr.retarget",
			Payload: json.RawMessage(`{"from_kbps":14000,"to_kbps":9000,"reason":"gcc_downshift"}`)},
	}
	dw := computeDerivedWindows(series, events)
	if len(dw.LikelyNetworkCongestion) == 0 {
		t.Fatalf("expected a congestion derived window, got none")
	}
	if len(dw.ABRDownshifts) != 1 {
		t.Fatalf("expected 1 abr downshift, got %d", len(dw.ABRDownshifts))
	}
	v := classify(classifyInputs{Series: series, Derived: dw, Events: events})
	if v.Verdict != verdictNetworkCongestion {
		t.Fatalf("verdict = %q want %q (evidence: %v)", v.Verdict, verdictNetworkCongestion, v.Evidence)
	}
	if len(v.Evidence) == 0 {
		t.Fatalf("congestion verdict carried no evidence")
	}
}

func TestClassifyEncoderSaturation(t *testing.T) {
	base := int64(2_000_000)
	series := map[string][]seriesPoint{
		// encode_ms p95 over the 16 ms ceiling AND host fps collapsed below steady.
		"encoder.encode_ms": pt(base, 18, 19, 20, 21, 22),
		"encoder.fps":       pt(base, 44, 43, 41, 40, 42),
		// No loss/rtt congestion signal.
		"transport.packets_lost": pt(base, 0, 0, 1, 1, 1),
		"transport.rtt_ms":       pt(base, 12, 14, 13, 12, 15),
	}
	dw := computeDerivedWindows(series, nil)
	if len(dw.EncoderSaturation) == 0 {
		t.Fatalf("expected an encoder-saturation derived window, got none")
	}
	if len(dw.LikelyNetworkCongestion) != 0 {
		t.Fatalf("did not expect a congestion window: %+v", dw.LikelyNetworkCongestion)
	}
	v := classify(classifyInputs{Series: series, Derived: dw, Events: nil})
	if v.Verdict != verdictEncoderSaturation {
		t.Fatalf("verdict = %q want %q (evidence: %v)", v.Verdict, verdictEncoderSaturation, v.Evidence)
	}
}

func TestClassifyClientPresentationLimit(t *testing.T) {
	base := int64(3_000_000)
	series := map[string][]seriesPoint{
		// Present-interval σ over the judder threshold, host fps steady, no congestion,
		// tab NOT hidden → client presentation limit.
		"client.present_interval_sd_ms": pt(base, 20, 24, 22, 26, 21),
		"client.is_hidden":              pt(base, 0, 0, 0, 0, 0),
		"encoder.fps":                   pt(base, 60, 60, 59, 60, 60),
		"encoder.encode_ms":             pt(base, 3, 4, 3, 4, 3),
		"transport.packets_lost":        pt(base, 0, 0, 0, 1, 1),
		"transport.rtt_ms":              pt(base, 10, 11, 12, 11, 10),
	}
	dw := computeDerivedWindows(series, nil)
	if len(dw.Hitches) == 0 {
		t.Fatalf("expected hitches, got none")
	}
	v := classify(classifyInputs{Series: series, Derived: dw, Events: nil})
	if v.Verdict != verdictClientPresentingLimit {
		t.Fatalf("verdict = %q want %q (evidence: %v)", v.Verdict, verdictClientPresentingLimit, v.Evidence)
	}
}

func TestClassifyHiddenTabNotFlagged(t *testing.T) {
	base := int64(4_000_000)
	series := map[string][]seriesPoint{
		// Identical judder to the presentation-limit case, but the tab is hidden — this
		// must NOT be flagged as a client presentation limit (false-positive guard).
		"client.present_interval_sd_ms": pt(base, 20, 24, 22, 26, 21),
		"client.is_hidden":              pt(base, 1, 1, 1, 1, 1),
		"encoder.fps":                   pt(base, 60, 60, 59, 60, 60),
		"encoder.encode_ms":             pt(base, 3, 4, 3, 4, 3),
		"transport.packets_lost":        pt(base, 0, 0, 0, 0, 0),
		"transport.rtt_ms":              pt(base, 10, 11, 12, 11, 10),
	}
	dw := computeDerivedWindows(series, nil)
	v := classify(classifyInputs{Series: series, Derived: dw, Events: nil})
	if v.Verdict == verdictClientPresentingLimit {
		t.Fatalf("hidden tab was flagged as a client presentation limit (false positive); evidence: %v", v.Evidence)
	}
	// ST-07 (#324): a hidden tab with no other negative signal is indeterminate, not
	// the old overloaded "unknown" — the classifier couldn't assess presentation only
	// because the tab was backgrounded.
	if v.Verdict != verdictIndeterminateClientHidden {
		t.Fatalf("hidden-tab verdict = %q want %q", v.Verdict, verdictIndeterminateClientHidden)
	}
	// And the visibility-event form of the guard.
	series2 := map[string][]seriesPoint{
		"client.present_interval_sd_ms": pt(base, 20, 24, 22, 26, 21),
		"encoder.fps":                   pt(base, 60, 60, 59, 60, 60),
	}
	events := []traceEventResp{
		{Source: "browser", TsUnixMs: base + 1000, Type: "client.visibility_changed",
			Payload: json.RawMessage(`{"hidden":true}`)},
	}
	dw2 := computeDerivedWindows(series2, events)
	v2 := classify(classifyInputs{Series: series2, Derived: dw2, Events: events})
	if v2.Verdict == verdictClientPresentingLimit {
		t.Fatalf("visibility_changed=hidden was flagged as client presentation limit; evidence: %v", v2.Evidence)
	}
	if v2.Verdict != verdictIndeterminateClientHidden {
		t.Fatalf("visibility_changed=hidden verdict = %q want %q", v2.Verdict, verdictIndeterminateClientHidden)
	}
}

// TestClassifyNominal is the ST-07 (#324) split's core case: no negative signal AND the
// tab was not hidden must produce the distinct "nominal" verdict, not "unknown" — a
// healthy session should read as healthy, not uninformative.
func TestClassifyNominal(t *testing.T) {
	base := int64(5_000_000)
	series := map[string][]seriesPoint{
		"encoder.encode_ms":             pt(base, 3, 4, 3, 4, 3),
		"encoder.fps":                   pt(base, 60, 60, 59, 60, 60),
		"transport.packets_lost":        pt(base, 0, 0, 0, 0, 0),
		"transport.rtt_ms":              pt(base, 10, 11, 12, 11, 10),
		"client.present_interval_sd_ms": pt(base, 4, 5, 4, 5, 4),
		"client.is_hidden":              pt(base, 0, 0, 0, 0, 0),
	}
	dw := computeDerivedWindows(series, nil)
	v := classify(classifyInputs{Series: series, Derived: dw, Events: nil})
	if v.Verdict != verdictNominal {
		t.Fatalf("verdict = %q want %q (evidence: %v)", v.Verdict, verdictNominal, v.Evidence)
	}
}

// TestClassifyIndeterminateClientHiddenNoJudder covers the exact screenshot scenario from
// #324: no negative signal at all (not even judder), but the tab was hidden. This must
// NOT read as "unknown" — it's indeterminate specifically because of the hidden tab.
func TestClassifyIndeterminateClientHiddenNoJudder(t *testing.T) {
	base := int64(6_000_000)
	series := map[string][]seriesPoint{
		"encoder.encode_ms":      pt(base, 3, 4, 3, 4, 3),
		"encoder.fps":            pt(base, 60, 60, 59, 60, 60),
		"transport.packets_lost": pt(base, 0, 0, 0, 0, 0),
		"transport.rtt_ms":       pt(base, 10, 11, 12, 11, 10),
		"client.is_hidden":       pt(base, 1, 1, 1, 1, 1),
	}
	dw := computeDerivedWindows(series, nil)
	v := classify(classifyInputs{Series: series, Derived: dw, Events: nil})
	if v.Verdict != verdictIndeterminateClientHidden {
		t.Fatalf("verdict = %q want %q (evidence: %v)", v.Verdict, verdictIndeterminateClientHidden, v.Evidence)
	}
}

// TestNormalizeSeriesTaxonomy verifies the normalize-at-read mapping: a raw agent
// sample's encode_ms → encoder.encode_ms, a raw browser sample's rtt_ms → transport.rtt_ms,
// and source-scoping (frames_dropped lands in the right namespace per source).
func TestNormalizeSeriesTaxonomy(t *testing.T) {
	samples := []telemetry.Sample{
		{Source: telemetry.SourceAgent, TsUnixMs: 100, Metrics: json.RawMessage(`{"encode_ms":4.6,"fps":59.8,"frames_dropped":2}`)},
		{Source: telemetry.SourceBrowser, TsUnixMs: 200, Metrics: json.RawMessage(`{"rtt_ms":28,"frames_dropped":7,"is_hidden":1}`)},
	}
	got := normalizeSeries(telemetry.AlignSeries(samples, nil, nil), nil)
	if len(got["encoder.encode_ms"]) != 1 || got["encoder.encode_ms"][0].V != 4.6 {
		t.Fatalf("encoder.encode_ms = %+v", got["encoder.encode_ms"])
	}
	if len(got["encoder.frames_dropped"]) != 1 || got["encoder.frames_dropped"][0].V != 2 {
		t.Fatalf("encoder.frames_dropped = %+v", got["encoder.frames_dropped"])
	}
	if len(got["client.frames_dropped"]) != 1 || got["client.frames_dropped"][0].V != 7 {
		t.Fatalf("client.frames_dropped = %+v", got["client.frames_dropped"])
	}
	if len(got["transport.rtt_ms"]) != 1 || got["transport.rtt_ms"][0].V != 28 {
		t.Fatalf("transport.rtt_ms = %+v", got["transport.rtt_ms"])
	}
	// The agent frames_dropped must NOT bleed into the client namespace and vice versa.
	if got["client.encode_ms"] != nil {
		t.Fatalf("encode_ms leaked to client namespace")
	}
	// names= filter: only the requested taxonomy series come back.
	filtered := normalizeSeries(telemetry.AlignSeries(samples, nil, nil), map[string]bool{"transport.rtt_ms": true})
	if len(filtered) != 1 || filtered["transport.rtt_ms"] == nil {
		t.Fatalf("names filter = %+v", filtered)
	}
}

func TestNormalizeSeriesGuardsUnmarkedGlassToGlassHistory(t *testing.T) {
	samples := []telemetry.Sample{
		{Source: telemetry.SourceBrowser, TsUnixMs: 100, Metrics: json.RawMessage(`{"glass_to_glass_ms":71,"network_pacing_ms":12,"decode_display_ms":30}`)},
		{Source: telemetry.SourceBrowser, TsUnixMs: 200, Metrics: json.RawMessage(`{"rvfc_capture_time_available":1,"abs_capture_time_negotiated":0,"glass_to_glass_ms":45,"network_pacing_ms":10,"decode_display_ms":20}`)},
	}
	got := normalizeSeries(telemetry.AlignSeries(samples, nil, nil), nil)
	if len(got["client.glass_to_glass_ms"]) != 1 || got["client.glass_to_glass_ms"][0].TsUnixMs != 200 {
		t.Fatalf("unqualified glass-to-glass history leaked: %+v", got["client.glass_to_glass_ms"])
	}
	if len(got["client.rvfc_capture_time_available"]) != 1 || got["client.abs_capture_time_negotiated"][0].V != 0 {
		t.Fatalf("RVFC method markers missing: %+v", got)
	}
}

// SPT-03: agentAdaptationLabels extracts the agent-reported host-side adaptation labels
// in chronological order, skipping browser samples and samples without the key (pre-SPT-03
// agents). The store returns newest-first; the helper reverses to oldest-first.
func TestAgentAdaptationLabels(t *testing.T) {
	// Mimic the store order: newest-first.
	samples := []telemetry.Sample{
		{Source: telemetry.SourceAgent, TsUnixMs: 300, Metrics: json.RawMessage(`{"fps":45,"adaptation_state":"encoder_saturated"}`)},
		{Source: telemetry.SourceBrowser, TsUnixMs: 250, Metrics: json.RawMessage(`{"adaptation_state":"network_congested"}`)}, // browser source ignored
		{Source: telemetry.SourceAgent, TsUnixMs: 200, Metrics: json.RawMessage(`{"fps":60}`)},                                 // no key → skipped
		{Source: telemetry.SourceAgent, TsUnixMs: 100, Metrics: json.RawMessage(`{"fps":59,"adaptation_state":"healthy"}`)},
	}
	got := agentAdaptationLabels(samples)
	if len(got) != 2 {
		t.Fatalf("expected 2 agent adaptation labels, got %d (%+v)", len(got), got)
	}
	// Chronological: healthy (100) then encoder_saturated (300).
	if got[0].TsUnixMs != 100 || got[0].State != "healthy" {
		t.Fatalf("first label = %+v, want {100, healthy}", got[0])
	}
	if got[1].TsUnixMs != 300 || got[1].State != "encoder_saturated" {
		t.Fatalf("second label = %+v, want {300, encoder_saturated}", got[1])
	}
}

func TestAgentAdaptationLabelsEmptyForPreSPT03(t *testing.T) {
	samples := []telemetry.Sample{
		{Source: telemetry.SourceAgent, TsUnixMs: 100, Metrics: json.RawMessage(`{"fps":60,"abr_mode":"protective"}`)},
	}
	if got := agentAdaptationLabels(samples); len(got) != 0 {
		t.Fatalf("expected no labels for a pre-SPT-03 agent, got %+v", got)
	}
}

// --- HTTP / DB tests ----------------------------------------------------------

// adminBundleSession is the common setup for the bundle/read tests: an admin token, an
// owner token, and a running session id owned by the owner.
func adminBundleSession(t *testing.T) (sid, ownerTok, adminTok string, store *Store, srvURL string) {
	t.Helper()
	pool := testDB(t)
	srv, authSvc, st := newMetricsServer(t, pool)
	ctx := context.Background()
	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email='admin@test.local'`); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	s := currentSeed(t, pool)
	sid = sessionForUser(t, st, s, owner.ID)
	ownerTok = loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")
	adminTok = loginTok(t, authSvc, "admin@test.local", "quasar-fixture-pw-01")
	return sid, ownerTok, adminTok, st, srv.URL
}

func TestDiagnosticBundleAdminGate(t *testing.T) {
	sid, ownerTok, adminTok, _, srvURL := adminBundleSession(t)

	// Non-admin (the owner!) → 403, before any lookup.
	resp := doJSON(t, http.MethodGet, srvURL+"/v1/admin/sessions/"+sid+"/diagnostic-bundle", ownerTok, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin bundle: got %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Admin → 200.
	resp = doJSON(t, http.MethodGet, srvURL+"/v1/admin/sessions/"+sid+"/diagnostic-bundle", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin bundle: got %d want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Admin, unknown session → 404.
	resp = doJSON(t, http.MethodGet,
		srvURL+"/v1/admin/sessions/00000000-0000-0000-0000-000000000000/diagnostic-bundle", adminTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("admin unknown bundle: got %d want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDiagnosticBundleUnmeasuredClock(t *testing.T) {
	sid, _, adminTok, _, srvURL := adminBundleSession(t)

	// No clock row was ever written → the bundle must report {"unmeasured": true}.
	resp := doJSON(t, http.MethodGet, srvURL+"/v1/admin/sessions/"+sid+"/diagnostic-bundle", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin bundle: got %d want 200", resp.StatusCode)
	}
	var out struct {
		Clock map[string]any `json:"clock"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if u, _ := out.Clock["unmeasured"].(bool); !u {
		t.Fatalf("unmeasured clock not surfaced: clock=%v", out.Clock)
	}
	if _, ok := out.Clock["client_offset_ms"]; ok {
		t.Fatalf("unmeasured clock must not carry an offset-0 default: %v", out.Clock)
	}
}

func TestDiagnosticBundleMeasuredClock(t *testing.T) {
	sid, _, adminTok, store, srvURL := adminBundleSession(t)
	ctx := context.Background()
	if err := store.Telemetry().UpsertClock(ctx, sid, -3.2, 1.8); err != nil {
		t.Fatalf("upsert clock: %v", err)
	}
	resp := doJSON(t, http.MethodGet, srvURL+"/v1/admin/sessions/"+sid+"/diagnostic-bundle", adminTok, nil)
	var out struct {
		Clock struct {
			ClientOffsetMs *float64 `json:"client_offset_ms"`
			UncertaintyMs  *float64 `json:"uncertainty_ms"`
			Unmeasured     *bool    `json:"unmeasured"`
		} `json:"clock"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Clock.Unmeasured != nil {
		t.Fatalf("measured clock wrongly reported unmeasured")
	}
	if out.Clock.ClientOffsetMs == nil || *out.Clock.ClientOffsetMs != -3.2 {
		t.Fatalf("client_offset_ms = %v want -3.2", out.Clock.ClientOffsetMs)
	}
}

// TestDiagnosticBundleCongestionVerdict drives the full DB path: insert a congestion
// trace (browser loss/rtt samples climbing) + an abr.retarget event, then assert the
// assembled bundle's verdict is likely_network_congestion.
func TestDiagnosticBundleCongestionVerdict(t *testing.T) {
	sid, _, adminTok, store, srvURL := adminBundleSession(t)
	ctx := context.Background()

	// Recent window: stamp samples in the last minute so the default 5-min window catches
	// them (the read window ends "now").
	base := time.Now().Add(-30 * time.Second).UnixMilli()
	lossSeq := []float64{0, 6, 16, 34, 60}
	rttSeq := []float64{45, 75, 95, 110, 130}
	for i := range lossSeq {
		ts := base + int64(i)*1000
		m := fmt.Sprintf(`{"packets_lost":%v,"rtt_ms":%v}`, lossSeq[i], rttSeq[i])
		if err := store.Telemetry().Append(ctx, sid, telemetry.SourceBrowser, telemetry.SampleInput{TsUnixMs: ts, Metrics: json.RawMessage(m)}); err != nil {
			t.Fatalf("insert browser metric: %v", err)
		}
		// Healthy host encode so the encoder is not the culprit.
		am := `{"encode_ms":4,"fps":60}`
		if err := store.Telemetry().Append(ctx, sid, telemetry.SourceAgent, telemetry.SampleInput{TsUnixMs: ts, Metrics: json.RawMessage(am)}); err != nil {
			t.Fatalf("insert agent metric: %v", err)
		}
	}
	if err := store.Telemetry().AppendEvent(ctx, sid, telemetry.SourceAgent, telemetry.EventInput{
		TsUnixMs: base + 2000, Type: "abr.retarget", Payload: json.RawMessage(`{"from_kbps":14000,"to_kbps":9000,"reason":"gcc_downshift"}`)}); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	resp := doJSON(t, http.MethodGet, srvURL+"/v1/admin/sessions/"+sid+"/diagnostic-bundle", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bundle: got %d want 200", resp.StatusCode)
	}
	var out struct {
		Series         map[string][]seriesPoint `json:"series"`
		Classifier     classifierVerdict        `json:"classifier"`
		DerivedWindows struct {
			LikelyNetworkCongestion []json.RawMessage `json:"likely_network_congestion"`
			ABRDownshifts           []json.RawMessage `json:"abr_downshifts"`
		} `json:"derived_windows"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	if out.Classifier.Verdict != verdictNetworkCongestion {
		t.Fatalf("verdict = %q want %q (evidence: %v)", out.Classifier.Verdict, verdictNetworkCongestion, out.Classifier.Evidence)
	}
	if len(out.DerivedWindows.LikelyNetworkCongestion) == 0 {
		t.Fatalf("no congestion derived window in bundle")
	}
	if len(out.DerivedWindows.ABRDownshifts) == 0 {
		t.Fatalf("abr downshift event not reflected in derived windows")
	}
	// Taxonomy normalization happened: the raw rtt_ms became transport.rtt_ms.
	if out.Series["transport.rtt_ms"] == nil {
		t.Fatalf("transport.rtt_ms series missing from bundle")
	}
}

// TestDiagnosticBundleEncoderVerdict drives the DB path for an encoder-ceiling trace.
func TestDiagnosticBundleEncoderVerdict(t *testing.T) {
	sid, _, adminTok, store, srvURL := adminBundleSession(t)
	ctx := context.Background()
	base := time.Now().Add(-30 * time.Second).UnixMilli()
	encSeq := []float64{18, 19, 20, 21, 22}
	fpsSeq := []float64{44, 43, 41, 40, 42}
	for i := range encSeq {
		ts := base + int64(i)*1000
		am := fmt.Sprintf(`{"encode_ms":%v,"fps":%v}`, encSeq[i], fpsSeq[i])
		if err := store.Telemetry().Append(ctx, sid, telemetry.SourceAgent, telemetry.SampleInput{TsUnixMs: ts, Metrics: json.RawMessage(am)}); err != nil {
			t.Fatalf("insert agent metric: %v", err)
		}
		// No congestion on the wire.
		bm := `{"packets_lost":0,"rtt_ms":12}`
		if err := store.Telemetry().Append(ctx, sid, telemetry.SourceBrowser, telemetry.SampleInput{TsUnixMs: ts, Metrics: json.RawMessage(bm)}); err != nil {
			t.Fatalf("insert browser metric: %v", err)
		}
	}
	resp := doJSON(t, http.MethodGet, srvURL+"/v1/admin/sessions/"+sid+"/diagnostic-bundle", adminTok, nil)
	var out struct {
		Classifier classifierVerdict `json:"classifier"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Classifier.Verdict != verdictEncoderSaturation {
		t.Fatalf("verdict = %q want %q (evidence: %v)", out.Classifier.Verdict, verdictEncoderSaturation, out.Classifier.Evidence)
	}
}

// TestAdminTraceReadsGate confirms the supporting trace reads are admin-gated too.
func TestAdminTraceReadsGate(t *testing.T) {
	sid, ownerTok, adminTok, _, srvURL := adminBundleSession(t)
	paths := []string{
		"/v1/admin/sessions/" + sid + "/trace",
		"/v1/admin/sessions/" + sid + "/trace/window?from=1&to=999999999999999",
		"/v1/admin/sessions/" + sid + "/trace/metrics?names=transport.rtt_ms",
		"/v1/admin/sessions/" + sid + "/trace/events?types=abr.retarget",
	}
	for _, p := range paths {
		resp := doJSON(t, http.MethodGet, srvURL+p, ownerTok, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s non-admin: got %d want 403", p, resp.StatusCode)
		}
		resp.Body.Close()
		resp = doJSON(t, http.MethodGet, srvURL+p, adminTok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s admin: got %d want 200", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestAdminTraceAnnotation confirms the operator annotation POST is admin-gated and
// records a session_trace_events row of the reserved operator.annotation type.
func TestAdminTraceAnnotation(t *testing.T) {
	sid, ownerTok, adminTok, store, srvURL := adminBundleSession(t)
	ctx := context.Background()
	body := map[string]any{"ts_unix_ms": time.Now().UnixMilli(), "label": "flipped abr floor", "tags": []string{"ab-test"}}

	// Non-admin → 403.
	resp := doJSON(t, http.MethodPost, srvURL+"/v1/admin/sessions/"+sid+"/trace/annotations", ownerTok, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin annotation: got %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Admin → 201 with an id.
	resp = doJSON(t, http.MethodPost, srvURL+"/v1/admin/sessions/"+sid+"/trace/annotations", adminTok, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin annotation: got %d want 201", resp.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == "" {
		t.Fatalf("annotation returned no id")
	}

	// The annotation is stored as the reserved type.
	events, err := store.Telemetry().Events(ctx, sid, telemetry.Range{FromMs: 0, ToMs: 0}, telemetry.Filter{Types: []string{operatorAnnotationType}, Limit: 10})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("annotation rows: got %d want 1", len(events))
	}

	// Missing label → 400.
	resp = doJSON(t, http.MethodPost, srvURL+"/v1/admin/sessions/"+sid+"/trace/annotations", adminTok,
		map[string]any{"ts_unix_ms": 123})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("annotation missing label: got %d want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestAdminTraceProjectionShapes is the golden guard on the four admin reads
// after they were collapsed into one handler with three projections
// (handleAdminTelemetryRead). It pins what each route puts on the wire: the
// exact top-level key set, and the fact that /trace and /trace/window are
// byte-identical bodies for the same window.
//
// If a future change to the shared handler leaks a key into the wrong
// projection, this fails before a client does.
func TestAdminTraceProjectionShapes(t *testing.T) {
	sid, _, adminTok, store, srvURL := adminBundleSession(t)
	ctx := context.Background()

	// Enough telemetry that every projection has something to render.
	base := time.Now().UnixMilli() - 60_000
	for i := 0; i < 3; i++ {
		ts := base + int64(i*1000)
		if err := store.Telemetry().Append(ctx, sid, telemetry.SourceAgent,
			telemetry.SampleInput{TsUnixMs: ts, Metrics: json.RawMessage(`{"fps":60,"encode_ms":4}`)}); err != nil {
			t.Fatalf("append sample: %v", err)
		}
	}
	if err := store.Telemetry().AppendEvent(ctx, sid, telemetry.SourceAgent, telemetry.EventInput{
		TsUnixMs: base + 1500, Type: "abr.retarget",
		Payload: json.RawMessage(`{"from_kbps":14000,"to_kbps":9000,"reason":"gcc_downshift"}`),
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := store.Telemetry().UpsertClock(ctx, sid, -12.5, 3); err != nil {
		t.Fatalf("upsert clock: %v", err)
	}

	// A pinned window, so /trace and /trace/window resolve identically and the
	// bodies are comparable byte-for-byte.
	win := "?from=" + strconv.FormatInt(base-1000, 10) + "&to=" + strconv.FormatInt(base+300_000, 10)

	read := func(path string) (string, map[string]json.RawMessage) {
		t.Helper()
		resp := doJSON(t, http.MethodGet, srvURL+path, adminTok, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: got %d want 200", path, resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s: read body: %v", path, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		return string(body), m
	}

	keysOf := func(m map[string]json.RawMessage) string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}

	traceBody, trace := read("/v1/admin/sessions/" + sid + "/trace" + win)
	windowBody, _ := read("/v1/admin/sessions/" + sid + "/trace/window" + win)
	_, metrics := read("/v1/admin/sessions/" + sid + "/trace/metrics" + win)
	_, events := read("/v1/admin/sessions/" + sid + "/trace/events" + win)

	if traceBody != windowBody {
		t.Fatalf("/trace and /trace/window must be the same body:\n  trace:  %s\n  window: %s", traceBody, windowBody)
	}
	if got, want := keysOf(trace), "clock,events,series,session_id,window"; got != want {
		t.Fatalf("/trace keys = %q, want %q", got, want)
	}
	if got, want := keysOf(metrics), "series,window"; got != want {
		t.Fatalf("/trace/metrics keys = %q, want %q", got, want)
	}
	if got, want := keysOf(events), "events,window"; got != want {
		t.Fatalf("/trace/events keys = %q, want %q", got, want)
	}

	// Content, not just shape: the series is populated, the event came back, and
	// the clock is the measured one rather than the unmeasured sentinel.
	if strings.Contains(string(trace["clock"]), "unmeasured") {
		t.Fatalf("/trace clock should be measured: %s", trace["clock"])
	}
	if !strings.Contains(string(trace["events"]), "abr.retarget") {
		t.Fatalf("/trace events missing the event: %s", trace["events"])
	}
	if !strings.Contains(string(events["events"]), "abr.retarget") {
		t.Fatalf("/trace/events missing the event: %s", events["events"])
	}
	if len(metrics["series"]) < 3 {
		t.Fatalf("/trace/metrics series looks empty: %s", metrics["series"])
	}

	// The ?names= narrowing still belongs to the metrics projection alone.
	_, narrowed := read("/v1/admin/sessions/" + sid + "/trace/metrics" + win + "&names=encoder.encode_ms")
	if string(narrowed["series"]) == string(metrics["series"]) {
		t.Fatalf("?names= did not narrow the series: %s", narrowed["series"])
	}
	// And ?types= to the events projection alone.
	_, noMatch := read("/v1/admin/sessions/" + sid + "/trace/events" + win + "&types=playout.changed")
	if strings.Contains(string(noMatch["events"]), "abr.retarget") {
		t.Fatalf("?types= did not filter: %s", noMatch["events"])
	}
}
