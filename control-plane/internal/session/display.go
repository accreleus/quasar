package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// Live render-resolution / UI-scale relay (control-api.md
// `PATCH /v1/sessions/{id}/display`, agent-api.md `session_display_update`).
//
// A relay to the assigned host's agent that never transitions session state. It
// must not write to `sessions` (render size and UI scale are EPHEMERAL
// agent-held state, and session_metrics is the only authoritative readback), and
// must not alter the encode caps, the interpipe boundary or the pinned stream
// WxH. Synchronous where swap is fire-and-forget, because the contract maps an
// agent rejection onto 409 display_update_rejected — safe precisely because
// nothing was mutated first.

// displayAckTimeout is the deadline that becomes a 409 rather than a hung
// request; the contract treats a missing ack as a rejection.
const displayAckTimeout = 5 * time.Second

// Display bounds, mirroring agent-api.md §session_display_update. The agent is
// the authority on its own compositor and re-validates; this is the fast
// rejection.
const (
	displayMinDim     = 16
	displayMinUIScale = 1.0
	displayMaxUIScale = 3.0
)

var (
	// ErrDisplayNotRunning ⇒ 409 session_not_running: the session's top-level
	// state is not `running`, so there is no compositor to talk to.
	ErrDisplayNotRunning = errors.New("session is not running")
	// ErrDisplayRejected ⇒ 409 display_update_rejected: the agent said no, or no
	// ack arrived in time. The session is left exactly as it was.
	ErrDisplayRejected = errors.New("agent rejected display update")
	// ErrDisplayInvalid ⇒ 400 validation_failed. Every validateDisplayUpdate
	// failure wraps this sentinel so the handler matches POSITIVELY rather than on
	// a catch-all `err != nil` — which served a pgx driver error string to the
	// client as validation_failed.
	ErrDisplayInvalid = errors.New("invalid display update")
	// ErrExternalResizeUnsupported ⇒ 409 external_resize_unsupported: the host
	// encoder AFFIRMATIVELY reported no live scale stage. Unknown support is not
	// this error — see externalState.Supported.
	ErrExternalResizeUnsupported = errors.New("host encoder cannot resize the stream live")
)

// DisplayUpdate is partial: a nil field means "leave unchanged", never "set to
// zero", and Render* / Stream* are each both-or-neither.
//
// The two pairs are INDEPENDENT axes, each bounded only by the session's pinned
// launch size, never by the other. Render* is the internal, app-facing
// compositor size; Stream* is the external ENCODED size, retargeting the encode
// pipeline's scale stage live within the session's rung ladder. The external
// size may sit BELOW the render size (the encoder downsamples under congestion)
// and stepping back up is a passthrough, so a stream-only update never changes
// the compositor's mode.
type DisplayUpdate struct {
	RenderWidth  *int32
	RenderHeight *int32
	StreamWidth  *int32
	StreamHeight *int32
	UIScale      *float64
}

// ExternalSize reports a running session's current EXTERNAL (encoded) size,
// falling back to the supplied launch size when nothing is cached. A readback
// only: it does not bound the render size.
func (c *Coordinator) ExternalSize(sessionID string, launchW, launchH int32) (int32, int32) {
	return c.display.externalSizeOf(sessionID, launchW, launchH)
}

// ExternalState is the cached external-resolution state the Session resource
// serializes as `stream.external_*`. The second result is false when nothing has
// been recorded at all.
func (c *Coordinator) ExternalState(sessionID string) (externalState, bool) {
	return c.display.get(sessionID)
}

// UpdateDisplay relays a live render-resolution / UI-scale change to the host
// agent and returns the unchanged session on acceptance. Ownership is the
// handler's job (404-before-403, like swap). Errors: ErrNotFound,
// ErrDisplayNotRunning, ErrDisplayInvalid, ErrDisplayRejected, plus any raw
// store error — which the handler must treat as a 500, not fold into those.
func (c *Coordinator) UpdateDisplay(ctx context.Context, sessionID string, upd DisplayUpdate) (Session, error) {
	sess, err := c.store.Get(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	// State first: the bounds come off the session's stream size.
	if sess.State != StateRunning || sess.HostID == nil {
		return Session{}, ErrDisplayNotRunning
	}
	// Encoder capability before validation: the only refusal about the host rather
	// than the request. Unknown support falls through permissively and the agent
	// nacks into display_update_rejected.
	if upd.StreamWidth != nil || upd.StreamHeight != nil {
		if st, ok := c.display.get(sessionID); ok && st.Supported != nil && !*st.Supported {
			return Session{}, ErrExternalResizeUnsupported
		}
	}
	// Wrapped here rather than in each branch, so validateDisplayUpdate stays a
	// plain predicate and there is one entry point into the caller's error space.
	if err := validateDisplayUpdate(upd, sess); err != nil {
		return Session{}, fmt.Errorf("%w: %s", ErrDisplayInvalid, err)
	}

	cmd := agentws.SessionDisplayUpdateCmd{
		Type:         "session_display_update",
		ID:           newCmdID(),
		SessionID:    sessionID,
		RenderWidth:  upd.RenderWidth,
		RenderHeight: upd.RenderHeight,
		StreamWidth:  upd.StreamWidth,
		StreamHeight: upd.StreamHeight,
		UIScale:      upd.UIScale,
	}
	sendCtx, cancel := context.WithTimeout(ctx, displayAckTimeout)
	defer cancel()
	res, err := c.dispatcher.SendWithAck(sendCtx, *sess.HostID, cmd.ID, cmd)
	if err != nil || !res.OK {
		reason := "agent unreachable"
		if err == nil {
			reason = res.Error
		}
		c.log.Warn("display update rejected/undeliverable",
			"session_id", sessionID, "reason", reason)
		return Session{}, fmt.Errorf("%w: %s", ErrDisplayRejected, reason)
	}
	// Optimistic: the ack means the agent accepted the command, not that the
	// pipeline renegotiated, so the next session_metrics sample overwrites this.
	// It exists so two rapid steps (1080, 720, 540) validate against the size the
	// second is actually landing on rather than a stale one.
	if upd.StreamWidth != nil && upd.StreamHeight != nil {
		c.display.setSize(sessionID, *upd.StreamWidth, *upd.StreamHeight, sess.Width, sess.Height)
	}
	c.log.Info("display update accepted by agent", "session_id", sessionID)
	return sess, nil
}

// validateDisplayUpdate enforces the 400 rules from control-api.md: at least one
// field; render dims both-or-neither, even, within [16, the session's launch
// dimension]; stream dims both-or-neither and on the rung ladder; ui_scale in
// [1.0, 3.0].
//
// Each axis is bounded only by the pinned LAUNCH size, never by the other and
// never by a cached current value. There is no "internal <= external" clamp — see
// DisplayUpdate.
func validateDisplayUpdate(upd DisplayUpdate, sess Session) error {
	hasW, hasH := upd.RenderWidth != nil, upd.RenderHeight != nil
	if hasW != hasH {
		return fmt.Errorf("render_width and render_height must be supplied together")
	}
	hasSW, hasSH := upd.StreamWidth != nil, upd.StreamHeight != nil
	if hasSW != hasSH {
		return fmt.Errorf("stream_width and stream_height must be supplied together")
	}
	if !hasW && !hasSW && upd.UIScale == nil {
		return fmt.Errorf("at least one of render_width+render_height, stream_width+stream_height or ui_scale is required")
	}
	if hasSW {
		sw, sh := *upd.StreamWidth, *upd.StreamHeight
		if !profile.IsRung(sw, sh, sess.Width, sess.Height) {
			return fmt.Errorf("stream_width/stream_height %dx%d is not one of the session's rungs (%s)",
				sw, sh, formatRungs(profile.AvailableRungs(sess.Width, sess.Height)))
		}
	}
	if hasW {
		if err := checkDim("render_width", *upd.RenderWidth, sess.Width); err != nil {
			return err
		}
		if err := checkDim("render_height", *upd.RenderHeight, sess.Height); err != nil {
			return err
		}
	}
	if upd.UIScale != nil {
		s := *upd.UIScale
		if s < displayMinUIScale || s > displayMaxUIScale {
			return fmt.Errorf("ui_scale %g is outside [%g, %g]", s, displayMinUIScale, displayMaxUIScale)
		}
	}
	return nil
}

func checkDim(name string, v, launchMax int32) error {
	if v < displayMinDim {
		return fmt.Errorf("%s %d is below the %d px floor", name, v, displayMinDim)
	}
	if v > launchMax {
		return fmt.Errorf("%s %d exceeds the session's launch size (%d)", name, v, launchMax)
	}
	if v%2 != 0 {
		return fmt.Errorf("%s %d must be even", name, v)
	}
	return nil
}

// formatRungs renders a ladder as "1920x1080, 1600x900" for the 400 message, so
// a client can correct itself rather than guess.
func formatRungs(rungs []profile.Rung) string {
	parts := make([]string, 0, len(rungs))
	for _, r := range rungs {
		parts = append(parts, fmt.Sprintf("%dx%d", r.Width, r.Height))
	}
	return strings.Join(parts, ", ")
}
