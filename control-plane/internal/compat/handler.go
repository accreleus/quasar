package compat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Error codes: control-api.md §Compatibility tools.
const (
	codeToolDisabled     = "compat_tool_disabled"
	codeToolInUse        = "compat_tool_in_use"
	codeToolLimit        = "compat_tool_limit"
	codeToolPinConflict  = "compat_tool_pin_conflict"
	codeToolExists       = "compat_tool_exists"
	codeToolUnverifiable = "compat_tool_unverifiable"
	codeUpstream         = "upstream_unavailable"
)

// appIDRe is the apps_external_id_ck regex; appid 0 is the fleet default's slot and not writable.
var appIDRe = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Handler struct {
	store   *Store
	auditor audit.Recorder
	conv    *Converger
	gh      *GitHub
}

// NewHandler: conv may be nil (no install job wired), and then no host has an install state.
func NewHandler(store *Store, auditor audit.Recorder, conv *Converger, gh *GitHub) *Handler {
	return &Handler{store: store, auditor: auditor, conv: conv, gh: gh}
}

// Register wires the admin routes. admin must compose RequireAuth → RequireAdmin.
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/admin/compat-tools", admin(http.HandlerFunc(h.handleList)))
	mux.Handle("POST /v1/admin/compat-tools", admin(http.HandlerFunc(h.handleAdd)))
	mux.Handle("GET /v1/admin/compat-tools/available", admin(http.HandlerFunc(h.handleAvailable)))
	mux.Handle("PUT /v1/admin/compat-tools/default", admin(http.HandlerFunc(h.handleSetDefault)))
	mux.Handle("PATCH /v1/admin/compat-tools/{id}", admin(http.HandlerFunc(h.handlePatch)))
	mux.Handle("DELETE /v1/admin/compat-tools/{id}", admin(http.HandlerFunc(h.handleDelete)))
	mux.Handle("POST /v1/admin/compat-tools/{id}/force-remove", admin(http.HandlerFunc(h.handleForceRemove)))
	mux.Handle("GET /v1/admin/hosts/{id}/compat-tools", admin(http.HandlerFunc(h.handleHostCard)))
	mux.Handle("GET /v1/admin/compat-tools/mappings", admin(http.HandlerFunc(h.handleListMappings)))
	mux.Handle("PUT /v1/admin/compat-tools/mappings/steam/{external_id}", admin(http.HandlerFunc(h.handleSetMapping)))
	mux.Handle("DELETE /v1/admin/compat-tools/mappings/steam/{external_id}", admin(http.HandlerFunc(h.handleClearMapping)))
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	items, def, err := h.listCounted(r.Context())
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

func (h *Handler) handlePatch(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	id := r.PathValue("id")
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Enabled == nil || dec.Decode(&json.RawMessage{}) != io.EOF {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "body must be {\"enabled\": bool}")
		return
	}
	if !uuidRe.MatchString(id) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such compatibility tool")
		return
	}
	res, err := h.store.SetEnabled(r.Context(), id, *req.Enabled)
	var inUse *InUseError
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such compatibility tool")
		return
	case errors.As(err, &inUse):
		httpx.WriteError(w, http.StatusConflict, codeToolInUse, inUse.Error())
		return
	case errors.Is(err, ErrLimit):
		httpx.WriteError(w, http.StatusConflict, codeToolLimit, "the most GE tools allowed are already enabled")
		return
	case errors.Is(err, ErrPinConflict):
		httpx.WriteError(w, http.StatusConflict, codeToolPinConflict, "two pins disagree for this tag")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not change the compatibility tool")
		return
	}
	if res.Changed {
		action := "compat_tool.disable"
		if *req.Enabled {
			action = "compat_tool.enable"
		}
		details := map[string]any{"tag": res.Tag, "sha512": res.SHA512, "host_ids": res.HostIDs}
		if len(res.HostIDs) > 100 {
			details["host_ids"], details["host_ids_truncated"] = res.HostIDs[:100], true
		}
		audit.TryRecord(r.Context(), h.auditor, user.ID, action, "compat_tool", id, details)
		if h.conv != nil {
			h.conv.All(r.Context(), true)
		}
	}
	h.writeTool(w, r, id, http.StatusOK)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such compatibility tool")
		return
	}
	tag, sha, err := h.store.Delete(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such compatibility tool")
	case errors.Is(err, ErrNotDeletable):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "this tool cannot be deleted; disable it instead")
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not delete the compatibility tool")
	default:
		audit.TryRecord(r.Context(), h.auditor, user.ID, "compat_tool.delete", "compat_tool", id,
			map[string]any{"tag": tag, "sha512": sha})
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handler) handleForceRemove(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	id := r.PathValue("id")
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&struct{}{}); err != nil || dec.Decode(&json.RawMessage{}) != io.EOF {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "body must be {}")
		return
	}
	if !uuidRe.MatchString(id) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such compatibility tool")
		return
	}
	res, err := h.store.ForceRemove(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such compatibility tool")
		return
	case errors.Is(err, ErrValve):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "Quasar installs no Valve build; there is nothing to remove")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not force-remove the compatibility tool")
		return
	}
	details := map[string]any{"tag": res.Tag, "sha512": res.SHA512, "mappings_cleared": res.MappingsCleared,
		"default_cleared": res.DefaultCleared, "sessions_stopped": res.SessionsStopped, "host_ids": res.HostIDs}
	if len(res.HostIDs) > 100 {
		details["host_ids"], details["host_ids_truncated"] = res.HostIDs[:100], true
	}
	audit.TryRecord(r.Context(), h.auditor, user.ID, "compat_tool.force_remove", "compat_tool", id, details)
	if h.conv != nil {
		h.conv.All(r.Context(), true)
	}
	items, _, err := h.listCounted(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the compatibility tool")
		return
	}
	for _, it := range items {
		if it.ID == id {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"tool": it, "mappings_cleared": res.MappingsCleared,
				"default_cleared": res.DefaultCleared, "sessions_stopped": res.SessionsStopped})
			return
		}
	}
	httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such compatibility tool")
}

// listCounted is the catalogue with each GE tool's per-host counts.
func (h *Handler) listCounted(ctx context.Context) ([]Tool, *string, error) {
	items, def, err := h.store.List(ctx)
	if err == nil && h.conv != nil {
		err = h.conv.counts(ctx, items)
	}
	return items, def, err
}

func (h *Handler) writeTool(w http.ResponseWriter, r *http.Request, id string, status int) {
	items, _, err := h.listCounted(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the compatibility tool")
		return
	}
	for _, it := range items {
		if it.ID == id {
			httpx.WriteJSON(w, status, it)
			return
		}
	}
	httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such compatibility tool")
}

// handleAdd is add-by-tag: the pin is taken from the release's own checksum file (github.go has
// the threat model), stored disabled, and nothing installs until an admin enables it.
func (h *Handler) handleAdd(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	var req struct {
		Tag *string `json:"tag"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Tag == nil || !tagRe.MatchString(*req.Tag) ||
		dec.Decode(&json.RawMessage{}) != io.EOF {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "body must be {\"tag\": \"GE-Proton<major>-<minor>\"}")
		return
	}
	tag := *req.Tag
	if exists, err := h.store.HasTag(r.Context(), tag); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the catalogue")
		return
	} else if exists {
		httpx.WriteError(w, http.StatusConflict, codeToolExists, "a tool with this tag exists")
		return
	}
	pin, err := h.gh.Resolve(r.Context(), tag)
	var unv *UnverifiableError
	switch {
	case errors.As(err, &unv):
		httpx.WriteError(w, http.StatusUnprocessableEntity, codeToolUnverifiable, unv.Reason)
		return
	case err != nil:
		httpx.WriteError(w, http.StatusServiceUnavailable, codeUpstream, "github.com is unavailable; try again")
		return
	}
	id, err := h.store.Add(r.Context(), pin, user.ID)
	switch {
	case errors.Is(err, ErrExists):
		httpx.WriteError(w, http.StatusConflict, codeToolExists, "a tool with this tag exists")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not add the compatibility tool")
		return
	}
	audit.TryRecord(r.Context(), h.auditor, user.ID, "compat_tool.add", "compat_tool", id, map[string]any{
		"tag": pin.Tag, "asset": pin.AssetName, "sha512": pin.SHA512, "source": "tag", "host_ids": []string{}})
	h.writeTool(w, r, id, http.StatusCreated)
}

// Available is one GET /v1/admin/compat-tools/available item.
type Available struct {
	Tag          string     `json:"tag"`
	Source       string     `json:"source"`
	DisplayName  string     `json:"display_name"`
	PublishedAt  *string    `json:"published_at"`
	ToolID       *string    `json:"tool_id"`
	Enabled      bool       `json:"enabled"`
	PinConflict  bool       `json:"pin_conflict"`
	NameConflict bool       `json:"name_conflict"`
	Hosts        *HostCount `json:"hosts"`
}

// handleAvailable answers 200 whatever GitHub did: a failure is github_latest.available false
// with one of four reasons, and the catalogue state is read live on every request.
func (h *Handler) handleAvailable(w http.ResponseWriter, r *http.Request) {
	latest, err := h.gh.Latest(r.Context())
	if err != nil {
		return // the caller went away; the shared fetch carries on for the others
	}
	items, _, err := h.listCounted(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list compatibility tools")
		return
	}
	byTag := map[string]Tool{}
	for _, t := range items {
		if t.Tag != nil {
			byTag[*t.Tag] = t
		}
	}
	item := func(tag, source string, pub *string) Available {
		a := Available{Tag: tag, Source: source, DisplayName: tag, PublishedAt: pub}
		if t, ok := byTag[tag]; ok {
			id := t.ID
			a.ToolID, a.DisplayName, a.Enabled, a.PinConflict, a.NameConflict, a.Hosts =
				&id, t.DisplayName, t.Enabled, t.PinConflict, t.NameConflict, t.Hosts
		}
		return a
	}
	rec := append([]recommendedTool(nil), recommended...)
	sort.SliceStable(rec, func(i, j int) bool { return tagNewer(rec[i].Tag, rec[j].Tag) })
	out := []Available{}
	for _, t := range rec {
		var pub *string
		if p, ok := latest.recPublished[t.Tag]; ok {
			pub = &p
		}
		out = append(out, item(t.Tag, "recommended", pub))
	}
	gl := map[string]any{"available": latest.reason == "", "reason": nil, "fetched_at": nil}
	if latest.reason != "" {
		gl["reason"] = latest.reason
	} else {
		gl["fetched_at"] = latest.fetchedAt.UTC().Format(time.RFC3339)
		for _, l := range latest.releases {
			out = append(out, item(l.Tag, "github_latest", l.Published))
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out, "github_latest": gl})
}

// tagNewer compares the two numbers of GE-Proton<major>-<minor> numerically.
func tagNewer(a, b string) bool {
	num := func(t string) (int, int) {
		maj, min, _ := strings.Cut(strings.TrimPrefix(t, "GE-Proton"), "-")
		x, _ := strconv.Atoi(maj)
		y, _ := strconv.Atoi(min)
		return x, y
	}
	am, an := num(a)
	bm, bn := num(b)
	return am > bm || (am == bm && an > bn)
}

func (h *Handler) handleHostCard(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such host")
		return
	}
	conv := h.conv
	if conv == nil {
		conv = &Converger{pool: h.store.pool, now: time.Now}
	}
	card, err := conv.HostCard(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such host")
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the host's compatibility tools")
	default:
		httpx.WriteJSON(w, http.StatusOK, card)
	}
}
