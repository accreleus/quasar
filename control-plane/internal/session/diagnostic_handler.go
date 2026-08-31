package session

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// ST-06 — diagnostic-bundle endpoint + admin trace reads (contract-amendment.md §B).
// Admin-gated (RequireAuth → RequireAdmin); the 403 happens before any lookup, so a
// non-admin never gets an existence leak. Every read is a bounded window (default
// 5 min, clamped to [2, 10] min per §B.3). Handlers resolve the window, pull the raw
// store join, then hand off to the pure assembly/classifier code in classifier.go.

const (
	defaultTraceWindow = 5 * time.Minute
	minTraceWindow     = 2 * time.Minute
	maxTraceWindow     = 10 * time.Minute

	// traceReadLimit bounds each series read (store clamps to 1000).
	traceReadLimit = 1000

	// operatorAnnotationType: reserved event type for a §B.4 operator annotation.
	operatorAnnotationType = "operator.annotation"
)

// --- bundle wire DTOs (contract-amendment.md §B.5) ---------------------------

type traceWindowResp struct {
	FromMs int64 `json:"from_ms"`
	ToMs   int64 `json:"to_ms"`
}

// The clock response is EITHER this measured shape or {"unmeasured": true} — never
// an offset-0 default (trace-format.md §4); clockJSON below picks one.
type traceClockMeasured struct {
	ClientOffsetMs float64   `json:"client_offset_ms"`
	UncertaintyMs  float64   `json:"uncertainty_ms"`
	MeasuredAt     time.Time `json:"measured_at"`
}

// traceEventResp is one event in a read/bundle (the §B.5 event shape). Payload is the
// verbatim stored JSONB.
type traceEventResp struct {
	Source   string          `json:"source"`
	TsUnixMs int64           `json:"ts_unix_ms"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
}

// traceMetaResp is the bundle's trace-metadata block (§B.5).
type traceMetaResp struct {
	SessionID string     `json:"session_id"`
	HostID    *string    `json:"host_id"`
	ProfileID *string    `json:"profile_id"`
	StartedAt *time.Time `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
}

// diagnosticBundleResp is the full §B.5 bundle shape, in order.
type diagnosticBundleResp struct {
	Trace  traceMetaResp   `json:"trace"`
	Window traceWindowResp `json:"window"`
	Clock  any             `json:"clock"`
	// AbrMode: "off" | "protective" | "smooth", QUASAR_ABR_MODE as reported by the
	// agent (SPT-02+); self-labels comparison reports.
	AbrMode        string                   `json:"abr_mode"`
	Series         map[string][]seriesPoint `json:"series"`
	Events         []traceEventResp         `json:"events"`
	DerivedWindows derivedWindows           `json:"derived_windows"`
	// Live per-window host-side adaptation labels (signal-only), distinct from
	// Classifier, which alone owns likely_client_presentation_limit. Empty for
	// pre-SPT-03 agents.
	AgentAdaptation []adaptationLabel `json:"agent_adaptation"`
	Classifier      Verdict           `json:"classifier"`
	// Present only when this control plane dropped something for this session;
	// counters are in-memory (ingest_counters.go), so a restart also clears it.
	Ingest *ingestReport `json:"ingest,omitempty"`
	// Every capture for this session regardless of the bundle's window: captures
	// are sparse, explicit, and exempt from the rolling prune. Always an array,
	// never null.
	Captures []map[string]any `json:"captures"`
}

// --- helpers -----------------------------------------------------------------

// resolveWindow computes [fromMs, toMs]. Explicit from/to are honored and clamped
// to the 10-min max; otherwise a default 5-min window ending now. An inverted pair
// or a sub-2-min span is widened to the floor; over 10 min is trimmed to the cap,
// anchored on `to`.
func resolveWindow(r *http.Request) (fromMs, toMs int64) {
	q := r.URL.Query()
	fromStr := q.Get("from")
	toStr := q.Get("to")

	nowMs := time.Now().UnixMilli()
	if fromStr == "" && toStr == "" {
		return nowMs - defaultTraceWindow.Milliseconds(), nowMs
	}

	from, ferr := strconv.ParseInt(fromStr, 10, 64)
	to, terr := strconv.ParseInt(toStr, 10, 64)
	if terr != nil || to <= 0 {
		to = nowMs
	}
	if ferr != nil || from <= 0 {
		from = to - defaultTraceWindow.Milliseconds()
	}
	if from >= to {
		from = to - defaultTraceWindow.Milliseconds()
	}
	span := to - from
	if span < minTraceWindow.Milliseconds() {
		from = to - minTraceWindow.Milliseconds()
	}
	if span > maxTraceWindow.Milliseconds() {
		from = to - maxTraceWindow.Milliseconds()
	}
	return from, to
}

// reportedAbrMode returns "abr_mode" from the first agent sample carrying it
// (SPT-02+ agents always include it). "" means a pre-SPT-02 agent; the caller
// falls back to the setpoint-presence heuristic.
func reportedAbrMode(samples []telemetry.Sample) string {
	for _, s := range samples {
		if s.Source != telemetry.SourceAgent {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(s.Metrics, &m); err != nil {
			continue
		}
		raw, ok := m["abr_mode"]
		if !ok {
			continue
		}
		var mode string
		if err := json.Unmarshal(raw, &mode); err != nil || mode == "" {
			continue
		}
		return mode
	}
	return ""
}

// agentAdaptationLabels extracts SPT-03 host-side adaptation labels in
// chronological order (the store returns newest-first, so we reverse). Empty for
// pre-SPT-03 agents. Live, signal-only; distinct from the fused Classifier
// verdict, which alone owns likely_client_presentation_limit (invariant #1).
func agentAdaptationLabels(samples []telemetry.Sample) []adaptationLabel {
	var out []adaptationLabel
	for _, s := range samples {
		if s.Source != telemetry.SourceAgent {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(s.Metrics, &m); err != nil {
			continue
		}
		raw, ok := m["adaptation_state"]
		if !ok {
			continue
		}
		var state string
		if err := json.Unmarshal(raw, &state); err != nil || state == "" {
			continue
		}
		out = append(out, adaptationLabel{TsUnixMs: s.TsUnixMs, State: state})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// adaptationLabel is one agent-reported SPT-03 adaptation-classifier sample.
type adaptationLabel struct {
	TsUnixMs int64  `json:"ts_unix_ms"`
	State    string `json:"state"`
}

// clockJSON: the measured shape, or {"unmeasured": true} when c is nil.
func clockJSON(c *telemetry.Clock) any {
	if c == nil {
		return map[string]any{"unmeasured": true}
	}
	return traceClockMeasured{
		ClientOffsetMs: c.ClientOffsetMs,
		UncertaintyMs:  c.UncertaintyMs,
		MeasuredAt:     c.MeasuredAt,
	}
}

func toEventResps(in []telemetry.Event) []traceEventResp {
	out := make([]traceEventResp, 0, len(in))
	for _, e := range in {
		payload := e.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		out = append(out, traceEventResp{
			Source: e.Source, TsUnixMs: e.TsUnixMs, Type: e.Type, Payload: payload,
		})
	}
	return out
}

// adminSessionOr404: 404 for an unknown id (no existence leak, admin gate already
// ran), 500 on a real DB error.
func (h *Handler) adminSessionOr404(w http.ResponseWriter, r *http.Request, id string) (Session, bool) {
	sess, err := h.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "session not found")
		return Session{}, false
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not get session")
		return Session{}, false
	}
	return sess, true
}

// --- GET /v1/admin/sessions/{id}/diagnostic-bundle (the money endpoint) -------

// handleDiagnosticBundle assembles trace metadata + clock + series + events +
// derived windows + the classifier verdict over a bounded window
// (contract-amendment.md §B.5). Admin-only; 404 unknown session.
func (h *Handler) handleDiagnosticBundle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := h.adminSessionOr404(w, r, id)
	if !ok {
		return
	}

	fromMs, toMs := resolveWindow(r)
	raw, err := h.store.Telemetry().Window(r.Context(), id,
		telemetry.Range{FromMs: fromMs, ToMs: toMs}, telemetry.Filter{Limit: traceReadLimit})
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not assemble trace bundle")
		return
	}

	// One assessment feeds both the served series and the classifier, so the
	// bundle can never show a timeline the verdict was not computed over.
	a := assess(raw, sess.StartedAt, fromMs, toMs)
	series, events, dw, verdict := a.Series, a.Events, a.Derived, a.Verdict

	// Observability only, no action taken; ST-08 checks this evidence.
	slog.Info("diagnostic bundle classified",
		"session_id", id, "verdict", verdict.Verdict,
		"evidence_tier", verdict.EvidenceTier, "clock_quality", verdict.Clock.Quality,
		"from_ms", fromMs, "to_ms", toMs,
		"hitches", len(dw.Hitches), "abr_downshifts", len(dw.ABRDownshifts),
		"encoder_saturation", len(dw.EncoderSaturation),
		"congestion_windows", len(dw.LikelyNetworkCongestion),
		"clock_measured", raw.Clock != nil, "clock_applied", verdict.Clock.Applied,
		"warmup_excluded_ms", verdict.Window.WarmupExcludedMs,
		"evidence", verdict.Evidence)

	abrMode := reportedAbrMode(raw.Samples)
	if abrMode == "" {
		// Pre-SPT-02 agent fallback: setpoint series present → "protective".
		if len(series["abr.setpoint_kbps"]) > 0 {
			abrMode = "protective"
		} else {
			abrMode = "off"
		}
	}

	// Non-nil so the bundle JSON always carries the key as an array.
	agentAdaptation := agentAdaptationLabels(raw.Samples)
	if agentAdaptation == nil {
		agentAdaptation = []adaptationLabel{}
	}

	resp := diagnosticBundleResp{
		Trace: traceMetaResp{
			SessionID: sess.ID, HostID: sess.HostID, ProfileID: sess.ProfileID,
			StartedAt: sess.StartedAt, EndedAt: sess.EndedAt,
		},
		Window:          traceWindowResp{FromMs: fromMs, ToMs: toMs},
		Clock:           clockJSON(raw.Clock),
		AbrMode:         abrMode,
		Series:          series,
		Events:          events,
		DerivedWindows:  dw,
		AgentAdaptation: agentAdaptation,
		Classifier:      verdict,
		Ingest:          h.ingest.report(id),
		Captures:        h.bundleCaptures(r.Context(), id),
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// --- GET /v1/admin/sessions/{id}/trace (full bounded recent trace) ------------

// handleAdminTrace returns the bounded recent trace for a session: clock + the
// taxonomy series + events (contract-amendment.md §B.3). Admin-only; 404 unknown
// session. One handler, four projections — collapsed from four near-copies that
// differed only in which of {clock, series, events} they returned, so a window
// bound or clamp can't drift between routes. Wire shapes are unchanged
// byte-for-byte; TestOpenAPIDrift and the diagnostic/verdict DB tests hold that
// in place.
type traceProjection int

const (
	// GET .../trace and .../trace/window (documented alias, same body).
	projectionTrace traceProjection = iota
	// GET .../trace/metrics — series only, optionally narrowed by ?names=.
	projectionMetrics
	// GET .../trace/events — events only, narrowed by ?types=; reads no samples/clock.
	projectionEvents
)

// handleAdminTelemetryRead serves every admin trace read. It is registered
// per-route with the projection that route wants.
func (h *Handler) handleAdminTelemetryRead(p traceProjection) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := h.adminSessionOr404(w, r, id); !ok {
			return
		}
		fromMs, toMs := resolveWindow(r)
		window := traceWindowResp{FromMs: fromMs, ToMs: toMs}

		if p == projectionEvents {
			events, err := h.store.Telemetry().Events(r.Context(), id,
				telemetry.Range{FromMs: fromMs, ToMs: toMs},
				telemetry.Filter{Types: csvValues(r.URL.Query().Get("types")), Limit: traceReadLimit})
			if err != nil {
				httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read trace events")
				return
			}
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"window": window,
				"events": toEventResps(events),
			})
			return
		}

		raw, err := h.store.Telemetry().Window(r.Context(), id,
			telemetry.Range{FromMs: fromMs, ToMs: toMs}, telemetry.Filter{Limit: traceReadLimit})
		if err != nil {
			// Wire-visible message stays per-projection (routes were distinguishable by it before).
			msg := "could not read trace"
			if p == projectionMetrics {
				msg = "could not read trace metrics"
			}
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, msg)
			return
		}

		aligned := telemetry.AlignSeries(raw.Samples, raw.Events, raw.Clock)
		switch p {
		case projectionMetrics:
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"window": window,
				"series": normalizeSeries(aligned, parseCSVSet(r.URL.Query().Get("names"))),
			})
		default:
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"session_id": id,
				"window":     window,
				"clock":      clockJSON(raw.Clock),
				"series":     normalizeSeries(aligned, nil),
				"events":     toEventResps(aligned.PlainEvents()),
			})
		}
	}
}

func (h *Handler) handlePostTraceAnnotation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := h.adminSessionOr404(w, r, id); !ok {
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxTraceEventsBodyBytes)
	var req struct {
		TsUnixMs *int64   `json:"ts_unix_ms"`
		Label    string   `json:"label"`
		Tags     []string `json:"tags"`
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"malformed or oversize annotation body")
		return
	}
	if req.Label == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "label is required")
		return
	}
	ts := time.Now().UnixMilli()
	if req.TsUnixMs != nil {
		ts = *req.TsUnixMs
	}

	payload, _ := json.Marshal(map[string]any{"label": req.Label, "tags": req.Tags})

	// Confirms an admin identity only; provenance is via source/type, not a user id.
	if _, ok := auth.UserFromContext(r.Context()); !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	eventID, err := h.store.Telemetry().AppendEventReturningID(r.Context(), id, telemetry.SourceAgent,
		telemetry.EventInput{TsUnixMs: ts, Type: operatorAnnotationType, Payload: payload})
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not record annotation")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": eventID})
}

// --- small parsing helper ----------------------------------------------------

// parseCSVSet splits a comma-separated query value into a set, trimming empties.
// Returns nil for an empty string (caller treats nil as "all" / "no filter").
// csvValues is parseCSVSet as a slice — the events read wants a list to pass to
// the store, the metrics read wants a set to test membership against.
func csvValues(s string) []string {
	set := parseCSVSet(s)
	if set == nil {
		return nil
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	return out
}

func parseCSVSet(s string) map[string]bool {
	if s == "" {
		return nil
	}
	out := map[string]bool{}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			tok := trimSpace(s[start:i])
			if tok != "" {
				out[tok] = true
			}
			start = i + 1
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// trimSpace trims ASCII spaces from both ends (avoids importing strings for one call).
func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && s[i] == ' ' {
		i++
	}
	for j > i && s[j-1] == ' ' {
		j--
	}
	return s[i:j]
}
