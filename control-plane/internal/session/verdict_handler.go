package session

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// The two Verdict reads (ST-09, control-api.md §Authorization) return exactly
// the diagnostic bundle's `classifier` value, no series or events — a small
// response instead of a megabyte of time series. Neither has session
// authority; reading a verdict changes nothing.

// assessment is one read window turned into everything both the bundle and the
// verdict need, computed once so the two can never disagree: aligned points,
// full series, warm-up-trimmed series, derived windows, and the verdict.
type assessment struct {
	Aligned telemetry.AlignedSet
	Series  map[string][]seriesPoint
	Events  []traceEventResp
	Derived derivedWindows
	Verdict Verdict
}

// assess: align, normalize, exclude warm-up, derive, classify, build.
func assess(raw telemetry.Slice, runningAt *time.Time, fromMs, toMs int64) assessment {
	aligned := telemetry.AlignSeries(raw.Samples, raw.Events, raw.Clock)
	al := alignmentOf(aligned)
	series := normalizeSeries(aligned, nil)
	events := toEventResps(aligned.PlainEvents())

	warm := warmupFor(runningAt, fromMs, toMs)
	assessed := warm.assessed(series)

	dw := computeDerivedWindows(assessed, events)
	nHost, nClient := countSamplesBySource(aligned.PlainSamples())

	v := buildVerdict(verdictInputs{
		Classifier: classify(classifyInputs{Series: assessed, Derived: dw, Events: events, Align: al}),
		Series:     assessed,
		FullSeries: series,
		Derived:    dw,
		Events:     events,
		FromMs:     fromMs,
		ToMs:       toMs,
		NHost:      nHost,
		NClient:    nClient,
		Clock:      raw.Clock,
		Align:      al,
		Warmup:     warm,
	})
	return assessment{Aligned: aligned, Series: series, Events: events, Derived: dw, Verdict: v}
}

// computeVerdict does the read + assembly. Returns false having already
// written the error response.
func (h *Handler) computeVerdict(w http.ResponseWriter, r *http.Request, id string, runningAt *time.Time) (Verdict, bool) {
	fromMs, toMs := resolveWindow(r)
	raw, err := h.store.Telemetry().Window(r.Context(), id,
		telemetry.Range{FromMs: fromMs, ToMs: toMs}, telemetry.Filter{Limit: traceReadLimit})
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not assemble verdict")
		return Verdict{}, false
	}
	return assess(raw, runningAt, fromMs, toMs).Verdict, true
}

// handleAdminVerdict — GET /v1/admin/sessions/{id}/verdict (admin).
func (h *Handler) handleAdminVerdict(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := h.adminSessionOr404(w, r, id)
	if !ok {
		return
	}
	verdict, ok := h.computeVerdict(w, r, id, sess.StartedAt)
	if !ok {
		return
	}
	slog.Info("session verdict read",
		"session_id", id, "scope", "admin", "verdict", verdict.Verdict,
		"evidence_tier", verdict.EvidenceTier, "clock_quality", verdict.Clock.Quality,
		"n_host", verdict.Window.NHost, "n_client", verdict.Window.NClient)
	httpx.WriteJSON(w, http.StatusOK, verdict)
}

// handleSessionVerdict — GET /v1/sessions/{id}/verdict (owner or admin).
// Ownership checked before any work: 404 unknown id, 403 someone else's
// session. Rate-limited on the same per-session limiter as owner telemetry
// POSTs, since this read assembles a window of samples.
func (h *Handler) handleSessionVerdict(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id := r.PathValue("id")

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
	if h.statsLimiter != nil && !h.statsLimiter.allow(id+"/verdict") {
		httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeRateLimited,
			"too many verdict reads; slow down")
		return
	}

	verdict, ok := h.computeVerdict(w, r, id, sess.StartedAt)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, verdict)
}
