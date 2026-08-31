package settings

import (
	"log/slog"
	"net/http"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/origins"
)

// Handler serves the admin instance-settings surface (LP-SEC-01 §B.1a):
// GET / PATCH /v1/admin/settings. Both are RequireAuth→RequireAdmin.
type Handler struct {
	store *Store

	// Called after a PATCH flips library_discovery_enabled false→true, and only
	// then (wired in app.go to the discovery janitor's Nudge). A plain func
	// field, not an import — internal/library imports settings, so importing
	// back would be a cycle; main is where the two meet. Nil is fine (tests,
	// off-path), and whatever is assigned must not block: it runs inline on the
	// request goroutine holding an admin's PATCH open.
	OnLibraryDiscoveryEnabled func()

	// Called on the same false→true transition: the P5 side effect that
	// auto-installs every library-provider catalog image (control-api.md
	// §"P5 side effect"). Same contract — plain func field, nil is fine, must
	// not block (app.go runs the install pass on its own goroutine).
	EnsureLibraryProviders func()

	// The mirror: called on true→false only. In app.go both fields feed the
	// same serialized level-triggered reconciler — this handler reports the
	// edge, the reconciler reads the level. Off means provider apps are
	// suspended, never deleted (#456), and the image is not uninstalled (that
	// stays an explicit admin action). Same non-blocking contract.
	DisableLibraryProviders func()

	auditor audit.Recorder
}

// NewHandler builds the settings HTTP handler.
func NewHandler(store *Store, auditors ...audit.Recorder) *Handler {
	h := &Handler{store: store}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

// Register wires the admin settings routes. admin must compose
// RequireAuth→RequireAdmin (server-enforced gate; UI hiding is never the control).
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/admin/settings", admin(http.HandlerFunc(h.handleGet)))
	mux.Handle("PATCH /v1/admin/settings", admin(http.HandlerFunc(h.handlePatch)))
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	st, err := h.store.Get(r.Context())
	if err != nil {
		slog.Error("get instance settings", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read settings")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"settings": st})
}

func (h *Handler) handlePatch(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	var req struct {
		// Every field is a pointer: absence means unchanged, never a zero-value
		// flip (a plain bool would silently switch discovery off on every
		// unrelated save). Setting a library field does not lift an env
		// override — GET /v1/admin/library/status reports the resolved value.
		RegistrationMode                  *string `json:"registration_mode"`
		StorageProvider                   *string `json:"storage_provider"`
		LibraryDiscoveryEnabled           *bool   `json:"library_discovery_enabled"`
		LibraryDiscoveryIntervalMinutes   *int    `json:"library_discovery_interval_minutes"`
		LibraryDiscoveryAppDetailsEnabled *bool   `json:"library_discovery_appdetails_enabled"`
		MicCaptureEnabled                 *bool   `json:"mic_capture_enabled"`
		ImageUpdatePolicy                 *string `json:"image_update_policy"`
		// Pointer to a slice: an explicit [] means "clear the list", which a
		// plain []string could not tell apart from absence.
		AllowedOrigins *[]string `json:"allowed_origins"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// Validate every provided field up front so a bad value never applies a partial
	// change (400 before any write).
	if req.RegistrationMode != nil && !ValidMode(*req.RegistrationMode) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"registration_mode must be closed, invite_only, or open")
		return
	}
	if req.StorageProvider != nil && !ValidStorageProvider(*req.StorageProvider) {
		// "volume" gets its own message (#473): the admin needs the why and
		// the replacement, not just "invalid".
		if IsRemovedVolumeProvider(*req.StorageProvider) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, ErrVolumeDriverRemovedMsg)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"storage_provider must be auto or local")
		return
	}
	if req.LibraryDiscoveryIntervalMinutes != nil && !ValidLibraryDiscoveryIntervalMinutes(*req.LibraryDiscoveryIntervalMinutes) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"library_discovery_interval_minutes must be between 15 and 10080")
		return
	}
	if req.ImageUpdatePolicy != nil && !ValidImageUpdatePolicy(*req.ImageUpdatePolicy) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"image_update_policy must be manual, notify, or auto")
		return
	}
	// The allow-list stores the canonical form the socket later compares
	// against, never the raw text — "what an admin saved" and "what /v1/signal
	// enforces" cannot diverge. "*" and malformed entries are refused.
	patch := Patch{
		RegistrationMode:                  req.RegistrationMode,
		StorageProvider:                   req.StorageProvider,
		LibraryDiscoveryEnabled:           req.LibraryDiscoveryEnabled,
		LibraryDiscoveryIntervalMinutes:   req.LibraryDiscoveryIntervalMinutes,
		LibraryDiscoveryAppDetailsEnabled: req.LibraryDiscoveryAppDetailsEnabled,
		MicCaptureEnabled:                 req.MicCaptureEnabled,
		ImageUpdatePolicy:                 req.ImageUpdatePolicy,
	}
	if req.AllowedOrigins != nil {
		normalized, err := origins.ValidateList(*req.AllowedOrigins)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
			return
		}
		patch.AllowedOrigins = &normalized
	}
	if patch.Empty() {
		// Nothing to change — return current state (partial PATCH with no known field).
		h.handleGet(w, r)
		return
	}

	// One transaction for the whole PATCH — it must land completely or not at
	// all (see Store.Apply).
	st, wasDiscoveryEnabled, err := h.store.Apply(r.Context(), patch, user.ID)
	if err != nil {
		slog.Error("update instance settings", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not update settings")
		return
	}

	// CHANGED KEY NAMES ONLY, never values. allowed_origins is operator text and
	// the settings surface will grow; a log that records values is one careless
	// field away from being where a secret lands.
	audit.TryRecord(r.Context(), h.auditor, user.ID, "instance.settings.updated", "instance", "",
		map[string]any{"keys": patch.ChangedKeys()})

	// Side effects run only after commit, and only on the transition, not the
	// value: a true→true re-save must not re-walk every home.
	if req.LibraryDiscoveryEnabled != nil {
		switch {
		case *req.LibraryDiscoveryEnabled && !wasDiscoveryEnabled:
			if h.OnLibraryDiscoveryEnabled != nil {
				h.OnLibraryDiscoveryEnabled()
			}
			if h.EnsureLibraryProviders != nil {
				h.EnsureLibraryProviders()
			}
		case !*req.LibraryDiscoveryEnabled && wasDiscoveryEnabled:
			if h.DisableLibraryProviders != nil {
				h.DisableLibraryProviders()
			}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"settings": st})
}
