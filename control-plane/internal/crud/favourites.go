package crud

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// user_app_favourites (schema.md): row presence is the fact, no boolean column,
// no soft delete; the composite PK is both uniqueness and idempotency key.

// appVisibleForFavourite reports whether id exists and is enabled-visible (an
// admin sees any app, a non-admin only enabled ones). Deliberately does not
// check entitlement: that yields a 403, not a 404, and must stay a separate
// check in handleFavouriteApp — folding it in here would drop the 403.
func (s *store) appVisibleForFavourite(ctx context.Context, id string, isAdmin bool) (bool, error) {
	q := `SELECT EXISTS(SELECT 1 FROM apps WHERE id::text = $1`
	if !isAdmin {
		q += ` AND enabled = true`
	}
	q += `)`
	var ok bool
	if err := s.pool.QueryRow(ctx, q, id).Scan(&ok); err != nil {
		return false, fmt.Errorf("check app visibility: %w", err)
	}
	return ok, nil
}

// favouriteApp records userID favouriting appID. Idempotent via ON CONFLICT DO
// NOTHING; a repeat call does not re-stamp created_at.
func (s *store) favouriteApp(ctx context.Context, userID, appID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO user_app_favourites (user_id, app_id)
		VALUES ($1::uuid, $2::uuid)
		ON CONFLICT (user_id, app_id) DO NOTHING
	`, userID, appID); err != nil {
		return fmt.Errorf("favourite app: %w", err)
	}
	return nil
}

// unfavouriteApp is unconditional, never ErrNotFound: semantics control-api.md
// §Favourites (idempotent, never 404s).
func (s *store) unfavouriteApp(ctx context.Context, userID, appID string) error {
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM user_app_favourites WHERE user_id = $1::uuid AND app_id = $2::uuid
	`, userID, appID); err != nil {
		return fmt.Errorf("unfavourite app: %w", err)
	}
	return nil
}

// PUT/DELETE /v1/me/favourites/{app_id}: RequireAuth only, not admin. Owner is
// always the bearer identity, so cross-user isolation is structural.

// handleFavouriteApp implements PUT /v1/me/favourites/{app_id}.
func (h *Handler) handleFavouriteApp(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	appID := r.PathValue("app_id")
	if !isValidUUID(appID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "app_id must be a UUID")
		return
	}

	// Same visibility rule as GET /v1/apps/{id}, else a 204 would confirm the
	// existence of an app the caller can't read.
	visible, err := h.store.appVisibleForFavourite(r.Context(), appID, user.Role == auth.RoleAdmin)
	if err != nil {
		slog.Warn("favourite app visibility check failed", "user_id", user.ID, "app_id", appID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not favourite app")
		return
	}
	if !visible {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
		return
	}

	// 403 after the 404, not folded into it: this is a write (toggle on a tile),
	// so "not entitled" must be distinguishable from "not found" (control-api.md
	// §6.3). Applied to every role including admin — /v1/me/* is the user surface.
	entitled, err := h.store.entitledToApp(r.Context(), user.ID, appID)
	if err != nil {
		slog.Warn("favourite entitlement check failed", "user_id", user.ID, "app_id", appID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not favourite app")
		return
	}
	if !entitled {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "you are not entitled to this app")
		return
	}

	if err := h.store.favouriteApp(r.Context(), user.ID, appID); err != nil {
		slog.Warn("favourite app failed", "user_id", user.ID, "app_id", appID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not favourite app")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUnfavouriteApp implements DELETE /v1/me/favourites/{app_id}: idempotent,
// never 404s for a well-formed UUID.
func (h *Handler) handleUnfavouriteApp(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	appID := r.PathValue("app_id")
	if !isValidUUID(appID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "app_id must be a UUID")
		return
	}

	if err := h.store.unfavouriteApp(r.Context(), user.ID, appID); err != nil {
		slog.Warn("unfavourite app failed", "user_id", user.ID, "app_id", appID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not unfavourite app")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isValidUUID guards a malformed path id into a 400 instead of a Postgres cast
// error (500). Mirrors internal/devices/handler.go's isUUID.
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
