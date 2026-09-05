package agentws

import (
	"context"
	"log/slog"
)

// Platform-release apply on the wire (agent-api.md §`release_apply`,
// §`release_state`). An agent predating the amendment ignores the unknown
// downstream type and is wire-silent, so only the ack timeout identifies it.

// --- upstream (agent → control plane) ----------------------------------------

// Bounds. Authentication is not validation; nothing here rejects a compliant
// agent.
const (
	maxReleaseRequestIDLen = 64
	maxReleaseStateLen     = 32
	maxReleaseReasonLen    = 64
	maxReleaseNameLen      = 64
	maxReleaseImageLen     = 512
	maxReleaseDigestLen    = 128
	// The contract's own bound: the agent truncates to this before sending, and
	// the column's CHECK is the same number.
	maxReleaseOutputLen = 8192
	// Today exactly one (`node-agent`); the ceiling is slack, not an invitation.
	maxReleaseComponents = 8
)

// ReleaseComponent is one `{name, image, digest}` of a `release_apply` or a
// `release_state` echo.
type ReleaseComponent struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

// ReleasePrevious is one `{name, digest}` the target was on BEFORE — `digest`
// null when the updater could not determine it, never omitted.
type ReleasePrevious struct {
	Name   string  `json:"name"`
	Digest *string `json:"digest"`
}

// ReleaseStateMsg is the agent's apply-progress callback. Fire-and-forget (no
// ack), like image_state, and it carries NO session authority: it says only
// what one apply on this host is doing.
type ReleaseStateMsg struct {
	Type       string             `json:"type"` // "release_state"
	RequestID  string             `json:"request_id"`
	State      string             `json:"state"`
	Reason     *string            `json:"reason"`
	Components []ReleaseComponent `json:"components"`
	Previous   []ReleasePrevious  `json:"previous"`
	Output     string             `json:"output"`
	StartedAt  string             `json:"started_at"`
	UpdatedAt  string             `json:"updated_at"`
	FinishedAt *string            `json:"finished_at"`
}

// validateReleaseState bounds m in place. False ⇒ drop the whole message
// (nothing safe to correlate); over-long strings are truncated rather than
// dropped, because a bad tail must not cost the state transition it describes.
func validateReleaseState(m *ReleaseStateMsg) bool {
	if m.RequestID == "" || len(m.RequestID) > maxReleaseRequestIDLen {
		return false
	}
	if m.State == "" || len(m.State) > maxReleaseStateLen {
		return false
	}
	if m.Reason != nil && len(*m.Reason) > maxReleaseReasonLen {
		trimmed := (*m.Reason)[:maxReleaseReasonLen]
		m.Reason = &trimmed
	}
	if len(m.Output) > maxReleaseOutputLen {
		// From the FRONT: the error is at the end.
		m.Output = m.Output[len(m.Output)-maxReleaseOutputLen:]
	}
	m.Components = boundComponents(m.Components)
	m.Previous = boundPrevious(m.Previous)
	return true
}

func boundComponents(in []ReleaseComponent) []ReleaseComponent {
	if len(in) > maxReleaseComponents {
		in = in[:maxReleaseComponents]
	}
	out := make([]ReleaseComponent, 0, len(in))
	for _, c := range in {
		if c.Name == "" || len(c.Name) > maxReleaseNameLen ||
			len(c.Image) > maxReleaseImageLen || len(c.Digest) > maxReleaseDigestLen {
			continue
		}
		out = append(out, c)
	}
	return out
}

func boundPrevious(in []ReleasePrevious) []ReleasePrevious {
	if len(in) > maxReleaseComponents {
		in = in[:maxReleaseComponents]
	}
	out := make([]ReleasePrevious, 0, len(in))
	for _, p := range in {
		if p.Name == "" || len(p.Name) > maxReleaseNameLen {
			continue
		}
		if p.Digest != nil && len(*p.Digest) > maxReleaseDigestLen {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ReleaseEvents is the platform-apply callback surface, parallel to ImageEvents
// rather than a widening of Events: apply progress has a different owner.
type ReleaseEvents interface {
	// AgentReleaseState relays one release_state. An unknown request id is
	// dropped by the implementation, never stored.
	AgentReleaseState(ctx context.Context, hostID string, m ReleaseStateMsg)
	// AgentRegistered fires on every successful register with the reported
	// identity commit (nil when none). It is the success evidence for an
	// in-flight apply, and the only thing that clears the unsupported flag.
	AgentRegistered(ctx context.Context, hostID string, sourceCommit *string)
}

// noopReleaseEvents is used until an implementation is wired (focused tests,
// and any build with no platform apply store).
type noopReleaseEvents struct{}

func (noopReleaseEvents) AgentReleaseState(context.Context, string, ReleaseStateMsg) {}
func (noopReleaseEvents) AgentRegistered(context.Context, string, *string)           {}

// --- downstream (control plane → agent) --------------------------------------

// ReleaseApplyRef is the `release` provenance block: what a human is looking
// at. The agent must resolve nothing from it — ADR 0001, what is applied is the
// digest and only the digest.
type ReleaseApplyRef struct {
	ID           string  `json:"id"`
	Version      *string `json:"version"`
	SourceCommit string  `json:"source_commit"`
}

// ReleaseApplyCmd moves a host's node-agent image to a release's pinned digest.
// Reserve/prepare semantics like image_ensure: the ack means accepted, and the
// outcome arrives as release_state or as the new agent's register.
type ReleaseApplyCmd struct {
	Type string `json:"type"` // "release_apply"
	ID   string `json:"id"`
	// Minted and persisted by the control plane BEFORE this message is sent:
	// the agent that receives it is normally destroyed by carrying it out.
	RequestID  string             `json:"request_id"`
	Release    ReleaseApplyRef    `json:"release"`
	Components []ReleaseComponent `json:"components"`
	// "The control plane has decided sessions may be killed" — a statement of a
	// decision already taken. The agent does no session logic on it.
	Force bool `json:"force"`
}

// SendReleaseApply dispatches a release_apply and waits for the ack. An error
// means undeliverable, or no ack before ctx expired — and that second case is
// the only evidence an agent predates the amendment.
func (r *Registry) SendReleaseApply(ctx context.Context, hostID, id string, cmd ReleaseApplyCmd) (AckResult, error) {
	cmd.Type = "release_apply"
	cmd.ID = id
	return r.SendWithAck(ctx, hostID, id, cmd)
}

// logReleaseDrop keeps the read loop's drop paths to one line each.
func logReleaseDrop(log *slog.Logger, hostID, why string) {
	log.Warn("release_state: "+why+"; dropped", "host_id", hostID)
}
