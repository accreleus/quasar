package hostcfg

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

var canonicalAttemptID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (h *Handler) handleIdleApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Group string `json:"group"`
		ApprovalReview
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid idle approval")
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid idle approval")
		return
	}
	attempt, err := h.store.ApproveIdleApply(r.Context(), r.PathValue("id"), req.Group, req.ApprovalReview)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusAccepted, attempt)
	case errors.Is(err, ErrHostNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
	case errors.Is(err, ErrApprovalSuperseded):
		httpx.WriteError(w, http.StatusConflict, "approval_superseded", "Approval needs a fresh policy review.")
	case errors.Is(err, ErrIdleAttemptConflict):
		current, currentErr := h.store.CurrentIdleApply(r.Context(), r.PathValue("id"))
		if currentErr != nil {
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load current attempt")
			return
		}
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{
			"error":   map[string]any{"code": "attempt_conflict", "message": "Another disruptive operation is open."},
			"current": current,
		})
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not approve idle apply")
	}
}

func (h *Handler) handleGetIdleApply(w http.ResponseWriter, r *http.Request) {
	if !canonicalAttemptID.MatchString(r.PathValue("attempt_id")) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid attempt ID")
		return
	}
	attempt, err := h.store.GetIdleApply(r.Context(), r.PathValue("id"), r.PathValue("attempt_id"))
	if errors.Is(err, ErrIdleAttemptNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "idle apply attempt not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load idle apply")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, attempt)
}

func (h *Handler) handleCancelIdleApply(w http.ResponseWriter, r *http.Request) {
	if !canonicalAttemptID.MatchString(r.PathValue("attempt_id")) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid attempt ID")
		return
	}
	attempt, err := h.store.CancelIdleApply(r.Context(), r.PathValue("id"), r.PathValue("attempt_id"))
	switch {
	case err == nil && attempt.Phase == "cancel_pending":
		httpx.WriteJSON(w, http.StatusAccepted, attempt)
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, attempt)
	case errors.Is(err, ErrIdleAttemptNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "idle apply attempt not found")
	case errors.Is(err, ErrIdleCancelTooLate):
		current, loadErr := h.store.GetIdleApply(r.Context(), r.PathValue("id"), r.PathValue("attempt_id"))
		if loadErr != nil {
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load accepted attempt")
			return
		}
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{
			"error":   map[string]any{"code": "cancel_too_late", "message": "Agent already accepted this attempt."},
			"current": current,
		})
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not cancel idle apply")
	}
}

// semantics: control-api.md "Releasing a recovery hold" (amendment 51, #656).
func (h *Handler) handleReleaseRecoveryHold(w http.ResponseWriter, r *http.Request) {
	hostID, attemptID := r.PathValue("id"), r.PathValue("attempt_id")
	if !canonicalAttemptID.MatchString(attemptID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid attempt ID")
		return
	}
	var req struct {
		Acknowledge string `json:"acknowledge"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Acknowledge != "unverified" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, `acknowledge must be "unverified": the host's configuration was never verified`)
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid release request")
		return
	}
	connected := false
	if c, ok := h.dispatcher.(interface{ IsConnected(string) bool }); ok {
		connected = c.IsConnected(hostID)
	}
	status, err := h.store.ReleaseRecoveryHold(r.Context(), hostID, attemptID, connected)
	switch {
	case errors.Is(err, ErrHostNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
		return
	case errors.Is(err, ErrRecoveryHoldNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no recovery hold for this attempt")
		return
	case errors.Is(err, ErrJournalNotReconciled):
		httpx.WriteError(w, http.StatusConflict, "journal_reconciliation_pending", "The host's agent journal is not reconciled. Reconnect the host and wait for reconciliation to finish.")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not release the recovery hold")
		return
	}
	if h.audit != nil {
		if err := h.audit.Record(r.Context(), actorID(r), "host.recovery.release", "host", hostID, map[string]any{"attempt_id": attemptID, "acknowledge": req.Acknowledge}); err != nil {
			slog.Warn("record admin activity failed", "action", "host.recovery.release", "err", err)
		}
	}
	// The release committed: a failed read-back must not turn it into an error.
	var attempt *IdleApplyAttempt
	if view, err := h.store.GetIdleApply(r.Context(), hostID, attemptID); err == nil {
		attempt = &view
	} else {
		slog.Warn("read released attempt failed", "host_id", hostID, "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"host_status": status, "attempt": attempt})
}
