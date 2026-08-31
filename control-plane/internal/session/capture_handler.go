package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// On-demand capture (session-capture; control-api.md §On-demand capture,
// agent-api.md §session_capture). A capture is a bounded, admin-triggered
// observation: arm it, the agent observes within a byte and time budget, and
// reports once as a single diag.* trace event.
//
// Not a probe (nothing is inserted into the media path), not session authority
// (a refusal never moves a still-running session), not a subscription: one
// command, one result, single-flight per session — a second capture while one is
// in flight is refused, never queued.
//
// The control plane mints capture_id, clamps the budget, dispatches, and maps
// the agent's ack onto a status code; what can actually be observed is the
// agent's judgement alone.

const (
	// captureAckTimeout: same as displayAckTimeout. The ack means armed, not
	// done; the result arrives later on the trace lane.
	captureAckTimeout = 5 * time.Second

	// The budget sent and the ceiling. MaxBytes bounds the compressed payload; the
	// agent truncates at a line boundary and reports truncated=true.
	captureMaxBytes = 256 * 1024
	captureMaxMs    = 10_000

	// burst_stats window clamps (agent-api.md); clamped again by the agent, which
	// pays for the windows.
	captureMinWindows  = 1
	captureMaxWindows  = 40
	captureMinWindowMs = 100
	captureMaxWindowMs = 1000

	// captureEventPrefix mirrors the agent's diag.<kind> naming: the stored type
	// IS `diag.` + kind, so a read finds a capture with no second table or index.
	captureEventPrefix = "diag."
)

// captureKinds is today's vocabulary; an unknown kind is refused here (422)
// before dispatch, not on the host.
var captureKinds = map[string]bool{
	"pipeline_dot":  true,
	"encoder_props": true,
	"burst_stats":   true,
}

var (
	// ErrCaptureNotRunning ⇒ 409 session_not_running: nothing is being observed
	// because nothing is running.
	ErrCaptureNotRunning = errors.New("session is not running")
	// ErrCaptureBusy ⇒ 409 capture_busy: single-flight. The remedy is to wait,
	// which is why it is distinct from every other refusal.
	ErrCaptureBusy = errors.New("a capture is already in flight for this session")
	// ErrCaptureKindUnsupported ⇒ 422: unknown to this control plane or agent, or
	// unsupported on this session right now. One error for all three; the message
	// carries which.
	ErrCaptureKindUnsupported = errors.New("capture kind is not supported")
	// ErrCaptureUnsupported ⇒ 501: agent predates captures. Reached via ack
	// timeout, not a nack — an unknown message type is wire-silent (agent-api.md:
	// ControlMsg::Unknown produces no reply). Retrying never helps; rebuilding does.
	ErrCaptureUnsupported = errors.New("this host's agent does not implement captures")
	// ErrCaptureAgentNotConnected ⇒ 503: no live agent connection, nothing dispatched.
	ErrCaptureAgentNotConnected = errors.New("the session's host agent is not connected")
	// ErrCaptureRejected ⇒ 409: agent nacked with a reason this control plane has
	// no code for; reported verbatim rather than mapped onto a misdescribing code.
	ErrCaptureRejected = errors.New("the host refused the capture")
)

// CaptureRequest is an arm request after defaulting: a kind plus, for
// burst_stats, a window plan. Params is nil for the kinds that ignore it, so the
// wire never carries a window plan nobody reads.
type CaptureRequest struct {
	Kind   string
	Params *agentws.CaptureParams
}

// CaptureAccepted is what an arm returns: the join key, plus enough context that
// the 202 body needs nothing else looked up.
type CaptureAccepted struct {
	CaptureID  string    `json:"capture_id"`
	Kind       string    `json:"kind"`
	SessionID  string    `json:"session_id"`
	AcceptedAt time.Time `json:"accepted_at"`
}

// clampCaptureParams applies the burst_stats window clamps; nil for every other kind.
func clampCaptureParams(kind string, p *agentws.CaptureParams) *agentws.CaptureParams {
	if kind != "burst_stats" {
		return nil
	}
	out := agentws.CaptureParams{Windows: 20, WindowMs: 250}
	if p != nil {
		if p.Windows != 0 {
			out.Windows = clampInt32(p.Windows, captureMinWindows, captureMaxWindows)
		}
		if p.WindowMs != 0 {
			out.WindowMs = clampInt32(p.WindowMs, captureMinWindowMs, captureMaxWindowMs)
		}
	}
	// Also bounded by the wall-clock budget: a legal 40x1000 plan would otherwise
	// ask for 40s inside a 10s budget.
	if out.Windows*out.WindowMs > captureMaxMs {
		out.Windows = captureMaxMs / out.WindowMs
		if out.Windows < captureMinWindows {
			out.Windows = captureMinWindows
		}
	}
	return &out
}

func clampInt32(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// captureErrorFor maps one agent ack (or its absence) onto this package's error
// space. The timeout branch differs from every other downstream command: those
// treat no-ack as a rejection because the command may have applied silently, but
// a capture mutates nothing, so silence is attributed to an agent build that
// predates the message (agent-api.md's wire-silent unknown-type rule) — turning
// a confusing 409 into a 501 naming `make rebuild`.
func captureErrorFor(res agentws.AckResult, sendErr error) error {
	if sendErr != nil {
		if errors.Is(sendErr, agentws.ErrAgentNotConnected) || errors.Is(sendErr, agentws.ErrSendQueueFull) {
			return ErrCaptureAgentNotConnected
		}
		if errors.Is(sendErr, context.DeadlineExceeded) || errors.Is(sendErr, context.Canceled) {
			return ErrCaptureUnsupported
		}
		return fmt.Errorf("%w: %s", ErrCaptureRejected, sendErr)
	}
	if res.OK {
		return nil
	}
	switch res.Error {
	case "busy":
		return ErrCaptureBusy
	case "unknown_kind":
		return fmt.Errorf("%w: the agent does not know kind", ErrCaptureKindUnsupported)
	case "unsupported":
		return fmt.Errorf("%w: the agent cannot capture this on this session right now", ErrCaptureKindUnsupported)
	case "no_such_session":
		return ErrCaptureNotRunning
	default:
		return fmt.Errorf("%w: %s", ErrCaptureRejected, res.Error)
	}
}

// ArmCapture dispatches one capture and returns the join key on acceptance. The
// session must be `running` and placed, checked before anything is minted so a
// refusal leaves no orphan id behind.
func (c *Coordinator) ArmCapture(ctx context.Context, sessionID string, req CaptureRequest) (CaptureAccepted, error) {
	if !captureKinds[req.Kind] {
		return CaptureAccepted{}, fmt.Errorf("%w: %q is not a capture kind", ErrCaptureKindUnsupported, req.Kind)
	}
	sess, err := c.store.Get(ctx, sessionID)
	if err != nil {
		return CaptureAccepted{}, err
	}
	if sess.State != StateRunning || sess.HostID == nil {
		return CaptureAccepted{}, ErrCaptureNotRunning
	}

	cmd := agentws.SessionCaptureCmd{
		Type:      "session_capture",
		ID:        newCmdID(),
		SessionID: sessionID,
		CaptureID: newCaptureID(),
		Kind:      req.Kind,
		Budget:    agentws.CaptureBudget{MaxBytes: captureMaxBytes, MaxMs: captureMaxMs},
		Params:    clampCaptureParams(req.Kind, req.Params),
	}
	sendCtx, cancel := context.WithTimeout(ctx, captureAckTimeout)
	defer cancel()
	res, sendErr := c.dispatcher.SendWithAck(sendCtx, *sess.HostID, cmd.ID, cmd)
	if err := captureErrorFor(res, sendErr); err != nil {
		c.log.Info("capture refused",
			"session_id", sessionID, "host_id", *sess.HostID, "kind", req.Kind, "reason", err)
		return CaptureAccepted{}, err
	}
	c.log.Info("capture armed",
		"session_id", sessionID, "host_id", *sess.HostID,
		"kind", req.Kind, "capture_id", cmd.CaptureID)
	return CaptureAccepted{
		CaptureID:  cmd.CaptureID,
		Kind:       req.Kind,
		SessionID:  sessionID,
		AcceptedAt: time.Now().UTC(),
	}, nil
}

// newCaptureID mints an RFC-4122 v4 string: the contract types capture_id as a
// uuid. Hand-rolled rather than adding a uuid dependency for one call site (see
// cert_run_manager.go's newCmdID for the same reason); unlike a command id, this
// one appears in URLs, so it gets the real shape.
func newCaptureID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// captureRespFrom renders a stored diag.* event as the wire shape: the agent's
// payload verbatim, plus ts_unix_ms and kind (recovered from the event type when
// the payload omits it). Verbatim so a new kind needs no control-plane release.
func captureRespFrom(e telemetry.Event) map[string]any {
	out := map[string]any{}
	if len(e.Payload) > 0 {
		if err := json.Unmarshal(e.Payload, &out); err != nil {
			out = map[string]any{}
		}
	}
	out["ts_unix_ms"] = e.TsUnixMs
	if _, ok := out["kind"]; !ok {
		out["kind"] = strings.TrimPrefix(e.Type, captureEventPrefix)
	}
	return out
}

// --- HTTP ----------------------------------------------------------------------

// handleArmCapture serves POST /v1/admin/sessions/{id}/capture. Admin-gated by
// the middleware, so the 403 precedes the lookup and no non-admin learns whether
// the session exists.
func (h *Handler) handleArmCapture(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := h.adminSessionOr404(w, r, id); !ok {
		return
	}
	var req struct {
		Kind   string `json:"kind"`
		Params *struct {
			Windows  int32 `json:"windows"`
			WindowMs int32 `json:"window_ms"`
		} `json:"params"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	arm := CaptureRequest{Kind: req.Kind}
	if req.Params != nil {
		arm.Params = &agentws.CaptureParams{Windows: req.Params.Windows, WindowMs: req.Params.WindowMs}
	}

	accepted, err := h.coord.ArmCapture(r.Context(), id, arm)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "session not found")
		return
	case errors.Is(err, ErrCaptureBusy):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeCaptureBusy,
			"a capture is already in flight for this session; captures are single-flight, wait for it to finish")
		return
	case errors.Is(err, ErrCaptureNotRunning):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeSessionNotRunning,
			"session is not running; there is nothing to observe")
		return
	case errors.Is(err, ErrCaptureKindUnsupported):
		// err.Error() verbatim: only text this package composed reaches it, and it
		// names which of the three reasons applied, so it leaks nothing.
		httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeCaptureKindUnsupported, err.Error())
		return
	case errors.Is(err, ErrCaptureUnsupported):
		httpx.WriteError(w, http.StatusNotImplemented, httpx.CodeCaptureUnsupported,
			"this host's agent predates on-demand captures (it never acked); rebuild the agent — retrying will not help")
		return
	case errors.Is(err, ErrCaptureAgentNotConnected):
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeAgentNotConnected,
			"the session's host has no live agent connection")
		return
	case errors.Is(err, ErrCaptureRejected):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, err.Error())
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not arm capture")
		return
	}

	user, _ := auth.UserFromContext(r.Context())
	h.recordActivity(r.Context(), user.ID, "session.capture", "session", id,
		map[string]any{"kind": accepted.Kind, "capture_id": accepted.CaptureID})
	slog.Info("capture accepted", "session_id", id, "kind", accepted.Kind,
		"capture_id", accepted.CaptureID)
	httpx.WriteJSON(w, http.StatusAccepted, accepted)
}

// handleReadCapture serves GET /v1/admin/sessions/{id}/captures/{capture_id}. A
// 404 here is the poll signal, not an error: the arm returned 202, and the
// result lands on the trace lane whenever the agent finishes. A capture that
// failed after acceptance still arrives, with `error` set, so a poller always
// terminates on a body.
func (h *Handler) handleReadCapture(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := h.adminSessionOr404(w, r, id); !ok {
		return
	}
	captureID := r.PathValue("capture_id")
	e, err := h.store.Telemetry().Capture(r.Context(), id, captureID)
	if errors.Is(err, telemetry.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound,
			"no such capture yet; a capture that has been armed but has not reported is a 404 until it lands")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read capture")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, captureRespFrom(e))
}

// bundleCaptures renders a session's captures for the diagnostic bundle. Always
// non-nil so the key is an array, never null.
func (h *Handler) bundleCaptures(ctx context.Context, sessionID string) []map[string]any {
	events, err := h.store.Telemetry().Captures(ctx, sessionID)
	if err != nil {
		// One unreadable side-table must not cost the caller the series/events/verdict.
		slog.Warn("bundle captures read failed", "session_id", sessionID, "err", err)
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, captureRespFrom(e))
	}
	return out
}
