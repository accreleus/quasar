// Admin entitlement surface (spec §6.6) plus the store primitives the filter
// and app-create default use. An entitlement is an authorization fact, not
// library metadata: every route is under the shared RequireAuth->RequireAdmin
// chain (handler.go Register) with no inline role check (CLAUDE.md invariant
// #6), and neither the filter (entitledSQL, store.go) nor the launch check
// (internal/session/scheduler.go) has an admin arm — an admin who wants access
// grants themselves the entitlement through this surface.
package crud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// ErrEntitlementExists: the (subject, app) pair already holds an entitlement
// (migration 0043's partial unique indexes). Mapped to 409, not swallowed.
var ErrEntitlementExists = errors.New("entitlement already exists")

// Entitlement is the domain/wire view of one entitlements row.
type Entitlement struct {
	ID string `json:"id"`
	// SubjectType is 'user' or 'all'; the column shape leaves room for a future
	// 'group', not shipped.
	SubjectType string `json:"subject_type"`
	// SubjectID is null for 'all'; the 0043 CHECK makes any other combination
	// unstorable.
	SubjectID *string `json:"subject_id"`
	// Read-side convenience so the admin UI needn't fan out to GET /v1/users.
	// Null for 'all'; the LEFT JOIN can't miss since subject_id FKs ON DELETE
	// CASCADE.
	SubjectUsername *string `json:"subject_username"`
	AppID           string  `json:"app_id"`
	AppName         string  `json:"app_name"`
	// GrantedBy is 'admin' | 'provider' | 'migration'. 'provider' is unwritten
	// today (Phase 4) but revokeEntitlement already handles it (keys on id only).
	GrantedBy     string    `json:"granted_by"`
	GrantedByUser *string   `json:"granted_by_user"`
	SourceRef     string    `json:"source_ref"`
	CreatedAt     time.Time `json:"created_at"`
}

// --- store -------------------------------------------------------------------

// entitlementSelect is the shared projection for both list directions.
const entitlementSelect = `
	SELECT e.id::text, e.subject_type, e.subject_id::text, u.username,
	       e.app_id::text, a.name,
	       e.granted_by, e.granted_by_user::text, e.source_ref, e.created_at
	FROM entitlements e
	JOIN apps a ON a.id = e.app_id
	LEFT JOIN users u ON u.id = e.subject_id`

func scanEntitlements(rows pgx.Rows) ([]Entitlement, error) {
	defer rows.Close()
	// Non-nil zero value: contract requires [] not null for an empty result.
	out := []Entitlement{}
	for rows.Next() {
		var e Entitlement
		if err := rows.Scan(&e.ID, &e.SubjectType, &e.SubjectID, &e.SubjectUsername,
			&e.AppID, &e.AppName,
			&e.GrantedBy, &e.GrantedByUser, &e.SourceRef, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan entitlement: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// listAppEntitlements returns every entitlement on one app. ErrNotFound when
// the app doesn't exist, so "no grants" is distinguishable from a typo'd id.
// The existence check is not entitlement-filtered: the admin must be able to
// inspect an app nobody, including themselves, is entitled to.
func (s *store) listAppEntitlements(ctx context.Context, appID string) ([]Entitlement, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM apps WHERE id::text = $1)`, appID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check app: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, entitlementSelect+`
		WHERE e.app_id::text = $1
		-- 'all' first, then users alphabetically: the admin UI's "Visible to"
		-- control reads top-down and "Everyone" is the fact that subsumes the rest.
		ORDER BY (e.subject_type = 'all') DESC, u.username ASC, e.created_at ASC`, appID)
	if err != nil {
		return nil, fmt.Errorf("list app entitlements: %w", err)
	}
	return scanEntitlements(rows)
}

// listUserEntitlements returns only personal grants, not the 'all' rows a user
// also benefits from — folding those in would offer a Revoke button that
// affects everyone. GET /v1/apps answers the effective library; this answers
// "what was granted to this person" (§6.6).
func (s *store) listUserEntitlements(ctx context.Context, userID string) ([]Entitlement, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id::text = $1)`, userID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check user: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, entitlementSelect+`
		WHERE e.subject_type = 'user' AND e.subject_id::text = $1
		ORDER BY a.name ASC, e.created_at ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list user entitlements: %w", err)
	}
	return scanEntitlements(rows)
}

// grantEntitlement writes one ('user'|'all') entitlement with granted_by='admin'.
// Validates app and (for 'user') subject before inserting so a bad id is a 400
// not a foreign-key 500; the FK still guards a concurrent delete.
func (s *store) grantEntitlement(ctx context.Context, appID, subjectType string, subjectID, actorID *string) (Entitlement, error) {
	var appExists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM apps WHERE id::text = $1)`, appID).Scan(&appExists); err != nil {
		return Entitlement{}, fmt.Errorf("check app: %w", err)
	}
	if !appExists {
		return Entitlement{}, ErrNotFound
	}
	if subjectType == "user" {
		var userExists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE id::text = $1)`, *subjectID).Scan(&userExists); err != nil {
			return Entitlement{}, fmt.Errorf("check subject: %w", err)
		}
		if !userExists {
			return Entitlement{}, ErrNotFound
		}
	}

	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, granted_by_user)
		VALUES ($1, $2::uuid, $3::uuid, 'admin', $4::uuid)
		RETURNING id::text`, subjectType, subjectID, appID, actorID).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return Entitlement{}, ErrEntitlementExists
		}
		return Entitlement{}, fmt.Errorf("grant entitlement: %w", err)
	}

	rows, qerr := s.pool.Query(ctx, entitlementSelect+` WHERE e.id::text = $1`, id)
	if qerr != nil {
		return Entitlement{}, fmt.Errorf("read back entitlement: %w", qerr)
	}
	list, serr := scanEntitlements(rows)
	if serr != nil {
		return Entitlement{}, serr
	}
	if len(list) != 1 {
		return Entitlement{}, fmt.Errorf("read back entitlement: %d rows", len(list))
	}
	return list[0], nil
}

// revokeEntitlement deletes one entitlement by id, scoped to the app in the
// path so a stale pair is a 404 not a cross-app delete. Does not filter on
// granted_by: a 'provider' row (Phase 4) must be revocable through this path
// too.
func (s *store) revokeEntitlement(ctx context.Context, appID, entitlementID string) (subjectType string, subjectID *string, grantedBy string, err error) {
	err = s.pool.QueryRow(ctx, `
		DELETE FROM entitlements
		WHERE id::text = $1 AND app_id::text = $2
		RETURNING subject_type, subject_id::text, granted_by`, entitlementID, appID).
		Scan(&subjectType, &subjectID, &grantedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, "", ErrNotFound
	}
	if err != nil {
		return "", nil, "", fmt.Errorf("revoke entitlement: %w", err)
	}
	return subjectType, subjectID, grantedBy, nil
}

// grantAllOnCreate writes the ('all', granted_by='admin') entitlement that
// makes a newly created app visible (§6.4) — without it every new app is
// invisible by default. ON CONFLICT DO NOTHING guards entitlements_all_uk if
// create ever gains a retry.
func (s *store) grantAllOnCreate(ctx context.Context, appID string, actorID *string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, granted_by_user)
		VALUES ('all', NULL, $1::uuid, 'admin', $2::uuid)
		ON CONFLICT DO NOTHING`, appID, actorID); err != nil {
		return fmt.Errorf("grant default entitlement: %w", err)
	}
	return nil
}

// entitledToApp reports whether userID may see/launch appID. Copy of
// entitledSQL (store.go, definition of record); kept in sync by hand since the
// predicate can't cross the crud/session package boundary.
//
// Casts the param, not the column (`e.app_id = $1::uuid`): casting the column
// would make entitlements_all_uk/entitlements_user_uk unusable and force a
// sequential scan. appID is isValidUUID-checked at both call sites first.
func (s *store) entitledToApp(ctx context.Context, userID, appID string) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM entitlements e
			WHERE e.app_id = $1::uuid
			  AND (e.subject_type = 'all'
			       OR (e.subject_type = 'user' AND e.subject_id = $2::uuid))
		)`, appID, userID).Scan(&ok); err != nil {
		return false, fmt.Errorf("check entitlement: %w", err)
	}
	return ok, nil
}

// --- handlers ----------------------------------------------------------------
// Admin role is guaranteed by RequireAdmin (Register) for all four; none
// re-checks it.

func (h *Handler) handleListAppEntitlements(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidUUID(id) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "app id must be a UUID")
		return
	}
	items, err := h.store.listAppEntitlements(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list entitlements")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) handleListUserEntitlements(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidUUID(id) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "user id must be a UUID")
		return
	}
	items, err := h.store.listUserEntitlements(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "user not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list entitlements")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) handleGrantEntitlement(w http.ResponseWriter, r *http.Request) {
	appID := r.PathValue("id")
	if !isValidUUID(appID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "app id must be a UUID")
		return
	}
	var req struct {
		SubjectType string  `json:"subject_type"`
		SubjectID   *string `json:"subject_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// The DB CHECK is the durable guard; this turns a bad request into a 400.
	switch req.SubjectType {
	case "all":
		if req.SubjectID != nil && *req.SubjectID != "" {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
				"subject_id must be omitted when subject_type is all")
			return
		}
		req.SubjectID = nil
	case "user":
		if req.SubjectID == nil || !isValidUUID(*req.SubjectID) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
				"subject_id must be a user UUID when subject_type is user")
			return
		}
	default:
		// A future 'group' widens this switch and the 0043 CHECK together.
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"subject_type must be user or all")
		return
	}

	actor := actorID(r)
	ent, err := h.store.grantEntitlement(r.Context(), appID, req.SubjectType, req.SubjectID, actor)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app or subject not found")
		return
	case errors.Is(err, ErrEntitlementExists):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "that subject already holds an entitlement for this app")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not grant entitlement")
		return
	}

	// Audited: identifiers and subject shape only, inside admin_activity's
	// 4096-byte details CHECK (migration 0028).
	h.recordActivity(r, "app.entitlement.grant", "app", appID, map[string]any{
		"entitlement_id": ent.ID,
		"subject_type":   ent.SubjectType,
		"subject_id":     derefOr(ent.SubjectID, ""),
	})
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"entitlement": ent})
}

func (h *Handler) handleRevokeEntitlement(w http.ResponseWriter, r *http.Request) {
	appID := r.PathValue("id")
	entID := r.PathValue("entitlement_id")
	if !isValidUUID(appID) || !isValidUUID(entID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "ids must be UUIDs")
		return
	}
	subjectType, subjectID, grantedBy, err := h.store.revokeEntitlement(r.Context(), appID, entID)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "entitlement not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not revoke entitlement")
		return
	}
	// granted_by is recorded because revoking a 'provider' row (Phase 4) can be
	// re-granted by the next sync; the audit row explains that.
	h.recordActivity(r, "app.entitlement.revoke", "app", appID, map[string]any{
		"entitlement_id": entID,
		"subject_type":   subjectType,
		"subject_id":     derefOr(subjectID, ""),
		"granted_by":     grantedBy,
	})
	w.WriteHeader(http.StatusNoContent)
}

func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// actorID returns the acting admin's user id, or nil (never nil on an admin
// route) so granted_by_user stores NULL rather than failing a uuid cast.
func actorID(r *http.Request) *string {
	user, ok := auth.UserFromContext(r.Context())
	if !ok || user.ID == "" {
		return nil
	}
	id := user.ID
	return &id
}
