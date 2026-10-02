package hostcfg

import (
	"encoding/json"
	"errors"
	"io"
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
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid idle approval")
		return
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
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
	connected := false
	if registry, ok := h.dispatcher.(interface{ IsConnected(string) bool }); ok {
		connected = registry.IsConnected(r.PathValue("id"))
	}
	attempt, err := h.store.CancelIdleApply(r.Context(), r.PathValue("id"), r.PathValue("attempt_id"), connected)
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
