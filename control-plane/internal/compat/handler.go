package compat

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// codeToolDisabled is control-api.md §Compatibility tools, PUT /default.
const codeToolDisabled = "compat_tool_disabled"

// appIDRe is the apps_external_id_ck regex; appid 0 is the fleet default's slot and not writable.
var appIDRe = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Handler struct {
	store   *Store
	auditor audit.Recorder
}

func NewHandler(store *Store, auditor audit.Recorder) *Handler {
	return &Handler{store: store, auditor: auditor}
}

// Register wires the admin routes. admin must compose RequireAuth → RequireAdmin.
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/admin/compat-tools", admin(http.HandlerFunc(h.handleList)))
	mux.Handle("PUT /v1/admin/compat-tools/default", admin(http.HandlerFunc(h.handleSetDefault)))
	mux.Handle("GET /v1/admin/compat-tools/mappings", admin(http.HandlerFunc(h.handleListMappings)))
	mux.Handle("PUT /v1/admin/compat-tools/mappings/steam/{external_id}", admin(http.HandlerFunc(h.handleSetMapping)))
	mux.Handle("DELETE /v1/admin/compat-tools/mappings/steam/{external_id}", admin(http.HandlerFunc(h.handleClearMapping)))
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	items, def, err := h.store.List(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list compatibility tools")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "default_tool_id": def})
}

func (h *Handler) handleSetDefault(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	// tool_id must be present: a missing key is not the same as null.
	var req struct {
		ToolID json.RawMessage `json:"tool_id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	// A second Decode must hit EOF: nothing may follow the body.
	if err := dec.Decode(&req); err != nil || len(req.ToolID) == 0 || dec.Decode(&json.RawMessage{}) != io.EOF {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "body must be {\"tool_id\": <uuid>|null}")
		return
	}
	var toolID *string
	if err := json.Unmarshal(req.ToolID, &toolID); err != nil || (toolID != nil && !uuidRe.MatchString(*toolID)) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "tool_id must be a lowercase uuid or null")
		return
	}
	prev, err := h.store.SetDefault(r.Context(), toolID)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such compatibility tool")
		return
	case errors.Is(err, ErrNotUsable):
		httpx.WriteError(w, http.StatusConflict, codeToolDisabled, "the tool is not enabled")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not set the default compatibility tool")
		return
	}
	audit.TryRecord(r.Context(), h.auditor, user.ID, "compat_tool.default.set", "instance", "",
		map[string]any{"tool_id": toolID, "previous_tool_id": prev})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"default_tool_id": toolID})
}

func (h *Handler) handleListMappings(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.Mappings(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list compatibility tool mappings")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) handleSetMapping(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	appID := r.PathValue("external_id")
	if !appIDRe.MatchString(appID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "external_id must be a Steam appid")
		return
	}
	var req struct {
		ToolID *string         `json:"tool_id"`
		Forced json.RawMessage `json:"forced"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	forced := false
	bad := dec.Decode(&req) != nil || dec.Decode(&json.RawMessage{}) != io.EOF ||
		req.ToolID == nil || !uuidRe.MatchString(*req.ToolID) ||
		(req.Forced != nil && json.Unmarshal(req.Forced, &forced) != nil) ||
		string(req.Forced) == "null"
	if bad {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "body must be {\"tool_id\": <uuid>, \"forced\": <bool>?}")
		return
	}
	m, prev, tool, prevTool, err := h.store.SetMapping(r.Context(), appID, *req.ToolID, forced, user.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such compatibility tool")
		return
	case errors.Is(err, ErrNotUsable):
		httpx.WriteError(w, http.StatusConflict, codeToolDisabled, "the tool is not enabled")
		return
	case errors.Is(err, ErrMappingsFull):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "the table already holds the most mappings it may")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not set the compatibility tool mapping")
		return
	}
	details := map[string]any{"tool_id": m.ToolID, "forced": m.Forced, "previous_tool_id": nil, "previous_forced": nil}
	addGE(details, "", tool)
	if prev != nil {
		details["previous_tool_id"], details["previous_forced"] = prev.ToolID, prev.Forced
		addGE(details, "previous_", prevTool)
	}
	audit.TryRecord(r.Context(), h.auditor, user.ID, "compat_tool.mapping.set", "compat_tool_mapping", "steam:"+appID, details)
	httpx.WriteJSON(w, http.StatusOK, m)
}

func (h *Handler) handleClearMapping(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	appID := r.PathValue("external_id")
	if !appIDRe.MatchString(appID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "external_id must be a Steam appid")
		return
	}
	prev, prevTool, err := h.store.ClearMapping(r.Context(), appID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not clear the compatibility tool mapping")
		return
	}
	if prev != nil { // clearing nothing is not a change, so it is not audited
		details := map[string]any{"tool_id": nil, "forced": nil, "previous_tool_id": prev.ToolID, "previous_forced": prev.Forced}
		addGE(details, "previous_", prevTool)
		audit.TryRecord(r.Context(), h.auditor, user.ID, "compat_tool.mapping.clear", "compat_tool_mapping", "steam:"+appID, details)
	}
	w.WriteHeader(http.StatusNoContent)
}

// addGE adds the pinned tag and SHA-512 of a GE tool to an audit row (control-api.md §Audit).
func addGE(details map[string]any, prefix string, t ToolRef) {
	if t.Tag != "" {
		details[prefix+"tag"], details[prefix+"sha512"] = t.Tag, t.SHA512
	}
}
