package compat

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// codeToolDisabled is control-api.md §Compatibility tools, PUT /default.
const codeToolDisabled = "compat_tool_disabled"

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
	if err := dec.Decode(&req); err != nil || len(req.ToolID) == 0 {
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
	if !sameID(prev, toolID) {
		audit.TryRecord(r.Context(), h.auditor, user.ID, "compat_tool.default.set", "instance", "",
			map[string]any{"tool_id": toolID, "previous_tool_id": prev})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"default_tool_id": toolID})
}

func sameID(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
