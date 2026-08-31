package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/ratelimit"
)

// ctxKey is the private type for context values set by this package.
type ctxKey int

const (
	userCtxKey ctxKey = iota
	deviceCtxKey
)

// Role values mirror the schema.md users.role CHECK constraint. Authorization is
// server-enforced on these (control-api.md): never trust a client-supplied flag.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// Handler serves the auth endpoints of protocol/control-api.md.
type Handler struct {
	svc     *Service
	limiter *loginLimiter

	// Native-client version handshake (P9-08 / #236). Both empty by default =
	// permissive (no floor advertised, no gating). Set via WithVersionPolicy.
	minClientVersion    string
	latestClientVersion string

	// trustedProxies is the #438 trusted-proxy policy; nil means "key on the
	// direct peer" (today's behaviour). Set via WithTrustedProxies.
	trustedProxies []*net.IPNet

	// auditor records the admin user-management mutations; nil in tests.
	auditor audit.Recorder
}

// NewHandler builds the auth HTTP handler.
func NewHandler(svc *Service, auditors ...audit.Recorder) *Handler {
	// 10 quick attempts, then ~1 every 5s per IP — generous for humans,
	// useless for credential stuffing.
	h := &Handler{svc: svc, limiter: newLoginLimiter(10, 5*time.Second)}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

// WithTrustedProxies configures which direct peers' X-Forwarded-For may be
// believed when keying the login/register limiter (#438). Unset, every user
// behind the hardened Caddy overlay shares one credential-endpoint bucket, so
// a single stuffing run locks the whole instance out of signing in.
func (h *Handler) WithTrustedProxies(nets []*net.IPNet) *Handler {
	h.trustedProxies = nets
	return h
}

// WithVersionPolicy configures the native-client version handshake (P9-08):
// minClientVersion is the hard floor (rejected at login below it),
// latestClientVersion the advisory for the client-side soft-warn. Empty leaves
// the permissive default.
func (h *Handler) WithVersionPolicy(minClientVersion, latestClientVersion string) *Handler {
	h.minClientVersion = minClientVersion
	h.latestClientVersion = latestClientVersion
	return h
}

// Register wires the auth routes onto mux (Go 1.22 method+path patterns).
func (h *Handler) Register(mux httpx.Router) {
	mux.HandleFunc("POST /v1/auth/register", h.handleRegister)
	mux.HandleFunc("POST /v1/auth/login", h.handleLogin)
	mux.HandleFunc("POST /v1/auth/logout", h.handleLogout)
	mux.Handle("GET /v1/me", h.RequireAuth(http.HandlerFunc(h.handleMe)))
	// Self-service change-password (CP-01). RequireAuth; revokes all tokens on
	// success, forcing re-authentication on every device.
	mux.Handle("POST /v1/me/password", h.RequireAuth(http.HandlerFunc(h.handleChangePassword)))

	// Client presentation preferences (session overlay today). RequireAuth and
	// keyed on the caller — no {id} form exists, so these are self-only.
	mux.Handle("GET /v1/me/ui-preferences", h.RequireAuth(http.HandlerFunc(h.handleGetUIPreferences)))
	mux.Handle("PATCH /v1/me/ui-preferences", h.RequireAuth(http.HandlerFunc(h.handlePatchUIPreferences)))

	// Admin user management (P2-11): list + update users. RequireAuth→RequireAdmin.
	admin := func(next http.Handler) http.Handler { return h.RequireAuth(h.RequireAdmin(next)) }
	mux.Handle("GET /v1/users", admin(http.HandlerFunc(h.handleListUsers)))
	mux.Handle("PATCH /v1/users/{id}", admin(http.HandlerFunc(h.handleUpdateUser)))
	// Additive admin-gated extension (control-api.md §Authorization exception
	// class; operator-requested): hard-delete an account. Terminal session
	// history cascades; active sessions / last admin / self are refused.
	mux.Handle("DELETE /v1/users/{id}", admin(http.HandlerFunc(h.handleDeleteUser)))
}

// --- DTOs (exact shapes from control-api.md) ---------------------------------

type userFull struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

type userBrief struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

func toFull(u User) userFull {
	return userFull{ID: u.ID, Email: u.Email, Username: u.Username, Role: u.Role, CreatedAt: u.CreatedAt}
}
func toBrief(u User) userBrief {
	return userBrief{ID: u.ID, Email: u.Email, Username: u.Username, Role: u.Role}
}

// --- handlers ----------------------------------------------------------------

func (h *Handler) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.allow(ratelimit.ClientIP(r, h.trustedProxies)) {
		httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeRateLimited,
			"too many attempts; try again later")
		return
	}
	var req struct {
		Email      string `json:"email"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		InviteCode string `json:"invite_code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	// Gated registration (LP-SEC-01 SEC-03): the persisted registration_mode decides
	// whether an invite is required. role is never taken from the request — it rides the
	// admin-minted invite. All of missing/unknown/expired/exhausted/revoked collapse to
	// one generic invalid_invite (no oracle, decision D2).
	user, err := h.svc.RegisterWithInvite(r.Context(), req.Email, req.Username, req.Password, req.InviteCode)
	switch {
	case errors.As(err, &ErrValidation{}):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	case errors.Is(err, ErrRegistrationClosed):
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeRegistrationClosed, "registration is closed")
		return
	case errors.Is(err, ErrInvalidInvite):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidInvite, "invalid or expired invite")
		return
	case errors.Is(err, ErrConflict):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "email or username already in use")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not create account")
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"user": toFull(user)})
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.allow(ratelimit.ClientIP(r, h.trustedProxies)) {
		httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeRateLimited,
			"too many attempts; try again later")
		return
	}
	var req struct {
		Email     string `json:"email"`
		Password  string `json:"password"`
		DeviceKey string `json:"device_key"`
		// Native-client version handshake (P9-01/P9-08, additive). Optional:
		// web/legacy clients omit them and behave exactly as before.
		ClientVersion   string `json:"client_version"`
		ContractVersion string `json:"contract_version"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	// Version gate (P9-08): reject a native client below the operator-configured
	// hard floor BEFORE authenticating — a too-old client is refused regardless
	// of credentials. A client that sends no version (legacy/web) always passes.
	// contract_version is accepted for forward compatibility; it is not persisted.
	if decideClientVersion(req.ClientVersion, h.minClientVersion) == versionGate {
		h.writeClientTooOld(w)
		return
	}

	// device_key is optional (additive, LP-SEC-01 §B.5): when present the minted token is
	// bound to that device so it can later be revoked per-device.
	tok, err := h.svc.LoginWithDevice(r.Context(), req.Email, req.Password, r.UserAgent(), req.DeviceKey)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeInvalidCredentials, "invalid email or password")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not log in")
		return
	}

	resp := map[string]any{
		"access_token": tok.Plaintext,
		"token_type":   "Bearer",
		"expires_at":   tok.ExpiresAt,
		"user":         toBrief(tok.User),
	}
	// Version advisories (P9-01/P9-08): present only when the operator set a
	// floor/latest; both absent ⇒ no floor advertised (permissive default).
	if h.minClientVersion != "" {
		resp["min_client_version"] = h.minClientVersion
	}
	if h.latestClientVersion != "" {
		resp["latest_client_version"] = h.latestClientVersion
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Logout is idempotent and does not require a valid token: it revokes
	// whatever bearer token is presented (if any) and always returns 204.
	token, _ := bearerToken(r)
	if err := h.svc.Logout(r.Context(), token); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not log out")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"user": toFull(user)})
}

// handleChangePassword lets an authenticated user rotate their own password
// (CP-01). It verifies current_password against the stored hash, applies the
// registration password-strength rule to new_password, rotates the hash, and
// revokes all of the user's active tokens — forcing re-authentication on every
// device (log out everywhere).
//   - 204 on success
//   - 401 invalid_credentials when current_password is wrong
//   - 400 validation_failed when new_password is too short/long
func (h *Handler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	err := h.svc.ChangePassword(r.Context(), user.ID, req.CurrentPassword, req.NewPassword)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.As(err, &ErrValidation{}):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
	case errors.Is(err, ErrInvalidCredentials):
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeInvalidCredentials, "current password is incorrect")
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not change password")
	}
}

// --- admin user management (P2-11) ------------------------------------------

func (h *Handler) handleListUsers(w http.ResponseWriter, r *http.Request) {
	cursor := r.URL.Query().Get("cursor")
	limit := int32(50)
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	users, next, err := h.svc.ListUsers(r.Context(), cursor, limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list users")
		return
	}
	items := make([]adminUserResp, 0, len(users))
	for _, u := range users {
		items = append(items, toAdminUserResp(u))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nullableStr(next)})
}

func (h *Handler) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Role                  *string `json:"role"`
		Disabled              *bool   `json:"disabled"`
		MaxConcurrentSessions *int32  `json:"max_concurrent_sessions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
		return
	}
	if req.Role != nil && *req.Role != RoleUser && *req.Role != RoleAdmin {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "role must be user or admin")
		return
	}
	u, err := h.svc.UpdateUser(r.Context(), id, req.Role, req.Disabled, req.MaxConcurrentSessions)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "user not found")
			return
		}
		if errors.Is(err, ErrLastAdmin) {
			httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "cannot demote the last admin")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not update user")
		return
	}
	// After the error switch, like handleDeleteUser: the feed records what
	// happened, so a refused change must leave no row.
	h.recordUserUpdate(r, id, req.Role, req.Disabled, req.MaxConcurrentSessions)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"user": toAdminUserResp(u)})
}

func (h *Handler) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Self-deletion is refused: the acting admin would orphan their own
	// session/token mid-request, and "delete the last admin" UX-wise should
	// always be a deliberate two-admin operation.
	if caller, ok := UserFromContext(r.Context()); ok && caller.ID == id {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "cannot delete your own account")
		return
	}
	err := h.svc.DeleteUser(r.Context(), id)
	switch {
	case err == nil:
		caller, _ := UserFromContext(r.Context())
		audit.TryRecord(r.Context(), h.auditor, caller.ID, "user.deleted", "user", id, nil)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrUserNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "user not found")
	case errors.Is(err, ErrLastAdmin):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "cannot delete the last admin")
	case errors.Is(err, ErrUserHasActiveSessions):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "user has active sessions — stop them first")
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not delete user")
	}
}

// recordUserUpdate writes one row PER CHANGED FIELD: role, quota and the
// disable switch are separate operator intentions with separate severities, and
// collapsing them into one "user.updated" row would lose which one happened.
// Values only where the value is the point (the new role, the new quota); never
// a password, never an email.
func (h *Handler) recordUserUpdate(r *http.Request, id string, role *string, disabled *bool, quota *int32) {
	caller, _ := UserFromContext(r.Context())
	if role != nil {
		audit.TryRecord(r.Context(), h.auditor, caller.ID, "user.role_changed", "user", id,
			map[string]any{"role": *role})
	}
	if disabled != nil {
		action := "user.enabled"
		if *disabled {
			action = "user.disabled"
		}
		audit.TryRecord(r.Context(), h.auditor, caller.ID, action, "user", id, nil)
	}
	if quota != nil {
		audit.TryRecord(r.Context(), h.auditor, caller.ID, "user.quota_changed", "user", id,
			map[string]any{"max_concurrent_sessions": *quota})
	}
}

type adminUserResp struct {
	ID                    string `json:"id"`
	Email                 string `json:"email"`
	Username              string `json:"username"`
	Role                  string `json:"role"`
	Disabled              bool   `json:"disabled"`
	MaxConcurrentSessions int32  `json:"max_concurrent_sessions"`
	CreatedAt             string `json:"created_at"`
	// Always serialized (null with no devices), so a client can tell "never seen"
	// from "an older server that does not send it".
	LastSeenAt         *string `json:"last_seen_at"`
	ActiveSessionCount int32   `json:"active_session_count"`
}

func toAdminUserResp(u AdminUser) adminUserResp {
	var lastSeen *string
	if u.LastSeenAt != nil {
		s := u.LastSeenAt.Format("2006-01-02T15:04:05Z07:00")
		lastSeen = &s
	}
	return adminUserResp{
		ID:                    u.ID,
		Email:                 u.Email,
		Username:              u.Username,
		Role:                  u.Role,
		Disabled:              u.DisabledAt != nil,
		MaxConcurrentSessions: u.MaxConcurrentSessions,
		CreatedAt:             u.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		LastSeenAt:            lastSeen,
		ActiveSessionCount:    u.ActiveSessionCount,
	}
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- middleware --------------------------------------------------------------

// RequireAuth validates the bearer token and injects the user into the request
// context. Missing/invalid/expired/revoked ⇒ 401.
func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "missing bearer token")
			return
		}
		user, deviceID, err := h.svc.Authenticate(r.Context(), token)
		if err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid or expired token")
			return
		}
		// Client version gate (#380): after authentication (an unauthenticated
		// caller can never probe the floor), before the role check (a too-old
		// admin client gets the actionable 426, not a 403). No header ⇒
		// web/legacy client ⇒ no gate.
		if decideClientVersionHeader(r.Header.Get(clientVersionHeader), h.minClientVersion) == versionGate {
			h.writeClientTooOld(w)
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, user)
		ctx = context.WithValue(ctx, deviceCtxKey, deviceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// writeClientTooOld emits the 426 client_too_old response, shared by the login
// gate (P9-08) and the bearer gate (#380) because control-api.md promises the
// two bodies are byte-identical. The advisory fields ride as top-level
// siblings of the error envelope: a gated client never reaches a 200, so this
// 426 is its only chance to learn which version to update to.
func (h *Handler) writeClientTooOld(w http.ResponseWriter) {
	body := map[string]any{
		"error": map[string]any{
			"code":    httpx.CodeClientTooOld,
			"message": "client version is below the minimum supported version; please update",
		},
		"min_client_version": h.minClientVersion,
	}
	if h.latestClientVersion != "" {
		body["latest_client_version"] = h.latestClientVersion
	}
	httpx.WriteJSON(w, http.StatusUpgradeRequired, body)
}

// RequireAdmin gates an endpoint on the admin role. It reads the user injected
// by RequireAuth, so it MUST be composed inside it: RequireAuth(RequireAdmin(h)).
// A non-admin token gets 403; a missing user (RequireAuth not run) gets 401.
// This is the server-side admin gate mandated by control-api.md — hiding the
// admin UI is never the access control, this is.
func (h *Handler) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFromContext(r.Context())
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
			return
		}
		if user.Role != RoleAdmin {
			httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "admin role required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// UserFromContext returns the authenticated user set by RequireAuth.
func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userCtxKey).(User)
	return u, ok
}

// TokenDeviceIDFromContext returns the user_devices id the bearer token is bound to, set
// by RequireAuth. The bool is true whenever RequireAuth ran; the string is "" when the
// token carries no device binding (a pre-0020 or no-device_key login). Used by
// GET /v1/me/devices to flag the caller's current device (LP-SEC-01 §B.6).
func TokenDeviceIDFromContext(ctx context.Context) (string, bool) {
	d, ok := ctx.Value(deviceCtxKey).(string)
	return d, ok
}

// --- helpers -----------------------------------------------------------------

// bearerToken extracts the token from "Authorization: Bearer <token>".
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	return token, token != ""
}

// decodeJSON decodes the request body, writing a 400 and returning false on
// malformed input.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
		return false
	}
	return true
}
