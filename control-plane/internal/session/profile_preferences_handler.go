package session

import (
	"errors"
	"net/http"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

func (h *Handler) handleGetProfilePreferences(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	prefs, err := h.store.GetUserProfilePreferences(r.Context(), user.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load profile preferences")
		return
	}
	policy, err := h.store.GetProfilePolicy(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load profile policy")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"default_profile_id":        prefs.DefaultProfileID,
		"global_default_profile_id": policy.GlobalDefaultProfileID,
		"user_overrides_allowed":    policy.UserOverridesAllowed,
	})
}

func (h *Handler) handlePatchProfilePreferences(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	// GlobalDefaultProfileID/UserOverridesAllowed are server-owned (written via
	// admin-gated PATCH /v1/admin/profile-policy) and declared only so
	// decodeJSON's DisallowUnknownFields accepts a client echoing the GET
	// response back per openapi.yaml; nothing reads them here.
	var req struct {
		DefaultProfileID *string `json:"default_profile_id"`

		GlobalDefaultProfileID *string `json:"global_default_profile_id"`
		UserOverridesAllowed   *bool   `json:"user_overrides_allowed"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	prefs, err := h.store.UpdateUserProfilePreferences(r.Context(), user.ID, req.DefaultProfileID)
	if errors.Is(err, ErrProfileUnknown) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "default_profile_id must be a user-facing stream profile")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save profile preferences")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"default_profile_id": prefs.DefaultProfileID})
}

func (h *Handler) handleGetProfilePolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := h.store.GetProfilePolicy(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load profile policy")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, policy)
}

func (h *Handler) handlePatchProfilePolicy(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	var req struct {
		GlobalDefaultProfileID *string `json:"global_default_profile_id"`
		UserOverridesAllowed   bool    `json:"user_overrides_allowed"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	policy, err := h.store.UpdateProfilePolicy(r.Context(), req.GlobalDefaultProfileID, req.UserOverridesAllowed, user.ID)
	if errors.Is(err, ErrProfileUnknown) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "global_default_profile_id must be a user-facing stream profile")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save profile policy")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, policy)
}
