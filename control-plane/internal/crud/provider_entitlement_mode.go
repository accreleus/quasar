// #465: wizard entitlement-mode wire. EnsureProviderApp (internal/images/
// provider_app.go) grants subject_type='all' unconditionally on create and
// deliberately takes no mode parameter there (see that file) — the wizard's
// "who can see this" step runs after the app already exists, often as a
// separate request. This is that later call.
//
// Not layered on the generic grant/revoke surface (entitlements.go, keyed by
// app id) because the wizard only has the provider name, not an app_id — the
// provider app is created off-thread by the settings PATCH's
// EnsureLibraryProviders side effect. This endpoint resolves provider name to
// app id and applies the whole all/user/none state atomically server-side.
package crud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Mirrors images.EntitlementMode's all|user|none without importing that
// package: crud (admin/API) must not depend on images (installer).
const (
	entitlementModeAll  = "all"
	entitlementModeUser = "user"
	entitlementModeNone = "none"
)

func validEntitlementMode(mode string) bool {
	switch mode {
	case entitlementModeAll, entitlementModeUser, entitlementModeNone:
		return true
	default:
		return false
	}
}

// findProviderAppID resolves a library_provider name to its app id, matching
// EnsureProviderApp's own normalization (lower-cased, trimmed). ErrNotFound
// covers both "no such provider" and "not enabled yet" — indistinguishable
// from the apps table alone.
//
// ORDER BY created_at ASC, id ASC: library_provider has no unique index
// (operators can set it by hand), so this picks a row deterministically if
// more than one app ever carries the same value.
func (s *store) findProviderAppID(ctx context.Context, provider string) (string, error) {
	var appID string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text FROM apps
		WHERE lower(library_provider) = lower($1)
		ORDER BY created_at ASC, id ASC
		LIMIT 1
	`, provider).Scan(&appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("find provider app for %q: %w", provider, err)
	}
	return appID, nil
}

// setProviderEntitlementMode replaces the provider app's entire entitlement
// set with what mode implies: "all" -> one ('all', NULL) row; "user" -> one
// ('user', actorID) row (the acting admin, not a passed-in subject); "none" ->
// zero rows.
//
// Replace, not merge: this is a mode (radio button), not an incremental grant
// — setting "all" after hand-granting three users removes those grants, since
// 'all' subsumes them (control-api.md). Delete-then-insert in one transaction,
// same pattern as setAppLaunchProfiles, so a partial write can't leave the app
// over- or under-entitled.
func (s *store) setProviderEntitlementMode(ctx context.Context, provider, mode string, actorID *string) (appID string, items []Entitlement, err error) {
	appID, err = s.findProviderAppID(ctx, provider)
	if err != nil {
		return "", nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("begin set provider entitlement mode: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	if _, err := tx.Exec(ctx, `DELETE FROM entitlements WHERE app_id::text = $1`, appID); err != nil {
		return "", nil, fmt.Errorf("clear provider app entitlements for %q: %w", provider, err)
	}

	switch mode {
	case entitlementModeAll:
		if _, err := tx.Exec(ctx, `
			INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, granted_by_user)
			VALUES ('all', NULL, $1::uuid, 'admin', $2::uuid)
		`, appID, actorID); err != nil {
			return "", nil, fmt.Errorf("grant all-users entitlement for %q: %w", provider, err)
		}
	case entitlementModeUser:
		if actorID == nil {
			// Unreachable via HTTP (RequireAuth guarantees an identity); guarded so
			// a NULL subject_id fails here, not on the entitlements CHECK constraint.
			return "", nil, fmt.Errorf("set provider entitlement mode to user: no acting admin in context")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, granted_by_user)
			VALUES ('user', $2::uuid, $1::uuid, 'admin', $2::uuid)
		`, appID, actorID); err != nil {
			return "", nil, fmt.Errorf("grant self-only entitlement for %q: %w", provider, err)
		}
	case entitlementModeNone:
		// Nothing to insert — the DELETE above already leaves it unentitled.
	default:
		return "", nil, fmt.Errorf("set provider entitlement mode: invalid mode %q", mode)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", nil, fmt.Errorf("commit provider entitlement mode for %q: %w", provider, err)
	}

	rows, err := s.pool.Query(ctx, entitlementSelect+` WHERE e.app_id::text = $1
		ORDER BY (e.subject_type = 'all') DESC, u.username ASC, e.created_at ASC`, appID)
	if err != nil {
		return "", nil, fmt.Errorf("read back provider entitlements for %q: %w", provider, err)
	}
	items, err = scanEntitlements(rows)
	if err != nil {
		return "", nil, err
	}
	return appID, items, nil
}

// --- handler -------------------------------------------------------------

func (h *Handler) handleSetProviderEntitlementMode(w http.ResponseWriter, r *http.Request) {
	// Lower-cased to match EnsureProviderApp's normalization, echoed back the
	// same way so a caller never sees two spellings of one provider.
	provider := strings.ToLower(strings.TrimSpace(r.PathValue("provider")))
	if provider == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "provider must not be empty")
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validEntitlementMode(req.Mode) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"mode must be all, user, or none")
		return
	}

	actor := actorID(r)
	appID, items, err := h.store.setProviderEntitlementMode(r.Context(), provider, req.Mode, actor)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound,
			"no provider app exists yet for "+provider+" — enable it first, then retry")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not set entitlement mode")
		return
	}

	// Audited like entitlements.go grant/revoke; identifiers + count only,
	// same 4096-byte CHECK discipline.
	h.recordActivity(r, "app.entitlement.set_mode", "app", appID, map[string]any{
		"provider":   provider,
		"mode":       req.Mode,
		"item_count": len(items),
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"entitlement_mode": map[string]any{
			"provider": provider,
			"app_id":   appID,
			"mode":     req.Mode,
			"items":    items,
		},
	})
}
