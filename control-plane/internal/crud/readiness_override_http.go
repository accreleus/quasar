package crud

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/readinessgate"
)

// checkIDPattern is control-api.md's whole validation rule for the URL
// segment: ServeMux's {check_id} is already percent-decoded once, so this runs
// against the decoded value — a literal `%` here means the client encoded it
// twice, which the contract rejects rather than decoding again.
var checkIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func (h *Handler) handleSetReadinessOverride(w http.ResponseWriter, r *http.Request) {
	checkID := r.PathValue("check_id")
	if !checkIDPattern.MatchString(checkID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"check_id must be 1-128 characters of [A-Za-z0-9._:-]")
		return
	}
	caller, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	o, _, err := h.store.readinessGate().SetOverride(r.Context(), r.PathValue("id"), checkID, caller.ID)
	var refused *readinessgate.NotOverridableError
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, o)
	case errors.Is(err, readinessgate.ErrHostNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
	case errors.As(err, &refused):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, refused.Reason)
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not set readiness override")
	}
}

func (h *Handler) handleClearReadinessOverride(w http.ResponseWriter, r *http.Request) {
	checkID := r.PathValue("check_id")
	if !checkIDPattern.MatchString(checkID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"check_id must be 1-128 characters of [A-Za-z0-9._:-]")
		return
	}
	caller, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	_, err := h.store.readinessGate().ClearOverride(r.Context(), r.PathValue("id"), checkID, caller.ID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, readinessgate.ErrHostNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not clear readiness override")
	}
}
