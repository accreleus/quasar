package platform

import "time"

// The wire shapes and closed vocabularies of the apply half; the read half's
// are in release.go. Every field serializes always — a client must read `null`,
// never an absent key. semantics: control-api.md §"Platform-release apply"

// Attempt kinds. A revert is an apply with an older digest set — same wire
// message, same states, same reasons — so it is a kind, not a second table.
const (
	KindApply  = "apply"
	KindRevert = "revert"
)

// `ApplyAttemptState`. The six middle values are exactly agent-api.md
// `release_state.state`, relayed unchanged; `queued` and `waiting_sessions`
// precede the wire, and `cancelled` follows a cancel that caught the attempt
// before it was sent.
const (
	AttemptQueued          = "queued"
	AttemptWaitingSessions = "waiting_sessions"
	AttemptPending         = "pending"
	AttemptPulling         = "pulling"
	AttemptRecreating      = "recreating"
	AttemptVerifying       = "verifying"
	AttemptSucceeded       = "succeeded"
	AttemptFailed          = "failed"
	AttemptCancelled       = "cancelled"
)

// TerminalAttemptState reports whether an attempt in this state is resolved.
// The open/terminal split is the same one the partial unique index encodes, so
// it is stated once here and once in SQL, and the DB test pins them together.
func TerminalAttemptState(state string) bool {
	switch state {
	case AttemptSucceeded, AttemptFailed, AttemptCancelled:
		return true
	}
	return false
}

// wireAttemptStates are the states an agent may report on `release_state`. A
// state outside this set is dropped rather than written: the two control-plane
// states are never sent by an agent, and `cancelled` is never a wire state.
var wireAttemptStates = map[string]bool{
	AttemptPending:    true,
	AttemptPulling:    true,
	AttemptRecreating: true,
	AttemptVerifying:  true,
	AttemptSucceeded:  true,
	AttemptFailed:     true,
}

// `ApplyFailureReason` — the closed vocabulary, shared verbatim with
// agent-api.md `release_state.reason` and with the `release_apply` ack's error,
// so one client-side mapping serves the wire, this API and the history.
// `unsupported` is written by the control plane and never sent on the wire.
const (
	ReasonUpdaterAbsentFailure = "updater_absent"
	ReasonBusy                 = "busy"
	ReasonInvalid              = "invalid"
	ReasonNamespaceRejected    = "namespace_rejected"
	ReasonDigestMalformed      = "digest_malformed"
	ReasonPullFailed           = "pull_failed"
	ReasonRecreateFailed       = "recreate_failed"
	ReasonNeverStarted         = "never_started"
	ReasonUnhealthy            = "unhealthy"
	ReasonUpdaterUnreachable   = "updater_unreachable"
	ReasonTimeout              = "timeout"
	ReasonUnsupported          = "unsupported"
)

// KnownFailureReason reports whether reason is one this build recognises. An
// unrecognised identifier is stored and rendered VERBATIM, never dropped
// (agent-api.md): a future agent's new reason must not cost the whole message.
func KnownFailureReason(reason string) bool {
	switch reason {
	case ReasonUpdaterAbsentFailure, ReasonBusy, ReasonInvalid, ReasonNamespaceRejected,
		ReasonDigestMalformed, ReasonPullFailed, ReasonRecreateFailed, ReasonNeverStarted,
		ReasonUnhealthy, ReasonUpdaterUnreachable, ReasonTimeout, ReasonUnsupported:
		return true
	}
	return false
}

// ComponentDigest is one pinned component: `ApplyComponentDigest`, the same
// shape as the manifest's component and as `release_apply.components`.
type ComponentDigest struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

// PreviousDigest is what a component was on BEFORE an attempt
// (`ApplyPreviousDigest`). Digest is nil when the updater could not determine
// it — never omitted, so a client can tell "nothing was there" from "nobody
// looked".
type PreviousDigest struct {
	Name   string  `json:"name"`
	Digest *string `json:"digest"`
}

// The component the control plane is allowed to send to a host. The
// control-plane component is applied by the updater beside IT and never over an
// agent connection (agent-api.md §release_apply), so it is filtered out here
// rather than trusted to be absent.
const ComponentNodeAgent = "node-agent"

// The component the control plane applies to ITSELF, over its own host's
// updater socket. Never sent to a host.
const ComponentControlPlane = "control-plane"

// Attempt is one `platform_apply_attempts` row and the `PlatformApplyAttempt`
// wire shape.
type Attempt struct {
	ID                string            `json:"id"`
	RunID             *string           `json:"run_id"`
	Kind              string            `json:"kind"`
	Target            string            `json:"target"`
	HostID            *string           `json:"host_id"`
	NodeName          *string           `json:"node_name"`
	ReleaseID         *string           `json:"release_id"`
	RequestedDigests  []ComponentDigest `json:"requested_digests"`
	PreviousDigests   []PreviousDigest  `json:"previous_digests"`
	State             string            `json:"state"`
	Reason            *string           `json:"reason"`
	SessionsRemaining *int              `json:"sessions_remaining"`
	Force             bool              `json:"force"`
	Output            string            `json:"output"`
	RequestedBy       *string           `json:"requested_by"`
	CreatedAt         time.Time         `json:"created_at"`
	StartedAt         *time.Time        `json:"started_at"`
	FinishedAt        *time.Time        `json:"finished_at"`
}

// ActiveApply is the view's `active_apply`: what is in flight right now. One
// field rather than a per-target one, so the same attempt is never in two
// places in one response where the two could disagree.
type ActiveApply struct {
	// The active fleet run, or null. There is at most one
	// (platform_apply_runs_active_uk).
	Run *ApplyRun `json:"run"`
	// EVERY open attempt on the instance, joined to `targets` by host_id.
	Attempts []Attempt `json:"attempts"`
}

// `ApplyRunState`. A run `succeeded` only when every target succeeded, and
// stops at its first failed target: there is no `partial`.
const (
	RunPending   = "pending"
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// TerminalRunState reports whether a run in this state is resolved. SQL twin:
// terminalRunStatesSQL.
func TerminalRunState(state string) bool {
	switch state {
	case RunSucceeded, RunFailed, RunCancelled:
		return true
	}
	return false
}

// RunSkip is one `PlatformApplySkip`: a host the run passed over as ineligible
// at its turn. An ineligibility is not a failure, so it produces no attempt row
// and is reported here instead.
type RunSkip struct {
	HostID   string `json:"host_id"`
	NodeName string `json:"node_name"`
	Reason   string `json:"reason"`
}

// ApplyRun is the `PlatformApplyRun` shape.
//
// `skipped` has no column in migration 0075: it is held by the sequencer for
// the life of the process. A run's skips are all computed AFTER its
// control-plane target, so the restart a fleet run causes cannot lose them;
// only a crash mid-fleet can, and then the list is empty rather than wrong.
type ApplyRun struct {
	ID                string     `json:"id"`
	ReleaseID         string     `json:"release_id"`
	State             string     `json:"state"`
	Force             bool       `json:"force"`
	RequestedBy       *string    `json:"requested_by"`
	CancelRequested   bool       `json:"cancel_requested"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at"`
	CurrentTarget     *string    `json:"current_target"`
	CurrentHostID     *string    `json:"current_host_id"`
	Error             *string    `json:"error"`
	Skipped           []RunSkip  `json:"skipped"`
	Attempts          []Attempt  `json:"attempts"`
	CreatedAt         time.Time  `json:"created_at"`
	StartedAt         *time.Time `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
}

// FleetApplyRequest is `PlatformApplyRequest`.
type FleetApplyRequest struct {
	ReleaseID string `json:"release_id"`
	Force     bool   `json:"force"`
}

// RunEnvelope is the body of every run response.
type RunEnvelope struct {
	Run ApplyRun `json:"run"`
}

// RunsResponse is the `GET /v1/admin/platform/apply/runs` body.
type RunsResponse struct {
	Runs []ApplyRun `json:"runs"`
}

// AttemptsResponse is the `GET /v1/admin/platform/attempts` body.
type AttemptsResponse struct {
	Attempts []Attempt `json:"attempts"`
}

// AttemptEnvelope is the `202` body of both apply endpoints.
type AttemptEnvelope struct {
	Attempt Attempt `json:"attempt"`
}

// HostApplyRequest is `PlatformHostApplyRequest`.
type HostApplyRequest struct {
	ReleaseID string `json:"release_id"`
	Force     bool   `json:"force"`
}

// ReleaseRef is the provenance block of `release_apply`: what a human is
// looking at, never an alternative identity for the images (ADR 0001).
type ReleaseRef struct {
	ID           string  `json:"id"`
	Version      *string `json:"version"`
	SourceCommit string  `json:"source_commit"`
}

// ReleaseStateReport is one relayed `release_state`, in this package's
// vocabulary. The agentws message is adapted onto it at the wiring seam, so
// this package never imports the websocket layer.
type ReleaseStateReport struct {
	RequestID  string
	State      string
	Reason     *string
	Components []ComponentDigest
	Previous   []PreviousDigest
	Output     string
	FinishedAt *time.Time
}

// NodeAgentComponents extracts the components a HOST may be sent from a parsed
// release manifest: today exactly the `node-agent` entry. Empty means the
// release cannot be applied to a host at all, which is a refusal and never an
// empty command.
func NodeAgentComponents(m Manifest) []ComponentDigest {
	out := make([]ComponentDigest, 0, 1)
	for _, c := range m.Components {
		if c.Name != ComponentNodeAgent {
			continue
		}
		out = append(out, ComponentDigest{Name: c.Name, Image: c.Image, Digest: c.Digest})
	}
	return out
}

// ControlPlaneComponents extracts the components the control plane may apply to
// itself: today exactly the `control-plane` entry. Empty means the release
// cannot move this control plane, which is a refusal and never an empty apply.
func ControlPlaneComponents(m Manifest) []ComponentDigest {
	out := make([]ComponentDigest, 0, 1)
	for _, c := range m.Components {
		if c.Name != ComponentControlPlane {
			continue
		}
		out = append(out, ComponentDigest{Name: c.Name, Image: c.Image, Digest: c.Digest})
	}
	return out
}

// unknownPrevious is what an attempt records for `previous_digests` before the
// updater has reported any: the component names it asked for, each with a null
// digest. "Nobody looked" is a fact worth serializing — an empty array would
// read as "there was nothing there".
func unknownPrevious(requested []ComponentDigest) []PreviousDigest {
	out := make([]PreviousDigest, 0, len(requested))
	for _, c := range requested {
		out = append(out, PreviousDigest{Name: c.Name})
	}
	return out
}
