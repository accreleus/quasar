package images

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

var hostUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Handler serves the app-image admin surface (protocol/control-api.md
// "App-image catalog + management"): catalog reads and the install/uninstall
// /pin/unpin/update actions. Every route is RequireAuth->RequireAdmin,
// server-enforced (CLAUDE.md invariant #6) — the caller composes that gate,
// this handler doesn't re-check role.
type Handler struct {
	store   *Store
	auditor audit.Recorder
	retry   interface {
		RetryHostImage(context.Context, string, string) error
	}
	cleanup *CleanupService
}

// NewHandler builds the images HTTP handler.
func NewHandler(store *Store, auditors ...audit.Recorder) *Handler {
	h := &Handler{store: store}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

// SetRetryEnsurer supplies the operator retry action after the Ensurer is
// constructed. Catalog CRUD and retry use the same dispatcher.
func (h *Handler) SetRetryEnsurer(e interface {
	RetryHostImage(context.Context, string, string) error
}) {
	h.retry = e
}

func (h *Handler) SetCleanupService(s *CleanupService) { h.cleanup = s }

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
	mux.Handle("POST /v1/admin/hosts/{id}/images/{image_id}/retry", admin(http.HandlerFunc(h.handleRetry)))
	mux.Handle("GET /v1/admin/hosts/{id}/images/cleanup", admin(http.HandlerFunc(h.handleCleanupPreview)))
	mux.Handle("POST /v1/admin/hosts/{id}/images/cleanup", admin(http.HandlerFunc(h.handleCleanupRequest)))
	mux.Handle("GET /v1/admin/hosts/{id}/images/cleanup/attempts/{attempt_id}", admin(http.HandlerFunc(h.handleCleanupAttempt)))
}

func (h *Handler) handleCleanupAttempt(w http.ResponseWriter, r *http.Request) {
	hostID, attemptID := r.PathValue("id"), r.PathValue("attempt_id")
	if !hostUUID.MatchString(hostID) || !hostUUID.MatchString(attemptID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid host or attempt ID")
		return
	}
	if h.cleanup == nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "image cleanup unavailable")
		return
	}
	attempt, err := h.cleanup.Attempt(r.Context(), hostID, attemptID)
	switch {
	case errors.Is(err, errCleanupNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "cleanup attempt not found")
	case err != nil:
		slog.Error("read image cleanup attempt", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read image cleanup attempt")
	default:
		httpx.WriteJSON(w, http.StatusOK, attempt)
	}
}

func (h *Handler) handleCleanupPreview(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	if !hostUUID.MatchString(hostID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid host ID")
		return
	}
	if h.cleanup == nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "image cleanup unavailable")
		return
	}
	view, err := h.cleanup.Preview(r.Context(), hostID)
	if errors.Is(err, errCleanupNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
	} else if err != nil {
		slog.Error("preview image cleanup", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not preview image cleanup")
	} else {
		httpx.WriteJSON(w, http.StatusOK, view)
	}
}

func (h *Handler) handleCleanupRequest(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	if !hostUUID.MatchString(hostID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid host ID")
		return
	}
	if h.cleanup == nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "image cleanup unavailable")
		return
	}
	var req CleanupRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid cleanup request")
		return
	}
	attempt, status, conflict, err := h.cleanup.Request(r.Context(), hostID, req)
	switch {
	case errors.Is(err, errCleanupValidation):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid cleanup request")
	case errors.Is(err, errCleanupNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host or managed image not found")
	case err != nil:
		slog.Error("request image cleanup", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not request image cleanup")
	case conflict != nil:
		httpx.WriteJSON(w, http.StatusConflict, struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Current *CleanupCandidate `json:"current"`
			Remedy  string            `json:"remedy"`
		}{Error: struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{conflict.Code, conflict.Message}, Current: conflict.Current, Remedy: conflict.Remedy})
	default:
		audit.TryRecord(r.Context(), h.auditor, actor(r), "image.cleanup.requested", "image", req.ImageID,
			map[string]any{"host_id": hostID, "attempt_id": attempt.AttemptID, "version": req.Version})
		httpx.WriteJSON(w, status, attempt)
	}
}

func (h *Handler) handleRetry(w http.ResponseWriter, r *http.Request) {
	hostID, imageID := r.PathValue("id"), r.PathValue("image_id")
	if !hostUUID.MatchString(hostID) || len(imageID) < 1 || len(imageID) > 128 {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid host or image ID")
		return
	}
	if h.retry == nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "image retry unavailable")
		return
	}
	err := h.retry.RetryHostImage(r.Context(), hostID, imageID)
	switch {
	case err == nil:
		audit.TryRecord(r.Context(), h.auditor, actor(r), "image.retry", "image", imageID,
			map[string]any{"host_id": hostID})
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host or image not found")
	case errors.Is(err, ErrNotInstalled):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotInstalled, "image is not installed")
	case errors.Is(err, ErrRetryOffline):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "Host is offline; retry after reconnect")
	case errors.Is(err, ErrRetryNotRequired):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "Image is not required on this host")
	case errors.Is(err, ErrRetryLazy):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "Lazy image downloads at first launch")
	case errors.Is(err, ErrRetryNotFailed):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "Image is not failed for the adopted version")
	case errors.Is(err, ErrRetryRemoving):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "Image cleanup is in progress; retry after it finishes")
	default:
		slog.Error("retry image", "host_id", hostID, "image_id", imageID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not schedule image retry")
	}
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
