package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/ice"
	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// Handler serves the session endpoints: launch and lifecycle.
type Handler struct {
	coord *Coordinator
	store *Store
	// statsLimiter rate-limits the untrusted browser telemetry POST per session.
	// Nil-safe: a nil limiter never throttles.
	statsLimiter *rateLimiter
	// ingest counts what client ingest dropped for an implausible ts_unix_ms, per
	// session, in memory (ingest_counters.go). Served in the diagnostic bundle;
	// nil-safe.
	ingest  *ingestCounters
	auditor interface {
		Record(context.Context, string, string, string, string, map[string]any) error
	}
	// publicBaseURL is PUBLIC_BASE_URL, consulted only for proxied requests; see
	// signalingURL.
	publicBaseURL string
	// iceServers is QUASAR_ICE_SERVERS (#509), handed to the client on every set
	// of signaling coordinates. Nil is the default and the LAN case; it still
	// serializes as [] — see newSignalingResp.
	iceServers []ice.Server
}

// capacityExhaustedRetryAfterSeconds is the Retry-After on a 503
// capacity_exhausted refusal (#494), seconds as a decimal string (RFC 9110
// §10.2.3). The encode-slot reservation holds through `stopping` (#489) and was
// measured clearing ~15 s after the peer's DELETE, so 5 s lets a client poll a
// few times inside that window.
const capacityExhaustedRetryAfterSeconds = "5"

// WithPublicBaseURL hands a proxied client a signaling address it can reach.
// Optional: unset keeps the header-derived behaviour.
func (h *Handler) WithPublicBaseURL(base string) *Handler {
	h.publicBaseURL = base
	return h
}

// WithICEServers declares the deployment's STUN/TURN servers
// (QUASAR_ICE_SERVERS). Optional: unset means the client gathers host candidates
// only, which is correct for a LAN deployment.
func (h *Handler) WithICEServers(servers []ice.Server) *Handler {
	h.iceServers = servers
	return h
}

// NewHandler builds the session HTTP handler.
func NewHandler(coord *Coordinator, store *Store, auditors ...interface {
	Record(context.Context, string, string, string, string, map[string]any) error
}) *Handler {
	h := &Handler{
		coord:        coord,
		store:        store,
		statsLimiter: newRateLimiter(statsRateBurst, statsRateRefill),
		ingest:       newIngestCounters(),
	}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

func (h *Handler) recordActivity(ctx context.Context, actor, action, targetType, targetID string, details map[string]any) {
	audit.TryRecord(ctx, h.auditor, actor, action, targetType, targetID, details)
}

// Register wires the session routes onto mux. All require authentication.
func (h *Handler) Register(mux httpx.Router, requireAuth func(http.Handler) http.Handler, requireAdmin ...func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/sessions", requireAuth(http.HandlerFunc(h.handleLaunch)))
	mux.Handle("GET /v1/sessions", requireAuth(http.HandlerFunc(h.handleList)))
	mux.Handle("GET /v1/sessions/{id}", requireAuth(http.HandlerFunc(h.handleGet)))
	mux.Handle("POST /v1/sessions/{id}/signaling-token", requireAuth(http.HandlerFunc(h.handleSignalingToken)))
	mux.Handle("DELETE /v1/sessions/{id}", requireAuth(http.HandlerFunc(h.handleStop)))
	mux.Handle("POST /v1/sessions/{id}/swap", requireAuth(http.HandlerFunc(h.handleSwap)))
	// Live render resolution / UI scale: a relay to the host agent. No state
	// transition, nothing persisted.
	mux.Handle("PATCH /v1/sessions/{id}/display", requireAuth(http.HandlerFunc(h.handleDisplayUpdate)))
	// Browser telemetry: owner-or-admin posts its session's getStats() samples.
	mux.Handle("POST /v1/sessions/{id}/stats", requireAuth(http.HandlerFunc(h.handlePostStats)))
	// Browser trace events (control-api.md §B.2). Unknown types are dropped.
	mux.Handle("POST /v1/sessions/{id}/trace/events", requireAuth(http.HandlerFunc(h.handlePostTraceEvents)))
	// Browser clock alignment (contract-amendment.md §A.2/§B): the measured
	// client-host offset and uncertainty, one row per session. No post means
	// unmeasured.
	mux.Handle("POST /v1/sessions/{id}/trace/clock", requireAuth(http.HandlerFunc(h.handlePostTraceClock)))
	// The owner-or-admin verdict read: the same body as the admin form, under
	// DELETE's ownership check and the owner posts' per-session rate limit.
	mux.Handle("GET /v1/sessions/{id}/verdict", requireAuth(http.HandlerFunc(h.handleSessionVerdict)))

	// Profile eligibility and recommendation for the caller's device, with reason
	// codes.
	mux.Handle("GET /v1/me/profiles", requireAuth(http.HandlerFunc(h.handleListProfiles)))
	mux.Handle("GET /v1/me/profile-preferences", requireAuth(http.HandlerFunc(h.handleGetProfilePreferences)))
	mux.Handle("PATCH /v1/me/profile-preferences", requireAuth(http.HandlerFunc(h.handlePatchProfilePreferences)))

	// Admin: GPU availability and the all-sessions view.
	if len(requireAdmin) > 0 && requireAdmin[0] != nil {
		admin := func(next http.Handler) http.Handler { return requireAuth(requireAdmin[0](next)) }
		mux.Handle("GET /v1/admin/sessions", admin(http.HandlerFunc(h.handleAdminList)))
		mux.Handle("GET /v1/hosts/{id}/gpus", admin(http.HandlerFunc(h.handleHostGPUs)))
		// Host lifecycle: cordon a host out of service, or return it.
		mux.Handle("POST /v1/hosts/{id}/drain", admin(http.HandlerFunc(h.handleDrainHost)))
		mux.Handle("POST /v1/hosts/{id}/uncordon", admin(http.HandlerFunc(h.handleUncordonHost)))
		// Per-session metrics read.
		mux.Handle("GET /v1/admin/sessions/{id}/metrics", admin(http.HandlerFunc(h.handleAdminMetrics)))
		// Session-trace reads (contract-amendment.md §B), bounded windows. The
		// diagnostic bundle carries metadata, clock, aligned series, events,
		// derived windows and the classifier verdict.
		mux.Handle("GET /v1/admin/sessions/{id}/diagnostic-bundle", admin(http.HandlerFunc(h.handleDiagnosticBundle)))
		// The verdict alone: what the bundle carries as `classifier`, without the
		// series. Observability only, no session authority.
		mux.Handle("GET /v1/admin/sessions/{id}/verdict", admin(http.HandlerFunc(h.handleAdminVerdict)))
		// One handler, four routes, three projections — see handleAdminTelemetryRead.
		// /trace and /trace/window are the same body by design.
		mux.Handle("GET /v1/admin/sessions/{id}/trace", admin(h.handleAdminTelemetryRead(projectionTrace)))
		mux.Handle("GET /v1/admin/sessions/{id}/trace/window", admin(h.handleAdminTelemetryRead(projectionTrace)))
		mux.Handle("GET /v1/admin/sessions/{id}/trace/metrics", admin(h.handleAdminTelemetryRead(projectionMetrics)))
		mux.Handle("GET /v1/admin/sessions/{id}/trace/events", admin(h.handleAdminTelemetryRead(projectionEvents)))
		mux.Handle("POST /v1/admin/sessions/{id}/trace/annotations", admin(http.HandlerFunc(h.handlePostTraceAnnotation)))
		// Arm ONE bounded observation of a live session, then read it by id.
		// Observability only, no session authority; arm, then poll until the
		// diag.* event lands.
		mux.Handle("POST /v1/admin/sessions/{id}/capture", admin(http.HandlerFunc(h.handleArmCapture)))
		mux.Handle("GET /v1/admin/sessions/{id}/captures/{capture_id}", admin(http.HandlerFunc(h.handleReadCapture)))
		mux.Handle("GET /v1/admin/profile-policy", admin(http.HandlerFunc(h.handleGetProfilePolicy)))
		mux.Handle("PATCH /v1/admin/profile-policy", admin(http.HandlerFunc(h.handlePatchProfilePolicy)))
		// Stream profiles are the encode RUNGS. DELETE is 409 while any launch
		// profile lists the rung, server-enforced and never UI-gated.
		mux.Handle("GET /v1/admin/stream-profiles", admin(http.HandlerFunc(h.handleAdminListStreamProfiles)))
		mux.Handle("POST /v1/admin/stream-profiles", admin(http.HandlerFunc(h.handleAdminCreateStreamProfile)))
		mux.Handle("PATCH /v1/admin/stream-profiles/{id}", admin(http.HandlerFunc(h.handleAdminPatchStreamProfile)))
		mux.Handle("DELETE /v1/admin/stream-profiles/{id}", admin(http.HandlerFunc(h.handleAdminDeleteStreamProfile)))
		// Launch profiles are the ordered CHAIN a user picks. DELETE is 409 while
		// any app, the global policy, or any user preference references it.
		mux.Handle("GET /v1/admin/launch-profiles", admin(http.HandlerFunc(h.handleAdminListLaunchProfiles)))
		mux.Handle("POST /v1/admin/launch-profiles", admin(http.HandlerFunc(h.handleAdminCreateLaunchProfile)))
		mux.Handle("GET /v1/admin/launch-profiles/{id}", admin(http.HandlerFunc(h.handleAdminGetLaunchProfile)))
		mux.Handle("PATCH /v1/admin/launch-profiles/{id}", admin(http.HandlerFunc(h.handleAdminPatchLaunchProfile)))
		mux.Handle("DELETE /v1/admin/launch-profiles/{id}", admin(http.HandlerFunc(h.handleAdminDeleteLaunchProfile)))
		// Encoder certification: a script opens a run, drives each cell (launch,
		// peer drives frames, finalize) and closes it.
		mux.Handle("GET /v1/admin/hosts/{id}/encoder-certification", admin(http.HandlerFunc(h.handleGetEncoderCerts)))
		mux.Handle("POST /v1/admin/hosts/{id}/encoder-certification/runs", admin(http.HandlerFunc(h.handleTriggerCertRun)))
		mux.Handle("GET /v1/admin/hosts/{id}/encoder-certification/runs/{run_id}", admin(http.HandlerFunc(h.handleGetCertRun)))
		mux.Handle("POST /v1/admin/hosts/{id}/encoder-certification/runs/{run_id}/complete", admin(http.HandlerFunc(h.handleCompleteCertRun)))
		mux.Handle("POST /v1/admin/hosts/{id}/encoder-certification/cells", admin(http.HandlerFunc(h.handleCertCellLaunch)))
		mux.Handle("POST /v1/admin/hosts/{id}/encoder-certification/cells/{session_id}/finalize", admin(http.HandlerFunc(h.handleCertCellFinalize)))
	}
}

// --- DTOs (shapes from control-api.md) ---------------------------------------

type streamResp struct {
	Width       int32  `json:"width"`
	Height      int32  `json:"height"`
	FPS         int32  `json:"fps"`
	BitrateKbps int32  `json:"bitrate_kbps"`
	H264Profile string `json:"h264_profile"`
	// The resolved codec, wire vocabulary, beside h264_profile (§3.3).
	Codec      string `json:"codec"`
	Playout0Ms int32  `json:"playout0_ms"`
	// Mic is the GRANTED state: what was negotiated, not what was requested.
	// Always serialized.
	Mic bool `json:"mic"`
	// The session's CURRENT encoded size; Width/Height above stay the launch size
	// the rung ladder derives from. Present whenever the answer is KNOWN,
	// including when it equals width/height — absence means unknown, never
	// "unstepped", so a client holding its last-acked value can still see a revert
	// to the launch size.
	ExternalWidth  *int32 `json:"external_width,omitempty"`
	ExternalHeight *int32 `json:"external_height,omitempty"`
	// Who owns the live external size: the ABR ladder ("auto") or a manual PATCH
	// ("pinned"). Present only when known and different from launch, the window
	// the agent reports it in (agent-api.md §session_metrics).
	ExternalOwner string `json:"external_owner,omitempty"`
	// The host encoder's live-resize capability as last reported. Omitted while
	// unknown, which the API treats as PERMISSIVE: a client should offer the
	// control and let the 409 land, not hide it.
	ExternalResizeSupported *bool `json:"external_resize_supported,omitempty"`
	// The ladder of external sizes this session may be stepped to, descending,
	// launch size first. Always serialized so a client never duplicates the family
	// table; an aspect ratio with no family gets one entry.
	Rungs [][2]int32 `json:"rungs"`
}

type sessionResp struct {
	ID           string  `json:"id"`
	UserID       string  `json:"user_id"`
	AppID        string  `json:"app_id"`
	HostID       *string `json:"host_id"`
	State        string  `json:"state"`
	StateDetail  *string `json:"state_detail"`
	ErrorMessage *string `json:"error_message"`
	// The machine-readable classification of a terminal failure; the UI branches
	// on this, not on the prose in error_message.
	FailureCode *string `json:"failure_code"`
	// The app container's last ~100 log lines, oldest first, captured while it
	// ran: containers use `--rm`, so the daemon has discarded them by the time
	// anyone looks (#463).
	AppLogTail *string `json:"app_log_tail"`
	// The launch profile the session came from, null for a legacy/tier/override
	// launch. Resolved values are in Stream; metadata at GET /v1/me/profiles.
	ProfileID *string `json:"profile_id"`
	// The RUNG the launch resolved to, null when none. profile_id is what the user
	// picked, this is what they got, and since a rung carries its own resolution
	// the two can legitimately disagree — `stream` below is always the truth.
	// control-api.md amendment A3; always serialized.
	StreamProfileID *string    `json:"stream_profile_id"`
	Stream          streamResp `json:"stream"`
	// CodecDecision records how the rung above was resolved. Always serialized,
	// null when no chain was walked. Shape and semantics: control-api.md
	// §Codec decision.
	CodecDecision json.RawMessage `json:"codec_decision"`
	// What the BROWSER reports decoding, normalised to the wire vocabulary. Beside
	// stream.codec rather than replacing it: their disagreement is the defect
	// signal, and reconciling them erases it.
	NegotiatedCodec *string `json:"negotiated_codec"`
	// Computed stream health with an optional reason; omitted when empty.
	HealthState  string     `json:"health_state,omitempty"`
	HealthReason *string    `json:"health_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at"`
}

type signalingResp struct {
	URL       string    `json:"url"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	// No omitempty: the contract requires this array always be serialized, so a
	// client never has to tell "configured nothing" from "key missing".
	// newSignalingResp turns a nil config into [] rather than JSON null.
	ICEServers []ice.Server `json:"ice_servers"`
}

// newSignalingResp builds the signaling coordinates. Both minting paths (launch
// and reconnect) must go through it: a launch that hands over ICE servers while
// a reconnect drops them yields a session that works until the first reconnect.
func (h *Handler) newSignalingResp(r *http.Request, token string, expiresAt time.Time) signalingResp {
	servers := h.iceServers
	if servers == nil {
		// [] rather than null: null is a third state the contract does not define.
		servers = []ice.Server{}
	}
	return signalingResp{
		URL:        h.signalingURL(r),
		Token:      token,
		ExpiresAt:  expiresAt,
		ICEServers: servers,
	}
}

// toSessionResp serializes with NO external-resolution overlay, for paths with
// no coordinator in hand; the HTTP handlers go through h.sessionResp.
func toSessionResp(s Session) sessionResp {
	return toSessionRespExt(s, externalState{}, false)
}

// sessionResp is toSessionResp with the coordinator's live external-resolution
// cache folded in. A nil coordinator degrades to the plain form.
func (h *Handler) sessionResp(s Session) sessionResp {
	if h.coord == nil {
		return toSessionResp(s)
	}
	st, ok := h.coord.ExternalState(s.ID)
	return toSessionRespExt(s, st, ok)
}

func toSessionRespExt(s Session, ext externalState, haveExt bool) sessionResp {
	stream := streamResp{
		Width: s.Width, Height: s.Height, FPS: s.FPS,
		BitrateKbps: s.BitrateKbps, H264Profile: s.H264Profile,
		Codec:      s.Codec,
		Playout0Ms: s.Playout0Ms,
		Mic:        s.Mic,
		Rungs:      rungPairs(profile.AvailableRungs(s.Width, s.Height)),
	}
	if haveExt {
		// Emitted whenever the size is KNOWN, including when it equals the launch
		// size. Suppressing that case would make the poll non-authoritative: a
		// client holding its last-acked value on absence could never learn that
		// another actor put the session back at launch size.
		w, h := s.Width, s.Height
		if ext.HasSize {
			w, h = ext.Width, ext.Height
		}
		stream.ExternalWidth, stream.ExternalHeight = &w, &h
		stream.ExternalResizeSupported = ext.Supported
		stream.ExternalOwner = ext.Owner
	}
	return sessionRespWithStream(s, stream)
}

// rungPairs flattens the ladder to the wire's [[w,h], …] shape.
func rungPairs(rungs []profile.Rung) [][2]int32 {
	out := make([][2]int32, 0, len(rungs))
	for _, r := range rungs {
		out = append(out, [2]int32{r.Width, r.Height})
	}
	return out
}

func sessionRespWithStream(s Session, stream streamResp) sessionResp {
	return sessionResp{
		ID:              s.ID,
		UserID:          s.UserID,
		AppID:           s.AppID,
		HostID:          s.HostID,
		State:           string(s.State),
		StateDetail:     s.StateDetail,
		ErrorMessage:    s.ErrorMessage,
		FailureCode:     s.FailureCode,
		AppLogTail:      s.AppLogTail,
		ProfileID:       s.ProfileID,
		StreamProfileID: s.StreamProfileID,
		CodecDecision:   s.CodecDecision,
		NegotiatedCodec: s.NegotiatedCodec,

		HealthState:  string(s.HealthState),
		HealthReason: s.HealthReason,
		Stream:       stream,
		CreatedAt:    s.CreatedAt,
		StartedAt:    s.StartedAt,
		EndedAt:      s.EndedAt,
	}
}

// --- handlers ----------------------------------------------------------------

func (h *Handler) handleLaunch(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	var req struct {
		AppID      string  `json:"app_id"`
		ProfileID  *string `json:"profile_id"`
		ClientType *string `json:"client_type"` // "native" | "browser"; absent ⇒ browser
		// The launch REQUEST, absent ⇒ false. A plain bool, not a pointer: there
		// is nothing to distinguish an absent value from an explicit false.
		Mic    bool `json:"mic"`
		Stream *struct {
			Width       *int32  `json:"width"`
			Height      *int32  `json:"height"`
			FPS         *int32  `json:"fps"`
			BitrateKbps *int32  `json:"bitrate_kbps"`
			H264Profile *string `json:"h264_profile"`
			Codec       *string `json:"codec"`
		} `json:"stream"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.AppID == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "app_id is required")
		return
	}

	var ov StreamOverride
	if req.Stream != nil {
		// Optional per-launch h264_profile, validated against the schema.md set;
		// absent means the constrained-baseline floor.
		if p := req.Stream.H264Profile; p != nil && !ValidH264Profile(*p) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
				"h264_profile must be one of: constrained-baseline, main, high")
			return
		}
		// Optional codec override against the wire set; absent auto-resolves.
		if c := req.Stream.Codec; c != nil && *c != "" && !ValidCodec(*c) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
				"codec must be one of: h264, h265, av1")
			return
		}
		ov = StreamOverride{
			Width: req.Stream.Width, Height: req.Stream.Height,
			FPS: req.Stream.FPS, BitrateKbps: req.Stream.BitrateKbps,
			H264Profile: req.Stream.H264Profile,
			Codec:       req.Stream.Codec,
		}
	}

	lp := LaunchParams{
		AppID:    req.AppID,
		Override: ov,
		IsAdmin:  user.Role == "admin",
	}
	if req.ProfileID != nil {
		lp.ProfileID = *req.ProfileID
	}
	// Binds the H.264 profile lift to the launching client's own declaration, so
	// a native session's stored probe cannot poison a later browser launch on the
	// same account. See LaunchParams.ClientType.
	if req.ClientType != nil {
		lp.ClientType = *req.ClientType
	}
	lp.Mic = req.Mic

	res, err := h.coord.LaunchByProfile(r.Context(), user.ID, lp)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
		return
	case errors.Is(err, ErrProfileUnknown):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"unknown profile_id")
		return
	case errors.Is(err, ErrProfileIneligible):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeProfileIneligible,
			"the selected stream profile is not eligible for this device")
		return
	case errors.Is(err, ErrCodecUnsupportedByHost):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict,
			"the requested codec is not supported by the assigned host's encoder")
		return
	case errors.Is(err, ErrProfileOverrideDisabled):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict,
			"profile overrides are disabled for this launch")
		return
	// A valid, user-visible profile refused by server-side configuration. Its own
	// code, not the generic `conflict` this endpoint already emits twice, because
	// the remedy differs: the caller's menu is stale.
	// semantics: control-api.md §Errors
	case errors.Is(err, ErrProfileNotLaunchableForApp):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeProfileNotLaunchable,
			"the selected launch profile is not offered by this app")
		return
	// 403, not the 404 GET /v1/apps/{id} gives: a read can say nothing, a launch
	// cannot. Emitted inside the scheduling transaction, for every role.
	case errors.Is(err, ErrNotEntitled):
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden,
			"you are not entitled to launch this app")
		return
	case errors.Is(err, ErrSessionQuotaExceeded):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeSessionQuota,
			"active session limit reached; stop a session before launching another")
		return
	// The body carries the conflicting SESSION ID (§2.1/§2.2), nested INSIDE the
	// error object like hostcfg's restart_required, so the client can link to the
	// running session instead of showing a generic failure for an app the user did
	// not click. Absent rather than empty when the guard named none.
	case errors.Is(err, ErrHomeInUse):
		writeHomeInUse(w, err, "you already have a live session backed by this app's storage; go to it or stop it before launching another")
		return
	// Names the PARENT app and the one action that fixes it. 409 rather than 503:
	// nothing is busy, and retrying changes nothing.
	case errors.Is(err, ErrHomeNotProvisioned):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeHomeNotProvisioned,
			"this game is launched through another app's library, and you have no library for it yet — launch that app once on a host to create your library, then try again")
		return
	// A tile borrows its parent's image, runtime and home, so a disabled parent
	// must stop the tile too — and the library still shows the tile as enabled.
	// Names the parent so the message is actionable.
	case errors.Is(err, ErrParentDisabled):
		writeParentDisabled(w, err)
		return
	case errors.Is(err, ErrNoHostAvailable):
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeNoHostAvailable,
			"no host is available to serve this launch")
		return
	case errors.Is(err, ErrCapacityExhausted):
		// The encode-slot reservation holds through `stopping` (#489 overlap
		// prevention; never shorten it to make this header smaller), so a relaunch
		// right after a peer's DELETE bounces here for the ~15 s that teardown
		// takes. Retry-After lets a polling client wait rather than error (#494).
		w.Header().Set("Retry-After", capacityExhaustedRetryAfterSeconds)
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeCapacityExhausted,
			"all capacity is in use; try again shortly")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not launch session")
		return
	}

	// Actor is the launching user, admin or not: "who started this session" is
	// the question an operator asks of a session, and this feed is the
	// instance's operational history, not a list of things admins did.
	launched := map[string]any{"app_id": res.Session.AppID}
	if res.Session.HostID != nil {
		launched["host_id"] = *res.Session.HostID
	}
	h.recordActivity(r.Context(), user.ID, "session.launched", "session", res.Session.ID, launched)

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"session":   h.sessionResp(res.Session),
		"signaling": h.newSignalingResp(r, res.SignalingToken, res.TokenExpiresAt),
	})
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	sess, err := h.store.Get(r.Context(), r.PathValue("id"))
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
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"session": h.sessionResp(sess)})
}

func (h *Handler) handleSignalingToken(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id := r.PathValue("id")
	sess, err := h.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) || (err == nil && !canAccess(user, sess)) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "session not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not get session")
		return
	}
	token, err := h.store.MintSignalingToken(r.Context(), id)
	if errors.Is(err, ErrSessionTerminal) {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "session is not reconnectable")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not mint signaling token")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"signaling": h.newSignalingResp(r, token.Plaintext, token.ExpiresAt),
	})
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	cursor := r.URL.Query().Get("cursor")
	var limit int32 = 50
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	sessions, next, err := h.store.ListByUser(r.Context(), user.ID, cursor, limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list sessions")
		return
	}
	items := make([]sessionResp, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, h.sessionResp(s))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nullable(next)})
}

func (h *Handler) handleStop(w http.ResponseWriter, r *http.Request) {
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

	wasTerminal := sess.State.IsTerminal()
	sess, err = h.coord.Stop(r.Context(), id, "user_requested")
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not stop session")
		return
	}
	if user.Role == auth.RoleAdmin {
		h.recordActivity(r.Context(), user.ID, "session.stop", "session", id, map[string]any{"already_terminal": wasTerminal})
	}

	// 202 Accepted while teardown proceeds; 200 if it was already terminal
	// (idempotent — control-api.md).
	status := http.StatusAccepted
	if wasTerminal {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, map[string]any{"session": h.sessionResp(sess)})
}

// handleSwap swaps a running session's source app. Owner-or-admin, validated
// here; the coordinator validates swappable state and reservation fit. 202 with
// the session still running at state_detail="swapping" while the agent works.
func (h *Handler) handleSwap(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id := r.PathValue("id")

	var req struct {
		AppID string `json:"app_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.AppID == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "app_id is required")
		return
	}

	// Ownership: the session must exist and belong to the caller (or admin). The
	// 403/404 precede the swap so a non-owner can't probe swappability.
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

	swapped, err := h.coord.Swap(r.Context(), id, req.AppID)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
		return
	// The swap target is entitlement-gated like a launch (see swapper.Swap).
	case errors.Is(err, ErrNotEntitled):
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden,
			"the session owner is not entitled to that app")
		return
	case errors.Is(err, ErrSessionNotSwappable):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeSessionNotSwappable,
			"session is not in a swappable state (must be running and not already swapping)")
		return
	case errors.Is(err, ErrSwapExceedsReservation):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeSwapExceedsReservation,
			"the new app needs more VRAM or encode slots than the session reserved")
		return
	case errors.Is(err, ErrHomeInUse):
		writeHomeInUse(w, err, "you already have a live session backed by that app's storage; stop it before swapping")
		return
	// A swap is pinned to the LIVE session's host with no placement step to
	// re-pin it, so swapping into a tile whose library lives elsewhere is an
	// ordinary user-correctable condition, not a 500.
	case errors.Is(err, ErrHomeNotProvisioned):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeHomeNotProvisioned,
			"that game is launched through another app's library, and this session's host holds no library for it — launch it directly instead of swapping into it")
		return
	case errors.Is(err, ErrParentDisabled):
		writeParentDisabled(w, err)
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not swap session")
		return
	}

	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"session": h.sessionResp(swapped)})
}

// handleDisplayUpdate changes a running session's render resolution or UI scale.
// Owner-or-admin, with the 404 BEFORE the 403 so a non-owner cannot probe which
// session ids exist.
//
// Unlike swap it touches no session state: nothing is persisted and a rejection
// is a pure no-op, which is why the ack is awaited synchronously and surfaced as
// a 409 rather than dispatched in the background.
func (h *Handler) handleDisplayUpdate(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id := r.PathValue("id")

	// Pointers throughout: an omitted field means unchanged, and a plain int32
	// would decode an absent render_width to 0 and read as "shrink to nothing".
	var req struct {
		RenderWidth  *int32   `json:"render_width"`
		RenderHeight *int32   `json:"render_height"`
		StreamWidth  *int32   `json:"stream_width"`
		StreamHeight *int32   `json:"stream_height"`
		UIScale      *float64 `json:"ui_scale"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

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

	updated, err := h.coord.UpdateDisplay(r.Context(), id, DisplayUpdate{
		RenderWidth:  req.RenderWidth,
		RenderHeight: req.RenderHeight,
		StreamWidth:  req.StreamWidth,
		StreamHeight: req.StreamHeight,
		UIScale:      req.UIScale,
	})
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "session not found")
		return
	case errors.Is(err, ErrDisplayNotRunning):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeSessionNotRunning,
			"session is not running")
		return
	case errors.Is(err, ErrDisplayRejected):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeDisplayUpdateRejected,
			"the host rejected the display update; the session is unchanged")
		return
	// Distinct from the above: the host encoder told us up front it has no live
	// scale stage, so nothing was ever dispatched.
	case errors.Is(err, ErrExternalResizeUnsupported):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeExternalResizeUnsupported,
			"this session's encoder cannot change the stream resolution live")
		return
	// 400 on a POSITIVE sentinel match, never as the fall-through arm: only text
	// this package composed reaches ErrDisplayInvalid, so returning it verbatim
	// leaks nothing, whereas a bare `err != nil` served a pgx driver string as
	// validation_failed.
	case errors.Is(err, ErrDisplayInvalid):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not update display")
		return
	}

	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"session": h.sessionResp(updated)})
}

// --- helpers -----------------------------------------------------------------

// writeHomeInUse emits the 409 home_in_use envelope with `session_id` inside the
// error object when the guard named one (§2.1). Hand-written rather than
// httpx.WriteError because the envelope gains a field, the same shape as
// internal/hostcfg's writeConflictBody. The field is OMITTED, never empty, so a
// client can branch on its presence instead of linking to nowhere.
func writeHomeInUse(w http.ResponseWriter, err error, message string) {
	body := map[string]any{"code": httpx.CodeHomeInUse, "message": message}
	if id := conflictingSessionID(err); id != "" {
		body["session_id"] = id
	}
	httpx.WriteJSON(w, http.StatusConflict, map[string]any{"error": body})
}

// writeParentDisabled emits the 409 parent_app_disabled envelope, naming the
// provider app to re-enable. The name rides in the MESSAGE, not a structured
// field: unlike home_in_use's session_id there is no client action to branch on,
// and a field nobody consumes is contract surface with no purchase.
func writeParentDisabled(w http.ResponseWriter, err error) {
	msg := "this game is launched through another app, and that app is currently disabled — ask an operator to re-enable it"
	if name := disabledParentName(err); name != "" {
		msg = fmt.Sprintf("this game is launched through %q, and that app is currently disabled — ask an operator to re-enable it", name)
	}
	httpx.WriteError(w, http.StatusConflict, httpx.CodeParentAppDisabled, msg)
}

// canAccess: the owner or an admin may read/stop a session.
func canAccess(u auth.User, s Session) bool {
	return s.UserID == u.ID || u.Role == "admin"
}

// signalingURL derives the ws/wss endpoint from the request, per signaling.md's
// "derive from origin" rule.
//
// A secure page must receive wss:// or the browser blocks it as mixed content —
// true for the native TLS listener (r.TLS, #376) and behind a TLS-terminating
// proxy, which reaches us over plain HTTP with X-Forwarded-Proto: https.
//
// The HOST half has the same problem: a proxy that rewrites Host to its upstream
// (the nginx default) makes us hand the browser our own private listener, which
// the client then dials directly and never reaches — the page loads while
// signaling silently never starts, reading as a hung launch. So a PROXIED
// request resolves the host most-authoritative-first: PUBLIC_BASE_URL, the only
// source a client cannot influence; then X-Forwarded-Host, which is
// client-settable and therefore used only for the address handed back to that
// same caller with their own single-use token.
//
// A request with no X-Forwarded-* header is not proxied and keeps r.Host, so a
// PUBLIC_BASE_URL set for invite links cannot break direct LAN access.
func (h *Handler) signalingURL(r *http.Request) string {
	scheme := "ws"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "wss"
	}
	host := r.Host
	if forwardedHost, forwardedScheme, ok := h.forwardedOrigin(r); ok {
		host = forwardedHost
		if forwardedScheme != "" {
			scheme = forwardedScheme
		}
	}
	return fmt.Sprintf("%s://%s/v1/signal", scheme, host)
}

// forwardedOrigin returns the host (and, when it comes from PUBLIC_BASE_URL, the
// ws scheme) the client should be sent back to, or ok=false when the request did
// not arrive through a proxy and r.Host already is the answer.
func (h *Handler) forwardedOrigin(r *http.Request) (host, scheme string, ok bool) {
	xfHost := r.Header.Get("X-Forwarded-Host")
	if xfHost == "" && r.Header.Get("X-Forwarded-Proto") == "" && r.Header.Get("X-Forwarded-For") == "" {
		return "", "", false // not proxied
	}
	if u, err := url.Parse(h.publicBaseURL); err == nil && u.Host != "" {
		switch u.Scheme {
		case "https":
			return u.Host, "wss", true
		case "http":
			return u.Host, "ws", true
		}
	}
	if xfHost != "" {
		// A comma list means a chain of proxies; the left-most is the client's.
		if i := strings.IndexByte(xfHost, ','); i >= 0 {
			xfHost = xfHost[:i]
		}
		if xfHost = strings.TrimSpace(xfHost); xfHost != "" {
			return xfHost, "", true
		}
	}
	return "", "", false
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// handleHostGPUs returns GPU availability for a specific host (P2-09 capacity dashboard).
func (h *Handler) handleHostGPUs(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	avail, err := h.store.GPUAvailability(r.Context(), hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not get GPU availability")
		return
	}
	type gpuResp struct {
		GPUID    string `json:"gpu_id"`
		GPUIndex int32  `json:"gpu_index"`
		Vendor   string `json:"vendor"`
		Model    string `json:"model"`
		// VramMBTotal is the agent-reported total. VramMBReserved is the DECLARED
		// accounting, retained + deprecated for one release (#383 §5): it is
		// permanently 0 for sessions created after declared VRAM left admission.
		VramMBTotal    int32 `json:"vram_mb_total"`
		VramMBReserved int32 `json:"vram_mb_reserved"`
		// The live figures (#383 §3). Nullable and additive — null means UNKNOWN
		// (never sampled / implausible / invalidated by a reconnect), and a client
		// must render it as such, not as 0%.
		VramMBUsed     *int32  `json:"vram_mb_used"`
		VramMBFree     *int32  `json:"vram_mb_free"`
		VramSampledAt  *string `json:"vram_sampled_at"`
		SlotsTotal     int32   `json:"slots_total"`
		SlotsReserved  int32   `json:"slots_reserved"`
		ActiveSessions int32   `json:"active_sessions"`
		RenderNode     *string `json:"render_node"`
		DevicePath     *string `json:"device_path"`
	}
	items := make([]gpuResp, 0, len(avail))
	for _, g := range avail {
		var sampledAt *string
		if g.VramSampledAt != nil {
			s := g.VramSampledAt.UTC().Format(time.RFC3339)
			sampledAt = &s
		}
		items = append(items, gpuResp{
			GPUID: g.GPUID, GPUIndex: g.GPUIndex,
			Vendor: g.Vendor, Model: g.Model,
			VramMBTotal: g.VramMBTotal, VramMBReserved: g.VramMBReserved,
			VramMBUsed: g.VramMBUsed, VramMBFree: g.VramMBFree, VramSampledAt: sampledAt,
			SlotsTotal: g.SlotsTotal, SlotsReserved: g.SlotsReserved,
			ActiveSessions: g.ActiveSessions,
			RenderNode:     g.RenderNode,
			DevicePath:     g.DevicePath,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// hostResp is the host body returned by the lifecycle endpoints (P3-03), matching
// the shape of GET /v1/hosts (control-api.md §Hosts).
type hostResp struct {
	ID            string  `json:"id"`
	NodeName      string  `json:"node_name"`
	Status        string  `json:"status"`
	AgentVersion  *string `json:"agent_version"`
	CPUCores      *int32  `json:"cpu_cores"`
	MemMB         *int32  `json:"mem_mb"`
	LastHeartbeat *string `json:"last_heartbeat_at"`
}

func hostToResp(h Host) hostResp {
	var lastHb *string
	if h.LastHeartbeat != nil {
		s := h.LastHeartbeat.Format(time.RFC3339)
		lastHb = &s
	}
	return hostResp{
		ID: h.ID, NodeName: h.NodeName, Status: h.Status,
		AgentVersion: h.AgentVersion, CPUCores: h.CPUCores, MemMB: h.MemMB,
		LastHeartbeat: lastHb,
	}
}

// writeHostLifecycleResult maps the shared DrainHost/UncordonHost error set onto
// the control-api.md status codes and, on success, returns 200 {host}.
func writeHostLifecycleResult(w http.ResponseWriter, h Host, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
	case errors.Is(err, ErrHostNotDrainable):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "host is offline; nothing to drain")
	case errors.Is(err, ErrHostNotResumable):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict,
			"host is offline; it returns online on its own when its agent reconnects")
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "host lifecycle operation failed")
	default:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"host": hostToResp(h)})
	}
}

// handleDrainHost cordons a host (P3-03). Optional body {"force": bool}; an empty
// body defaults to graceful (force=false).
func (h *Handler) handleDrainHost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Force bool `json:"force"`
	}
	if r.Body != nil {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
			return
		}
	}
	host, err := h.coord.DrainHost(r.Context(), r.PathValue("id"), req.Force)
	if err == nil {
		user, _ := auth.UserFromContext(r.Context())
		h.recordActivity(r.Context(), user.ID, "host.drain", "host", host.ID, map[string]any{"force": req.Force})
	}
	writeHostLifecycleResult(w, host, err)
}

// handleUncordonHost returns a draining host to service (P3-03).
func (h *Handler) handleUncordonHost(w http.ResponseWriter, r *http.Request) {
	host, err := h.coord.UncordonHost(r.Context(), r.PathValue("id"))
	if err == nil {
		user, _ := auth.UserFromContext(r.Context())
		// host.drain records {"force": …}; its inverse has no body, so the useful
		// fact is which host came back — a uuid alone names nothing on the page.
		h.recordActivity(r.Context(), user.ID, "host.uncordon", "host", host.ID,
			map[string]any{"node_name": host.NodeName})
	}
	writeHostLifecycleResult(w, host, err)
}

// handleAdminList returns all sessions across users for operator oversight (P2-10).
// Gated by admin middleware wired in Register.
func (h *Handler) handleAdminList(w http.ResponseWriter, r *http.Request) {
	cursor := r.URL.Query().Get("cursor")
	var limit int32 = 100
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	filter, ok := ParseAdminStateFilter(r.URL.Query().Get("state"))
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidState,
			"state must be one of: all, active, ended, failed")
		return
	}

	sessions, next, err := h.store.ListAll(r.Context(), cursor, limit, filter)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list sessions")
		return
	}

	// P4-05: the optional, additive latest_metrics object per item (the most-recent
	// merged sample per source). One cheap query for the whole page (no N+1); absent
	// when a session has no telemetry yet. Best-effort: a read error degrades to the
	// pre-P4-05 list (no latest_metrics), never a 500.
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	latest, err := h.store.Telemetry().LatestPerSession(r.Context(), ids)
	if err != nil {
		slog.Warn("latest metrics for admin list failed", "err", err)
		latest = nil
	}

	items := make([]adminSessionResp, 0, len(sessions))
	for _, s := range sessions {
		item := adminSessionResp{
			sessionResp: h.sessionResp(s.Session),
			Username:    s.Username,
			AppName:     s.AppName,
			HostName:    s.HostName,
		}
		if ml, ok := latest[s.ID]; ok {
			item.LatestMetrics = toLatestMetrics(ml)
		}
		items = append(items, item)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nullable(next)})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
		return false
	}
	return true
}
