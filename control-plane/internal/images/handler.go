package images

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Handler serves the app-image admin surface (protocol/control-api.md
// "App-image catalog + management"): catalog reads and the install/uninstall
// /pin/unpin/update actions. Every route is RequireAuth->RequireAdmin,
// server-enforced (CLAUDE.md invariant #6) — the caller composes that gate,
// this handler doesn't re-check role.
type Handler struct {
	store   *Store
	auditor audit.Recorder
}

// NewHandler builds the images HTTP handler.
func NewHandler(store *Store, auditors ...audit.Recorder) *Handler {
	h := &Handler{store: store}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

// actor is the acting admin's id for an audit row.
func actor(r *http.Request) string {
	u, _ := auth.UserFromContext(r.Context())
	return u.ID
}

// Register wires the admin image routes. admin must compose
// RequireAuth→RequireAdmin — mirrors settings.Handler.Register.
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/admin/images", admin(http.HandlerFunc(h.handleGet)))
	mux.Handle("POST /v1/admin/images/sync", admin(http.HandlerFunc(h.handleSync)))
	mux.Handle("POST /v1/admin/images/{id}/install", admin(http.HandlerFunc(h.handleInstall)))
	mux.Handle("DELETE /v1/admin/images/{id}/install", admin(http.HandlerFunc(h.handleUninstall)))
	mux.Handle("POST /v1/admin/images/{id}/pin", admin(http.HandlerFunc(h.handlePin)))
	mux.Handle("DELETE /v1/admin/images/{id}/pin", admin(http.HandlerFunc(h.handleUnpin)))
	mux.Handle("POST /v1/admin/images/{id}/update", admin(http.HandlerFunc(h.handleUpdate)))
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	env, err := h.store.Envelope(r.Context())
	if err != nil {
		slog.Error("get image catalog", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read image catalog")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, env)
}

// handleSync re-fetches, validates, and upserts the cached catalog. A
// fetch/parse/validate failure is never a non-200 here (control-api.md: a
// sync failure must not affect launches) — the client-visible signal is
// sync_error, not an HTTP error status.
func (h *Handler) handleSync(w http.ResponseWriter, r *http.Request) {
	env, err := h.store.Sync(r.Context())
	if err != nil {
		// Only a genuine read failure after a sync attempt reaches here (e.g.
		// DB unavailable) — fetch/parse/upsert failures return an
		// error-carrying Envelope with err == nil.
		slog.Error("sync image catalog", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not sync image catalog")
		return
	}
	// A failed fetch is still a recorded sync — it is a 200 by contract, and the
	// operator needs the attempt in the log to explain a stale catalog. The error
	// text itself stays out: it can carry a registry URL with credentials.
	audit.TryRecord(r.Context(), h.auditor, actor(r), "image.synced", "image", "",
		map[string]any{"images": len(env.Images), "sync_error": env.SyncError != nil})
	if env.SyncError != nil {
		slog.Warn("image catalog sync failed; serving cached catalog", "err", *env.SyncError, "catalog_ref", env.CatalogRef)
	}
	httpx.WriteJSON(w, http.StatusOK, env)
}

// installRequest is POST .../install's optional body.
type installRequest struct {
	Lazy bool `json:"lazy"`
}

// updateResult is POST .../update's 200 body (openapi ImageUpdateResult).
type updateResult struct {
	Applied bool         `json:"applied"`
	Image   CatalogImage `json:"image"`
}

func (h *Handler) handleInstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Body is optional; absent/empty means {lazy:false}, so EOF isn't a
	// validation failure. Size-bounded like every other handler.
	var req installRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid JSON body")
		return
	}

	img, err := h.store.Install(r.Context(), id, req.Lazy)
	if err != nil {
		h.writeActionError(w, "install", id, err)
		return
	}
	audit.TryRecord(r.Context(), h.auditor, actor(r), "image.installed", "image", id,
		map[string]any{"version": img.Version, "lazy": req.Lazy})
	httpx.WriteJSON(w, http.StatusCreated, img)
}

func (h *Handler) handleUninstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.store.Uninstall(r.Context(), id); err != nil {
		h.writeActionError(w, "uninstall", id, err)
		return
	}
	audit.TryRecord(r.Context(), h.auditor, actor(r), "image.uninstalled", "image", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handlePin(w http.ResponseWriter, r *http.Request) { h.setPinned(w, r, true) }

func (h *Handler) handleUnpin(w http.ResponseWriter, r *http.Request) { h.setPinned(w, r, false) }

func (h *Handler) setPinned(w http.ResponseWriter, r *http.Request, pinned bool) {
	id := r.PathValue("id")
	if err := h.store.SetPinned(r.Context(), id, pinned); err != nil {
		h.writeActionError(w, "pin", id, err)
		return
	}
	action := "image.unpinned"
	if pinned {
		action = "image.pinned"
	}
	audit.TryRecord(r.Context(), h.auditor, actor(r), action, "image", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	applied, img, err := h.store.Update(r.Context(), id)
	if err != nil {
		h.writeActionError(w, "update", id, err)
		return
	}
	audit.TryRecord(r.Context(), h.auditor, actor(r), "image.updated", "image", id,
		map[string]any{"applied": applied, "version": img.Version})
	// applied:false is still a 200: "already at the catalog version" is a
	// success, and a UI button must not branch on status code to tell it apart.
	httpx.WriteJSON(w, http.StatusOK, updateResult{Applied: applied, Image: img})
}

// writeActionError maps an action error to the status + discriminator
// control-api.md documents; unrecognized is a 500, never dressed up as a
// client mistake.
func (h *Handler) writeActionError(w http.ResponseWriter, action, id string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such image in the catalog")
	case errors.Is(err, ErrNotInstalled):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotInstalled, "image is not installed")
	case errors.Is(err, ErrAlreadyInstalled):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeAlreadyInstalled,
			"image is already installed; use POST /v1/admin/images/{id}/update to move it to a newer version")
	case errors.Is(err, ErrDigestUnresolved):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeDigestUnresolved,
			"the catalog has no resolved content digest for this image; re-sync and retry")
	case errors.Is(err, ErrContextUnresolved):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeContextUnresolved,
			"the catalog has no resolved commit sha for this template's build context; re-sync and retry")
	case errors.Is(err, ErrPinned):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "image is pinned; unpin it first")
	case errors.Is(err, ErrProviderEnabled):
		var pe *ProviderEnabledError
		name := "This provider's"
		if errors.As(err, &pe) && pe.DisplayName != "" {
			name = pe.DisplayName
		}
		httpx.WriteError(w, http.StatusConflict, httpx.CodeProviderEnabled,
			name+" library discovery is enabled; disable it in Settings first, or the image will be reinstalled automatically.")
	default:
		slog.Error("image action failed", "action", action, "image_id", id, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not apply the image action")
	}
}
