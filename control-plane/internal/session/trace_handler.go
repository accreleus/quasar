package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// ST-04 — browser trace-event ingest (control-api.md §B.2), POST
// /v1/sessions/{id}/trace/events. Owner-or-admin, same as POST
// /v1/sessions/{id}/stats. Untrusted input: bounded (<=64 events, <=32KB);
// events outside the §3.3 allow-list, or malformed, are dropped, never a 500.
// Accepting trace events never affects session state.

// browserEventTypes is the §3.3 v1 allow-list; unknown types are dropped at
// ingest, the event analogue of browserMetricKeys.
var browserEventTypes = map[string]bool{
	"playout.changed":           true,
	"client.freeze_detected":    true,
	"client.visibility_changed": true,
	"webrtc.state_changed":      true,
	// Bench mode (`?bench=1`) per-second marker-decode window. Additive only;
	// payload stays free-form and never becomes a persisted stats key (schema.md).
	"bench.window": true,
}

// Mirrors the stats POST limit (control-api.md §B.2).
const (
	maxTraceEventsBodyBytes = 32 * 1024 // ≤ 32 KB body
	maxTraceEventsCount     = 64        // ≤ 64 events per request
)

// handlePostTraceEvents ingests a batch of browser trace events (ST-04).
func (h *Handler) handlePostTraceEvents(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id := r.PathValue("id")

	// Ownership precedes everything, and the 404 must precede the 403: otherwise
	// a non-owner can probe which session ids exist.
	sess, err := h.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "session not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not get session")
		return
	}
	if !canAccess(user, sess) {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "not your session")
		return
	}

	if h.statsLimiter != nil && !h.statsLimiter.allow(id+"/trace") {
		httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeRateLimited,
			"too many trace event posts; slow down")
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxTraceEventsBodyBytes)
	var req struct {
		Client string `json:"client"` // "browser" (default) | "native" — P9-01 parity
		Events []struct {
			TsUnixMs int64           `json:"ts_unix_ms"`
			Type     string          `json:"type"`
			Payload  json.RawMessage `json:"payload"`
		} `json:"events"`
	}
	dec := json.NewDecoder(body)
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"malformed or oversize trace events body")
		return
	}
	if len(req.Events) > maxTraceEventsCount {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"too many events in one request")
		return
	}

	batch := make([]telemetry.EventInput, 0, len(req.Events))
	for _, e := range req.Events {
		if !browserEventTypes[e.Type] {
			continue
		}
		if !h.acceptTs(id, "trace_event:"+e.Type, e.TsUnixMs) {
			continue
		}
		payload := e.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		batch = append(batch, telemetry.EventInput{
			TsUnixMs: e.TsUnixMs,
			Type:     e.Type,
			Payload:  payload,
		})
	}

	// Best-effort: a DB error is logged, POST still returns 202 (trace data is
	// observability, never authority).
	if len(batch) > 0 {
		if err := h.store.Telemetry().AppendEvents(r.Context(), id, telemetry.SourceBrowser, batch); err != nil {
			slog.Warn("insert browser trace events batch failed", "session_id", id, "err", err)
		}
		if h.coord != nil {
			for _, e := range batch {
				if e.Type == "client.visibility_changed" {
					if isHidden, present := telemetry.HiddenFlag(e.Payload); present {
						go func() {
							cs := ClientHealthSample{
								Class:    "smooth",
								IsHidden: isHidden,
							}
							h.coord.EvaluateClientHealth(context.Background(), id, cs)
						}()
					}
					break
				}
			}
		}
	}

	httpx.WriteJSON(w, http.StatusAccepted, nil)
}

// ST-05 — clock-alignment ingest (control-api.md §B; contract-amendment.md §A.2),
// POST /v1/sessions/{id}/trace/clock. Owner-or-admin, same as
// /trace/events. Persisted via UpsertClock (one row per session; a re-measure
// refines it in place). Best-effort, never a 500. Absence of a clock row is the
// "unmeasured" state (trace-format.md §4): GetClock returns nil, never a
// synthesized offset-0 default.

// maxTraceClockBodyBytes: the payload is two floats; this is a defensive cap on
// untrusted input, not a sizing constraint.
const maxTraceClockBodyBytes = 4 * 1024 // ≤ 4 KB body

// handlePostTraceClock ingests one client-host clock-offset estimate (ST-05).
func (h *Handler) handlePostTraceClock(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id := r.PathValue("id")

	// 404 before 403, as in handlePostTraceEvents.
	sess, err := h.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "session not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not get session")
		return
	}
	if !canAccess(user, sess) {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "not your session")
		return
	}

	if h.statsLimiter != nil && !h.statsLimiter.allow(id+"/trace/clock") {
		httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeRateLimited,
			"too many clock posts; slow down")
		return
	}

	// Pointers: both fields are required, and 0.0 is a legitimate measured offset,
	// not an absent one.
	body := http.MaxBytesReader(w, r.Body, maxTraceClockBodyBytes)
	var req struct {
		ClientOffsetMs *float64 `json:"client_offset_ms"`
		UncertaintyMs  *float64 `json:"uncertainty_ms"`
	}
	dec := json.NewDecoder(body)
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"malformed or oversize trace clock body")
		return
	}
	if req.ClientOffsetMs == nil || req.UncertaintyMs == nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"client_offset_ms and uncertainty_ms are required")
		return
	}
	// No false precision (trace-format.md §4): NaN/Inf is not a real measurement;
	// uncertainty_ms must be non-negative.
	if math.IsNaN(*req.ClientOffsetMs) || math.IsInf(*req.ClientOffsetMs, 0) ||
		math.IsNaN(*req.UncertaintyMs) || math.IsInf(*req.UncertaintyMs, 0) ||
		*req.UncertaintyMs < 0 {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"client_offset_ms and uncertainty_ms must be finite (uncertainty non-negative)")
		return
	}

	if err := h.store.Telemetry().UpsertClock(r.Context(), id, *req.ClientOffsetMs, *req.UncertaintyMs); err != nil {
		slog.Warn("upsert trace clock failed", "session_id", id, "err", err)
	}

	httpx.WriteJSON(w, http.StatusAccepted, nil)
}
