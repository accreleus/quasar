package invites

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Handler serves the admin invite surface (LP-SEC-01 §B.2/B.3):
// POST/GET /v1/admin/invites, DELETE /v1/admin/invites/{id}. All RequireAuth→RequireAdmin.
type Handler struct {
	store   *Store
	baseURL string // configured public base URL; "" -> invite_url omitted (UI composes it)
	auditor audit.Recorder
}

// NewHandler builds the invites HTTP handler. baseURL is the instance's public base URL
// (PUBLIC_BASE_URL); when empty, mint responses omit invite_url and the admin UI composes
// the magic link from code + window.location.origin (contract B.2).
func NewHandler(store *Store, baseURL string, auditors ...audit.Recorder) *Handler {
	h := &Handler{store: store, baseURL: strings.TrimRight(baseURL, "/")}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

// Register wires the admin invite routes. admin must compose RequireAuth→RequireAdmin.
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/admin/invites", admin(http.HandlerFunc(h.handleMint)))
	mux.Handle("GET /v1/admin/invites", admin(http.HandlerFunc(h.handleList)))
	mux.Handle("DELETE /v1/admin/invites/{id}", admin(http.HandlerFunc(h.handleRevoke)))
}

// mintResp is the 201 body: the plaintext code + ready-to-send magic link, returned
// EXACTLY ONCE (never retrievable again). invite_url is omitted when no base URL is set.
type mintResp struct {
	ID        string     `json:"id"`
	Code      string     `json:"code"`
	InviteURL string     `json:"invite_url,omitempty"`
	Role      string     `json:"role"`
	MaxUses   int        `json:"max_uses"`
	UsedCount int        `json:"used_count"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
}

func (h *Handler) handleMint(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	// All fields optional; a bare {} mints a single-use user invite.
	var req struct {
		Role      *string    `json:"role"`
		MaxUses   *int       `json:"max_uses"`
		ExpiresAt *time.Time `json:"expires_at"`
		Note      *string    `json:"note"`
	}
	if r.ContentLength != 0 { // tolerate an empty body ({} or nothing)
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
			return
		}
	}

	p := MintParams{CreatedBy: user.ID, ExpiresAt: req.ExpiresAt}
	if req.Role != nil {
		if *req.Role != "user" && *req.Role != "admin" {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "role must be user or admin")
			return
		}
		p.Role = *req.Role
	}
	if req.MaxUses != nil {
		if *req.MaxUses < 1 {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "max_uses must be >= 1")
			return
		}
		p.MaxUses = *req.MaxUses
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "expires_at must be in the future")
		return
	}
	if req.Note != nil {
		p.Note = *req.Note
	}

	inv, code, err := h.store.Mint(r.Context(), p)
	if err != nil {
		slog.Error("mint invite", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not mint invite")
		return
	}

	resp := mintResp{
		ID:        inv.ID,
		Code:      code,
		Role:      inv.Role,
		MaxUses:   inv.MaxUses,
		UsedCount: inv.UsedCount,
		ExpiresAt: inv.ExpiresAt,
		CreatedAt: inv.CreatedAt,
	}
	if h.baseURL != "" {
		resp.InviteURL = h.baseURL + "/register?invite=" + code
	}
	// Never the code or the url: this row is readable by every admin forever,
	// and the plaintext is shown exactly once, to the minter.
	details := map[string]any{"role": inv.Role, "max_uses": inv.MaxUses}
	if inv.ExpiresAt != nil {
		details["expires_at"] = inv.ExpiresAt.UTC().Format(time.RFC3339)
	}
	audit.TryRecord(r.Context(), h.auditor, user.ID, "invite.minted", "invite", inv.ID, details)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"invite": resp})
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	filter, ok := ParseStateFilter(r.URL.Query().Get("state"))
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidState,
			"state must be all or pending")
		return
	}
	list, err := h.store.List(r.Context(), filter)
	if err != nil {
		slog.Error("list invites", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list invites")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"invites": list})
}

func (h *Handler) handleRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Guard a malformed id before the `$1::uuid` cast so it becomes a clean 400 rather
	// than a generic 500 (error-contract consistency; SEC-review LOW-1).
	if !isUUID(id) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid invite id")
		return
	}
	if err := h.store.Revoke(r.Context(), id); err != nil {
		slog.Error("revoke invite", "err", err, "id", id)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not revoke invite")
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	audit.TryRecord(r.Context(), h.auditor, user.ID, "invite.revoked", "invite", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hex UUID — a cheap guard so a
// malformed path id becomes a clean 400 instead of a DB cast error (500).
func isUUID(s string) bool {
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
