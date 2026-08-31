package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// sampleIsHidden reads is_hidden from a browser sample. A hidden tab must never
// be recorded as a client failure. Goes through the one reader
// (telemetry.HiddenFlag) so this and trace_handler.go agree on encoding.
// Missing/garbage → false (visible).
func sampleIsHidden(raw json.RawMessage) bool {
	hidden, _ := telemetry.HiddenFlag(raw)
	return hidden
}

// plausibleTs is the ingest gate's predicate, kept here so both client ingest
// paths (stats samples and trace events) read the same clock.
func plausibleTs(ts int64) (bool, string) {
	return telemetry.PlausibleTsUnixMs(ts, time.Now())
}

// P4-05 — the telemetry HTTP surface (control-api.md § Telemetry & devices):
//   POST /v1/sessions/{id}/stats         — browser ingestion (owner-or-admin)
//   GET  /v1/admin/sessions/{id}/metrics — admin read (bounded recent window)
// Plus the optional latest_metrics object on GET /v1/admin/sessions (handler.go).

// Browser-POST limits (control-api.md): the body is untrusted client input.
const (
	maxStatsBodyBytes = 32 * 1024 // ≤ 32 KB body
	maxStatsSamples   = 64        // ≤ 64 samples per request

	// Per-session token bucket: heartbeat cadence ~5s; burst tolerates a reconnect flush.
	statsRateBurst  = 10
	statsRateRefill = 2 * time.Second
)

// --- DTOs --------------------------------------------------------------------

// metricSampleResp is one telemetry sample in the admin read (control-api.md).
// The server-only created_at is intentionally not surfaced.
type metricSampleResp struct {
	Source   string          `json:"source"`
	TsUnixMs int64           `json:"ts_unix_ms"`
	Metrics  json.RawMessage `json:"metrics"`
}

// latestMetricsResp is the optional per-item latest_metrics on the admin session
// list (control-api.md). Kept source-scoped (schema.md cross-source note: never
// blind-overlay frames_dropped); an absent source is omitted.
type latestMetricsResp struct {
	Agent   *metricSampleResp `json:"agent,omitempty"`
	Browser *metricSampleResp `json:"browser,omitempty"`
}

// adminSessionResp augments the session body with the optional latest_metrics
// object (absent when no telemetry) and the user/app/host display names (#385
// item 7), resolved by a LEFT JOIN so each is omitted when its row is gone
// (host_name also omitted while unassigned). Never on sessionResp: the
// user-facing body must not leak other rows' names.
type adminSessionResp struct {
	sessionResp
	LatestMetrics *latestMetricsResp `json:"latest_metrics,omitempty"`
	Username      *string            `json:"username,omitempty"`
	AppName       *string            `json:"app_name,omitempty"`
	HostName      *string            `json:"host_name,omitempty"`
}

func toSampleResp(m telemetry.Sample) *metricSampleResp {
	metrics := m.Metrics
	if len(metrics) == 0 {
		metrics = json.RawMessage("{}")
	}
	return &metricSampleResp{Source: m.Source, TsUnixMs: m.TsUnixMs, Metrics: metrics}
}

func toLatestMetrics(ml telemetry.Latest) *latestMetricsResp {
	if ml.Agent == nil && ml.Browser == nil {
		return nil
	}
	out := &latestMetricsResp{}
	if ml.Agent != nil {
		out.Agent = toSampleResp(*ml.Agent)
	}
	if ml.Browser != nil {
		out.Browser = toSampleResp(*ml.Browser)
	}
	return out
}

// statSample is one sample in a POST /v1/sessions/{id}/stats body. Metrics is
// key-filtered against the schema.md dictionary, which is numeric-only; these
// string fields are siblings for that reason. Unknown fields are ignored, so an
// older client omitting any of them keeps working.
type statSample struct {
	TsUnixMs int64           `json:"ts_unix_ms"`
	Metrics  json.RawMessage `json:"metrics"`
	// Browser-classified client health (zero value = no signal on older browsers).
	// is_hidden rides inside Metrics and is filtered by telemetry.FilterBrowserMetrics.
	ClientHealth       string `json:"client_health"`
	ClientHealthReason string `json:"client_health_reason"`
	DeviceKey          string `json:"device_key"`
	// CodecMimeType is the getStats() codec mimeType the receiver reports it is
	// actually decoding, shown beside the server-resolved codec.
	CodecMimeType string `json:"codec_mime_type"`
}

// latestNegotiatedCodec scans a batch backwards for the newest usable codec.
// Not just the last sample: getStats() resolves the codec asynchronously, so a
// batch can legitimately end with empty codec_mime_type values; those are
// skipped rather than letting them mask an earlier good one.
func latestNegotiatedCodec(samples []statSample) (string, bool) {
	for i := len(samples) - 1; i >= 0; i-- {
		if samples[i].CodecMimeType == "" {
			continue
		}
		if codec, ok := normaliseNegotiatedCodec(samples[i].CodecMimeType); ok {
			return codec, true
		}
	}
	return "", false
}

// --- POST /v1/sessions/{id}/stats (browser ingestion) ------------------------

// handlePostStats ingests a batch of client-reported getStats() samples
// (control-api.md). Owner-or-admin, same as DELETE /v1/sessions/{id}: non-owner
// 403, unknown id 404, both before any write. Bounded (<=64 samples, <=32KB) and
// rate-limited (429 rate_limited). source comes from the optional "client" field
// (default browser, or native; any other value 400) and only schema.md
// dictionary keys are stored. Malformed samples are dropped, not fataled.
// Returns 202; accepting telemetry never affects session state.
func (h *Handler) handlePostStats(w http.ResponseWriter, r *http.Request) {
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

	if h.statsLimiter != nil && !h.statsLimiter.allow(id) {
		httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeRateLimited,
			"too many telemetry posts; slow down")
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxStatsBodyBytes)
	var req struct {
		// Optional reporter discriminator, "browser" (default) | "native".
		// Exact-match only: an unknown value is rejected below, never coerced.
		Client  string       `json:"client"`
		Samples []statSample `json:"samples"`
	}
	dec := json.NewDecoder(body)
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"malformed or oversize telemetry body")
		return
	}
	if len(req.Samples) > maxStatsSamples {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"too many samples in one request")
		return
	}

	// Sets session_metrics.source; absent -> SourceBrowser, any other value 400.
	source := telemetry.SourceBrowser
	if req.Client != "" {
		switch req.Client {
		case telemetry.SourceBrowser, telemetry.SourceNative:
			source = req.Client
		default:
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
				"unknown client value")
			return
		}
	}

	// One pgx.Batch instead of sequential INSERTs. Write is best-effort, never
	// fatal: a DB error is logged and the POST still returns 202.
	batch := make([]telemetry.SampleInput, 0, len(req.Samples))
	for _, s := range req.Samples {
		// A stamp not plausibly Unix-epoch ms is dropped; counted in the
		// diagnostic bundle's `ingest`.
		if !h.acceptTs(id, "stats_sample", s.TsUnixMs) {
			continue
		}
		batch = append(batch, telemetry.SampleInput{TsUnixMs: s.TsUnixMs, Metrics: telemetry.FilterBrowserMetrics(s.Metrics)})
	}
	// Append only; retention is the telemetry janitor's job (internal/telemetry).
	if len(batch) > 0 {
		if err := h.store.Telemetry().AppendBatch(r.Context(), id, source, batch); err != nil {
			slog.Warn("insert browser metrics batch failed", "session_id", id, "err", err)
		}
	}

	// Records the actually-decoded codec beside the server-resolved one. Only
	// written on change: steady state is zero writes per POST.
	if codec, ok := latestNegotiatedCodec(req.Samples); ok && codec != deref(sess.NegotiatedCodec) {
		if err := h.store.UpdateSessionNegotiatedCodec(r.Context(), id, codec); err != nil {
			slog.Warn("update session negotiated codec failed", "session_id", id, "err", err)
		}
	}

	// Latest sample's client health maps into AS10-06 client_* states and
	// profile-certification history (network states win; a hidden tab never
	// fails). Runs in its own goroutine; never affects the 202 or ABR.
	if n := len(req.Samples); n > 0 {
		last := req.Samples[n-1]
		if last.ClientHealth != "" && h.coord != nil {
			cs := ClientHealthSample{
				Class:     last.ClientHealth,
				Reason:    last.ClientHealthReason,
				DeviceKey: last.DeviceKey,
				IsHidden:  sampleIsHidden(last.Metrics),
			}
			go h.coord.EvaluateClientHealth(context.Background(), id, cs)
		}
	}

	httpx.WriteJSON(w, http.StatusAccepted, nil)
}

// --- GET /v1/admin/sessions/{id}/metrics (admin read) ------------------------

// handleAdminMetrics returns the bounded recent telemetry window for a session,
// both sources, newest first (P4-05, control-api.md). Admin-only (the 403 is
// enforced by the RequireAdmin middleware before this runs); 404 for an unknown
// session. Optional ?limit=&cursor=&source=agent|browser.
func (h *Handler) handleAdminMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if _, err := h.store.Get(r.Context(), id); errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "session not found")
		return
	} else if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not get session")
		return
	}

	cursor := r.URL.Query().Get("cursor")
	var limit int32 = 100
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	var source *string
	if s := r.URL.Query().Get("source"); s == telemetry.SourceAgent || s == telemetry.SourceBrowser || s == telemetry.SourceNative {
		source = &s
	}

	samples, next, err := h.store.Telemetry().Recent(r.Context(), id, limit, source, cursor)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read metrics")
		return
	}
	items := make([]metricSampleResp, 0, len(samples))
	for _, s := range samples {
		items = append(items, *toSampleResp(s))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nullable(next)})
}

// --- rate limiter ------------------------------------------------------------

// rateLimiter is a per-key token bucket. Keys accrue tokens up to burst, one per
// refill interval; allow() consumes one. Idle full buckets are pruned lazily.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	burst   int
	refill  time.Duration
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(burst int, refill time.Duration) *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*bucket), burst: burst, refill: refill}
}

// allow reports whether the key may proceed, consuming a token if so.
//
// Lazy prune (#403) on insert: a bucket idle long enough to have refilled to
// full is dropped, since that's indistinguishable from not existing. Without it
// the map grows one permanent entry per session ever created. The O(len(buckets))
// sweep per new key is fine here because keys are ownership-checked session ids,
// unlike internal/auth's IP-keyed limiter which needs a hard cap instead.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok {
		if rl.refill > 0 {
			idleToFull := time.Duration(rl.burst) * rl.refill
			for k, old := range rl.buckets {
				if now.Sub(old.last) >= idleToFull {
					delete(rl.buckets, k)
				}
			}
		}
		b = &bucket{tokens: float64(rl.burst), last: now}
		rl.buckets[key] = b
	}
	if rl.refill > 0 {
		b.tokens += now.Sub(b.last).Seconds() / rl.refill.Seconds()
	}
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
