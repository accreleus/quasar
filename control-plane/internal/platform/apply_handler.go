package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// The two apply endpoints. Every refusal is evaluated against the same view the
// page reads, so the button's absence and the endpoint's refusal carry the same
// identifier. semantics: control-api.md §"Platform-release apply"

// Error codes this surface adds. Not in internal/httpx: nothing else emits them.
const (
	CodeReleaseNotOffered         = "release_not_offered"          // 409
	CodeHostNotEligible           = "host_not_eligible"            // 409
	CodeAttemptInFlight           = "attempt_in_flight"            // 409
	CodeRunActive                 = "run_active"                   // 409
	CodeReleaseBelowSchemaVersion = "release_below_schema_version" // 422
	CodeApplyUnsupported          = "apply_unsupported"            // 501
)

// The history read's bounds.
const (
	defaultAttemptLimit = 50
	maxAttemptLimit     = 200
)

// ApplyHandler serves the write half. Separate from Handler so the read
// surface still constructs (and still answers) on a build with no runner
// wired, exactly as Handler does with nil deps.
type ApplyHandler struct {
	store   *Store
	runner  *Runner
	view    func(ctx context.Context) (View, error)
	auditor audit.Recorder
	log     logger
	// Resolves the node-agent digest for a release with no manifest (edge).
	// Nil refuses those applies rather than guessing.
	edge ApplyComponentResolver
}

// logger is the sliver of *slog.Logger this file uses.
type logger interface {
	Error(msg string, args ...any)
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// NewApplyHandler builds the apply endpoints. view is the same release view the
// page reads, so eligibility is evaluated once and in one place.
func NewApplyHandler(store *Store, runner *Runner, view func(ctx context.Context) (View, error), auditor audit.Recorder, log logger) *ApplyHandler {
	return &ApplyHandler{store: store, runner: runner, view: view, auditor: auditor, log: log}
}

// WithEdgeResolver wires registry resolution for manifest-less releases.
func (h *ApplyHandler) WithEdgeResolver(r ApplyComponentResolver) *ApplyHandler {
	h.edge = r
	return h
}

// Register wires the apply routes. admin must compose RequireAuth→RequireAdmin:
// the gate is server-enforced, and hiding the button is never the access
// control.
func (h *ApplyHandler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/admin/platform/hosts/{id}/apply", admin(http.HandlerFunc(h.handleHostApply)))
	mux.Handle("POST /v1/admin/platform/hosts/{id}/revert", admin(http.HandlerFunc(h.handleHostRevert)))
	mux.Handle("GET /v1/admin/platform/attempts", admin(http.HandlerFunc(h.handleAttempts)))
}

// notEligible is the `409 host_not_eligible` body: the envelope plus `reason`
// as a top-level sibling, the shape the 426 client-too-old body uses.
type notEligible struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Reason string `json:"reason"`
}

func writeNotEligible(w http.ResponseWriter, reason string) {
	var body notEligible
	body.Error.Code = CodeHostNotEligible
	body.Error.Message = "this host cannot take this release right now"
	body.Reason = reason
	httpx.WriteJSON(w, http.StatusConflict, body)
}

func (h *ApplyHandler) ready(w http.ResponseWriter) bool {
	if h.store == nil || h.runner == nil || h.view == nil {
		h.log.Error("platform apply has no dependencies wired")
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "apply is not available on this control plane")
		return false
	}
	return true
}

func (h *ApplyHandler) handleHostApply(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	hostID := r.PathValue("id")
	if !looksLikeUUID(hostID) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such host")
		return
	}
	var req HostApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "the request body is not valid JSON")
		return
	}
	if !looksLikeUUID(req.ReleaseID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "release_id must be a uuid")
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
	release, err := h.store.Release(ctx, req.ReleaseID)
	if err != nil {
		if errors.Is(err, ErrReleaseNotFound) {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such release")
			return
		}
		h.internal(w, "read release", err)
		return
	}

	view, err := h.view(ctx)
	if err != nil {
		h.internal(w, "build release view", err)
		return
	}

	// ADR 0002 first: a release below this control plane's schema is
	// unprocessable, not merely un-offered, and saying so names the remedy.
	if release.SchemaVersion < view.Installed.ControlPlane.SchemaVersion {
		httpx.WriteError(w, http.StatusUnprocessableEntity, CodeReleaseBelowSchemaVersion,
			"this release carries an older schema than the control plane; a downgrade is never offered")
		return
	}
	if !offered(view, release.ID) {
		httpx.WriteError(w, http.StatusConflict, CodeReleaseNotOffered,
			"this release is not offered on this instance's channel")
		return
	}
	// ADR 0001: what is applied is a digest. A stable release carries one in its
	// manifest; an edge release has no manifest, so it is resolved from the
	// commit's image tag now (apply_edge.go).
	components := releaseComponents(release)
	if len(components) == 0 {
		if h.edge == nil {
			httpx.WriteError(w, http.StatusConflict, CodeReleaseNotOffered,
				"this release carries no manifest and this control plane cannot reach the registry to resolve one")
			return
		}
		c, err := h.edge.NodeAgentComponent(ctx, release)
		if err != nil {
			h.log.Warn("platform apply: could not resolve the edge node-agent digest", "err", err)
			httpx.WriteError(w, http.StatusConflict, CodeReleaseNotOffered,
				"this release's node-agent image could not be resolved: "+err.Error())
			return
		}
		components = []ComponentDigest{c}
	}
	switch reason := hostTargetReason(view, hostID); reason {
	case "":
	case ReasonAttemptInFlight:
		// Its own code, not a generic ineligibility: the client shows progress
		// for this, not "cannot".
		httpx.WriteError(w, http.StatusConflict, CodeAttemptInFlight,
			"an update is already in flight on this host")
		return
	case ReasonRunActive:
		httpx.WriteError(w, http.StatusConflict, CodeRunActive,
			"a fleet update is running; a standalone apply may not start while it owns the fleet")
		return
	default:
		writeNotEligible(w, reason)
		return
	}
	if !h.runner.Supported(hostID) {
		// Not retried until that host registers again: a register is the only
		// evidence the build changed.
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
			"a fleet update is running; a standalone apply may not start while it owns the fleet")
		return
	}

	previous, err := h.previousDigests(ctx, hostID, components)
	if err != nil {
		h.internal(w, "read previous digests", err)
		return
	}
	actor := actorID(r)
	attempt, err := h.store.CreateHostAttempt(ctx, NewHostAttempt{
		Kind:      KindApply,
		HostID:    hostID,
		ReleaseID: &release.ID,
		Requested: components,
		Previous:  previous,
		Force:     req.Force,
		Actor:     nilIfEmpty(actor),
	})
	if err != nil {
		if errors.Is(err, ErrAttemptInFlight) {
			// The database's partial unique index, not a code check: two admins
			// pressing Apply at the same moment produce this, not two applies.
			httpx.WriteError(w, http.StatusConflict, CodeAttemptInFlight,
				"an update is already in flight on this host")
			return
		}
		h.internal(w, "create attempt", err)
		return
	}

	// The N the operator is agreeing to lose, recorded before the 202 so the
	// page renders it immediately rather than a beat later.
	if n, err := h.store.NonTerminalSessions(ctx, hostID); err == nil {
		if err := h.store.SetWaitingSessions(ctx, attempt.ID, n); err == nil {
			attempt.State = AttemptWaitingSessions
			attempt.SessionsRemaining = &n
		}
	}

	audit.TryRecord(ctx, h.auditor, actor, "platform.apply.host", "host", hostID, map[string]any{
		"attempt_id": attempt.ID,
		"release_id": release.ID,
		"force":      req.Force,
	})
	h.runner.Start(attempt)
	httpx.WriteJSON(w, http.StatusAccepted, AttemptEnvelope{Attempt: attempt})
}

func (h *ApplyHandler) handleAttempts(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	q := r.URL.Query()
	hostID := strings.TrimSpace(q.Get("host_id"))
	if hostID != "" && !looksLikeUUID(hostID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "host_id must be a uuid")
		return
	}
	limit := defaultAttemptLimit
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxAttemptLimit {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
				"limit must be an integer between 1 and 200")
			return
		}
		limit = n
	}
	// An unknown host_id yields an empty list, never a 404.
	attempts, err := h.store.ListAttempts(r.Context(), hostID, limit)
	if err != nil {
		h.internal(w, "list attempts", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, AttemptsResponse{Attempts: attempts})
}

// previousDigests is what this host is demonstrably on: the digests of its last
// succeeded attempt. With none, the component names with null digests — "nobody
// looked", which a client can tell from "there was nothing there".
func (h *ApplyHandler) previousDigests(ctx context.Context, hostID string, requested []ComponentDigest) ([]PreviousDigest, error) {
	last, err := h.store.LastSucceededDigests(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if len(last) == 0 {
		return unknownPrevious(requested), nil
	}
	out := make([]PreviousDigest, 0, len(last))
	for _, c := range last {
		digest := c.Digest
		out = append(out, PreviousDigest{Name: c.Name, Digest: &digest})
	}
	return out, nil
}

func (h *ApplyHandler) internal(w http.ResponseWriter, what string, err error) {
	h.log.Error("platform apply: "+what+" failed", "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not start the apply")
}

// offered reports whether the release is one the page is currently offering —
// the same `available` list, so an apply can never reach a release the UI would
// not show.
func offered(v View, releaseID string) bool {
	for _, r := range v.Available {
		if r.ID == releaseID {
			return true
		}
	}
	return false
}

// hostTargetReason returns the host target's ineligibility reason, "" when it is
// eligible, and `identity_unknown` when the view has no row for it at all (a
// host the release view never saw is not one an apply may be aimed at).
func hostTargetReason(v View, hostID string) string {
	for _, t := range v.Targets {
		if t.Kind != TargetHost || t.HostID == nil || *t.HostID != hostID {
			continue
		}
		if t.Eligible {
			return ""
		}
		if t.Reason == nil {
			return ReasonIdentityUnknown
		}
		return *t.Reason
	}
	return ReasonIdentityUnknown
}

// releaseComponents is the node-agent component of a release's manifest, and
// only that: the control-plane component is never sent to a host.
func releaseComponents(r Release) []ComponentDigest {
	if len(r.Manifest) == 0 {
		return nil
	}
	m, err := ParseManifest(r.Manifest)
	if err != nil {
		return nil
	}
	return NodeAgentComponents(m)
}

func actorID(r *http.Request) string {
	if u, ok := auth.UserFromContext(r.Context()); ok {
		return u.ID
	}
	return ""
}

// looksLikeUUID is a shape check, not a parse: the database does the parsing,
// and this exists so a malformed path segment is a 404/400 rather than a 500.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
