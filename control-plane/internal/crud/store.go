package crud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// ErrAppHasActiveSessions: stop the sessions first; only terminal history cascades.
var ErrAppHasActiveSessions = errors.New("app has active sessions")

// ErrAppHasDerivedTiles: deleting a provider app whose tiles weren't opted into
// (spec §4.1). See deleteApp for why the FK cascade alone isn't enough.
var ErrAppHasDerivedTiles = errors.New("app has derived tiles")

// ErrHostHasActiveSessions: deleting a host with non-terminal sessions.
var ErrHostHasActiveSessions = errors.New("host has active sessions")

// ErrCoverURLOwnedByArtwork: a direct cover_url write on updateApp targets an
// app that already has an app_artwork provenance row. Must refuse outright,
// not silently drop, per openapi.yaml AppListItem.cover_url (semantics:
// control-api.md). internal/artwork's own writes never go through updateApp.
var ErrCoverURLOwnedByArtwork = errors.New("cover_url is owned by the artwork service once an artwork record exists")

// store is the CRUD data-access layer over the pgx pool.
type store struct {
	pool *pgxpool.Pool
}

// App is the domain view of an app/library entry (public + admin views use the same shape).
type App struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	CoverURL    *string `json:"cover_url"`
	// HeroURL (UI-P7): wide hero/detail crop, read-only here (written only by
	// internal/artwork). Null on both = gradient tile.
	HeroURL *string `json:"hero_url"`
	// Kind: presentation-only. Discovery triggers on library_provider, never
	// kind (spec §4.5.3).
	Kind string `json:"kind"`
	// ExternalSource/ExternalID (migration 0042): "this app IS provider X's title
	// Y" (today "steam", <appid>). Both "" = not a provider title. Identity, not
	// config — artwork resolves by appid for these, skipping the fuzzy matcher.
	ExternalSource string `json:"external_source"`
	ExternalID     string `json:"external_id"`
	// ParentAppID (migration 0044): the provider app this is a derived tile of.
	// Public (unlike Origin/LibraryProvider): §2.2 req 2 needs the client to
	// group a family to block sibling tiles during a shared-home session.
	ParentAppID *string `json:"parent_app_id"`
	// Origin: 'manual'/'discovered' (Phase 4). Admin-only, no user consumer.
	Origin string `json:"origin"`
	// LibraryProvider marks this app as scanned for installed titles. Admin-only,
	// operator-set (§1.1) — never inferred from the image name (a wrong inference
	// scans a user's home). Distinct from Kind='launcher'; §4.5.3 forbids
	// branching on kind.
	LibraryProvider string `json:"library_provider"`
	// LibraryDiscoverySuspended (migration 0060, #534): reconciler-disabled, not
	// operator-disabled. Without it, a reconciler-suspended app misdiagnoses as
	// an entitlement/catalog fault at launch.
	LibraryDiscoverySuspended bool `json:"library_discovery_suspended"`
	// Favourite: the calling user's own favourite state, resolved per request,
	// never stored or settable via AppWrite.
	Favourite bool `json:"favourite"`
	// Sessions30d: fleet-wide launches in the last 30 days, populated by
	// listAllApps only. Admin-only, so it never reaches App/AppListItem.
	Sessions30d        int32           `json:"-"`
	DefaultVramMB      int32           `json:"default_vram_mb"`
	DefaultEncodeSlots int32           `json:"default_encode_slots"`
	DefaultWidth       int32           `json:"default_width"`
	DefaultHeight      int32           `json:"default_height"`
	DefaultFPS         int32           `json:"default_fps"`
	DefaultBitratekbps int32           `json:"default_bitrate_kbps"`
	Enabled            bool            `json:"enabled"`
	DefaultProfileID   *string         `json:"default_profile_id"`
	ProfilePolicy      string          `json:"profile_policy"`
	DisplayWidth       int32           `json:"display_width"`
	DisplayHeight      int32           `json:"display_height"`
	DisplayFPS         int32           `json:"display_fps"`
	DisplayBitratekbps int32           `json:"display_bitrate_kbps"`
	RuntimeSpec        json.RawMessage `json:"runtime_spec"`
	// P5-01/02: managed per-(user, app) home opt-in.
	ManagedHome       bool   `json:"managed_home"`
	HomeContainerPath string `json:"home_container_path"`
	// RuntimePresetID (UI-P3): preset this app inherits container config from.
	// Own stored column; flattening happens at launch
	// (internal/session/runtime_preset.go), never here.
	RuntimePresetID *string `json:"runtime_preset_id"`
	// LaunchableProfileIDs (UI-P5): allow-list beside Play. Empty = unrestricted,
	// never null. Admin-only; a client is served the already-filtered
	// GET /v1/me/profiles?app_id=… instead.
	LaunchableProfileIDs []string  `json:"launchable_profile_ids"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// Host is the domain view of a host (agent).
type Host struct {
	ID             string     `json:"id"`
	NodeName       string     `json:"node_name"`
	Status         string     `json:"status"` // online|offline|draining
	AgentVersion   *string    `json:"agent_version"`
	CPUCores       *int32     `json:"cpu_cores"`
	MemMB          *int32     `json:"mem_mb"`
	LastRegistered *time.Time `json:"last_registered_at"`
	LastHeartbeat  *time.Time `json:"last_heartbeat_at"`
	// Storage: agent-reported volumes (schema.md hosts.storage), null until
	// an amendment-aware agent reports.
	Storage json.RawMessage `json:"storage"`
	// CPUModel: agent-reported marketing name, null until reported.
	CPUModel *string `json:"cpu_model"`
	// Readiness: raw JSONB, stored and served opaquely (agent-owned) so a new
	// check needs no control-plane change. Advisory — nothing schedules on it.
	Readiness json.RawMessage `json:"readiness"`
	// ReadinessReportedAt: freshness of that set (kept-if-absent, so it can go stale).
	ReadinessReportedAt *time.Time `json:"readiness_reported_at"`
	CapacityDetection   string     `json:"capacity_detection"`
	CapacityReason      *string    `json:"capacity_reason"`
	CreatedAt           time.Time  `json:"created_at"`
	// AgentConnectedSince/AgentRestartCount/AgentLastRestartAt (#429, migration
	// 0067): surfaces a container silently revived by Docker's `unless-stopped`.
	// Derived server-side in agentws.reconnectHost from WS timing, not
	// agent-reported. Null until the host's first (re)connect.
	AgentConnectedSince *time.Time `json:"agent_connected_since"`
	AgentRestartCount   int32      `json:"agent_restart_count"`
	AgentLastRestartAt  *time.Time `json:"agent_last_restart_at"`
	// Capacity: the GPU roll-up, filled by hostCapacities after the row read.
	// Nil = nothing to sum (no reported GPUs), which is not the same fact as zero.
	Capacity *HostCapacity `json:"capacity"`
}

// HostCapacity sums a host's reported GPUs and the reservations held on them.
// Derived at read time so it cannot drift from GET /v1/hosts/{id}/gpus.
// semantics: control-api.md §UI v3 console
type HostCapacity struct {
	SlotsTotal  int32 `json:"slots_total"`
	SlotsUsed   int32 `json:"slots_used"`
	VramMBTotal int32 `json:"vram_mb_total"`
	// VramMBUsed sums the LIVE-SAMPLED per-GPU figure, not reserved_vram_mb:
	// #383 removed declared VRAM from admission, so the reservation is
	// permanently 0 and a gauge built on it reads empty on a full host.
	VramMBUsed     int32 `json:"vram_mb_used"`
	ActiveSessions int32 `json:"active_sessions"`
	GPUCount       int32 `json:"gpu_count"`
}

// capacityActiveStatesSQL is the reservation-holding set. Hand-copy of
// internal/session's activeStatesSQL (session imports crud, so the import cannot
// go the other way); the two must not drift.
const capacityActiveStatesSQL = `('assigned','starting','running','stopping')`

// hostCapacities rolls up every GPU of the given hosts in ONE aggregate - a
// per-host query here is the 1+N the field exists to remove. Hosts with no
// schedulable GPU are absent from the map, which is how they end up null.
//
// The gpus/hosts predicate must stay identical to session.GPUAvailability's, or
// the summary claims capacity the per-GPU detail view denies.
func (s *store) hostCapacities(ctx context.Context, hostIDs []string) (map[string]HostCapacity, error) {
	out := make(map[string]HostCapacity, len(hostIDs))
	if len(hostIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT g.host_id::text,
		       COALESCE(SUM(g.encode_slots_total), 0)::int,
		       COALESCE(SUM(res.slots), 0)::int,
		       COALESCE(SUM(g.vram_mb_total), 0)::int,
		       COALESCE(SUM(g.vram_mb_used), 0)::int,
		       COALESCE(SUM(res.sessions), 0)::int,
		       COUNT(*)::int
		FROM gpus g
		JOIN hosts h ON h.id = g.host_id
		LEFT JOIN LATERAL (
		    SELECT COALESCE(SUM(sess.reserved_encode_slots), 0) AS slots,
		           COUNT(*)                                     AS sessions
		    FROM sessions sess
		    WHERE sess.gpu_id = g.id AND sess.state IN `+capacityActiveStatesSQL+`
		) res ON true
		WHERE h.capacity_detection = 'ok' AND g.reported
		  AND g.host_id::text = ANY($1)
		GROUP BY g.host_id
	`, hostIDs)
	if err != nil {
		return nil, fmt.Errorf("query host capacity: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c HostCapacity
		if err := rows.Scan(&id, &c.SlotsTotal, &c.SlotsUsed,
			&c.VramMBTotal, &c.VramMBUsed, &c.ActiveSessions, &c.GPUCount); err != nil {
			return nil, fmt.Errorf("scan host capacity: %w", err)
		}
		out[id] = c
	}
	return out, rows.Err()
}

// attachCapacities fills Host.Capacity for a page of hosts in one round trip.
func (s *store) attachCapacities(ctx context.Context, hosts []Host) error {
	if len(hosts) == 0 {
		return nil
	}
	ids := make([]string, 0, len(hosts))
	for _, h := range hosts {
		ids = append(ids, h.ID)
	}
	caps, err := s.hostCapacities(ctx, ids)
	if err != nil {
		return err
	}
	for i := range hosts {
		if c, ok := caps[hosts[i].ID]; ok {
			cc := c
			hosts[i].Capacity = &cc
		}
	}
	return nil
}

// entitledSQL is the entitlement predicate (spec §6.3). callerParam is the $N
// placeholder holding the caller's user id.
//
// Must be EXISTS, never a JOIN: a user can hold both an ('all') and a
// ('user', them) row for the same app, and a join on that pair emits the app
// twice, which listApps' limit+1 OFFSET paging turns into a silently dropped
// app rather than a visible duplicate.
//
// No role arm (§6.5): never `OR <caller is admin>` — that's the CLAUDE.md
// invariant #6 bypass class. GET /v1/admin/apps is the separate admin view.
//
// Hand-copied (internal/session cannot import internal/crud) at
// entitlements.go (entitledToApp), session/store.go (IsEntitled),
// session/scheduler.go (scheduleAttempt, + FOR SHARE). A future 'group'
// subject type must be added in all four. Do not add a fifth copy.
func entitledSQL(callerParam string) string {
	return `EXISTS (
			SELECT 1 FROM entitlements e
			WHERE e.app_id = apps.id
			  AND (e.subject_type = 'all'
			       OR (e.subject_type = 'user' AND e.subject_id = ` + callerParam + `::uuid))
		)`
}

// listApps returns enabled apps the caller is entitled to, cursor-paginated.
// callerID resolves both the per-item `favourite` flag and the entitlement
// filter, so it can never be empty on this path.
func (s *store) listApps(ctx context.Context, callerID, cursor string, limit int32) ([]App, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}

	var offset int64
	if cursor != "" {
		fmt.Sscanf(cursor, "%d", &offset)
	}

	rows, err := s.pool.Query(ctx, `
			SELECT apps.id::text, name, description, cover_url, hero_url, kind,
		       external_source, external_id,
		       -- Phase 3 (§2.2): PUBLIC. The client groups a family by it to mark
		       -- sibling tiles blocked while a session on the shared home is live.
		       apps.parent_app_id::text,
		       default_vram_mb, default_encode_slots,
		       default_width, default_height, default_fps, default_bitrate_kbps,
		       enabled, default_profile_id, profile_policy,
		       COALESCE(dp.width, default_width),
		       COALESCE(dp.height, default_height),
		       COALESCE(dp.fps, default_fps),
		       COALESCE(dp.nominal_bitrate_kbps, default_bitrate_kbps),
		       apps.created_at, apps.updated_at,
		       (fav.app_id IS NOT NULL)
		FROM apps
		-- display_stream (UI-P4): resolve the app's effective LAUNCH PROFILE, then
		-- its TOP RUNG (position 1), and COALESCE to the app defaults when nothing
		-- resolves. The "profile_policy <> custom" join conditions are gone with
		-- the custom policy itself (migration 0036 narrowed the CHECK).
		--
		-- The COALESCE to apps.default_* is LOAD-BEARING, not vestigial: with no
		-- global default set (Tower shipped state) an inherit app resolves no
		-- profile at all, and the app defaults are the only thing left to display.
		-- Dropping those columns would render the whole library as zeros.
		LEFT JOIN stream_profile_policy spp ON true
		LEFT JOIN launch_profile_rungs dpr ON dpr.position = 1 AND dpr.launch_profile_id = CASE
			WHEN apps.profile_policy IN ('prefer', 'force') AND apps.default_profile_id IS NOT NULL THEN apps.default_profile_id
			ELSE spp.global_default_profile_id
		END
		LEFT JOIN stream_profiles dp ON dp.id = dpr.stream_profile_id
		LEFT JOIN user_app_favourites fav ON fav.app_id = apps.id AND fav.user_id = $3::uuid
		WHERE apps.enabled = true
		  -- Phase 2 (§6.3). $3 is the caller, the same parameter the favourite
		  -- join uses. See entitledSQL: EXISTS, never a JOIN, and no role arm.
		  AND `+entitledSQL("$3")+`
		ORDER BY apps.created_at DESC
		LIMIT $1 OFFSET $2
	`, limit+1, offset, callerID) // +1 to detect if there's a next page
	if err != nil {
		return nil, "", fmt.Errorf("query apps: %w", err)
	}
	defer rows.Close()

	var apps []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.CoverURL, &a.HeroURL, &a.Kind,
			&a.ExternalSource, &a.ExternalID,
			&a.ParentAppID,
			&a.DefaultVramMB, &a.DefaultEncodeSlots,
			&a.DefaultWidth, &a.DefaultHeight, &a.DefaultFPS, &a.DefaultBitratekbps,
			&a.Enabled, &a.DefaultProfileID, &a.ProfilePolicy,
			&a.DisplayWidth, &a.DisplayHeight, &a.DisplayFPS, &a.DisplayBitratekbps,
			&a.CreatedAt, &a.UpdatedAt, &a.Favourite); err != nil {
			return nil, "", fmt.Errorf("scan app: %w", err)
		}
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var nextCursor string
	if int32(len(apps)) > limit {
		apps = apps[:limit]
		nextCursor = fmt.Sprintf("%d", offset+int64(limit))
	}
	return apps, nextCursor, nil
}

func (s *store) validUserProfileID(ctx context.Context, id *string) (bool, error) {
	if id == nil || *id == "" {
		return true, nil
	}
	// default_profile_id FKs launch_profiles (migration 0036); validating against
	// stream_profiles would accept a rung id and 500 at write time instead.
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM launch_profiles
			WHERE id = $1 AND visibility = 'user'
		)
	`, *id).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("validate launch profile: %w", err)
	}
	return ok, nil
}

// getApp returns a single enabled app by ID. callerID (UI-P1) resolves `favourite`.
func (s *store) getApp(ctx context.Context, callerID, id string) (App, error) {
	var a App
	err := s.pool.QueryRow(ctx, `
			SELECT apps.id::text, name, description, cover_url, hero_url, kind,
		       external_source, external_id,
		       -- Phase 3 (§2.2): PUBLIC. The client groups a family by it to mark
		       -- sibling tiles blocked while a session on the shared home is live.
		       apps.parent_app_id::text,
		       default_vram_mb, default_encode_slots,
		       default_width, default_height, default_fps, default_bitrate_kbps,
		       enabled, default_profile_id, profile_policy,
		       COALESCE(dp.width, default_width),
		       COALESCE(dp.height, default_height),
		       COALESCE(dp.fps, default_fps),
		       COALESCE(dp.nominal_bitrate_kbps, default_bitrate_kbps),
		       apps.created_at, apps.updated_at,
		       (fav.app_id IS NOT NULL)
		FROM apps
		-- display_stream (UI-P4): resolve the app's effective LAUNCH PROFILE, then
		-- its TOP RUNG (position 1), and COALESCE to the app defaults when nothing
		-- resolves. The "profile_policy <> custom" join conditions are gone with
		-- the custom policy itself (migration 0036 narrowed the CHECK).
		--
		-- The COALESCE to apps.default_* is LOAD-BEARING, not vestigial: with no
		-- global default set (Tower shipped state) an inherit app resolves no
		-- profile at all, and the app defaults are the only thing left to display.
		-- Dropping those columns would render the whole library as zeros.
		LEFT JOIN stream_profile_policy spp ON true
		LEFT JOIN launch_profile_rungs dpr ON dpr.position = 1 AND dpr.launch_profile_id = CASE
			WHEN apps.profile_policy IN ('prefer', 'force') AND apps.default_profile_id IS NOT NULL THEN apps.default_profile_id
			ELSE spp.global_default_profile_id
		END
		LEFT JOIN stream_profiles dp ON dp.id = dpr.stream_profile_id
		LEFT JOIN user_app_favourites fav ON fav.app_id = apps.id AND fav.user_id = $2::uuid
		WHERE apps.id::text = $1 AND apps.enabled = true
		  -- Phase 2 (§6.3): 404, not 403. This endpoint is already an existence
		  -- check for a non-admin (a disabled app is 404 too), so folding
		  -- "not entitled" into the same no-rows result leaks nothing — a caller
		  -- cannot distinguish "no such app" from "not yours", which is the
		  -- correct posture for a per-user library.
		  AND `+entitledSQL("$2")+`
	`, id, callerID).Scan(&a.ID, &a.Name, &a.Description, &a.CoverURL, &a.HeroURL, &a.Kind,
		&a.ExternalSource, &a.ExternalID,
		&a.ParentAppID,
		&a.DefaultVramMB, &a.DefaultEncodeSlots,
		&a.DefaultWidth, &a.DefaultHeight, &a.DefaultFPS, &a.DefaultBitratekbps,
		&a.Enabled, &a.DefaultProfileID, &a.ProfilePolicy,
		&a.DisplayWidth, &a.DisplayHeight, &a.DisplayFPS, &a.DisplayBitratekbps,
		&a.CreatedAt, &a.UpdatedAt, &a.Favourite)
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, fmt.Errorf("query app: %w", err)
	}
	return a, nil
}

// createApp inserts a new app.
//
// The six numeric defaults are `*int32`, nil omitting the column so Postgres
// applies the schema DEFAULT. Must stay pointers: a plain int32 decodes an
// absent field to 0 and clobbers the default — this created four live apps
// with default_encode_slots=0, admitted needing no VRAM/encode slot.
//
// kind/externalSource/externalID/parentAppID/libraryProvider follow the same
// omit-the-column idiom (nil = schema default); unlike kind, explicit "" on
// externalSource/externalID is a valid domain value, not absence.
//
// `origin` is not a parameter — it is server-owned provenance a client cannot
// forge (see validOrigin). A non-nil parentAppID makes this a derived tile;
// apps_derived_shape_ck backstops the shape, translated to ErrDerivedShape.
// callerID resolves `favourite` via the final getAppFull read.
func (s *store) createApp(ctx context.Context, name, desc string, coverURL, kind *string,
	externalSource, externalID *string,
	parentAppID, libraryProvider *string,
	vram, slots, w, h, fps, kbps *int32, runtimeSpec json.RawMessage,
	managedHome bool, homeContainerPath string, defaultProfileID *string, profilePolicy string,
	runtimePresetID *string, callerID string) (App, error) {
	if len(runtimeSpec) == 0 {
		runtimeSpec = json.RawMessage(`{}`)
	}
	if homeContainerPath == "" {
		homeContainerPath = "/home/quasar"
	}
	// §1.2: default_profile_id/profile_policy are copied once from the parent at
	// creation, then owned by the admin — applied only where the caller expressed
	// no preference. Must be a copy, not a live resolution: GetLaunchApp reads
	// these off the tile at launch, or a per-tile edit would be undone by the
	// parent on the next launch.
	if parentAppID != nil && *parentAppID != "" {
		inherited, err := s.parentLaunchDefaults(ctx, *parentAppID)
		if err != nil {
			return App{}, err
		}
		if defaultProfileID == nil {
			defaultProfileID = inherited.DefaultProfileID
		}
		if profilePolicy == "" {
			profilePolicy = inherited.ProfilePolicy
		}
	}
	if profilePolicy == "" {
		profilePolicy = "inherit"
	}

	cols := []string{"name", "description", "cover_url", "runtime_spec", "enabled",
		"managed_home", "home_container_path", "default_profile_id", "profile_policy",
		"runtime_preset_id"}
	args := []any{name, desc, coverURL, runtimeSpec, true,
		managedHome, homeContainerPath, defaultProfileID, profilePolicy,
		runtimePresetID}
	if kind != nil {
		cols = append(cols, "kind")
		args = append(args, *kind)
	}
	for _, opt := range []struct {
		col string
		val *string
	}{
		{"external_source", externalSource},
		{"external_id", externalID},
		{"parent_app_id", parentAppID},
		{"library_provider", libraryProvider},
	} {
		if opt.val != nil {
			cols = append(cols, opt.col)
			args = append(args, *opt.val)
		}
	}
	for _, opt := range []struct {
		col string
		val *int32
	}{
		{"default_vram_mb", vram},
		{"default_encode_slots", slots},
		{"default_width", w},
		{"default_height", h},
		{"default_fps", fps},
		{"default_bitrate_kbps", kbps},
	} {
		if opt.val != nil {
			cols = append(cols, opt.col)
			args = append(args, *opt.val)
		}
	}
	placeholders := make([]string, len(args))
	for i := range args {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}

	var id string
	query := fmt.Sprintf(`INSERT INTO apps (%s) VALUES (%s) RETURNING id::text`,
		strings.Join(cols, ", "), strings.Join(placeholders, ", "))
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&id); err != nil {
		if translated := appConstraintError(err); translated != nil {
			return App{}, translated
		}
		return App{}, fmt.Errorf("insert app: %w", err)
	}
	return s.getAppFull(ctx, callerID, id)
}

// ErrDerivedShape: apps_derived_shape_ck refused a write giving a derived tile
// a runtime of its own (spec §4.1). Translated to a 400 (request defect), not
// propagated as a 500; the CHECK stays the enforcement.
var ErrDerivedShape = errors.New("a derived tile carries identity only")

// ErrDuplicateDerivedTile: apps_parent_external_uk refused a second tile for
// the same (provider app, source, appid) — one tile per game, fleet-wide.
var ErrDuplicateDerivedTile = errors.New("a tile for that provider app and external id already exists")

// appConstraintError maps the two Phase-3 constraints to sentinels, or nil
// (stays a 500). Keyed on constraint name, never message text: names are ours,
// wording is Postgres's and localizable.
func appConstraintError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case "apps_derived_shape_ck":
			return ErrDerivedShape
		case "apps_parent_external_uk":
			return ErrDuplicateDerivedTile
		}
	}
	return nil
}

// hasArtwork reports whether an app already has an app_artwork provenance row.
// Queried directly rather than via internal/artwork: crud must not import it
// for a one-row check, avoiding the reverse-import artwork already avoids.
func (s *store) hasArtwork(ctx context.Context, appID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM app_artwork WHERE app_id::text = $1)`, appID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check app artwork: %w", err)
	}
	return exists, nil
}

// updateApp patches an app by ID (only non-nil/non-empty fields).
//
// externalSource/externalID: nil = unchanged, explicit "" = deliberate clear
// ("no longer a provider title") — the two must stay distinguishable, hence
// pointers rather than plain strings.
//
// parentAppID (**string) is tri-state like runtimePresetID: nil = unchanged,
// *parentAppID == nil = clear, otherwise set. `origin` is absent here for the
// same reason as createApp.
//
// callerID resolves `favourite` on the returned App.
func (s *store) updateApp(ctx context.Context, id string, name, desc *string, coverURL, kind *string,
	externalSource, externalID *string,
	parentAppID **string, libraryProvider *string,
	vram, slots, w, h, fps, kbps *int32, enabled *bool, runtimeSpec json.RawMessage,
	managedHome *bool, homeContainerPath *string, defaultProfileID **string, profilePolicy *string,
	runtimePresetID **string, callerID string) (App, error) {
	// Checked before any SET clause is built (see ErrCoverURLOwnedByArtwork), so
	// a patch touching cover_url and other fields is refused atomically.
	if coverURL != nil {
		hasArt, err := s.hasArtwork(ctx, id)
		if err != nil {
			return App{}, err
		}
		if hasArt {
			return App{}, ErrCoverURLOwnedByArtwork
		}
	}
	query := `UPDATE apps SET `
	var args []any
	var setClauses []string
	argIdx := 1

	if name != nil {
		setClauses = append(setClauses, fmt.Sprintf("name = $%d", argIdx))
		args = append(args, *name)
		argIdx++
	}
	if desc != nil {
		setClauses = append(setClauses, fmt.Sprintf("description = $%d", argIdx))
		args = append(args, *desc)
		argIdx++
	}
	if coverURL != nil {
		setClauses = append(setClauses, fmt.Sprintf("cover_url = $%d", argIdx))
		args = append(args, *coverURL)
		argIdx++
	}
	if kind != nil {
		setClauses = append(setClauses, fmt.Sprintf("kind = $%d", argIdx))
		args = append(args, *kind)
		argIdx++
	}
	if externalSource != nil {
		setClauses = append(setClauses, fmt.Sprintf("external_source = $%d", argIdx))
		args = append(args, *externalSource)
		argIdx++
	}
	if externalID != nil {
		setClauses = append(setClauses, fmt.Sprintf("external_id = $%d", argIdx))
		args = append(args, *externalID)
		argIdx++
	}
	if parentAppID != nil {
		setClauses = append(setClauses, fmt.Sprintf("parent_app_id = $%d", argIdx))
		args = append(args, *parentAppID)
		argIdx++
	}
	if libraryProvider != nil {
		setClauses = append(setClauses, fmt.Sprintf("library_provider = $%d", argIdx))
		args = append(args, *libraryProvider)
		argIdx++
	}
	if vram != nil {
		setClauses = append(setClauses, fmt.Sprintf("default_vram_mb = $%d", argIdx))
		args = append(args, *vram)
		argIdx++
	}
	if slots != nil {
		setClauses = append(setClauses, fmt.Sprintf("default_encode_slots = $%d", argIdx))
		args = append(args, *slots)
		argIdx++
	}
	if w != nil {
		setClauses = append(setClauses, fmt.Sprintf("default_width = $%d", argIdx))
		args = append(args, *w)
		argIdx++
	}
	if h != nil {
		setClauses = append(setClauses, fmt.Sprintf("default_height = $%d", argIdx))
		args = append(args, *h)
		argIdx++
	}
	if fps != nil {
		setClauses = append(setClauses, fmt.Sprintf("default_fps = $%d", argIdx))
		args = append(args, *fps)
		argIdx++
	}
	if kbps != nil {
		setClauses = append(setClauses, fmt.Sprintf("default_bitrate_kbps = $%d", argIdx))
		args = append(args, *kbps)
		argIdx++
	}
	if enabled != nil {
		setClauses = append(setClauses, fmt.Sprintf("enabled = $%d", argIdx))
		args = append(args, *enabled)
		argIdx++
		// The operator is authoritative about enabled: writing it at all clears
		// library_discovery_suspended in the same statement, else re-enabling a
		// suspended app would violate 0060's CHECK (NOT suspended OR NOT enabled),
		// and disabling one would leave it to be resurrected by the next
		// library-discovery enable. Literal, not a bound param: constant, not input.
		setClauses = append(setClauses, "library_discovery_suspended = false")
	}
	if len(runtimeSpec) > 0 {
		setClauses = append(setClauses, fmt.Sprintf("runtime_spec = $%d", argIdx))
		args = append(args, runtimeSpec)
		argIdx++
	}
	if managedHome != nil {
		setClauses = append(setClauses, fmt.Sprintf("managed_home = $%d", argIdx))
		args = append(args, *managedHome)
		argIdx++
	}
	if homeContainerPath != nil {
		setClauses = append(setClauses, fmt.Sprintf("home_container_path = $%d", argIdx))
		args = append(args, *homeContainerPath)
		argIdx++
	}
	if defaultProfileID != nil {
		setClauses = append(setClauses, fmt.Sprintf("default_profile_id = $%d", argIdx))
		args = append(args, *defaultProfileID)
		argIdx++
	}
	if profilePolicy != nil {
		setClauses = append(setClauses, fmt.Sprintf("profile_policy = $%d", argIdx))
		args = append(args, *profilePolicy)
		argIdx++
	}
	if runtimePresetID != nil {
		setClauses = append(setClauses, fmt.Sprintf("runtime_preset_id = $%d", argIdx))
		args = append(args, *runtimePresetID)
		argIdx++
	}
	if len(setClauses) == 0 {
		return s.getAppFull(ctx, callerID, id)
	}

	for i, clause := range setClauses {
		if i > 0 {
			query += ", "
		}
		query += clause
	}
	query += fmt.Sprintf(" WHERE id::text = $%d ", argIdx)
	query += `RETURNING id::text, name, description, cover_url, hero_url, kind,
	          external_source, external_id,
	          apps.parent_app_id::text, origin, library_provider,
	          default_vram_mb, default_encode_slots,
	          default_width, default_height, default_fps, default_bitrate_kbps,
	          enabled, default_profile_id, profile_policy, runtime_spec, managed_home, home_container_path,
	          runtime_preset_id::text, created_at, updated_at`

	args = append(args, id)
	var a App
	err := s.pool.QueryRow(ctx, query, args...).
		Scan(&a.ID, &a.Name, &a.Description, &a.CoverURL, &a.HeroURL, &a.Kind,
			&a.ExternalSource, &a.ExternalID,
			&a.ParentAppID, &a.Origin, &a.LibraryProvider,
			&a.DefaultVramMB, &a.DefaultEncodeSlots,
			&a.DefaultWidth, &a.DefaultHeight, &a.DefaultFPS, &a.DefaultBitratekbps,
			&a.Enabled, &a.DefaultProfileID, &a.ProfilePolicy, &a.RuntimeSpec, &a.ManagedHome, &a.HomeContainerPath,
			&a.RuntimePresetID, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		if translated := appConstraintError(err); translated != nil {
			return App{}, translated
		}
		return App{}, fmt.Errorf("update app: %w", err)
	}
	return s.getAppFull(ctx, callerID, a.ID)
}

// appDerivedRefs is the stored shape of one app, read by the PATCH path to
// evaluate the effective value of a partially-supplied request.
type appDerivedRefs struct {
	ParentAppID     string
	LibraryProvider string
	// Enabled (#534): the stored state, so PATCH can evaluate the effective one
	// for a request that omits `enabled` — else the provider guard would assume
	// enabled and refuse edits to an app the operator had already turned off.
	Enabled bool
}

// appDerivedRefs reads an app's stored parent_app_id/library_provider/enabled.
// ErrNotFound doubles as the existence check.
func (s *store) appDerivedRefs(ctx context.Context, id string) (appDerivedRefs, error) {
	var out appDerivedRefs
	var parent *string
	err := s.pool.QueryRow(ctx,
		`SELECT parent_app_id::text, library_provider, enabled FROM apps WHERE id::text = $1`, id,
	).Scan(&parent, &out.LibraryProvider, &out.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return appDerivedRefs{}, ErrNotFound
	}
	if err != nil {
		return appDerivedRefs{}, fmt.Errorf("read app derived refs: %w", err)
	}
	if parent != nil {
		out.ParentAppID = *parent
	}
	return out, nil
}

// libraryDiscoveryEnabled reads instance_settings.library_discovery_enabled for
// the #534 provider-app guard. Raw SQL, not an internal/settings import (like
// images.Store.libraryDiscoveryEnabledTx), and read fresh, never boot-cached.
//
// A missing singleton row reads as false, fail-closed: refusing loses nothing
// since the reconciler would suspend the app anyway.
func (s *store) libraryDiscoveryEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx,
		`SELECT library_discovery_enabled FROM instance_settings WHERE id = true`).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read library_discovery_enabled: %w", err)
	}
	return enabled, nil
}

// validParentApp reports whether id names an app that may be a parent: must
// exist and must not itself be a derived tile. homeAppID (spec §2) resolves
// exactly one level (`COALESCE(parent_app_id, id)`), so a grandchild would
// resolve its home to a parent tile that owns none. A CHECK can't express
// "the parent must not have a parent" (can't read another row), so this is
// the gate.
func (s *store) validParentApp(ctx context.Context, id *string) (bool, error) {
	if id == nil || *id == "" {
		return true, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM apps WHERE id::text = $1 AND parent_app_id IS NULL
		)
	`, *id).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("validate parent app: %w", err)
	}
	return ok, nil
}

// hasDerivedTiles reports whether any app names id as its parent — the other
// half of the "one level, never a chain" gate. validParentApp alone doesn't
// close it: without this, create A, create tile T under A, then PATCH A to
// point at P leaves T a grandchild. apps_derived_shape_ck bounds today's
// consequence to T being unlaunchable, but Phase 4's reconciler walks
// parent_app_id and a chain is the resurrection-loop shape.
func (s *store) hasDerivedTiles(ctx context.Context, id string) (bool, error) {
	var has bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM apps WHERE parent_app_id::text = $1)`, id).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("check derived tiles: %w", err)
	}
	return has, nil
}

// parentLaunchDefaults is a parent's launch-profile config, copied onto a
// derived tile once at creation (§1.2), then owned by the admin.
type parentLaunchDefaults struct {
	DefaultProfileID *string
	ProfilePolicy    string
}

// parentLaunchDefaults reads the fields a new tile inherits from its parent.
// Must be copied here, not coalesced at launch: GetLaunchApp deliberately never
// coalesces them (that would undo per-tile edits), so if not copied at create
// they're never copied at all. Phase 4's reconciler (§7.7) inherits this
// behaviour rather than reimplementing it.
func (s *store) parentLaunchDefaults(ctx context.Context, parentID string) (parentLaunchDefaults, error) {
	var out parentLaunchDefaults
	err := s.pool.QueryRow(ctx,
		`SELECT default_profile_id, profile_policy FROM apps WHERE id::text = $1`, parentID,
	).Scan(&out.DefaultProfileID, &out.ProfilePolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return parentLaunchDefaults{}, ErrNotFound
	}
	if err != nil {
		return parentLaunchDefaults{}, fmt.Errorf("read parent launch defaults: %w", err)
	}
	return out, nil
}

// DerivedTile names one tile of a provider app, for the delete-confirmation 409.
type DerivedTile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// derivedTilesOf lists the tiles whose parent is id, newest first, capped.
func (s *store) derivedTilesOf(ctx context.Context, id string) ([]DerivedTile, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name FROM apps WHERE parent_app_id::text = $1
		ORDER BY name LIMIT 200
	`, id)
	if err != nil {
		return nil, fmt.Errorf("list derived tiles: %w", err)
	}
	defer rows.Close()
	tiles := []DerivedTile{}
	for rows.Next() {
		var t DerivedTile
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, fmt.Errorf("scan derived tile: %w", err)
		}
		tiles = append(tiles, t)
	}
	return tiles, rows.Err()
}

// listAllApps returns all apps (enabled + disabled) for admin use, with
// runtime_spec. callerID resolves `favourite` scoped to the acting admin's own
// favourites.
//
// Deliberately not entitlement-filtered (§6.5): this is the admin god view,
// which is exactly what lets GET /v1/apps stay filtered for admins too. An
// admin "cannot see an app" is fixed by granting the entitlement, never by
// adding a role arm to entitledSQL.
func (s *store) listAllApps(ctx context.Context, callerID, cursor string, limit int32) ([]App, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	var offset int64
	fmt.Sscanf(cursor, "%d", &offset)

	rows, err := s.pool.Query(ctx, `
			SELECT apps.id::text, name, description, cover_url, hero_url, kind,
		       external_source, external_id,
		       -- Phase 3. parent_app_id is public (§2.2); origin and library_provider
		       -- are operator provenance/configuration and stay on the admin shape.
		       apps.parent_app_id::text, origin, library_provider,
		       -- #534: WHY this app is disabled when the operator did not disable it.
		       library_discovery_suspended,
		       default_vram_mb, default_encode_slots,
		       default_width, default_height, default_fps, default_bitrate_kbps,
		       enabled, default_profile_id, profile_policy, runtime_spec, managed_home, home_container_path,
		       apps.runtime_preset_id::text,
		       -- UI-P5 allow-list. ARRAY(...) rather than a join + array_agg so the
		       -- display_stream joins above keep their one-row-per-app shape.
		       ARRAY(SELECT alp.launch_profile_id FROM app_launch_profiles alp
		             WHERE alp.app_id = apps.id ORDER BY alp.launch_profile_id),
		       COALESCE(dp.width, default_width),
		       COALESCE(dp.height, default_height),
		       COALESCE(dp.fps, default_fps),
		       COALESCE(dp.nominal_bitrate_kbps, default_bitrate_kbps),
		       apps.created_at, apps.updated_at,
		       (fav.app_id IS NOT NULL),
		       -- sessions_30d (UI v3 amendment): every state counts, failures
		       -- included — a launch that failed is still a launch someone
		       -- attempted, and an admin ranking a catalogue wants attempts.
		       -- COALESCE, not the join's NULL: an app nobody launched is 0.
		       COALESCE(s30.n, 0)::int
		FROM apps
		-- display_stream (UI-P4): resolve the app's effective LAUNCH PROFILE, then
		-- its TOP RUNG (position 1), and COALESCE to the app defaults when nothing
		-- resolves. The "profile_policy <> custom" join conditions are gone with
		-- the custom policy itself (migration 0036 narrowed the CHECK).
		--
		-- The COALESCE to apps.default_* is LOAD-BEARING, not vestigial: with no
		-- global default set (Tower shipped state) an inherit app resolves no
		-- profile at all, and the app defaults are the only thing left to display.
		-- Dropping those columns would render the whole library as zeros.
		LEFT JOIN stream_profile_policy spp ON true
		LEFT JOIN launch_profile_rungs dpr ON dpr.position = 1 AND dpr.launch_profile_id = CASE
			WHEN apps.profile_policy IN ('prefer', 'force') AND apps.default_profile_id IS NOT NULL THEN apps.default_profile_id
			ELSE spp.global_default_profile_id
		END
		LEFT JOIN stream_profiles dp ON dp.id = dpr.stream_profile_id
		LEFT JOIN user_app_favourites fav ON fav.app_id = apps.id AND fav.user_id = $3::uuid
		-- sessions_30d, aggregated ONCE for the whole page rather than per row: a
		-- correlated count re-scans the window for every app, which on a large
		-- catalogue is the fan-out this field exists to remove. Reads
		-- sessions_app_created_idx (migration 0070).
		LEFT JOIN (
		    SELECT app_id, count(*) AS n FROM sessions
		    WHERE created_at >= now() - interval '30 days'
		    GROUP BY app_id
		) s30 ON s30.app_id = apps.id
		ORDER BY apps.created_at DESC
		LIMIT $1 OFFSET $2
	`, limit+1, offset, callerID)
	if err != nil {
		return nil, "", fmt.Errorf("query all apps: %w", err)
	}
	defer rows.Close()

	var apps []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.CoverURL, &a.HeroURL, &a.Kind,
			&a.ExternalSource, &a.ExternalID,
			&a.ParentAppID, &a.Origin, &a.LibraryProvider,
			&a.LibraryDiscoverySuspended,
			&a.DefaultVramMB, &a.DefaultEncodeSlots,
			&a.DefaultWidth, &a.DefaultHeight, &a.DefaultFPS, &a.DefaultBitratekbps,
			&a.Enabled, &a.DefaultProfileID, &a.ProfilePolicy, &a.RuntimeSpec, &a.ManagedHome, &a.HomeContainerPath,
			&a.RuntimePresetID, &a.LaunchableProfileIDs,
			&a.DisplayWidth, &a.DisplayHeight, &a.DisplayFPS, &a.DisplayBitratekbps,
			&a.CreatedAt, &a.UpdatedAt, &a.Favourite, &a.Sessions30d); err != nil {
			return nil, "", fmt.Errorf("scan app: %w", err)
		}
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var nextCursor string
	if int32(len(apps)) > limit {
		apps = apps[:limit]
		nextCursor = fmt.Sprintf("%d", offset+int64(limit))
	}
	return apps, nextCursor, nil
}

// getAppFull returns an app by ID without the enabled filter (admin use).
// Includes runtime_spec — never returned to non-admin callers. callerID resolves
// `favourite` scoped to the acting caller's own favourites.
func (s *store) getAppFull(ctx context.Context, callerID, id string) (App, error) {
	var a App
	err := s.pool.QueryRow(ctx, `
			SELECT apps.id::text, name, description, cover_url, hero_url, kind,
		       external_source, external_id,
		       -- Phase 3. parent_app_id is public (§2.2); origin and library_provider
		       -- are operator provenance/configuration and stay on the admin shape.
		       apps.parent_app_id::text, origin, library_provider,
		       -- #534: WHY this app is disabled when the operator did not disable it.
		       library_discovery_suspended,
		       default_vram_mb, default_encode_slots,
		       default_width, default_height, default_fps, default_bitrate_kbps,
		       enabled, default_profile_id, profile_policy, runtime_spec, managed_home, home_container_path,
		       apps.runtime_preset_id::text,
		       -- UI-P5 allow-list; see listAllApps for why ARRAY(...) and not a join.
		       ARRAY(SELECT alp.launch_profile_id FROM app_launch_profiles alp
		             WHERE alp.app_id = apps.id ORDER BY alp.launch_profile_id),
		       COALESCE(dp.width, default_width),
		       COALESCE(dp.height, default_height),
		       COALESCE(dp.fps, default_fps),
		       COALESCE(dp.nominal_bitrate_kbps, default_bitrate_kbps),
		       apps.created_at, apps.updated_at,
		       (fav.app_id IS NOT NULL)
		FROM apps
		-- display_stream (UI-P4): resolve the app's effective LAUNCH PROFILE, then
		-- its TOP RUNG (position 1), and COALESCE to the app defaults when nothing
		-- resolves. The "profile_policy <> custom" join conditions are gone with
		-- the custom policy itself (migration 0036 narrowed the CHECK).
		--
		-- The COALESCE to apps.default_* is LOAD-BEARING, not vestigial: with no
		-- global default set (Tower shipped state) an inherit app resolves no
		-- profile at all, and the app defaults are the only thing left to display.
		-- Dropping those columns would render the whole library as zeros.
		LEFT JOIN stream_profile_policy spp ON true
		LEFT JOIN launch_profile_rungs dpr ON dpr.position = 1 AND dpr.launch_profile_id = CASE
			WHEN apps.profile_policy IN ('prefer', 'force') AND apps.default_profile_id IS NOT NULL THEN apps.default_profile_id
			ELSE spp.global_default_profile_id
		END
		LEFT JOIN stream_profiles dp ON dp.id = dpr.stream_profile_id
		LEFT JOIN user_app_favourites fav ON fav.app_id = apps.id AND fav.user_id = $2::uuid
		WHERE apps.id::text = $1
	`, id, callerID).Scan(&a.ID, &a.Name, &a.Description, &a.CoverURL, &a.HeroURL, &a.Kind,
		&a.ExternalSource, &a.ExternalID,
		&a.ParentAppID, &a.Origin, &a.LibraryProvider,
		&a.LibraryDiscoverySuspended,
		&a.DefaultVramMB, &a.DefaultEncodeSlots,
		&a.DefaultWidth, &a.DefaultHeight, &a.DefaultFPS, &a.DefaultBitratekbps,
		&a.Enabled, &a.DefaultProfileID, &a.ProfilePolicy, &a.RuntimeSpec, &a.ManagedHome, &a.HomeContainerPath,
		&a.RuntimePresetID, &a.LaunchableProfileIDs,
		&a.DisplayWidth, &a.DisplayHeight, &a.DisplayFPS, &a.DisplayBitratekbps,
		&a.CreatedAt, &a.UpdatedAt, &a.Favourite)
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, fmt.Errorf("query app: %w", err)
	}
	return a, nil
}

// listHosts returns all hosts with pagination.
func (s *store) listHosts(ctx context.Context, cursor string, limit int32) ([]Host, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}

	var offset int64
	fmt.Sscanf(cursor, "%d", &offset)

	rows, err := s.pool.Query(ctx, `
		SELECT id::text, node_name, status, agent_version, cpu_cores, mem_mb,
		       last_registered_at, last_heartbeat_at, storage, cpu_model,
		       readiness, readiness_reported_at,
		       capacity_detection, capacity_reason, created_at,
		       agent_process_started_at, agent_restart_count, agent_last_restart_at
		FROM hosts
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`, limit+1, offset)
	if err != nil {
		return nil, "", fmt.Errorf("query hosts: %w", err)
	}
	defer rows.Close()

	var hosts []Host
	for rows.Next() {
		var h Host
		var rawStorage, rawReadiness []byte
		if err := rows.Scan(&h.ID, &h.NodeName, &h.Status, &h.AgentVersion,
			&h.CPUCores, &h.MemMB, &h.LastRegistered, &h.LastHeartbeat, &rawStorage, &h.CPUModel,
			&rawReadiness, &h.ReadinessReportedAt,
			&h.CapacityDetection, &h.CapacityReason, &h.CreatedAt,
			&h.AgentConnectedSince, &h.AgentRestartCount, &h.AgentLastRestartAt); err != nil {
			return nil, "", fmt.Errorf("scan host: %w", err)
		}
		h.Storage = json.RawMessage(rawStorage) // nil scans to a JSON "null" (json.RawMessage.MarshalJSON)
		h.Readiness = json.RawMessage(rawReadiness)
		hosts = append(hosts, h)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var nextCursor string
	if int32(len(hosts)) > limit {
		hosts = hosts[:limit]
		nextCursor = fmt.Sprintf("%d", offset+int64(limit))
	}
	// After the page is trimmed, so the aggregate covers exactly what is served.
	if err := s.attachCapacities(ctx, hosts); err != nil {
		return nil, "", err
	}
	return hosts, nextCursor, nil
}

// getHost returns a single host by ID.
func (s *store) getHost(ctx context.Context, id string) (Host, error) {
	var h Host
	var rawStorage, rawReadiness []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, node_name, status, agent_version, cpu_cores, mem_mb,
		       last_registered_at, last_heartbeat_at, storage, cpu_model,
		       readiness, readiness_reported_at,
		       capacity_detection, capacity_reason, created_at,
		       agent_process_started_at, agent_restart_count, agent_last_restart_at
		FROM hosts WHERE id::text = $1
	`, id).Scan(&h.ID, &h.NodeName, &h.Status, &h.AgentVersion,
		&h.CPUCores, &h.MemMB, &h.LastRegistered, &h.LastHeartbeat, &rawStorage, &h.CPUModel,
		&rawReadiness, &h.ReadinessReportedAt,
		&h.CapacityDetection, &h.CapacityReason, &h.CreatedAt,
		&h.AgentConnectedSince, &h.AgentRestartCount, &h.AgentLastRestartAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Host{}, ErrNotFound
	}
	if err != nil {
		return Host{}, fmt.Errorf("query host: %w", err)
	}
	h.Storage = json.RawMessage(rawStorage) // nil scans to a JSON "null" (json.RawMessage.MarshalJSON)
	h.Readiness = json.RawMessage(rawReadiness)
	caps, err := s.hostCapacities(ctx, []string{h.ID})
	if err != nil {
		return Host{}, err
	}
	if c, ok := caps[h.ID]; ok {
		h.Capacity = &c
	}
	return h, nil
}

// deleteApp hard-deletes an app in one transaction: 404 if absent, 409 on any
// non-terminal session (ErrAppHasActiveSessions), tombstone its managed homes
// for GC, then DELETE — terminal sessions cascade (migration 0014), home rows'
// app_id is SET NULL (migration 0009). Returns the deleted name for the audit
// record; the existence check doubles as that read.
//
// deleteDerived is the caller's explicit opt-in (§4.1) to taking derived tiles
// with it. The FK (apps_parent_external_uk, ON DELETE CASCADE) is an integrity
// backstop, not a UX one — deleting a provider app silently cascades every
// derived tile and their favourites/artwork, irreversibly. So the API refuses
// first and lists what would go (ErrAppHasDerivedTiles); only an explicit
// "yes, those too" proceeds.
func (s *store) deleteApp(ctx context.Context, id string, deleteDerived bool) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin delete app tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	var name string
	err = tx.QueryRow(ctx, `SELECT name FROM apps WHERE id::text = $1`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("check app exists: %w", err)
	}

	// Counted inside the transaction so a tile created between the handler's
	// listing call and this delete is still caught.
	if !deleteDerived {
		var tiles int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM apps WHERE parent_app_id::text = $1`, id).Scan(&tiles); err != nil {
			return "", fmt.Errorf("count derived tiles: %w", err)
		}
		if tiles > 0 {
			return "", ErrAppHasDerivedTiles
		}
	}

	// Also refuses on a session against a derived tile: without this,
	// deleteDerived=true would cascade a tile out from under a running session.
	var active int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM sessions s
		JOIN apps a ON a.id = s.app_id
		WHERE (a.id::text = $1 OR a.parent_app_id::text = $1)
		  AND s.state NOT IN ('stopped','failed')
	`, id).Scan(&active); err != nil {
		return "", fmt.Errorf("count active sessions for app: %w", err)
	}
	if active > 0 {
		return "", ErrAppHasActiveSessions
	}

	if _, err := tx.Exec(ctx,
		`UPDATE user_homes SET gc_after = now() WHERE app_id::text = $1 AND gc_after IS NULL`, id); err != nil {
		return "", fmt.Errorf("tombstone app homes: %w", err)
	}

	// Delete the app; terminal sessions cascade (migration 0014).
	tag, err := tx.Exec(ctx, `DELETE FROM apps WHERE id::text = $1`, id)
	if err != nil {
		return "", fmt.Errorf("delete app: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return name, nil
}

// deleteHost hard-deletes a host in one transaction: 404 if absent, 409 on any
// non-terminal session (ErrHostHasActiveSessions), tombstone managed homes,
// then DELETE — GPU records and terminal sessions cascade (migration 0014).
// Caller must verify the host isn't connected in the agent registry first (the
// handler layer, where the registry is available). Returns node_name for the
// audit record.
func (s *store) deleteHost(ctx context.Context, id string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin delete host tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	var nodeName string
	err = tx.QueryRow(ctx, `SELECT node_name FROM hosts WHERE id::text = $1`, id).Scan(&nodeName)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("check host exists: %w", err)
	}

	var active int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE host_id::text = $1 AND state NOT IN ('stopped','failed')
	`, id).Scan(&active); err != nil {
		return "", fmt.Errorf("count active sessions for host: %w", err)
	}
	if active > 0 {
		return "", ErrHostHasActiveSessions
	}

	if _, err := tx.Exec(ctx,
		`UPDATE user_homes SET gc_after = now() WHERE host_id::text = $1 AND gc_after IS NULL`, id); err != nil {
		return "", fmt.Errorf("tombstone host homes: %w", err)
	}

	// Delete the host; GPU records and terminal sessions cascade (migration 0014).
	tag, err := tx.Exec(ctx, `DELETE FROM hosts WHERE id::text = $1`, id)
	if err != nil {
		return "", fmt.Errorf("delete host: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return nodeName, nil
}
