package storage

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Handler serves the P5-05 storage endpoints.
type Handler struct {
	mgr     *Manager
	auditor interface {
		Record(context.Context, string, string, string, string, map[string]any) error
	}
}

// NewHandler builds the storage HTTP handler.
func NewHandler(mgr *Manager, auditors ...interface {
	Record(context.Context, string, string, string, string, map[string]any) error
}) *Handler {
	h := &Handler{mgr: mgr}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

func (h *Handler) recordActivity(ctx context.Context, actor, action, targetType, targetID string, details map[string]any) {
	if h.auditor != nil {
		if err := h.auditor.Record(ctx, actor, action, targetType, targetID, details); err != nil {
			slog.Warn("record admin activity failed", "action", action, "err", err)
		}
	}
}

// Register wires the storage routes onto mux.
func (h *Handler) Register(mux httpx.Router, requireAuth func(http.Handler) http.Handler, requireAdmin func(http.Handler) http.Handler) {
	admin := func(next http.Handler) http.Handler { return requireAuth(requireAdmin(next)) }
	mux.Handle("GET /v1/admin/storage/homes", admin(http.HandlerFunc(h.handleList)))
	mux.Handle("DELETE /v1/admin/storage/homes/{id}", admin(http.HandlerFunc(h.handleTombstone)))
	mux.Handle("GET /v1/me/storage", requireAuth(http.HandlerFunc(h.handleMyStorage)))

	// Agent-authenticated backing-store reaping (#175). These are NOT behind the
	// user/admin bearer middleware — they authenticate the calling node-agent by
	// its node_secret (see authAgent). control-api.md §Agent storage GC.
	mux.Handle("GET /v1/agent/storage/gc-pending", http.HandlerFunc(h.handleGCPending))
	mux.Handle("POST /v1/agent/storage/gc-confirm", http.HandlerFunc(h.handleGCConfirm))
}

// authAgent verifies the agent bearer (node_secret) + X-Quasar-Node header and
// resolves the calling host_id. On any failure it writes 401 and returns ok=false.
func (h *Handler) authAgent(w http.ResponseWriter, r *http.Request) (hostID string, ok bool) {
	nodeName := r.Header.Get("X-Quasar-Node")
	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "agent authentication required")
		return "", false
	}
	secret := strings.TrimSpace(strings.TrimPrefix(authz, prefix))
	id, err := h.mgr.AuthAgentHost(r.Context(), nodeName, secret)
	if err != nil {
		if errors.Is(err, ErrAgentAuth) {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "agent authentication failed")
			return "", false
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "agent auth failed")
		return "", false
	}
	return id, true
}

// GET /v1/agent/storage/gc-pending — homes on the caller's host past GC grace.
func (h *Handler) handleGCPending(w http.ResponseWriter, r *http.Request) {
	hostID, ok := h.authAgent(w, r)
	if !ok {
		return
	}
	homes, err := h.mgr.GCPending(r.Context(), hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list pending homes")
		return
	}
	if homes == nil {
		homes = []PendingHome{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"homes": homes})
}

// gcConfirmReq is the POST /v1/agent/storage/gc-confirm body.
type gcConfirmReq struct {
	HomeIDs []string `json:"home_ids"`
}

// POST /v1/agent/storage/gc-confirm — hard-delete the reaped rows on the caller's host.
func (h *Handler) handleGCConfirm(w http.ResponseWriter, r *http.Request) {
	hostID, ok := h.authAgent(w, r)
	if !ok {
		return
	}
	var req gcConfirmReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid request body")
		return
	}
	deleted, err := h.mgr.GCConfirm(r.Context(), hostID, req.HomeIDs)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not confirm gc")
		return
	}
	h.recordActivity(r.Context(), "", "storage.gc.confirm", "host", hostID, map[string]any{"requested": len(req.HomeIDs), "deleted": deleted})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

// homeResp is the JSON shape for one AdminHome row.
//
// username / app_name / host_name are resolved names for the ids beside them.
// They are nullable for the same reason the ids are — a home outlives the app,
// user or host it belonged to, and that row must stay in the listing so its
// bytes stay visible. The ids remain authoritative: the tombstone action and
// any operator debugging key off them, not off the names.
type homeResp struct {
	ID         string     `json:"id"`
	UserID     *string    `json:"user_id"`
	AppID      *string    `json:"app_id"`
	HostID     *string    `json:"host_id"`
	Username   *string    `json:"username"`
	AppName    *string    `json:"app_name"`
	HostName   *string    `json:"host_name"`
	Provider   string     `json:"provider"`
	Ref        string     `json:"ref"`
	BytesUsed  int64      `json:"bytes_used"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt time.Time  `json:"last_used_at"`
	GCAfter    *time.Time `json:"gc_after"`
}

func toHomeResp(h Home) homeResp {
	return homeResp{
		ID:         h.ID,
		UserID:     h.UserID,
		AppID:      h.AppID,
		HostID:     h.HostID,
		Username:   h.Username,
		AppName:    h.AppName,
		HostName:   h.HostName,
		Provider:   h.Provider,
		Ref:        h.Ref,
		BytesUsed:  h.BytesUsed,
		CreatedAt:  h.CreatedAt,
		LastUsedAt: h.LastUsedAt,
		GCAfter:    h.GCAfter,
	}
}

// GET /v1/admin/storage/homes
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := ListHomesOpts{
		UserID: q.Get("user_id"),
		AppID:  q.Get("app_id"),
		Cursor: q.Get("cursor"),
	}
	if v := q.Get("pending_gc"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "pending_gc must be true or false")
			return
		}
		opts.PendingGC = &b
	}

	homes, next, err := h.mgr.ListHomes(r.Context(), opts)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list homes")
		return
	}

	items := make([]homeResp, len(homes))
	for i, hm := range homes {
		items[i] = toHomeResp(hm)
	}
	var nextCursor *string
	if next != "" {
		nextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
	})
}

// DELETE /v1/admin/storage/homes/{id} — tombstone for GC; 202 accepted.
func (h *Handler) handleTombstone(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tombstoned, err := h.mgr.TombstoneHome(r.Context(), id)
	switch {
	case errors.Is(err, ErrHomeNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "home not found")
	case errors.Is(err, ErrHomeInUse):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeHomeInUse,
			"a live session is currently using this home; stop the session first")
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not tombstone home")
	default:
		user, _ := auth.UserFromContext(r.Context())
		// Names the (user, app) whose data is now scheduled for deletion. Which
		// home id it was is already the target; whose data it holds is the fact an
		// operator reading this line back actually needs.
		h.recordActivity(r.Context(), user.ID, "storage.home.tombstone", "storage_home", id,
			map[string]any{"username": tombstoned.Username, "app_name": tombstoned.AppName})
		w.WriteHeader(http.StatusAccepted)
	}
}

// GET /v1/me/storage — caller's own per-app storage summary.
func (h *Handler) handleMyStorage(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	items, err := h.mgr.ListUserStorage(r.Context(), user.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load storage")
		return
	}
	if items == nil {
		items = []MyStorageItem{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
