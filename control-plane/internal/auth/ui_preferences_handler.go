package auth

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// handleGetUIPreferences returns the caller's own client presentation state.
// There is deliberately no {id} form and no admin variant: a user's UI
// preferences are visible to that user alone.
func (h *Handler) handleGetUIPreferences(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	prefs, err := h.svc.store.GetUIPreferences(r.Context(), user.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load ui preferences")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, prefs)
}

// handlePatchUIPreferences merges the body into the caller's stored
// preferences. Partial: absent fields are untouched. An out-of-vocabulary value
// is a 400 rather than a clamp — see control-api.md.
func (h *Handler) handlePatchUIPreferences(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
		return
	}
	prefs, err := h.svc.store.PatchUIPreferences(r.Context(), user.ID, patch)
	if errors.Is(err, ErrInvalidUIPreference) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save ui preferences")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, prefs)
}
