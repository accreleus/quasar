package console

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Dispatcher is the subset of the agent registry the handler needs: push a
// config_update to a host's agent. agentws.Registry.Send satisfies it. Kept
// as a local interface so console does not import agentws — agentws imports
// console (for the after-registered snapshot + capacity upsert), and that
// direction would cycle if console imported back.
type Dispatcher interface {
	Send(hostID string, v any) error
}

// configUpdateCmd mirrors agentws.ConfigUpdateCmd on the wire (agent-api.md
// `config_update`). Defined locally to avoid the import cycle noted above;
// only the JSON shape is load-bearing since Dispatcher.Send marshals it.
type configUpdateCmd struct {
	Type          string        `json:"type"` // "config_update"
	ConsoleConfig ConsoleConfig `json:"console_config"`
}

// Handler serves the admin console-config endpoints (GET/PATCH,
// control-api.md §"Console mode (CM-01)").
type Handler struct {
	store      *Store
	dispatcher Dispatcher
	auditor    interface {
		Record(context.Context, string, string, string, string, map[string]any) error
	}
}

// NewHandler builds the admin console-config handler. dispatcher pushes the
// resolved console_config to the host's agent on every successful PATCH.
func NewHandler(store *Store, d Dispatcher, auditors ...interface {
	Record(context.Context, string, string, string, string, map[string]any) error
}) *Handler {
	h := &Handler{store: store, dispatcher: d}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

// Register wires the admin console-config routes onto mux. admin composes the
// server-enforced RequireAuth->RequireAdmin gate (never UI-gated).
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/admin/hosts/{id}/console-config", admin(http.HandlerFunc(h.handleGet)))
	mux.Handle("PATCH /v1/admin/hosts/{id}/console-config", admin(http.HandlerFunc(h.handlePatch)))
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hostID := r.PathValue("id")

	exists, err := h.store.HostExists(ctx, hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load host")
		return
	}
	if !exists {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
		return
	}

	sparse, err := h.store.Get(ctx, hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load console config")
		return
	}
	resolved, err := Resolve(sparse)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not resolve console config")
		return
	}
	caps, err := h.store.GetCapabilities(ctx, hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load console capabilities")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"config":       resolved,
		"capabilities": caps,
	})
}

func (h *Handler) handlePatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hostID := r.PathValue("id")

	exists, err := h.store.HostExists(ctx, hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load host")
		return
	}
	if !exists {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
		return
	}

	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid body")
		return
	}

	caps, err := h.store.GetCapabilities(ctx, hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load console capabilities")
		return
	}

	if err := ValidatePatch(patch, caps, func(appID string) (bool, error) {
		return h.store.AppExists(ctx, appID)
	}, func(userID string) (bool, error) {
		return h.store.UserExists(ctx, userID)
	}); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}

	old, err := h.store.Get(ctx, hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load console config")
		return
	}

	// Merge the partial patch onto the stored sparse config: a null value
	// clears the key (reverts to Defaults() on the next Resolve); any other
	// value sets it (control-api.md — `enabled:true` with `audio_output:null`
	// is valid, since audio_output's default is already null).
	merged := map[string]any{}
	for k, v := range old {
		merged[k] = v
	}
	for k, v := range patch {
		if v == nil {
			delete(merged, k)
		} else {
			merged[k] = v
		}
	}
	if err := ValidateOutputSelection(merged, caps); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}

	if err := h.store.Upsert(ctx, hostID, merged, adminUserID(r)); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save console config")
		return
	}

	resolved, err := Resolve(merged)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not resolve console config")
		return
	}

	// Persist + push (control-api.md): the resolved console_config is pushed to
	// the agent immediately via config_update. Fire-and-forget — no ack, not
	// restart-class; a disconnected agent picks it up on its next registered
	// snapshot.
	_ = h.dispatcher.Send(hostID, configUpdateCmd{Type: "config_update", ConsoleConfig: resolved})
	if h.auditor != nil {
		keys := make([]string, 0, len(patch))
		for key := range patch {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		actor := ""
		if id := adminUserID(r); id != nil {
			actor = *id
		}
		if err := h.auditor.Record(ctx, actor, "console.config.update", "host", hostID, map[string]any{"keys": keys}); err != nil {
			slog.Warn("record admin activity failed", "action", "console.config.update", "err", err)
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"config":       resolved,
		"capabilities": caps,
	})
}

// adminUserID returns the authenticated admin's user id for the updated_by
// audit column, or nil if unavailable (column is nullable — never block on it).
func adminUserID(r *http.Request) *string {
	if u, ok := auth.UserFromContext(r.Context()); ok && u.ID != "" {
		id := u.ID
		return &id
	}
	return nil
}
