package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/jackc/pgx/v5"
)

// Per-host revert. A revert is an apply with an older digest set: the same
// `release_apply` message and the same machine (apply_runner.go), distinguished
// only by `kind: "revert"`. semantics: control-api.md §revert.
//
// The control plane is never revertible (ADR 0002) — it carries migrations. The
// route is per-host, so nothing here can name the control-plane target.

// CodeNothingToRevert is the `409 nothing_to_revert` refusal: this host has no
// succeeded attempt, or its last succeeded one recorded no previous digest.
const CodeNothingToRevert = "nothing_to_revert"

// RevertInputs is every fact the decision reads; PlanRevert does no I/O, the
// same split as plan.go.
type RevertInputs struct {
	// The host's last `succeeded` attempt of either kind: a revert succeeds too,
	// so reverting twice walks back and then forward rather than sticking.
	LastSucceeded *Attempt
	// The row the restored digests belong to, when still knowable.
	PreviousRelease *Release
	// The row this control plane is on: what "above the control plane" is
	// measured against, as in plan.go's faults().
	ControlPlaneRelease *Release
	ControlPlane        buildinfo.Identity
}

// RevertDecision is the attempt to create, or the refusal to write.
type RevertDecision struct {
	OK bool
	// CodeNothingToRevert or CodeHostNotEligible when !OK.
	Code string
	// The `EligibilityReason` carried beside a host_not_eligible.
	Reason string
	// What the revert sends: the previous digest set.
	Requested []ComponentDigest
	// What the host is on now, so the revert is itself revertible.
	Previous []PreviousDigest
	// Provenance, nil when the build cannot be named; the digests are the
	// authority (schema.md).
	ReleaseID *string
}

// PlanRevert decides one revert.
//
// A revert restores a digest, and the digest alone is what is applied (ADR
// 0001); the release row is provenance and the input to the ADR 0002 bound.
// When no row can name the build, the revert is still allowed: that digest ran
// on THIS host under this or an older control plane, and the control plane only
// moves forward, so its schema cannot be above the current one.
func PlanRevert(in RevertInputs) RevertDecision {
	if in.LastSucceeded == nil {
		return RevertDecision{Code: CodeNothingToRevert}
	}
	prev := nodeAgentPrevious(in.LastSucceeded.PreviousDigests)
	if prev == nil || prev.Digest == nil || *prev.Digest == "" {
		// A null digest is "nobody looked": nothing that can be sent.
		return RevertDecision{Code: CodeNothingToRevert}
	}
	image := revertImage(in.PreviousRelease, in.LastSucceeded)
	if image == "" {
		// No repository, no `image@digest`; a guessed one is a different
		// image (ADR 0001).
		return RevertDecision{Code: CodeNothingToRevert}
	}
	// ADR 0002's ceiling, reachable only if the control plane was moved
	// backwards by hand: never create an agent-ahead-of-control-plane fault.
	if in.PreviousRelease != nil && ordersAbove(*in.PreviousRelease, in.ControlPlaneRelease, in.ControlPlane) {
		return RevertDecision{Code: CodeHostNotEligible, Reason: ReasonReleaseAboveControlPlane}
	}

	d := RevertDecision{
		OK:        true,
		Requested: []ComponentDigest{{Name: ComponentNodeAgent, Image: image, Digest: *prev.Digest}},
		Previous:  revertedFrom(in.LastSucceeded.RequestedDigests),
	}
	if in.PreviousRelease != nil {
		id := in.PreviousRelease.ID
		d.ReleaseID = &id
	}
	return d
}

// nodeAgentPrevious is the node-agent entry: the only component a host is sent.
func nodeAgentPrevious(prev []PreviousDigest) *PreviousDigest {
	for i := range prev {
		if prev[i].Name == ComponentNodeAgent {
			return &prev[i]
		}
	}
	return nil
}

// revertImage is the repository the restored digest composes against: the
// manifest's, else the one this host was sent last time. Never a default.
func revertImage(release *Release, last *Attempt) string {
	if release != nil {
		for _, c := range releaseComponents(*release) {
			if c.Name == ComponentNodeAgent && c.Image != "" {
				return c.Image
			}
		}
	}
	for _, c := range last.RequestedDigests {
		if c.Name == ComponentNodeAgent && c.Image != "" {
			return c.Image
		}
	}
	return ""
}

// revertedFrom is what a revert of this revert would restore.
func revertedFrom(requested []ComponentDigest) []PreviousDigest {
	out := make([]PreviousDigest, 0, len(requested))
	for _, c := range requested {
		digest := c.Digest
		out = append(out, PreviousDigest{Name: c.Name, Digest: &digest})
	}
	return out
}

// revertBlockingReason is the subset of the eligibility vocabulary a revert
// honours. The view's reason is evaluated against the NEWEST release, so the
// forward-only ones say nothing about going backwards; the durable install
// facts and the two single-flight reasons still apply.
func revertBlockingReason(v View, hostID string) string {
	switch reason := hostTargetReason(v, hostID); reason {
	case ReasonIdentityUnknown, ReasonInstallModeSource, ReasonUpdaterAbsent,
		ReasonHostOffline, ReasonAttemptInFlight, ReasonRunActive:
		return reason
	default:
		return ""
	}
}

// handleHostRevert serves POST /v1/admin/platform/hosts/{id}/revert.
func (h *ApplyHandler) handleHostRevert(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	hostID := r.PathValue("id")
	if !looksLikeUUID(hostID) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such host")
		return
	}
	// The body is optional: an absent one is force false, not a 400.
	var req struct {
		Force bool `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "the request body is not valid JSON")
		return
	}

	ctx := r.Context()
	if _, err := h.store.HostStatus(ctx, hostID); err != nil {
		if errors.Is(err, ErrHostNotFound) {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such host")
			return
		}
		h.internal(w, "read host", err)
		return
	}
	view, err := h.view(ctx)
	if err != nil {
		h.internal(w, "build release view", err)
		return
	}
	switch reason := revertBlockingReason(view, hostID); reason {
	case "":
	case ReasonAttemptInFlight:
		httpx.WriteError(w, http.StatusConflict, CodeAttemptInFlight,
			"an update is already in flight on this host")
		return
	case ReasonRunActive:
		httpx.WriteError(w, http.StatusConflict, CodeRunActive,
			"a fleet update is running; a standalone revert may not start while it owns the fleet")
		return
	default:
		writeNotEligible(w, reason)
		return
	}

	in, err := h.revertInputs(ctx, view, hostID)
	if err != nil {
		h.internal(w, "read revert history", err)
		return
	}
	decision := PlanRevert(in)
	if !decision.OK {
		switch decision.Code {
		case CodeNothingToRevert:
			httpx.WriteError(w, http.StatusConflict, CodeNothingToRevert,
				"this host has no earlier build recorded to go back to")
		default:
			writeNotEligible(w, decision.Reason)
		}
		return
	}
	if !h.runner.Supported(hostID) {
		httpx.WriteError(w, http.StatusNotImplemented, CodeApplyUnsupported,
			"this host's agent did not answer a previous apply, so it predates the platform-release amendment; update it another way")
		return
	}
	active, err := h.store.ActiveRunExists(ctx)
	if err != nil {
		h.internal(w, "read active run", err)
		return
	}
	if active {
		httpx.WriteError(w, http.StatusConflict, CodeRunActive,
			"a fleet update is running; a standalone revert may not start while it owns the fleet")
		return
	}

	actor := actorID(r)
	attempt, err := h.store.CreateHostAttempt(ctx, NewHostAttempt{
		Kind:      KindRevert,
		HostID:    hostID,
		ReleaseID: decision.ReleaseID,
		Requested: decision.Requested,
		Previous:  decision.Previous,
		Force:     req.Force,
		Actor:     nilIfEmpty(actor),
	})
	if err != nil {
		if errors.Is(err, ErrAttemptInFlight) {
			httpx.WriteError(w, http.StatusConflict, CodeAttemptInFlight,
				"an update is already in flight on this host")
			return
		}
		h.internal(w, "create attempt", err)
		return
	}
	if n, err := h.store.NonTerminalSessions(ctx, hostID); err == nil {
		if err := h.store.SetWaitingSessions(ctx, attempt.ID, n); err == nil {
			attempt.State = AttemptWaitingSessions
			attempt.SessionsRemaining = &n
		}
	}

	audit.TryRecord(ctx, h.auditor, actor, "platform.revert.host", "host", hostID, map[string]any{
		"attempt_id": attempt.ID,
		"release_id": decision.ReleaseID,
		"digest":     decision.Requested[0].Digest,
		"force":      req.Force,
	})
	h.runner.Start(attempt)
	httpx.WriteJSON(w, http.StatusAccepted, AttemptEnvelope{Attempt: attempt})
}

// revertInputs is every read the decision needs.
func (h *ApplyHandler) revertInputs(ctx context.Context, view View, hostID string) (RevertInputs, error) {
	in := RevertInputs{ControlPlane: view.Installed.ControlPlane}
	if cpCommit := view.Installed.ControlPlane.SourceCommit; cpCommit != nil {
		// With no row for the control plane, ordersAbove falls back to
		// schema_version, the key that always exists.
		in.ControlPlaneRelease = matchRelease(view.Available, view.Channel, *cpCommit)
	}
	last, err := h.store.LastSucceededAttempt(ctx, hostID)
	if errors.Is(err, ErrAttemptNotFound) {
		return in, nil
	}
	if err != nil {
		return in, err
	}
	in.LastSucceeded = &last

	prev := nodeAgentPrevious(last.PreviousDigests)
	if prev == nil || prev.Digest == nil || *prev.Digest == "" {
		return in, nil
	}
	rel, err := h.store.ReleaseForNodeAgentDigest(ctx, hostID, *prev.Digest)
	if errors.Is(err, ErrReleaseNotFound) {
		// Provenance lost; PlanRevert says why the revert still runs.
		return in, nil
	}
	if err != nil {
		return in, err
	}
	in.PreviousRelease = &rel
	return in, nil
}

// ─── store reads ────────────────────────────────────────────────────────────

// LastSucceededAttempt is the row whose `previous_digests` a revert restores.
func (s *Store) LastSucceededAttempt(ctx context.Context, hostID string) (Attempt, error) {
	a, err := scanAttempt(s.pool.QueryRow(ctx, `
		SELECT `+attemptColumns+attemptFrom+`
		 WHERE a.host_id = $1::uuid AND a.state = 'succeeded'
		 ORDER BY a.created_at DESC, a.id DESC LIMIT 1
	`, hostID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, ErrAttemptNotFound
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("read last succeeded attempt: %w", err)
	}
	return a, nil
}

// ReleaseForNodeAgentDigest names the build a digest belongs to, the two ways
// it can still be known: a manifest that pins it (stable), else the release a
// succeeded attempt on THIS host asked for it under (edge carries no manifest,
// so its digest was resolved from the commit at apply time).
// ErrReleaseNotFound is provenance lost, not a reason to refuse (PlanRevert).
func (s *Store) ReleaseForNodeAgentDigest(ctx context.Context, hostID, digest string) (Release, error) {
	r, err := s.releaseRow(ctx, `
		SELECT `+releaseColumns+` FROM platform_releases
		 WHERE manifest -> 'components' @> jsonb_build_array(jsonb_build_object('digest', $1::text))
		 ORDER BY schema_version DESC, built_at DESC LIMIT 1`, digest)
	if err == nil || !errors.Is(err, ErrReleaseNotFound) {
		return r, err
	}
	return s.releaseRow(ctx, `
		SELECT `+prefixedReleaseColumns+`
		  FROM platform_releases r
		  JOIN platform_apply_attempts a ON a.release_id = r.id
		 WHERE a.host_id = $1::uuid AND a.state = 'succeeded'
		   AND a.requested_digests @> jsonb_build_array(jsonb_build_object('digest', $2::text))
		 ORDER BY a.created_at DESC, a.id DESC LIMIT 1`, hostID, digest)
}

// prefixedReleaseColumns is releaseColumns for a joined query: same columns,
// same order, so one scan serves both.
const prefixedReleaseColumns = `r.id::text, r.channel, r.version, r.source_commit, r.built_at,
	r.schema_version, r.prerelease, r.notes, r.compare_url, r.manifest, r.discovered_at`

func (s *Store) releaseRow(ctx context.Context, sql string, args ...any) (Release, error) {
	var r Release
	var manifest []byte
	err := s.pool.QueryRow(ctx, sql, args...).Scan(&r.ID, &r.Channel, &r.Version, &r.SourceCommit,
		&r.BuiltAt, &r.SchemaVersion, &r.Prerelease, &r.Notes, &r.CompareURL, &manifest, &r.DiscoveredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Release{}, ErrReleaseNotFound
	}
	if err != nil {
		return Release{}, fmt.Errorf("read platform_release: %w", err)
	}
	if len(manifest) > 0 {
		r.Manifest = manifest
	}
	return r, nil
}

// ─── success evidence for a revert with no known release ────────────────────

// revertIdentityStore is the sliver of Store the register hook needs, asserted
// rather than added to applyStore: a store that cannot resolve a release simply
// has no second evidence rule.
type revertIdentityStore interface {
	ReleaseForNodeAgentDigest(ctx context.Context, hostID, digest string) (Release, error)
}

// revertRegisterEvidence resolves a revert whose target build has no commit to
// match a `register` against. An apply succeeds on the release's `source_commit`
// (apply_runner.go); a revert to an unnameable digest has none, so its evidence
// is the updater's `succeeded` (HandleReleaseState) or a register reporting any
// commit other than the one it was reverted from — a register on THAT commit is
// the agent that has not restarted yet.
func (r *Runner) revertRegisterEvidence(ctx context.Context, a Attempt, reported string) {
	if a.Kind != KindRevert || a.HostID == nil {
		return
	}
	store, ok := r.store.(revertIdentityStore)
	if !ok {
		return
	}
	from := nodeAgentPrevious(a.PreviousDigests)
	if from == nil || from.Digest == nil || *from.Digest == "" {
		return
	}
	rel, err := store.ReleaseForNodeAgentDigest(ctx, *a.HostID, *from.Digest)
	if err != nil {
		// Nothing to compare against; the updater's report or the deadline
		// decides.
		return
	}
	if commitsMatch(rel.SourceCommit, reported) {
		return // still the build being reverted away from
	}
	done, err := r.store.SucceedAttempt(ctx, a.ID)
	if err != nil {
		r.log.Warn("register: could not resolve the revert", "host_id", *a.HostID, "attempt_id", a.ID, "err", err)
		return
	}
	if done {
		r.log.Info("revert succeeded: the host registered on a build other than the one it was reverted from",
			"host_id", *a.HostID, "attempt_id", a.ID, "source_commit", reported)
	}
}
