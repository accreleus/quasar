package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// The four fleet endpoints. Every refusal is evaluated against the same view
// the page reads. semantics: control-api.md §"Platform-release apply"

// CodeRunNotActive is the cancel refusal: the run is already terminal and there
// is nothing to stop.
const CodeRunNotActive = "run_not_active" // 409

// The run history read's bounds.
const (
	defaultRunLimit = 20
	maxRunLimit     = 200
)

// WithFleet wires the fleet sequencer. Nil leaves the four fleet routes
// answering 500, exactly as a nil store does for the per-host ones.
func (h *ApplyHandler) WithFleet(f *FleetRunner) *ApplyHandler {
	h.fleet = f
	return h
}

func (h *ApplyHandler) fleetReady(w http.ResponseWriter) bool {
	if !h.ready(w) {
		return false
	}
	if h.fleet == nil {
		h.log.Error("platform fleet apply has no sequencer wired")
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal,
			"fleet apply is not available on this control plane")
		return false
	}
	return true
}

// ActiveRun is the view's `active_apply.run`: the run that owns the fleet right
// now, with its attempts and its skips. Nil when nothing is in flight.
func (h *ApplyHandler) ActiveRun(ctx context.Context) (*ApplyRun, error) {
	if h.store == nil {
		return nil, nil
	}
	run, err := h.store.ActiveRun(ctx)
	if err != nil || run == nil {
		return nil, err
	}
	h.fillRun(ctx, run)
	return run, nil
}

// fillRun adds what no single row carries: the per-target attempts, and the
// hosts the sequencer passed over.
func (h *ApplyHandler) fillRun(ctx context.Context, run *ApplyRun) {
	attempts, err := h.store.RunAttempts(ctx, run.ID)
	if err != nil {
		h.log.Warn("platform apply: could not read a run's attempts", "run_id", run.ID, "err", err)
	} else {
		run.Attempts = attempts
	}
	if h.fleet != nil {
		run.Skipped = h.fleet.Skips(run.ID)
	}
}

func (h *ApplyHandler) handleFleetApply(w http.ResponseWriter, r *http.Request) {
	if !h.fleetReady(w) {
		return
	}
	var req FleetApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "the request body is not valid JSON")
		return
	}
	if !looksLikeUUID(req.ReleaseID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "release_id must be a uuid")
		return
	}

	ctx := r.Context()
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
	// ADR 0002 first: a downgrade can never become offerable, so it is
	// unprocessable rather than a conflict.
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
	// A run that cannot move the control plane must not start: ADR 0002 puts it
	// first, so with it ineligible every host reads control_plane_not_first and
	// the run would do nothing but fail. `up_to_date` is the one reason that is
	// not a refusal — the run then goes straight to the hosts.
	// Only the durable reasons refuse here: the two transient ones have their
	// own, more specific codes below.
	if reason := fleetTargetReason(view, nil); reason != "" &&
		reason != ReasonUpToDate && reason != ReasonAttemptInFlight {
		httpx.WriteError(w, http.StatusConflict, CodeReleaseNotOffered,
			"this release cannot be applied to the control plane ("+reason+"), and nothing moves before it")
		return
	}
	open, err := h.store.OpenAttempts(ctx)
	if err != nil {
		h.internal(w, "read open attempts", err)
		return
	}
	if len(open) > 0 {
		httpx.WriteError(w, http.StatusConflict, CodeAttemptInFlight,
			"an update is already in flight on this instance")
		return
	}

	actor := actorID(r)
	run, err := h.store.CreateRun(ctx, release.ID, req.Force, nilIfEmpty(actor))
	if err != nil {
		if errors.Is(err, ErrRunActive) {
			// The database's active-run index, not a code check: two admins
			// pressing Update at the same moment produce this, not two fleets.
			httpx.WriteError(w, http.StatusConflict, CodeRunActive,
				"a fleet update is already running on this instance")
			return
		}
		h.internal(w, "create run", err)
		return
	}

	audit.TryRecord(ctx, h.auditor, actor, "platform.apply.run", "platform", release.ID, map[string]any{
		"release_id":    release.ID,
		"source_commit": release.SourceCommit,
		"force":         req.Force,
	})
	h.fleet.Start(run)
	h.fillRun(ctx, &run)
	httpx.WriteJSON(w, http.StatusAccepted, RunEnvelope{Run: run})
}

func (h *ApplyHandler) handleRuns(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	limit := defaultRunLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxRunLimit {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
				"limit must be an integer between 1 and 200")
			return
		}
		limit = n
	}
	runs, err := h.store.ListRuns(r.Context(), limit)
	if err != nil {
		h.internal(w, "list runs", err)
		return
	}
	for i := range runs {
		h.fillRun(r.Context(), &runs[i])
	}
	httpx.WriteJSON(w, http.StatusOK, RunsResponse{Runs: runs})
}

func (h *ApplyHandler) handleRun(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	run, ok := h.readRun(w, r)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, RunEnvelope{Run: run})
}

func (h *ApplyHandler) handleRunCancel(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	runID := r.PathValue("id")
	if !looksLikeUUID(runID) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such run")
		return
	}
	ctx := r.Context()
	run, err := h.store.RequestCancel(ctx, runID)
	switch {
	case errors.Is(err, ErrRunNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such run")
		return
	case errors.Is(err, ErrRunNotActive):
		httpx.WriteError(w, http.StatusConflict, CodeRunNotActive,
			"this run is already finished; there is nothing to stop")
		return
	case err != nil:
		h.internal(w, "cancel run", err)
		return
	}
	// It stops the run before its NEXT target: an attempt already sent finishes,
	// and the run goes cancelled when it does.
	audit.TryRecord(ctx, h.auditor, actorID(r), "platform.apply.cancel", "platform", runID, map[string]any{
		"run_id": runID,
	})
	h.fillRun(ctx, &run)
	httpx.WriteJSON(w, http.StatusOK, RunEnvelope{Run: run})
}

func (h *ApplyHandler) readRun(w http.ResponseWriter, r *http.Request) (ApplyRun, bool) {
	runID := r.PathValue("id")
	if !looksLikeUUID(runID) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such run")
		return ApplyRun{}, false
	}
	run, err := h.store.Run(r.Context(), runID)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such run")
			return ApplyRun{}, false
		}
		h.internal(w, "read run", err)
		return ApplyRun{}, false
	}
	h.fillRun(r.Context(), &run)
	return run, true
}
