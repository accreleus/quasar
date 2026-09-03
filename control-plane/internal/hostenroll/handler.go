package hostenroll

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Handler serves the admin host-enrollment surface (#12/#96):
// POST/GET /v1/admin/hosts/enrollments, DELETE /v1/admin/hosts/enrollments/{id}.
// All RequireAuth→RequireAdmin. Shape and custody model mirror /v1/admin/invites.
type Handler struct {
	store   *Store
	auditor audit.Recorder
}

func NewHandler(store *Store, auditors ...audit.Recorder) *Handler {
	h := &Handler{store: store}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

// Register wires the admin routes. admin must compose RequireAuth→RequireAdmin.
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/admin/hosts/enrollments", admin(http.HandlerFunc(h.handleMint)))
	mux.Handle("GET /v1/admin/hosts/enrollments", admin(http.HandlerFunc(h.handleList)))
	mux.Handle("DELETE /v1/admin/hosts/enrollments/{id}", admin(http.HandlerFunc(h.handleRevoke)))
}

// mintResp is the 201 body: the plaintext token, returned EXACTLY ONCE. The admin UI
// composes the one-paste enrollment string from it plus the page origin and the
// certificate fingerprint it already has from /v1/admin/access-check — the server does
// not know its own reachable address, so it never composes that string itself.
type mintResp struct {
	ID        string     `json:"id"`
	Token     string     `json:"token"`
	NodeName  *string    `json:"node_name"`
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
	// All fields optional; a bare {} mints a single-use, one-hour, any-node token.
	var req struct {
		NodeName  *string    `json:"node_name"`
		MaxUses   *int       `json:"max_uses"`
		ExpiresAt *time.Time `json:"expires_at"`
		Note      *string    `json:"note"`
	}
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
			return
		}
	}

	p := MintParams{CreatedBy: user.ID, ExpiresAt: req.ExpiresAt}
	if req.NodeName != nil {
		if len(*req.NodeName) > 253 {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "node_name too long")
			return
		}
		p.NodeName = *req.NodeName
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

	e, token, err := h.store.Mint(r.Context(), p)
	if err != nil {
		slog.Error("mint host enrollment", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not mint enrollment token")
		return
	}

	resp := mintResp{
		ID:        e.ID,
		Token:     token,
		NodeName:  e.NodeName,
		MaxUses:   e.MaxUses,
		UsedCount: e.UsedCount,
		ExpiresAt: e.ExpiresAt,
		CreatedAt: e.CreatedAt,
	}
	// Never the token: the audit row is readable by every admin forever.
	details := map[string]any{"max_uses": e.MaxUses}
	if e.NodeName != nil {
		details["node_name"] = *e.NodeName
	}
	if e.ExpiresAt != nil {
		details["expires_at"] = e.ExpiresAt.UTC().Format(time.RFC3339)
	}
	audit.TryRecord(r.Context(), h.auditor, user.ID, "host_enrollment.minted", "host_enrollment", e.ID, details)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"enrollment": resp})
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	var pendingOnly bool
	switch r.URL.Query().Get("state") {
	case "", "all":
	case "pending":
		pendingOnly = true
	default:
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidState, "state must be all or pending")
		return
	}
	list, err := h.store.List(r.Context(), pendingOnly)
	if err != nil {
		slog.Error("list host enrollments", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list enrollment tokens")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"enrollments": list})
}

func (h *Handler) handleRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid enrollment id")
		return
	}
	if err := h.store.Revoke(r.Context(), id); err != nil {
		slog.Error("revoke host enrollment", "err", err, "id", id)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not revoke enrollment token")
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	audit.TryRecord(r.Context(), h.auditor, user.ID, "host_enrollment.revoked", "host_enrollment", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// isUUID: canonical 8-4-4-4-12 hex, so a malformed path id is a clean 400 rather than a
// `$1::uuid` cast error surfacing as 500.
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
