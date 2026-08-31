package hostcfg

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Dispatcher is the subset of the agent registry the handler needs: enqueue a
// command (config_update / restart) to a host's agent. agentws.Registry.Send
// satisfies it. Kept as a local interface so hostcfg does not import agentws
// (which imports hostcfg — that would cycle).
type Dispatcher interface {
	Send(hostID string, v any) error
}

// LiveSessionCounter reports how many active sessions a host currently runs,
// used by the restart guard. session.Store satisfies it.
type LiveSessionCounter interface {
	LiveSessions(hostID string) int
}

// configUpdateCmd mirrors agentws.ConfigUpdateCmd on the wire (agent-api.md
// `config_update`). Defined locally to avoid an import cycle; only the JSON
// shape is load-bearing since Dispatcher.Send marshals it.
type configUpdateCmd struct {
	Type     string         `json:"type"` // "config_update"
	Settings map[string]any `json:"settings"`
}

// restartCmd mirrors agentws.RestartCmd on the wire (agent-api.md `restart`).
type restartCmd struct {
	Type string `json:"type"` // "restart"
	ID   string `json:"id"`
}

// Handler serves the admin host-settings endpoints (catalog/get/patch).
type Handler struct {
	store      *Store
	dispatcher Dispatcher
	counter    LiveSessionCounter
	audit      interface {
		Record(context.Context, string, string, string, string, map[string]any) error
	}
}

// NewHandler builds the admin config handler. dispatcher pushes config_update /
// restart to agents; counter backs the restart guard.
func NewHandler(store *Store, d Dispatcher, c LiveSessionCounter, auditors ...interface {
	Record(context.Context, string, string, string, string, map[string]any) error
}) *Handler {
	h := &Handler{store: store, dispatcher: d, counter: c}
	if len(auditors) > 0 {
		h.audit = auditors[0]
	}
	return h
}

// Register wires the admin config routes onto mux (mirrors storage.Handler).
func (h *Handler) Register(mux httpx.Router, requireAuth func(http.Handler) http.Handler, requireAdmin func(http.Handler) http.Handler) {
	admin := func(next http.Handler) http.Handler { return requireAuth(requireAdmin(next)) }
	mux.Handle("GET /v1/admin/config/catalog", admin(http.HandlerFunc(h.handleCatalog)))
	mux.Handle("GET /v1/admin/hosts/{id}/settings", admin(http.HandlerFunc(h.handleGet)))
	mux.Handle("PATCH /v1/admin/hosts/{id}/settings", admin(http.HandlerFunc(h.handlePatch)))
	mux.Handle("POST /v1/admin/hosts/{id}/restart", admin(http.HandlerFunc(h.handleRestart)))
}

func (h *Handler) handleCatalog(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"knobs": Catalog()})
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	overrides, err := h.store.Get(r.Context(), hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load host settings")
		return
	}
	// host-observability: the agent's reported env←overrides overlay, null when
	// never reported (control-api.md GET /v1/admin/hosts/{id}/settings).
	effective, err := h.store.GetEffective(r.Context(), hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load host effective settings")
		return
	}
	pendingRestart, err := h.store.GetPendingRestart(r.Context(), hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load host pending_restart")
		return
	}
	// wizard-v2 §S5: last-reported wire codec set, GET-only (an agent
	// observation, not an override — control-api.md). nil marshals to null, the
	// contract's "never reported"; never flatten it to ["h264"].
	codecs, err := h.store.GetCodecs(r.Context(), hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load host codecs")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"resolved":        Resolve(overrides),
		"overrides":       overrides,
		"effective":       effective,
		"codecs":          codecs,
		"pending_restart": pendingRestart,
	})
}

type patchReq struct {
	Overrides      map[string]any `json:"overrides"`
	RestartConfirm bool           `json:"restart_confirm"`
}

// decision is the pure outcome of a PATCH, computed without HTTP/DB so it can be
// unit-tested directly via decide().
type decision struct {
	// merged is the new full override map to persist.
	merged map[string]any
	// needsRestart is true when a restart-class knob changed.
	needsRestart bool
	// blocked is true when a restart-class change needs confirmation but the host
	// has live sessions and restart_confirm was not set: caller returns 409.
	blocked      bool
	liveSessions int
}

// decide computes the merge + restart-guard outcome for a PATCH. It is pure
// given old overrides, the request, and a live-session count; callers Validate
// req.Overrides first. A null value in req.Overrides clears that key.
func decide(old map[string]any, req patchReq, liveSessions int) decision {
	merged := map[string]any{}
	for k, v := range old {
		merged[k] = v
	}
	for k, v := range req.Overrides {
		if v == nil {
			delete(merged, k)
		} else {
			merged[k] = v
		}
	}
	needsRestart := RestartChange(old, merged)
	d := decision{merged: merged, needsRestart: needsRestart}
	if needsRestart && liveSessionRestartBlocked(liveSessions, req.RestartConfirm) {
		d.blocked = true
		d.liveSessions = liveSessions
	}
	return d
}

// liveSessionRestartBlocked is the restart-class live-session guard shared by
// PATCH .../settings (via decide) and POST .../restart (handleRestart):
// a restart-class action is refused with 409 restart_required unless the
// caller confirms, when the host holds one or more live sessions.
func liveSessionRestartBlocked(liveSessions int, confirm bool) bool {
	return liveSessions > 0 && !confirm
}

func (h *Handler) handlePatch(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	var req patchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid body")
		return
	}
	if err := ValidatePatch(req.Overrides); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}
	// storage-root-constrained: home_root must be the agent-reported root or a
	// subpath (see ValidateHomeRootUnder). A null (clear) never reaches the
	// string case below.
	if v, ok := req.Overrides["home_root"]; ok {
		if s, ok := v.(string); ok {
			eff, err := h.store.GetEffective(r.Context(), hostID)
			if err != nil {
				httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load host effective settings")
				return
			}
			agentRoot := ""
			if eff != nil {
				agentRoot = eff["home_root"]
			}
			if err := ValidateHomeRootUnder(s, agentRoot); err != nil {
				httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
				return
			}
		}
	}

	old, err := h.store.Get(r.Context(), hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load host settings")
		return
	}

	d := decide(old, req, h.counter.LiveSessions(hostID))
	if d.blocked {
		writeConflictBody(w, d.liveSessions)
		return
	}

	resolved := Resolve(d.merged) // display view for the API response; also the cross-knob validation input
	if err := ValidateResolved(resolved); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}

	if err := h.store.Upsert(r.Context(), hostID, d.merged, adminUserID(r)); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save host settings")
		return
	}

	// #194: send the sparse overrides, not the resolved map, so the agent
	// overlays them on its env baseline — a cleared override reverts to env,
	// not the catalog default.
	_ = h.dispatcher.Send(hostID, configUpdateCmd{Type: "config_update", Settings: AgentOverrides(d.merged)})
	// Send before marking pending_restart: if the agent dropped since the
	// online-check, Send fails and pending_restart must not stick true with no
	// restart in flight. The overrides above stay persisted either way.
	if d.needsRestart {
		if err := h.dispatcher.Send(hostID, restartCmd{Type: "restart", ID: "settings-change"}); err != nil {
			slog.Warn("restart dispatch failed; not marking pending_restart", "host_id", hostID, "err", err)
			httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "settings saved but the host agent is unreachable; restart not sent")
			return
		}
		if err := h.store.SetPendingRestart(r.Context(), hostID, true); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save host state")
			return
		}
	}
	if h.audit != nil {
		keys := make([]string, 0, len(req.Overrides))
		for key := range req.Overrides {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if err := h.audit.Record(r.Context(), actorID(r), "host.settings.update", "host", hostID, map[string]any{"keys": keys, "restart_triggered": d.needsRestart}); err != nil {
			slog.Warn("record admin activity failed", "action", "host.settings.update", "err", err)
		}
	}

	// host-observability: same shape as GET otherwise (openapi.yaml HostSettings)
	// — reflects the agent's last-reported effective settings, not this PATCH
	// (that only updates once the agent re-reports after applying config_update).
	effective, err := h.store.GetEffective(r.Context(), hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load host effective settings")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"resolved":          resolved,
		"overrides":         d.merged,
		"effective":         effective,
		"restart_triggered": d.needsRestart,
	})
}

// restartReq is the optional POST /v1/admin/hosts/{id}/restart body
// (openapi.yaml HostRestartRequest). confirm mirrors patchReq.RestartConfirm.
type restartReq struct {
	Confirm bool `json:"confirm"`
}

// handleRestart sends the restart command without changing any override — the
// standalone lever for applying an already-persisted restart-class change
// (control-api.md POST /v1/admin/hosts/{id}/restart). Shares the PATCH
// live-session guard (liveSessionRestartBlocked).
func (h *Handler) handleRestart(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")

	var req restartReq
	if r.Body != nil {
		// The request body is optional (openapi.yaml requestBody required: false) —
		// an empty body is not an error, only a malformed one.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid body")
			return
		}
	}

	status, found, err := h.store.HostStatus(r.Context(), hostID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load host")
		return
	}
	if !found {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
		return
	}
	if status == "offline" {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "host is offline; agent not connected")
		return
	}

	liveSessions := h.counter.LiveSessions(hostID)
	if liveSessionRestartBlocked(liveSessions, req.Confirm) {
		writeConflictBody(w, liveSessions)
		return
	}

	// Send before marking pending_restart — same rule as handlePatch.
	if err := h.dispatcher.Send(hostID, restartCmd{Type: "restart", ID: "admin-restart"}); err != nil {
		slog.Warn("restart dispatch failed; not marking pending_restart", "host_id", hostID, "err", err)
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "host agent is unreachable; restart not sent")
		return
	}
	if err := h.store.SetPendingRestart(r.Context(), hostID, true); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save host state")
		return
	}
	if h.audit != nil {
		if err := h.audit.Record(r.Context(), actorID(r), "host.restart", "host", hostID, map[string]any{"confirmed": req.Confirm, "live_sessions": liveSessions}); err != nil {
			slog.Warn("record admin activity failed", "action", "host.restart", "err", err)
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"restart_triggered": true})
}

// writeConflictBody writes HTTP 409 with the restart_required error shape
// defined in control-api.md: live_sessions is nested inside the error object.
func writeConflictBody(w http.ResponseWriter, liveSessions int) {
	httpx.WriteJSON(w, http.StatusConflict, map[string]any{
		"error": map[string]any{
			"code":          "restart_required",
			"message":       "restart-class change needs confirmation",
			"live_sessions": liveSessions,
		},
	})
}

// adminUserID returns the authenticated admin's user id for the updated_by audit
// column, or nil if unavailable (column is nullable — never block on it).
func adminUserID(r *http.Request) *string {
	if u, ok := auth.UserFromContext(r.Context()); ok && u.ID != "" {
		id := u.ID
		return &id
	}
	return nil
}

func actorID(r *http.Request) string {
	if id := adminUserID(r); id != nil {
		return *id
	}
	return ""
}
