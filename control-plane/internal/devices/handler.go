package devices

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// POST /v1/me/devices limits (control-api.md § Telemetry & devices):
// the body is untrusted client input; keep the caps tight.
const (
	maxDeviceBodyBytes = 8 * 1024 // 8 KB — generous for the small capability payload

	// Per-user token-bucket rate limit. A login-frequency call is ample; the
	// upsert is idempotent so this cap is effectively "once per login burst".
	deviceRateBurst  = 5
	deviceRateRefill = 10 * time.Second
)

// SessionStopper ends a live session by id (LP-SEC-01 §B.6): device revocation tears down
// that device's live sessions via the existing session teardown (no agent-wire change).
// Implemented in main by an adapter over the session coordinator's Stop; nil disables the
// live-session-end policy (tokens are still revoked).
type SessionStopper func(ctx context.Context, sessionID, reason string) error

// Handler serves the device surface: POST /v1/me/devices (P4-08 upsert) and the
// LP-SEC-01 owner-self management endpoints (GET list, PATCH rename/trust, DELETE revoke).
type Handler struct {
	store   *Store
	limiter *rateLimiter
	stopper SessionStopper
}

// NewHandler builds the devices HTTP handler. stopper (may be nil) ends a revoked device's
// live sessions; when nil, revoke still invalidates the device's tokens.
func NewHandler(store *Store, stopper SessionStopper) *Handler {
	return &Handler{
		store:   store,
		limiter: newRateLimiter(deviceRateBurst, deviceRateRefill),
		stopper: stopper,
	}
}

// Register wires the device routes onto mux. requireAuth must be the auth.RequireAuth
// middleware so the bearer identity (and its bound device id) is in the request context.
func (h *Handler) Register(mux httpx.Router, requireAuth func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/me/devices", requireAuth(http.HandlerFunc(h.handleUpsert)))
	mux.Handle("GET /v1/me/devices", requireAuth(http.HandlerFunc(h.handleList)))
	mux.Handle("PATCH /v1/me/devices/{id}", requireAuth(http.HandlerFunc(h.handlePatch)))
	mux.Handle("DELETE /v1/me/devices/{id}", requireAuth(http.HandlerFunc(h.handleRevoke)))
}

// deviceResp is the minimal 200 body (control-api.md): just id + timestamps.
type deviceResp struct {
	ID          string    `json:"id"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// handleUpsert implements POST /v1/me/devices (P4-08, control-api.md).
//
// Owner is the bearer identity (auth.UserFromContext), NEVER a body field.
// Upserts on (user_id, device_key): insert on first sight, update
// capabilities/last_seen_at/user_agent thereafter. Body is size-validated
// (≤ 8 KB) and rate-limited (429 rate_limited). measured_at inside
// capabilities is server-stamped, ignoring any client-supplied value.
// Returns 200 { device: { id, first_seen_at, last_seen_at } }.
func (h *Handler) handleUpsert(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	if !h.limiter.allow(user.ID) {
		httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeRateLimited,
			"too many device upserts; slow down")
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxDeviceBodyBytes)
	var req struct {
		DeviceKey    string          `json:"device_key"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	dec := json.NewDecoder(body)
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"malformed or oversize device body")
		return
	}

	if req.DeviceKey == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"device_key is required")
		return
	}

	// capabilities must be a JSON object if non-empty.
	if len(req.Capabilities) > 0 {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(req.Capabilities, &probe); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
				"capabilities must be a JSON object")
			return
		}
	}

	dev, err := h.store.Upsert(r.Context(), UpsertParams{
		UserID:       user.ID, // always from the bearer token — never the body
		DeviceKey:    req.DeviceKey,
		UserAgent:    r.UserAgent(),
		Capabilities: req.Capabilities,
	})
	if err != nil {
		slog.Warn("device upsert failed", "user_id", user.ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not upsert device")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"device": deviceResp{
			ID:          dev.ID,
			FirstSeenAt: dev.FirstSeenAt,
			LastSeenAt:  dev.LastSeenAt,
		},
	})
}

// handleList implements GET /v1/me/devices (LP-SEC-01 §B.6 — the D3 shape change from the
// AS10-08 single-latest to the full list). Owner-self (bearer identity): returns all of
// the caller's devices, newest-first, each flagged current (bound to the bearer token)
// and carrying its live active_session_id, if any. The capabilities blob is verbatim.
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	currentDeviceID, _ := auth.TokenDeviceIDFromContext(r.Context())

	list, err := h.store.List(r.Context(), user.ID, currentDeviceID)
	if err != nil {
		slog.Warn("device list failed", "user_id", user.ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list devices")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"devices": list})
}

// handlePatch implements PATCH /v1/me/devices/{id} (LP-SEC-01 §B.6): owner-scoped rename /
// set trust. A non-owned or unknown id ⇒ 403 (never 404, no existence leak). trusted is
// advisory-only in W1 (decision D6) — not an authorization input.
func (h *Handler) handlePatch(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	id := r.PathValue("id")
	if !isUUID(id) {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "not your device")
		return
	}
	currentDeviceID, _ := auth.TokenDeviceIDFromContext(r.Context())

	body := http.MaxBytesReader(w, r.Body, maxDeviceBodyBytes)
	var req struct {
		Name    *string `json:"name"`
		Trusted *bool   `json:"trusted"`
	}
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed device body")
		return
	}
	// Bound the display label; an unbounded name is untrusted client input.
	if req.Name != nil && len(*req.Name) > 128 {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "name must be <= 128 characters")
		return
	}

	item, err := h.store.UpdateNameTrust(r.Context(), user.ID, id, currentDeviceID, req.Name, req.Trusted)
	if errors.Is(err, ErrForbidden) {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "not your device")
		return
	}
	if err != nil {
		slog.Warn("device patch failed", "user_id", user.ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not update device")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"device": item})
}

// handleRevoke implements DELETE /v1/me/devices/{id} (LP-SEC-01 §B.6 — the load-bearing
// endpoint). Owner-scoped: a non-owned/unknown id ⇒ 403. On success it invalidates every
// token bound to the device (real revocation) and ends the device's live sessions via the
// injected stopper, then returns 204.
func (h *Handler) handleRevoke(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	id := r.PathValue("id")
	if !isUUID(id) {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "not your device")
		return
	}

	sessionIDs, err := h.store.Revoke(r.Context(), user.ID, id)
	if errors.Is(err, ErrForbidden) {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "not your device")
		return
	}
	if err != nil {
		slog.Warn("device revoke failed", "user_id", user.ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not revoke device")
		return
	}

	// Best-effort teardown: the token revocation (the security-critical part)
	// has already committed, so a stopper failure is logged, not surfaced.
	if h.stopper != nil {
		for _, sid := range sessionIDs {
			if err := h.stopper(r.Context(), sid, "device_revoked"); err != nil {
				slog.Warn("end session on device revoke failed", "session_id", sid, "err", err)
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hex UUID. A cheap format guard so a
// malformed path id becomes a clean 403 instead of a DB cast error (500) — and never
// distinguishes "malformed" from "not yours".
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// --- rate limiter (mirrors the one in session/metrics_handler.go) ---------------

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

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok {
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
