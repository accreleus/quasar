package session

import (
	"encoding/json"
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
	dialogs, err := h.store.GetSteamDialogs(r.Context(), user.ID)
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
		"steam_dialogs":             dialogs,
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
	// default_profile_id is raw so an absent key is told apart from null.
	var req struct {
		DefaultProfileID json.RawMessage `json:"default_profile_id"`
		SteamDialogs     *string         `json:"steam_dialogs"`

		GlobalDefaultProfileID *string `json:"global_default_profile_id"`
		UserOverridesAllowed   *bool   `json:"user_overrides_allowed"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	var profileID *string
	if len(req.DefaultProfileID) > 0 && json.Unmarshal(req.DefaultProfileID, &profileID) != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "default_profile_id must be a string or null")
		return
	}
	if req.SteamDialogs != nil && !ValidSteamDialogs(*req.SteamDialogs) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "steam_dialogs must be show or dismiss_safe")
		return
	}
	// semantics: control-api.md §Warm Steam, "steam_dialogs": naming only
	// steam_dialogs leaves the profile; otherwise today's rule (absent clears).
	var prefs UserProfilePreferences
	var err error
	if req.SteamDialogs == nil || len(req.DefaultProfileID) > 0 {
		prefs, err = h.store.UpdateUserProfilePreferences(r.Context(), user.ID, profileID)
	}
	if err == nil && req.SteamDialogs != nil {
		prefs, err = h.store.SetSteamDialogs(r.Context(), user.ID, *req.SteamDialogs)
	} else if err == nil {
		prefs.SteamDialogs, err = h.store.GetSteamDialogs(r.Context(), user.ID)
	}
	if errors.Is(err, ErrProfileUnknown) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "default_profile_id must be a user-facing stream profile")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save profile preferences")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"default_profile_id": prefs.DefaultProfileID,
		"steam_dialogs":      prefs.SteamDialogs,
	})
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
