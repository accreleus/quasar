package crud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// RegistryChecker avoids an agentws import here (agentws imports session, which
// imports crud — a cycle). Concrete type is *agentws.Registry, set via SetRegistry.
type RegistryChecker interface {
	IsConnected(hostID string) bool
}

// Handler serves the CRUD endpoints (P1-3: admin surface + public library read).
type Handler struct {
	store    *store
	registry RegistryChecker // nil until SetRegistry is called
	auditor  interface {
		Record(context.Context, string, string, string, string, map[string]any) error
	}
}

// NewHandler builds the CRUD handler.
func NewHandler(pool *pgxpool.Pool, auditors ...interface {
	Record(context.Context, string, string, string, string, map[string]any) error
}) *Handler {
	h := &Handler{store: &store{pool: pool}}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

// recordActivity writes one admin_activity row. details must carry field NAMES
// and identifiers, never field values — keeps it under migration 0028's
// 4096-byte CHECK and off-limits to request-body secrets.
func (h *Handler) recordActivity(r *http.Request, action, targetType, targetID string, details map[string]any) {
	if h.auditor == nil {
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	if err := h.auditor.Record(r.Context(), user.ID, action, targetType, targetID, details); err != nil {
		slog.Warn("record admin activity failed", "action", action, "err", err)
	}
}

// SetRegistry lets DELETE /v1/hosts/{id} check live connection state, not just
// the DB status column. Call once after NewHandler, before RegisterRoutes.
func (h *Handler) SetRegistry(r RegistryChecker) {
	h.registry = r
}

// Register wires the CRUD routes onto mux. requireAdmin enforces the admin role
// server-side — the actual access gate, never UI route-hiding
// (control-api.md §Authorization).
func (h *Handler) Register(mux httpx.Router, requireAuth, requireAdmin func(http.Handler) http.Handler) {
	admin := func(next http.Handler) http.Handler { return requireAuth(requireAdmin(next)) }

	// GET /v1/apps requires auth (UI-P1 amendment, was an info-disclosure gap) —
	// also needed to resolve the per-item `favourite`.
	mux.Handle("GET /v1/apps", requireAuth(http.HandlerFunc(h.handleListApps)))
	mux.Handle("GET /v1/apps/{id}", requireAuth(http.HandlerFunc(h.handleGetApp)))

	// Admin app list — all apps including disabled, with runtime_spec (P2-08).
	mux.Handle("GET /v1/admin/apps", admin(http.HandlerFunc(h.handleAdminListApps)))

	mux.Handle("POST /v1/apps", admin(http.HandlerFunc(h.handleCreateApp)))
	mux.Handle("PATCH /v1/apps/{id}", admin(http.HandlerFunc(h.handleUpdateApp)))
	mux.Handle("DELETE /v1/apps/{id}", admin(http.HandlerFunc(h.handleDeleteApp)))
	mux.Handle("GET /v1/hosts", admin(http.HandlerFunc(h.handleListHosts)))
	mux.Handle("GET /v1/hosts/{id}", admin(http.HandlerFunc(h.handleGetHost)))
	mux.Handle("DELETE /v1/hosts/{id}", admin(http.HandlerFunc(h.handleDeleteHost)))

	// Runtime presets (UI-P3). The admin UI's disabled Delete on an in-use preset
	// is UX only; the 409 from DELETE is the enforcement (presets.go).
	mux.Handle("GET /v1/admin/runtime-presets", admin(http.HandlerFunc(h.handleListRuntimePresets)))
	mux.Handle("POST /v1/admin/runtime-presets", admin(http.HandlerFunc(h.handleCreateRuntimePreset)))
	mux.Handle("GET /v1/admin/runtime-presets/{id}", admin(http.HandlerFunc(h.handleGetRuntimePreset)))
	mux.Handle("PATCH /v1/admin/runtime-presets/{id}", admin(http.HandlerFunc(h.handleUpdateRuntimePreset)))
	mux.Handle("DELETE /v1/admin/runtime-presets/{id}", admin(http.HandlerFunc(h.handleDeleteRuntimePreset)))

	// Entitlements (Phase 2, spec §6.6) are the only way to widen visibility, which
	// keeps GET /v1/apps and POST /v1/sessions filtered for admins too (§6.5): an
	// admin grants access here, and the grant is audited. See entitlements.go.
	mux.Handle("GET /v1/admin/apps/{id}/entitlements", admin(http.HandlerFunc(h.handleListAppEntitlements)))
	mux.Handle("POST /v1/admin/apps/{id}/entitlements", admin(http.HandlerFunc(h.handleGrantEntitlement)))
	mux.Handle("DELETE /v1/admin/apps/{id}/entitlements/{entitlement_id}", admin(http.HandlerFunc(h.handleRevokeEntitlement)))
	mux.Handle("GET /v1/admin/users/{id}/entitlements", admin(http.HandlerFunc(h.handleListUserEntitlements)))

	// Provider entitlement mode (#465, additive): sets the whole entitlement state
	// (all|user|none) by provider name, for a wizard step with no app id yet
	// (created off-thread by the settings PATCH). See provider_entitlement_mode.go.
	mux.Handle("POST /v1/admin/library-providers/{provider}/entitlement-mode", admin(http.HandlerFunc(h.handleSetProviderEntitlementMode)))

	// Favourites (UI-P1): auth only, never admin-gated. Owner is always the bearer
	// identity; see favourites.go.
	mux.Handle("PUT /v1/me/favourites/{app_id}", requireAuth(http.HandlerFunc(h.handleFavouriteApp)))
	mux.Handle("DELETE /v1/me/favourites/{app_id}", requireAuth(http.HandlerFunc(h.handleUnfavouriteApp)))

	// Home rail (2026-08-05): auth only, no admin variant — resolved entirely from
	// the bearer identity, so an admin's home page is their own. See highlights.go.
	mux.Handle("GET /v1/me/highlights", requireAuth(http.HandlerFunc(h.handleHighlights)))
}

// --- DTOs (response shapes match schema columns) ---

type appResp struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	CoverURL    *string `json:"cover_url"`
	// HeroURL (UI-P7): wide hero/detail crop. Null = gradient-tile default.
	HeroURL *string `json:"hero_url"`
	// Kind: presentation-only, always serialized.
	Kind string `json:"kind"`
	// ExternalSource/ExternalID (migration 0042): "this app IS provider X's title
	// Y" (today only "steam", <appid>). Both "" = not a provider title. Always
	// serialized (never omitempty) so a client can tell "" from absent.
	ExternalSource string `json:"external_source"`
	ExternalID     string `json:"external_id"`
	// ParentAppID (migration 0044): the provider app this is a derived tile of, or
	// null. Public and always serialized — §2.2 req 2 needs it to group a
	// family's tiles so the client can block them during a shared-home session.
	ParentAppID *string `json:"parent_app_id"`
	// Favourite: the CALLING USER's own favourite state, resolved per request.
	Favourite          bool               `json:"favourite"`
	DefaultWidth       int32              `json:"default_width"`
	DefaultHeight      int32              `json:"default_height"`
	DefaultFPS         int32              `json:"default_fps"`
	DefaultBitratekbps int32              `json:"default_bitrate_kbps"`
	Enabled            bool               `json:"enabled"`
	DefaultProfileID   *string            `json:"default_profile_id"`
	ProfilePolicy      string             `json:"profile_policy"`
	DisplayStream      streamDefaultsResp `json:"display_stream"`
}

type streamDefaultsResp struct {
	Width       int32 `json:"width"`
	Height      int32 `json:"height"`
	FPS         int32 `json:"fps"`
	Bitratekbps int32 `json:"bitrate_kbps"`
}

type hostResp struct {
	ID             string  `json:"id"`
	NodeName       string  `json:"node_name"`
	Status         string  `json:"status"`
	AgentVersion   *string `json:"agent_version"`
	CPUCores       *int32  `json:"cpu_cores"`
	MemMB          *int32  `json:"mem_mb"`
	LastRegistered *string `json:"last_registered_at"`
	LastHeartbeat  *string `json:"last_heartbeat_at"`
	// Storage, CPUModel: always serialized, null until an amendment-aware agent
	// reports (openapi.yaml Host.storage/cpu_model, both required).
	Storage  json.RawMessage `json:"storage"`
	CPUModel *string         `json:"cpu_model"`
	// Readiness (openapi.yaml Host.readiness, required): opaque array of
	// {id, status, summary, remediation} the agent owns end to end. Advisory —
	// a failing check never blocked registration.
	Readiness json.RawMessage `json:"readiness"`
	// ReadinessReportedAt: freshness of that set, so the UI can flag it stale.
	ReadinessReportedAt *string `json:"readiness_reported_at"`
	CapacityDetection   string  `json:"capacity_detection"`
	CapacityReason      *string `json:"capacity_reason"`
	// AgentConnectedSince/AgentRestartCount/AgentLastRestartAt (#429 follow-on):
	// not yet in protocol/openapi.yaml (see store.go's Host doc comment).
	// null/0 until the host's first connect.
	AgentConnectedSince *string `json:"agent_connected_since"`
	AgentRestartCount   int32   `json:"agent_restart_count"`
	AgentLastRestartAt  *string `json:"agent_last_restart_at"`
	// Capacity: always serialized, null when the host has no reported GPUs to sum.
	Capacity *HostCapacity `json:"capacity"`
}

func appToResp(a App) appResp {
	return appResp{
		ID:                 a.ID,
		Name:               a.Name,
		Description:        a.Description,
		CoverURL:           a.CoverURL,
		HeroURL:            a.HeroURL,
		Kind:               a.Kind,
		ExternalSource:     a.ExternalSource,
		ExternalID:         a.ExternalID,
		ParentAppID:        a.ParentAppID,
		Favourite:          a.Favourite,
		DefaultWidth:       a.DefaultWidth,
		DefaultHeight:      a.DefaultHeight,
		DefaultFPS:         a.DefaultFPS,
		DefaultBitratekbps: a.DefaultBitratekbps,
		Enabled:            a.Enabled,
		DefaultProfileID:   a.DefaultProfileID,
		ProfilePolicy:      a.ProfilePolicy,
		DisplayStream: streamDefaultsResp{
			Width:       a.DisplayWidth,
			Height:      a.DisplayHeight,
			FPS:         a.DisplayFPS,
			Bitratekbps: a.DisplayBitratekbps,
		},
	}
}

// adminAppResp extends the public shape with runtime_spec, managed-home, and
// scheduler-internal resource defaults (#397) — all admin-only.
type adminAppResp struct {
	appResp
	// DefaultVramMB/DefaultEncodeSlots (#397): admin-only, per openapi.yaml
	// AdminApp vs App/AppListItem and control-api.md. Must stay off appResp.
	DefaultVramMB      int32           `json:"default_vram_mb"`
	DefaultEncodeSlots int32           `json:"default_encode_slots"`
	RuntimeSpec        json.RawMessage `json:"runtime_spec"`
	ManagedHome        bool            `json:"managed_home"`
	HomeContainerPath  string          `json:"home_container_path"`
	// RuntimePresetID (UI-P3): the app's OWN stored preset, null = none. Not the
	// merged effective value — that's a launch-time concern.
	RuntimePresetID *string `json:"runtime_preset_id"`
	// LaunchableProfileIDs (UI-P5): `[]` = unrestricted. Admin-only — a client is
	// served the already-filtered menu via GET /v1/me/profiles?app_id=… instead.
	// Always an array, never null.
	LaunchableProfileIDs []string `json:"launchable_profile_ids"`
	// Origin: provenance ('manual'/'discovered'), admin-only, no user surface
	// consumes it. LibraryProvider: operator config, same category as
	// runtime_preset_id — the switch that arms a background home-directory walk.
	Origin          string `json:"origin"`
	LibraryProvider string `json:"library_provider"`
	// LibraryDiscoverySuspended (#534, additive): distinguishes "operator
	// disabled this app" from "the reconciler suspended it because discovery is
	// off", which `enabled: false` alone can't express. False for non-provider
	// apps. Stays off appResp — a suspended app is already filtered out of
	// GET /v1/apps by `enabled`, so a public client has nothing to explain.
	LibraryDiscoverySuspended bool `json:"library_discovery_suspended"`
	// Sessions30d: launches in the last 30 days, every state. Fleet-wide across
	// users, which is why it is admin-only.
	Sessions30d int32 `json:"sessions_30d"`
}

func appToAdminResp(a App) adminAppResp {
	spec := a.RuntimeSpec
	if len(spec) == 0 {
		spec = json.RawMessage(`{}`)
	}
	allowList := a.LaunchableProfileIDs
	if allowList == nil {
		allowList = []string{}
	}
	return adminAppResp{
		appResp:              appToResp(a),
		DefaultVramMB:        a.DefaultVramMB,
		DefaultEncodeSlots:   a.DefaultEncodeSlots,
		RuntimeSpec:          spec,
		ManagedHome:          a.ManagedHome,
		HomeContainerPath:    a.HomeContainerPath,
		RuntimePresetID:      a.RuntimePresetID,
		LaunchableProfileIDs: allowList,
		Origin:               a.Origin,
		LibraryProvider:      a.LibraryProvider,

		LibraryDiscoverySuspended: a.LibraryDiscoverySuspended,
		Sessions30d:               a.Sessions30d,
	}
}

func hostToResp(h Host) hostResp {
	var lastReg, lastHb, connectedSince, lastRestart *string
	if h.LastRegistered != nil {
		s := h.LastRegistered.Format("2006-01-02T15:04:05Z07:00")
		lastReg = &s
	}
	if h.LastHeartbeat != nil {
		s := h.LastHeartbeat.Format("2006-01-02T15:04:05Z07:00")
		lastHb = &s
	}
	if h.AgentConnectedSince != nil {
		s := h.AgentConnectedSince.Format("2006-01-02T15:04:05Z07:00")
		connectedSince = &s
	}
	if h.AgentLastRestartAt != nil {
		s := h.AgentLastRestartAt.Format("2006-01-02T15:04:05Z07:00")
		lastRestart = &s
	}
	var readinessAt *string
	if h.ReadinessReportedAt != nil {
		s := h.ReadinessReportedAt.Format("2006-01-02T15:04:05Z07:00")
		readinessAt = &s
	}
	return hostResp{
		ID:                  h.ID,
		NodeName:            h.NodeName,
		Status:              h.Status,
		AgentVersion:        h.AgentVersion,
		CPUCores:            h.CPUCores,
		MemMB:               h.MemMB,
		LastRegistered:      lastReg,
		LastHeartbeat:       lastHb,
		Storage:             h.Storage,
		CPUModel:            h.CPUModel,
		Readiness:           h.Readiness,
		ReadinessReportedAt: readinessAt,
		CapacityDetection:   h.CapacityDetection,
		CapacityReason:      h.CapacityReason,
		Capacity:            h.Capacity,
		AgentConnectedSince: connectedSince,
		AgentRestartCount:   h.AgentRestartCount,
		AgentLastRestartAt:  lastRestart,
	}
}

// --- handlers ---

func (h *Handler) handleListApps(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	cursor := r.URL.Query().Get("cursor")
	limit := int32(50)
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	apps, nextCursor, err := h.store.listApps(r.Context(), caller.ID, cursor, limit)
	if err != nil {
		slog.Warn("list apps failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list apps")
		return
	}

	items := []appResp{} // [] not null for an empty page (control-api items array)
	for _, a := range apps {
		items = append(items, appToResp(a))
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
	})
}

func (h *Handler) handleGetApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, _ := auth.UserFromContext(r.Context())
	if caller.Role == auth.RoleAdmin {
		// Admin: fetch regardless of enabled status, include runtime_spec. This is
		// the admin app editor's load path, not a §6.5 library/launch bypass —
		// those stay filtered elsewhere (listApps, scheduling). Entitlement-
		// filtering here would make a restricted app un-editable by its own admin.
		app, err := h.store.getAppFull(r.Context(), caller.ID, id)
		if err != nil {
			if err == ErrNotFound {
				httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
				return
			}
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not get app")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"app": appToAdminResp(app)})
		return
	}
	app, err := h.store.getApp(r.Context(), caller.ID, id)
	if err != nil {
		if err == ErrNotFound {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not get app")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"app": appToResp(app)})
}

func (h *Handler) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string  `json:"name"`
		Description string  `json:"description"`
		CoverURL    *string `json:"cover_url"`
		// Kind: nil = schema default. Explicit "" is invalid, never "use default"
		// (control-api.md, the cb97bfb trap: absence must never decode as a zero value).
		Kind *string `json:"kind"`
		// ExternalSource/ExternalID: nil = schema default (''). Unlike Kind, explicit
		// "" is valid — '' is the real value for "not a provider title".
		ExternalSource *string `json:"external_source"`
		ExternalID     *string `json:"external_id"`
		// ParentAppID: raw JSON so absent and explicit null both mean "no parent",
		// one parser shared with the PATCH path (parseOptionalUUIDPatch). Setting it
		// makes this a derived tile, enforced tile-shaped by apps_derived_shape_ck.
		ParentAppID json.RawMessage `json:"parent_app_id"`
		// LibraryProvider: nil = schema default (""). `origin` is deliberately
		// absent from this struct (see validOrigin); DisallowUnknownFields makes a
		// client that sends it get 400, not a silently-ignored field.
		LibraryProvider *string `json:"library_provider"`
		// Entitle (§6.4): "all" (default, and what absent means) grants the new app
		// to everyone; "none" grants nobody. The default must stay "all" — the
		// alternative is "I made an app and nobody can see it" as the norm (§17
		// row 2), the mirror of the 0043 empty-table lockout.
		Entitle string `json:"entitle"`
		// Pointers, not plain int32: absence must fall through to the schema
		// DEFAULT (store.createApp). A plain int32 would decode absence as 0 and
		// admit an app with no encode slot and no VRAM reservation.
		DefaultVramMB      *int32          `json:"default_vram_mb"`
		DefaultEncodeSlots *int32          `json:"default_encode_slots"`
		DefaultWidth       *int32          `json:"default_width"`
		DefaultHeight      *int32          `json:"default_height"`
		DefaultFPS         *int32          `json:"default_fps"`
		DefaultBitratekbps *int32          `json:"default_bitrate_kbps"`
		RuntimeSpec        json.RawMessage `json:"runtime_spec"`
		ManagedHome        bool            `json:"managed_home"`
		HomeContainerPath  string          `json:"home_container_path"`
		DefaultProfileID   *string         `json:"default_profile_id"`
		ProfilePolicy      string          `json:"profile_policy"`
		// RuntimePresetID: absent/null = no preset, the app carries everything itself.
		RuntimePresetID *string `json:"runtime_preset_id"`
		// LaunchableProfileIDs: absent/[] = unrestricted. Raw JSON so an explicit
		// null is distinguishable and rejected rather than reinterpreted (parseAllowList).
		LaunchableProfileIDs json.RawMessage `json:"launchable_profile_ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "name is required")
		return
	}
	if !validAppKind(req.Kind) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "kind must be game, desktop or launcher")
		return
	}
	if !validExternalSource(req.ExternalSource) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errExternalSource)
		return
	}
	if !validExternalID(req.ExternalID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errExternalID)
		return
	}
	if !validLibraryProvider(req.LibraryProvider) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errLibraryProvider)
		return
	}
	parentPatch, _, err := parseOptionalUUIDPatch(req.ParentAppID)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "parent_app_id must be a uuid or null")
		return
	}
	// Branches kept separate: collapsing to `err != nil || !ok` would 400 an
	// operator over a connection-pool failure, not a bad parent_app_id.
	validParent, err := h.store.validParentApp(r.Context(), parentPatch)
	if err != nil {
		slog.Warn("validate parent_app_id failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not validate parent_app_id")
		return
	}
	if !validParent {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errParentApp)
		return
	}
	// §11.3: a raw CHECK violation wouldn't tell the operator which field to fix.
	if derivedProviderConflict(parentPatch != nil, req.LibraryProvider) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errDerivedProvider)
		return
	}
	// #534: refuse before the insert, else a provider app created with discovery
	// off is suspended by the reconciler seconds later and 404s forever. Create
	// is always `enabled = true`, so a non-empty library_provider is sufficient.
	if h.refuseProviderWriteWhileDiscoveryOff(w, r, req.LibraryProvider != nil && *req.LibraryProvider != "") {
		return
	}
	// Rejected, not treated as "none": a typo ("nome") silently creating an
	// invisible app would misdiagnose as "the catalogue is broken".
	if req.Entitle != "" && req.Entitle != "all" && req.Entitle != "none" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "entitle must be all or none")
		return
	}
	if !validDirectCoverURL(req.CoverURL) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "cover_url must be a relative path or an http(s) URL")
		return
	}
	if req.HomeContainerPath != "" && !strings.HasPrefix(req.HomeContainerPath, "/") {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "home_container_path must be absolute")
		return
	}
	if !validProfilePolicy(req.ProfilePolicy) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "profile_policy must be inherit, prefer, or force")
		return
	}
	if field, ok := invalidResourceDefault(req.DefaultVramMB, req.DefaultEncodeSlots,
		req.DefaultWidth, req.DefaultHeight, req.DefaultFPS, req.DefaultBitratekbps); !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, field+" must be greater than zero")
		return
	}
	if ok, err := h.store.validUserProfileID(r.Context(), req.DefaultProfileID); err != nil || !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "default_profile_id must be a user-facing stream profile")
		return
	}
	if ok, err := h.store.validRuntimePresetID(r.Context(), req.RuntimePresetID); err != nil || !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "runtime_preset_id must reference an existing runtime preset")
		return
	}
	// Validated before the insert so a bad list never creates a half-configured app.
	allowList, err := parseAllowList(req.LaunchableProfileIDs)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}
	if len(allowList.ids) > 0 && req.ProfilePolicy == "force" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errAllowListUnderForce.Error())
		return
	}
	if ok, err := h.store.validLaunchProfileIDs(r.Context(), allowList.ids); err != nil || !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"launchable_profile_ids must all name user-facing launch profiles")
		return
	}

	caller, _ := auth.UserFromContext(r.Context())
	app, err := h.store.createApp(r.Context(), req.Name, req.Description, req.CoverURL, req.Kind,
		req.ExternalSource, req.ExternalID,
		parentPatch, req.LibraryProvider,
		req.DefaultVramMB, req.DefaultEncodeSlots,
		req.DefaultWidth, req.DefaultHeight, req.DefaultFPS, req.DefaultBitratekbps,
		req.RuntimeSpec, req.ManagedHome, req.HomeContainerPath, req.DefaultProfileID, req.ProfilePolicy,
		req.RuntimePresetID, caller.ID)
	if err != nil {
		if writeAppConstraintError(w, err) {
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not create app")
		return
	}

	// §6.4 default 'all' entitlement, skipped only on entitle:"none". Fail closed:
	// on write failure, delete the app rather than leave it created-but-invisible
	// with no field in the editor explaining why. The app is seconds old and
	// cannot have sessions, so deleteApp's refuse-if-in-use guard cannot fire.
	// (Discovered tiles get no 'all' row; that's Phase 4, not this handler.)
	if req.Entitle != "none" {
		if err := h.store.grantAllOnCreate(r.Context(), app.ID, actorID(r)); err != nil {
			if _, delErr := h.store.deleteApp(r.Context(), app.ID, true); delErr != nil {
				slog.Error("Phase 2: could not roll back an app whose default entitlement write failed — it exists INVISIBLE",
					"app_id", app.ID, "write_err", err, "rollback_err", delErr)
			}
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not set the app's default entitlement")
			return
		}
	}

	if len(allowList.ids) > 0 {
		if err := h.store.setAppLaunchProfiles(r.Context(), app.ID, allowList.ids); err != nil {
			// Fail closed: a created app with no allow-list reads as unrestricted,
			// the opposite of what was asked. Delete it (seconds old, no sessions
			// possible, so the refuse-if-in-use guard cannot fire).
			if _, delErr := h.store.deleteApp(r.Context(), app.ID, true); delErr != nil {
				slog.Error("UI-P5: could not roll back an app whose allow-list write failed — it exists UNRESTRICTED",
					"app_id", app.ID, "write_err", err, "rollback_err", delErr)
			}
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not set the app's launchable launch profiles")
			return
		}
		app.LaunchableProfileIDs = allowList.ids
	}

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"app": appToAdminResp(app)})
}

func (h *Handler) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		CoverURL    *string `json:"cover_url"`
		// Kind: nil = unchanged. Explicit "" is invalid, never unchanged/default.
		Kind *string `json:"kind"`
		// ExternalSource/ExternalID: nil = unchanged (the cb97bfb rule — absence must
		// never overwrite with ""). Explicit "" is a deliberate clear.
		ExternalSource *string `json:"external_source"`
		ExternalID     *string `json:"external_id"`
		// ParentAppID: tri-state via raw JSON, like RuntimePresetID — absent =
		// unchanged, null = clear (tile becomes ordinary app), uuid = set.
		ParentAppID json.RawMessage `json:"parent_app_id"`
		// LibraryProvider: nil = unchanged (cb97bfb rule). Explicit "" clears it.
		// `origin` is deliberately absent here too — see validOrigin.
		LibraryProvider    *string         `json:"library_provider"`
		DefaultVramMB      *int32          `json:"default_vram_mb"`
		DefaultEncodeSlots *int32          `json:"default_encode_slots"`
		DefaultWidth       *int32          `json:"default_width"`
		DefaultHeight      *int32          `json:"default_height"`
		DefaultFPS         *int32          `json:"default_fps"`
		DefaultBitratekbps *int32          `json:"default_bitrate_kbps"`
		Enabled            *bool           `json:"enabled"`
		RuntimeSpec        json.RawMessage `json:"runtime_spec"`
		ManagedHome        *bool           `json:"managed_home"`
		HomeContainerPath  *string         `json:"home_container_path"`
		DefaultProfileID   json.RawMessage `json:"default_profile_id"`
		ProfilePolicy      *string         `json:"profile_policy"`
		// RuntimePresetID: tri-state raw JSON like DefaultProfileID — absent =
		// unchanged, null = clear, uuid = set.
		RuntimePresetID json.RawMessage `json:"runtime_preset_id"`
		// LaunchableProfileIDs: absent = unchanged, [] = clear (unrestricted),
		// non-empty = replace wholesale. Explicit null is 400 — undefined here.
		LaunchableProfileIDs json.RawMessage `json:"launchable_profile_ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validAppKind(req.Kind) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "kind must be game, desktop or launcher")
		return
	}
	if !validExternalSource(req.ExternalSource) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errExternalSource)
		return
	}
	if !validExternalID(req.ExternalID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errExternalID)
		return
	}
	if !validLibraryProvider(req.LibraryProvider) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errLibraryProvider)
		return
	}
	if !validDirectCoverURL(req.CoverURL) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "cover_url must be a relative path or an http(s) URL")
		return
	}
	if req.HomeContainerPath != nil && !strings.HasPrefix(*req.HomeContainerPath, "/") {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "home_container_path must be absolute")
		return
	}
	if req.ProfilePolicy != nil && !validProfilePolicy(*req.ProfilePolicy) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "profile_policy must be inherit, prefer, or force")
		return
	}
	if field, ok := invalidResourceDefault(req.DefaultVramMB, req.DefaultEncodeSlots,
		req.DefaultWidth, req.DefaultHeight, req.DefaultFPS, req.DefaultBitratekbps); !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, field+" must be greater than zero")
		return
	}
	defaultProfilePatch, ok, err := parseOptionalUUIDPatch(req.DefaultProfileID)
	validProfile := true
	if err == nil && ok {
		validProfile, err = h.store.validUserProfileID(r.Context(), defaultProfilePatch)
	}
	if err != nil || !validProfile {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "default_profile_id must be a user-facing stream profile")
		return
	}
	presetPatch, presetOK, err := parseOptionalUUIDPatch(req.RuntimePresetID)
	validPreset := true
	if err == nil && presetOK {
		validPreset, err = h.store.validRuntimePresetID(r.Context(), presetPatch)
	}
	if err != nil || !validPreset {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "runtime_preset_id must reference an existing runtime preset")
		return
	}
	parentPatch, parentOK, err := parseOptionalUUIDPatch(req.ParentAppID)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "parent_app_id must be a uuid or null")
		return
	}
	if parentOK {
		// Branches kept separate: a pool failure is a 500, not a 400 blaming the
		// operator's parent_app_id.
		validParent, verr := h.store.validParentApp(r.Context(), parentPatch)
		if verr != nil {
			slog.Warn("validate parent_app_id failed", "err", verr)
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not validate parent_app_id")
			return
		}
		if !validParent {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errParentApp)
			return
		}
	}
	// The other half of the one-level gate: validParentApp proves the prospective
	// parent isn't itself a tile; this proves the app being GIVEN a parent has no
	// children of its own — else PATCH A to point at P leaves T a grandchild whose
	// home resolves to nothing. Only checked when a parent is actually being set;
	// clearing one (null) can never deepen a chain.
	if parentPatch != nil && *parentPatch != "" {
		hasTiles, herr := h.store.hasDerivedTiles(r.Context(), id)
		if herr != nil {
			slog.Warn("check derived tiles failed", "app_id", id, "err", herr)
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not check the app's derived tiles")
			return
		}
		if hasTiles {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errParentOfAParent)
			return
		}
	}
	// An app may not be its own parent — not expressible as a CHECK (the row
	// doesn't know its own id at INSERT time).
	if parentPatch != nil && *parentPatch == id {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "an app cannot be its own parent_app_id")
		return
	}
	// §11.3: evaluated against the EFFECTIVE shape (patched-or-stored) so a patch
	// touching only one of the two fields is judged correctly. The stored read
	// doubles as the existence check.
	stored, err := h.store.appDerivedRefs(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the app")
		return
	}
	effectiveDerived := stored.ParentAppID != ""
	if parentOK {
		effectiveDerived = parentPatch != nil
	}
	effectiveProvider := stored.LibraryProvider
	if req.LibraryProvider != nil {
		effectiveProvider = *req.LibraryProvider
	}
	if effectiveDerived && effectiveProvider != "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errDerivedProvider)
		return
	}
	// #534, the PATCH half: keyed on the EFFECTIVE provider/enabled state (like
	// the derived-tile rule above) because two writes reach the same trap —
	// making an app a provider, or re-enabling one that already is (updateApp
	// clears library_discovery_suspended on `enabled` in the same statement).
	// Clearing library_provider or disabling the app must always stay allowed —
	// they're how an operator escapes without the instance-wide switch — and any
	// patch whose result is a disabled app is exempt (SuspendProviderApps only
	// touches `enabled = true` rows).
	effectiveEnabled := stored.Enabled
	if req.Enabled != nil {
		effectiveEnabled = *req.Enabled
	}
	becomesProvider := req.LibraryProvider != nil && *req.LibraryProvider != ""
	enablesApp := req.Enabled != nil && *req.Enabled
	reArmsProviderApp := effectiveEnabled && effectiveProvider != "" && (becomesProvider || enablesApp)
	if h.refuseProviderWriteWhileDiscoveryOff(w, r, reArmsProviderApp) {
		return
	}
	allowList, err := parseAllowList(req.LaunchableProfileIDs)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}
	// Read unconditionally, even when the request supplies a policy: the read
	// doubles as the existence check, so skipping it would let a missing app
	// surface as an FK-violation 500 instead of the contracted 404.
	storedPolicy, err := h.store.appProfilePolicy(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the app's profile policy")
		return
	}
	effectivePolicy := storedPolicy
	if req.ProfilePolicy != nil {
		effectivePolicy = *req.ProfilePolicy
	}
	if effectivePolicy == "force" && len(allowList.ids) > 0 {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errAllowListUnderForce.Error())
		return
	}
	if ok, err := h.store.validLaunchProfileIDs(r.Context(), allowList.ids); err != nil || !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"launchable_profile_ids must all name user-facing launch profiles")
		return
	}

	// Two writes need opposite orderings against updateApp (not one transaction):
	// (a) caller sent a list — write it BEFORE updateApp, since the safe half to
	//     have applied on partial failure is the restriction, not a wider menu.
	// (b) effective policy is `force` — clear any stored list AFTER a successful
	//     updateApp; clearing first here would leave a failed patch's app
	//     `prefer` with no list at all, i.e. silently unrestricted. Clearing
	//     after makes the worst case a stale list on a `force` app, which is
	//     inert (AppProfileRestrictionFor treats `force` as unrestricted).
	if allowList.present {
		ids := allowList.ids
		if effectivePolicy == "force" {
			ids = nil
		}
		if err := h.store.setAppLaunchProfiles(r.Context(), id, ids); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not set the app's launchable launch profiles")
			return
		}
	}

	caller, _ := auth.UserFromContext(r.Context())
	app, err := h.store.updateApp(r.Context(), id,
		req.Name, req.Description, req.CoverURL, req.Kind,
		req.ExternalSource, req.ExternalID,
		optionalUUIDArg(parentPatch, parentOK), req.LibraryProvider,
		req.DefaultVramMB, req.DefaultEncodeSlots,
		req.DefaultWidth, req.DefaultHeight, req.DefaultFPS, req.DefaultBitratekbps,
		req.Enabled, req.RuntimeSpec, req.ManagedHome, req.HomeContainerPath, optionalUUIDArg(defaultProfilePatch, ok), req.ProfilePolicy,
		optionalUUIDArg(presetPatch, presetOK), caller.ID)
	if err != nil {
		if err == ErrNotFound {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
			return
		}
		if errors.Is(err, ErrCoverURLOwnedByArtwork) {
			httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict,
				"cover_url is managed by the artwork service for this app — use POST/DELETE /v1/admin/apps/{id}/artwork instead of a direct write")
			return
		}
		if writeAppConstraintError(w, err) {
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not update app")
		return
	}

	// (b) from above: run only once updateApp's policy write has landed.
	if !allowList.present && effectivePolicy == "force" {
		if err := h.store.setAppLaunchProfiles(r.Context(), id, nil); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not clear the app's launchable launch profiles")
			return
		}
		app.LaunchableProfileIDs = nil
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"app": appToAdminResp(app)})
}

// invalidResourceDefault rejects an explicit non-positive resource default; nil
// is always fine. Zero is never meaningful: `default_encode_slots = 0` made four
// apps admissible on a GPU with no free encode slots, bypassing admission entirely.
func invalidResourceDefault(vram, slots, w, h, fps, kbps *int32) (string, bool) {
	for _, f := range []struct {
		name string
		val  *int32
	}{
		{"default_vram_mb", vram},
		{"default_encode_slots", slots},
		{"default_width", w},
		{"default_height", h},
		{"default_fps", fps},
		{"default_bitrate_kbps", kbps},
	} {
		if f.val != nil && *f.val <= 0 {
			return f.name, false
		}
	}
	return "", true
}

// validProfilePolicy gates apps.profile_policy. `custom` was retired (amendment
// B2, migration 0036 narrows the DB CHECK); this is the write gate in front of
// it. Explicit "" is still "use the default" here (unlike validAppKind).
func validProfilePolicy(policy string) bool {
	return policy == "" || policy == "inherit" || policy == "prefer" || policy == "force"
}

// validAppKind rejects an explicit kind outside the enum; nil is always fine.
// Unlike validProfilePolicy, explicit "" is never valid (cb97bfb trap: absence
// must never decode as a meaningful zero value) — the DB CHECK is the backstop,
// not the primary gate. `kind` stays presentation-only: nothing server-side
// branches on it, and library discovery triggers on `library_provider`, never
// on `kind = 'launcher'` (spec §4.5.3), except the artwork short-circuit in
// internal/artwork.
func validAppKind(kind *string) bool {
	return kind == nil || *kind == "game" || *kind == "desktop" || *kind == "launcher"
}

// steamAppIDPattern is an injection grammar, not a format preference (spec §10):
// the appid reaches STEAM_STARTUP_FLAGS, word-split with `read -r -a`, so
// "480 -foo" becomes two args. Must match `apps_external_id_ck` (migration 0042).
var steamAppIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

const (
	errExternalSource = `external_source must be "" or "steam"`
	errExternalID     = "external_id must be \"\" or a Steam appid (a bare positive integer, no leading zero)"
)

// validExternalSource / validExternalID gate provider identity (migration 0042);
// nil is fine, explicit "" is a deliberate clear (unlike validAppKind). Validated
// independently: artwork resolution requires both set, so a half-set pair is inert.
func validExternalSource(source *string) bool {
	return source == nil || *source == "" || *source == "steam"
}

func validExternalID(id *string) bool {
	return id == nil || *id == "" || steamAppIDPattern.MatchString(*id)
}

const (
	errLibraryProvider = `library_provider must be "" or "steam"`
	errParentApp       = "parent_app_id must reference an existing app that is not itself a derived tile"
	errParentOfAParent = "this app already has derived tiles of its own, so it cannot be given a parent — a tile borrows its parent's runtime one level only, and a chain leaves the middle app's tiles with no home to resolve to"
	errDerivedProvider = "library_provider cannot be set on a derived tile — a tile borrows its parent's runtime and cannot itself be a library provider"
	errDerivedShape    = "a derived tile carries identity only: with parent_app_id set, runtime_spec must be empty, runtime_preset_id must be null, managed_home must be false, library_provider must be empty, and external_source/external_id must both be set"
	errDuplicateTile   = "a derived tile for that parent app and external_id already exists"
	// errDiscoveryDisabled (#534) names the setting AND the remedy, the same
	// message shape the mirror-image refusal on DELETE /v1/admin/images/{id}/install
	// already uses ("disable it in Settings first").
	errDiscoveryDisabled = "library discovery is disabled for this instance, so a library-provider app would be suspended (disabled) again immediately by the provider reconciler — " +
		"enable it first with PATCH /v1/admin/settings {\"library_discovery_enabled\": true}, or leave library_provider empty"
)

// refuseProviderWriteWhileDiscoveryOff is the #534 guard: 409s when `write`
// would leave an enabled library-provider app behind while
// instance_settings.library_discovery_enabled is false.
//
// The reconciler is level-triggered on that setting (images.Store.
// SuspendProviderApps): at off, it suspends every enabled provider app, so a
// create with discovery off previously 201'd and was silently disabled seconds
// later, reading as a bare 404 on launch (#534).
//
// Refuses rather than auto-enabling discovery: that setting is the instance-wide
// switch for a job that walks every user's home directory and auto-publishes
// into their libraries (fail-closed by design, dark by default, spec §11.2).
// Flipping it as a side effect of one app create would infer fleet-wide consent
// from a local action — the same fail-open-on-ambiguity move #464 rejected for
// the entitlement backfill. The operator flips it themselves.
func (h *Handler) refuseProviderWriteWhileDiscoveryOff(w http.ResponseWriter, r *http.Request, wouldBeProviderApp bool) bool {
	if !wouldBeProviderApp {
		return false
	}
	enabled, err := h.store.libraryDiscoveryEnabled(r.Context())
	if err != nil {
		slog.Warn("read library_discovery_enabled for the provider-app guard failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal,
			"could not read the library-discovery setting")
		return true
	}
	if enabled {
		return false
	}
	httpx.WriteError(w, http.StatusConflict, httpx.CodeLibraryDiscoveryDisabled, errDiscoveryDisabled)
	return true
}

// validOrigin is deliberately not wired to a request field: `origin` is
// read-only provenance (operator ruling 2026-07-29), absent from both request
// structs above, so DisallowUnknownFields 400s a client that sends it. A
// writable origin would let a client forge provenance (same rule as
// entitlements.granted_by), and apps_parent_external_uk makes relabelling either
// direction a resurrection-loop footgun via a tile's (parent, source, appid)
// slot. Retained as the Go-side counterpart of the DB CHECK, for Phase 4's
// reconciler (the only writer of 'discovered').
func validOrigin(origin *string) bool {
	return origin == nil || *origin == "manual" || *origin == "discovered"
}

// validLibraryProvider gates library_provider, which IS writable (§1.1: operator
// config, an admin marks the Steam app). nil is always fine; explicit "" is a
// deliberate un-marking, like external_source.
func validLibraryProvider(provider *string) bool {
	return provider == nil || *provider == "" || *provider == "steam"
}

// derivedProviderConflict reports whether a CREATE is asking for a derived tile
// that is also a library provider (§11.3). The patch path computes the same thing
// from the effective (patched-or-stored) values, inline, because it has both.
func derivedProviderConflict(derived bool, libraryProvider *string) bool {
	return derived && libraryProvider != nil && *libraryProvider != ""
}

// writeAppConstraintError turns the two apps CHECK constraints into a 4xx
// naming what the operator can fix, rather than a 500 that blames the server.
func writeAppConstraintError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, ErrDerivedShape):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errDerivedShape)
		return true
	case errors.Is(err, ErrDuplicateDerivedTile):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, errDuplicateTile)
		return true
	}
	return false
}

// validDirectCoverURL gates a direct AppWrite cover_url write (nil = no
// change; ErrCoverURLOwnedByArtwork in updateApp handles an artwork-owned
// field). The artwork service always writes a same-origin /v1/artwork/<sha256>
// path into this column (UI-P7 "never hotlink" goal); this pre-existing
// direct-write escape hatch must not become a way to hand the browser an
// arbitrary scheme in an `<img src>`. Empty and schemeless values are accepted;
// only http/https are allowed off-box schemes.
func validDirectCoverURL(coverURL *string) bool {
	if coverURL == nil || *coverURL == "" {
		return true
	}
	u, err := url.Parse(*coverURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "", "http", "https":
		return true
	default:
		return false
	}
}

func parseOptionalUUIDPatch(raw json.RawMessage) (*string, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return nil, true, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, true, err
	}
	return &s, true, nil
}

func optionalUUIDArg(id *string, present bool) **string {
	if !present {
		return nil
	}
	return &id
}

func (h *Handler) handleListHosts(w http.ResponseWriter, r *http.Request) {
	cursor := r.URL.Query().Get("cursor")
	limit := int32(50)
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	hosts, nextCursor, err := h.store.listHosts(r.Context(), cursor, limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list hosts")
		return
	}

	items := []hostResp{} // [] not null for an empty page (control-api items array)
	for _, host := range hosts {
		items = append(items, hostToResp(host))
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
	})
}

func (h *Handler) handleGetHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	host, err := h.store.getHost(r.Context(), id)
	if err != nil {
		if err == ErrNotFound {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not get host")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"host": hostToResp(host)})
}

// handleAdminListApps returns ALL apps (enabled + disabled) with runtime_spec
// (P2-08). `favourite` is the acting admin's OWN favourites, not a placeholder.
func (h *Handler) handleAdminListApps(w http.ResponseWriter, r *http.Request) {
	caller, _ := auth.UserFromContext(r.Context())
	cursor := r.URL.Query().Get("cursor")
	limit := int32(50)
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	apps, nextCursor, err := h.store.listAllApps(r.Context(), caller.ID, cursor, limit)
	if err != nil {
		slog.Warn("list admin apps failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list apps")
		return
	}
	items := []adminAppResp{}
	for _, a := range apps {
		items = append(items, appToAdminResp(a))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}

func (h *Handler) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// §4.1: deleting a provider app cascades into its derived tiles and their
	// favourites/artwork, irreversibly — the caller must say so explicitly. A
	// query param, not a body field (DELETE bodies are poorly supported by
	// fetch()/intermediaries); only the exact string "true" opts in, so a typo
	// ("1", "yes") can't trigger a silent cascade.
	deleteDerived := r.URL.Query().Get("delete_derived") == "true"

	name, err := h.store.deleteApp(r.Context(), id, deleteDerived)
	switch {
	case err == nil:
		h.recordActivity(r, "app.delete", "app", id, map[string]any{
			"name": name, "delete_derived": deleteDerived,
		})
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
	case errors.Is(err, ErrAppHasActiveSessions):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "app is in use by an active session — stop it first")
	case errors.Is(err, ErrAppHasDerivedTiles):
		// List the tiles, not just a count, so the admin sees what they'd
		// destroy. Nested in the error object, mirroring the restart_required /
		// live_sessions precedent (internal/hostcfg).
		tiles, listErr := h.store.derivedTilesOf(r.Context(), id)
		if listErr != nil {
			slog.Warn("could not list derived tiles for the delete conflict", "app_id", id, "err", listErr)
			tiles = []DerivedTile{}
		}
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{
			"code": httpx.CodeConflict,
			"message": "this app has derived tiles that will be deleted with it — " +
				"re-send with ?delete_derived=true to confirm",
			"derived_tiles": tiles,
		}})
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not delete app")
	}
}

func (h *Handler) handleDeleteHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Live registry, not just the DB status column, so a racing reconnect is caught.
	if h.registry != nil && h.registry.IsConnected(id) {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "host is online — drain and wait for it to disconnect first")
		return
	}

	nodeName, err := h.store.deleteHost(r.Context(), id)
	switch {
	case err == nil:
		h.recordActivity(r, "host.delete", "host", id, map[string]any{"node_name": nodeName})
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
	case errors.Is(err, ErrHostHasActiveSessions):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "host has active sessions — stop them first")
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not delete host")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
		return false
	}
	return true
}
