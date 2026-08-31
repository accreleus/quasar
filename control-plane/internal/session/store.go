package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// Lifecycle sentinels. Status codes and remedies are control-api.md §Errors;
// the notes here are only what the mapping cannot show.
var (
	ErrNotFound = errors.New("not found")
	// No online GPU whose TOTALS could ever serve the launch: "nothing could",
	// not "everything is busy". 503 no_host_available.
	ErrNoHostAvailable = errors.New("no host available")
	// Totals suffice but derived availability does not. 503, retryable.
	ErrCapacityExhausted    = errors.New("capacity exhausted")
	ErrSessionQuotaExceeded = errors.New("session quota exceeded") // 409, retryable
	ErrProfileUnknown       = errors.New("unknown stream profile") // 400
	// Hard eligibility failure, or a non-user-facing profile without an
	// admin/explicit override. 409; the override is the documented escape.
	ErrProfileIneligible = errors.New("stream profile not eligible")
	// Not running, or a swap already in progress. 409.
	ErrSessionNotSwappable = errors.New("session not swappable")
	// The swap target needs more than the held reservation; swaps never resize. 409.
	ErrSwapExceedsReservation = errors.New("swap exceeds reservation")
	// Not permitted by the state machine (see CanTransition).
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrHostNotDrainable  = errors.New("host not drainable") // offline: nothing to drain
	// Offline: its agent is not connected, and it returns online on reconnect.
	ErrHostNotResumable = errors.New("host not resumable")
	// The per-(user, home) single-writer invariant (P5-04). 409, retryable.
	ErrHomeInUse = errors.New("home already in use")
	// A derived tile launched with no live user_homes row for its PARENT on any
	// host. A refusal, never a provision: creating one would mount an empty
	// directory and reach `running` looking healthy.
	ErrHomeNotProvisioned = errors.New("home not provisioned for this app")
	// A tile borrows the parent's image, runtime, mounts and home, so launching
	// one IS running the parent and `enabled = false` must stop it.
	ErrParentDisabled = errors.New("the provider app this tile launches through is disabled")
	// Neither an ('all') nor a ('user', them) entitlement (§6.3). 403, not a 404
	// existence-leak: the caller already named a specific app. The authorization
	// boundary — the filtered GET /v1/apps is UX, and no role skips this (§6.5).
	ErrNotEntitled = errors.New("not entitled to this app")
	// A stream.codec override naming a codec the placed host cannot produce. The
	// client-decode clamp is overridable; host encoder capability is not, and a
	// doomed assignment must fail at launch rather than late at the agent.
	ErrCodecUnsupportedByHost = errors.New("codec not supported by host encoder")
)

// HomeInUseError names the session already holding the home (§2.1/§2.2). It
// unwraps to ErrHomeInUse, so errors.Is and the status mapping are unchanged.
// The id earns a type because one lock is held by the PARENT's home, so a whole
// tile family 409s and a user who clicked a game would otherwise see only a
// generic failure.
type HomeInUseError struct {
	// SessionID is never empty when this type is returned; a guard that cannot
	// name the conflict returns plain ErrHomeInUse.
	SessionID string
}

func (e *HomeInUseError) Error() string {
	return fmt.Sprintf("%v (session %s)", ErrHomeInUse, e.SessionID)
}

func (e *HomeInUseError) Unwrap() error { return ErrHomeInUse }

// ParentDisabledError carries the disabled parent's id and NAME, so the 409 can
// say which app to re-enable; a uuid is not actionable. Unwraps to
// ErrParentDisabled.
type ParentDisabledError struct {
	ParentAppID string
	ParentName  string
}

func (e *ParentDisabledError) Error() string {
	if e.ParentName != "" {
		return fmt.Sprintf("%v (%s)", ErrParentDisabled, e.ParentName)
	}
	return ErrParentDisabled.Error()
}

func (e *ParentDisabledError) Unwrap() error { return ErrParentDisabled }

// disabledParentName is "" when err is not a parent-disabled refusal.
func disabledParentName(err error) string {
	var pd *ParentDisabledError
	if errors.As(err, &pd) {
		return pd.ParentName
	}
	return ""
}

// conflictingSessionID is "" when err is not a home-in-use conflict naming one.
func conflictingSessionID(err error) string {
	var hiu *HomeInUseError
	if errors.As(err, &hiu) {
		return hiu.SessionID
	}
	return ""
}

// Store is the session data-access layer over the pgx pool.
type Store struct {
	pool   *pgxpool.Pool
	policy PlacementPolicy // default PolicySpread
	// vram is the live free-VRAM veto tuning (#383). The zero value has the veto
	// OFF, so a Store built without WithVramAdmission is fail-open, slots-only.
	vram VramAdmission
	// tel is a separate module on the same pool: this Store owns the sessions
	// table and the trust boundary, internal/telemetry owns observability storage
	// and its retention.
	tel telemetry.Store
}

// NewStore defaults to PolicySpread with the live free-VRAM veto disabled.
func NewStore(pool *pgxpool.Pool, opts ...StoreOption) *Store {
	s := &Store{pool: pool, tel: telemetry.Postgres(pool)}
	for _, opt := range opts {
		opt(s)
	}
	// Normalize unconditionally, not just inside WithVramAdmission: the spread
	// ordering's freshness gate reads the staleness window even with the veto off,
	// and `make_interval(secs => 0)` would mark every sample stale.
	s.vram = s.vram.normalize()
	return s
}

// Telemetry is the one telemetry surface; handlers and the coordinator go
// through it rather than session-shaped forwarding methods.
func (s *Store) Telemetry() telemetry.Store { return s.tel }

// LaunchApp is the scheduler/agent-facing view of an app row, resolved at launch.
// runtime_spec is opaque to the scheduler (agent-internal); the resource columns
// are what the scheduler reserves.
type LaunchApp struct {
	ID string
	// ParentAppID (migration 0044) is the provider app a DERIVED TILE borrows its
	// runtime from, "" for an ordinary app. When set, every executable field below
	// is ALREADY the parent's by the time GetLaunchApp returns.
	//
	// Never read it to re-derive the storage key; call homeAppID.
	ParentAppID string
	// The app's provider identity ("steam", appid). For a derived tile ExternalID
	// is the appid composeSteamFlags renders into STEAM_STARTUP_FLAGS, and
	// apps_derived_shape_ck guarantees both are non-empty. Observability only on
	// an ordinary app.
	ExternalSource     string
	ExternalID         string
	RuntimeSpec        json.RawMessage
	DefaultVramMB      int32
	DefaultEncodeSlots int32
	DefaultWidth       int32
	DefaultHeight      int32
	DefaultFPS         int32
	DefaultBitrateKbps int32
	DefaultProfileID   *string
	ProfilePolicy      string
	// Managed-home opt-in (apps.managed_home / apps.home_container_path).
	ManagedHome       bool
	HomeContainerPath string
	// RuntimePresetID is the preset this app inherited from, nil when it carries
	// everything itself (the byte-identical path). Logging only: the fields above
	// are already the merged effective values when GetLaunchApp returns.
	RuntimePresetID *string
}

// Session is the domain view of a sessions row (the fields the lifecycle and the
// control-api responses need).
type Session struct {
	ID           string
	UserID       string
	AppID        string
	HostID       *string
	GPUID        *string
	GPUIndex     *int32 // local index of the reserved GPU; not persisted on the row
	State        State
	StateDetail  *string
	ErrorMessage *string
	// FailureCode (migration 0062) is the agent's machine-readable classification
	// of a terminal failure. It sits beside ErrorMessage, which stays operator
	// prose: the UI branches on this and reads that.
	FailureCode *string
	// AppLogTail (migration 0062) is the app container's last ~100 log lines,
	// newline-joined. The ONLY copy: app containers run with `--rm` (#463).
	AppLogTail  *string
	Width       int32
	Height      int32
	FPS         int32
	BitrateKbps int32
	H264Profile string
	// Codec is the session's resolved video codec (wire vocabulary), default
	// "h264". H264Profile applies to h264 only; the agent ignores it otherwise.
	Codec string
	// ProfileID is the LAUNCH PROFILE the session came from, nil for a
	// legacy/tier/override/console launch. It answers "what did the user pick".
	ProfileID *string
	// StreamProfileID is the RUNG the launch resolved to (migration 0036), nil
	// when none was resolved. It answers "what did they get", and since a rung
	// carries its own resolution the two can legitimately disagree — the concrete
	// Width/Height/FPS/BitrateKbps above are always the truth for a live session.
	StreamProfileID *string
	// CodecDecision (migration 0038) records HOW the rung above was resolved:
	// every rung walked, its rejecting clamp, and whether the dispatched one won
	// on merit, was forced, or is the floor. nil when no chain was walked.
	// Observability only; nothing reads it back server-side. Shape:
	// codec_decision.go's codecDecisionDoc.
	CodecDecision json.RawMessage
	// NegotiatedCodec is the wire codec the BROWSER reports decoding, normalised
	// at ingest, nil until reported. Disagreement with Codec is a silent-fallback
	// or mis-negotiated-m-line signal, which is why both are kept.
	NegotiatedCodec *string
	// Mic is the GRANTED state (migration 0049): request AND instance setting at
	// launch, sent as session_assign.stream.mic.
	Mic           bool
	ReservedVram  int32
	ReservedSlots int32
	Playout0Ms    int32
	// Computed stream health, defaulting to "healthy", with an optional reason and
	// the timestamp of the last transition.
	HealthState     HealthState
	HealthReason    *string
	HealthChangedAt time.Time
	CreatedAt       time.Time
	AssignedAt      *time.Time
	StartedAt       *time.Time
	EndedAt         *time.Time
}

// IsEntitled is a hand-copy of entitledSQL (internal/crud/store.go, the
// definition of record; session cannot import crud). The NON-transactional form,
// for the three sites with no transaction to sit inside: swapper.Swap,
// LaunchByProfile's ordering pre-check, and GET /v1/me/profiles?app_id=…
//
// NOT the authorization boundary, and no caller may treat it as one — that is
// scheduleAttempt's copy with FOR SHARE, inside the transaction that inserts the
// session. Never simplify the launch path onto it.
//
// Swap is checked even though §6.3 does not list it: swap replaces the app a
// live session runs, so without it the gate is defeated in two requests. Keyed
// on the SESSION OWNER, so an admin cannot launder their own entitlements into
// someone else's session.
func (s *Store) IsEntitled(ctx context.Context, userID, appID string) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM entitlements e
			WHERE e.app_id = $1::uuid
			  AND (e.subject_type = 'all'
			       OR (e.subject_type = 'user' AND e.subject_id = $2::uuid))
		)`, appID, userID).Scan(&ok); err != nil {
		return false, fmt.Errorf("check entitlement: %w", err)
	}
	return ok, nil
}

// logIfDiscoverySuspended names the cause of the one genuinely misleading
// `404 app not found` (#534), in the log only. The response must stay a bare
// 404: POST /v1/sessions is the authorization boundary (control-api.md §6.5) and
// may never distinguish "does not exist" from "exists but you cannot have it".
// One extra query, on the 404 path only.
func (s *Store) logIfDiscoverySuspended(ctx context.Context, appID string) {
	var name string
	err := s.pool.QueryRow(ctx, `
		SELECT name FROM apps
		 WHERE id = $1::uuid AND enabled = false AND library_discovery_suspended = true
	`, appID).Scan(&name)
	if err != nil {
		return // absent, not suspended, or unreadable: nothing extra to say
	}
	slog.Warn("launch refused for a provider app the library-discovery reconciler suspended; "+
		"set library_discovery_enabled=true (PATCH /v1/admin/settings) to restore it",
		"app_id", appID, "app", name)
}

// GetLaunchApp resolves an enabled app for launch; ErrNotFound if
// absent/disabled. It is the only app-resolution step on every dispatch path, so
// the preset merge and the derived-tile resolution both belong here and
// everything downstream reads already-merged effective values.
//
// An app with no preset takes the untouched-bytes path: the LEFT JOIN yields no
// preset row and a.RuntimeSpec is the stored JSONB verbatim, with no
// decode/re-encode. Guarded by TestRuntimeSpecUnchangedWithoutPreset.
//
// A tile resolves its EFFECTIVE runtime app through the self-join, so
// RuntimeSpec/ManagedHome/HomeContainerPath and every resource and stream
// default come from the parent while name/artwork/entitlements/profile stay the
// tile's. The preset joins on the EFFECTIVE app's runtime_preset_id (a tile's own
// is NULL by apps_derived_shape_ck), so editing the parent changes every tile's
// next launch. A non-derived app is unaffected: no parent row, and every COALESCE
// takes its second argument.
func (s *Store) GetLaunchApp(ctx context.Context, appID string) (LaunchApp, error) {
	if !isValidUUID(appID) {
		return LaunchApp{}, ErrNotFound
	}
	var a LaunchApp
	var (
		parentID      *string
		presetID      *string
		presetImage   *string
		presetArgs    json.RawMessage
		presetEnv     json.RawMessage
		presetMounts  json.RawMessage
		presetHome    *bool
		presetPath    *string
		presetNetwork *string
	)
	var (
		parentEnabled *bool
		parentName    *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT apps.id::text, apps.parent_app_id::text,
		       parent.enabled, parent.name,
		       apps.external_source, apps.external_id,
		       COALESCE(parent.runtime_spec,          apps.runtime_spec),
		       COALESCE(parent.default_vram_mb,       apps.default_vram_mb),
		       COALESCE(parent.default_encode_slots,  apps.default_encode_slots),
		       COALESCE(parent.default_width,         apps.default_width),
		       COALESCE(parent.default_height,        apps.default_height),
		       COALESCE(parent.default_fps,           apps.default_fps),
		       COALESCE(parent.default_bitrate_kbps,  apps.default_bitrate_kbps),
		       COALESCE(parent.managed_home,          apps.managed_home),
		       COALESCE(parent.home_container_path,   apps.home_container_path),
		       -- NOT coalesced to the parent: which launch profile the user gets is
		       -- presentation/curation and lives on the tile (spec §1.2). It is
		       -- copied from the parent ONCE at tile creation and owned by the admin
		       -- thereafter, so reading it from the parent here would silently undo
		       -- every per-tile edit.
		       apps.default_profile_id, apps.profile_policy,
		       rp.id::text, rp.image, rp.args, rp.env, rp.mounts, rp.managed_home, rp.home_container_path,
		       rp.network
		FROM apps
		-- The effective runtime app. Self-join, LEFT so an ordinary app is unchanged.
		LEFT JOIN apps parent ON parent.id = apps.parent_app_id
		-- Joined on the EFFECTIVE app's preset: a derived tile's own
		-- runtime_preset_id is NULL by CHECK, so this is the parent's or nothing.
		LEFT JOIN runtime_presets rp ON rp.id = COALESCE(parent.runtime_preset_id, apps.runtime_preset_id)
		WHERE apps.id = $1::uuid AND apps.enabled = true
	`, appID).Scan(&a.ID, &parentID,
		&parentEnabled, &parentName,
		&a.ExternalSource, &a.ExternalID,
		&a.RuntimeSpec,
		&a.DefaultVramMB, &a.DefaultEncodeSlots,
		&a.DefaultWidth, &a.DefaultHeight, &a.DefaultFPS, &a.DefaultBitrateKbps,
		&a.ManagedHome, &a.HomeContainerPath, &a.DefaultProfileID, &a.ProfilePolicy,
		&presetID, &presetImage, &presetArgs, &presetEnv, &presetMounts, &presetHome, &presetPath,
		&presetNetwork)
	if errors.Is(err, pgx.ErrNoRows) {
		s.logIfDiscoverySuspended(ctx, appID)
		return LaunchApp{}, ErrNotFound
	}
	if err != nil {
		return LaunchApp{}, fmt.Errorf("query app: %w", err)
	}
	a.ParentAppID = deref(parentID)

	// A disabled parent blocks its tiles — a documented deviation from §1.2's
	// table, which says which ROW a field is read from, not that the parent's
	// `enabled` is irrelevant: a tile has no independent existence, so an operator
	// taking the provider out of service would otherwise have every tile keep
	// launching that image against that home.
	//
	// Checked in Go, not as `AND parent.enabled` in the WHERE: a WHERE clause
	// collapses this into the same no-rows result as "no such app". Selecting
	// enabled+name costs the same query and yields a 409 naming the app.
	if parentEnabled != nil && !*parentEnabled {
		return LaunchApp{}, &ParentDisabledError{
			ParentAppID: a.ParentAppID,
			ParentName:  deref(parentName),
		}
	}

	// Validate the app's OWN runtime_spec.network above the no-preset return: a
	// malformed or disallowed value must fail the launch with a named error
	// whether or not a preset is attached, rather than being silently overwritten
	// by one or failing late in the agent's session_assign deserialization.
	// Read-only, so the byte-identical guarantee below is untouched.
	if err := validateRuntimeNetwork(a.RuntimeSpec); err != nil {
		return LaunchApp{}, fmt.Errorf("app %s: %w", appID, err)
	}

	if presetID == nil {
		return a, nil // stored spec returned verbatim
	}

	p := runtimePreset{
		ID:                *presetID,
		Image:             deref(presetImage),
		Args:              presetArgs,
		Env:               presetEnv,
		Mounts:            presetMounts,
		ManagedHome:       presetHome != nil && *presetHome,
		HomeContainerPath: deref(presetPath),
		Network:           deref(presetNetwork),
	}
	merged, err := mergeRuntimePreset(a.RuntimeSpec, p)
	if err != nil {
		return LaunchApp{}, fmt.Errorf("merge runtime preset %s: %w", p.ID, err)
	}
	a.RuntimeSpec = merged
	a.ManagedHome, a.HomeContainerPath = mergeManagedHome(a.ManagedHome, a.HomeContainerPath, p)
	a.RuntimePresetID = presetID
	return a, nil
}

// Get returns a session by id. ErrNotFound if absent.
func (s *Store) Get(ctx context.Context, id string) (Session, error) {
	if !isValidUUID(id) {
		return Session{}, ErrNotFound
	}
	return scanSession(s.pool.QueryRow(ctx, selectSessionSQL+` WHERE id = $1::uuid`, id))
}

// ListByUser returns a user's sessions, newest first, with offset-cursor
// pagination (matching the crud store's scheme).
func (s *Store) ListByUser(ctx context.Context, userID, cursor string, limit int32) ([]Session, string, error) {
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
	if !isValidUUID(userID) {
		return nil, "", nil
	}

	rows, err := s.pool.Query(ctx, selectSessionSQL+`
		WHERE user_id = $1::uuid
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, userID, limit+1, offset)
	if err != nil {
		return nil, "", fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		sess, err := scanSessionRow(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var next string
	if int32(len(out)) > limit {
		out = out[:limit]
		next = fmt.Sprintf("%d", offset+int64(limit))
	}
	return out, next, nil
}

// AdminSessionRow is a session plus the display names of the rows it references,
// for the admin oversight surface only. Every name is a pointer because the join
// that resolves it is a LEFT JOIN — see ListAll.
type AdminSessionRow struct {
	Session
	// Username is users.username; nil when the user row is gone.
	Username *string
	// AppName is apps.name; nil when the app row is gone.
	AppName *string
	// HostName is hosts.node_name; nil while the session is unassigned and when
	// the host row is gone. The one name routinely absent in normal operation.
	HostName *string
}

// ListAll returns all sessions across users, newest first, paginated, each
// carrying its user / app / host display names, for the admin oversight view.
//
// The join must stay layered rather than folded into `selectSessionSQL`: four
// read paths share that constant and scanSessionRow consumes it POSITIONALLY, so
// widening it changes what the others select and a scanner one column out of
// step mis-assigns rather than failing. The name columns are appended by the
// outer SELECT and taken as trailing variadic dests.
//
// LEFT, not inner: a session whose app or host row was deleted must still list
// with a null name rather than vanish from an operator's view. LIMIT/OFFSET stay
// INSIDE the subquery so the join touches one page, and the ORDER BY is repeated
// outside because a join does not preserve subquery ordering.
// nonTerminalStates backs ?state=active. One state wider than activeStatesSQL
// (resource.go, the reservation-holding set): `pending` holds no GPU but is a
// session an operator can see and stop. Do not substitute one for the other.
// semantics: control-api.md §UI v3 console
var nonTerminalStates = []string{"pending", "assigned", "starting", "running", "stopping"}

// AdminStateFilter is the optional ?state= narrowing on ListAll; AdminStateAll
// filters nothing, so an absent parameter takes the pre-amendment path.
type AdminStateFilter string

const (
	AdminStateAll    AdminStateFilter = "all"
	AdminStateActive AdminStateFilter = "active"
	AdminStateEnded  AdminStateFilter = "ended"
	AdminStateFailed AdminStateFilter = "failed"
)

// ParseAdminStateFilter maps the query vocabulary onto the filter. "" is `all`;
// an unrecognized value is refused, never silently widened to `all`.
func ParseAdminStateFilter(v string) (AdminStateFilter, bool) {
	switch AdminStateFilter(v) {
	case "":
		return AdminStateAll, true
	case AdminStateAll, AdminStateActive, AdminStateEnded, AdminStateFailed:
		return AdminStateFilter(v), true
	}
	return "", false
}

// states returns the SQL predicate's state list, or nil for "no filter".
func (f AdminStateFilter) states() []string {
	switch f {
	case AdminStateActive:
		return nonTerminalStates
	case AdminStateEnded:
		return []string{string(StateStopped)}
	case AdminStateFailed:
		return []string{string(StateFailed)}
	}
	return nil
}

// ListAll pages the whole fleet's sessions, newest first. The filter must stay
// inside the paged subquery: the limit+1 lookahead has to count filtered rows,
// or next_cursor pages past the end of a narrow filter.
func (s *Store) ListAll(ctx context.Context, cursor string, limit int32, filter AdminStateFilter) ([]AdminSessionRow, string, error) {
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

	// $3 NULL for `all` keeps this one statement with no concatenated filter value.
	states := filter.states()
	rows, err := s.pool.Query(ctx, `
		SELECT s.*, u.username, a.name, h.node_name
		FROM (`+selectSessionSQL+`
			WHERE ($3::text[] IS NULL OR state = ANY($3::text[]))
			ORDER BY created_at DESC
			LIMIT $1 OFFSET $2
		) s
		LEFT JOIN users u ON u.id = s.user_id::uuid
		LEFT JOIN apps  a ON a.id = s.app_id::uuid
		LEFT JOIN hosts h ON h.id = s.host_id::uuid
		ORDER BY s.created_at DESC
	`, limit+1, offset, states)
	if err != nil {
		return nil, "", fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var out []AdminSessionRow
	for rows.Next() {
		var r AdminSessionRow
		sess, err := scanSessionRow(rows, &r.Username, &r.AppName, &r.HostName)
		if err != nil {
			return nil, "", err
		}
		r.Session = sess
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var next string
	if int32(len(out)) > limit {
		out = out[:limit]
		next = fmt.Sprintf("%d", offset+int64(limit))
	}
	return out, next, nil
}

// Transition moves a session to `to` in one transaction, validated against the
// state machine. It stamps started_at on running and ended_at on a terminal
// transition; the reservation releases implicitly, since availability derives
// from the active-state filter (State.HoldsReservation). errMsg applies only on
// failed. ErrInvalidTransition if the move is not permitted, ErrNotFound if the
// row is gone; a same-state report is an idempotent no-op.
func (s *Store) Transition(ctx context.Context, id string, to State, detail, errMsg *string) (Session, error) {
	if !isValidUUID(id) {
		return Session{}, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var cur State
	err = tx.QueryRow(ctx, `SELECT state FROM sessions WHERE id = $1::uuid FOR UPDATE`, id).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("lock session: %w", err)
	}

	// Teardown-race coercion (see CoerceReport): a `failed` report on an already
	// `stopping` row is a clean stop. Record `stopped`, keep non-SCTP evidence in
	// state_detail, discard the expected SCTP teardown signature, and clear errMsg
	// so the session is not user-visibly a failure.
	if eff := CoerceReport(cur, to); eff != to {
		if errMsg != nil && *errMsg != "" && detail == nil && !isExpectedSCTPTeardown(*errMsg) {
			detail = errMsg
		}
		to = eff
		errMsg = nil
	}

	if !CanTransition(cur, to) {
		return Session{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, cur, to)
	}

	// Same-state reports update detail without churning lifecycle timestamps,
	// carrying launch sub-stages while the state stays `starting`.
	if cur == to && detail != nil {
		_, err = tx.Exec(ctx, `UPDATE sessions SET state_detail = $2 WHERE id = $1::uuid`, id, detail)
		if err != nil {
			return Session{}, fmt.Errorf("update state detail: %w", err)
		}
	} else if cur != to {
		_, err = tx.Exec(ctx, `
			UPDATE sessions SET
			    state         = $2,
			    state_detail  = COALESCE($3, state_detail),
			    error_message = CASE WHEN $2 = 'failed' THEN $4 ELSE error_message END,
			    started_at    = CASE WHEN $2 = 'running' AND started_at IS NULL THEN now() ELSE started_at END,
			    ended_at      = CASE WHEN $2 IN ('stopped','failed') AND ended_at IS NULL THEN now() ELSE ended_at END
			WHERE id = $1::uuid
		`, id, string(to), detail, errMsg)
		if err != nil {
			return Session{}, fmt.Errorf("update state: %w", err)
		}
	}

	sess, err := scanSession(tx.QueryRow(ctx, selectSessionSQL+` WHERE id = $1::uuid`, id))
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, fmt.Errorf("commit: %w", err)
	}
	return sess, nil
}

func isExpectedSCTPTeardown(message string) bool {
	return strings.Contains(strings.ToLower(message), "sctp association")
}

// SetFailureDetail records `failure_code` and the app container's log tail;
// either may be nil, and COALESCE leaves an unreported column untouched.
//
// Not state-guarded, and must not be: the agent reports this in the callback
// that carries the terminal state, so the live-session updaters' `state NOT IN
// ('stopped','failed')` predicate would match nothing and discard every log
// tail. Addressed by session id and idempotent.
func (s *Store) SetFailureDetail(ctx context.Context, id string, failureCode, appLogTail *string) error {
	if !isValidUUID(id) {
		return ErrNotFound
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions
		SET failure_code = COALESCE($2, failure_code),
		    app_log_tail = COALESCE($3, app_log_tail)
		WHERE id = $1::uuid
	`, id, failureCode, appLogTail)
	if err != nil {
		return fmt.Errorf("set session failure detail: %w", err)
	}
	return nil
}

// SetStateDetail updates state_detail without changing the top-level state; a
// swap rides as a detail within `running`. Applies only while non-terminal: a
// terminal session is immutable.
func (s *Store) SetStateDetail(ctx context.Context, id, detail string) error {
	if !isValidUUID(id) {
		return nil // no such row to update; mirrors the 0-rows-affected no-op below
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET state_detail = $2
		WHERE id = $1::uuid AND state NOT IN ('stopped','failed')
	`, id, detail)
	if err != nil {
		return fmt.Errorf("set state_detail: %w", err)
	}
	return nil
}

// CommitSwappedApp sets app_id and state_detail in one statement, leaving the
// state `running` and the reservation unchanged. Applies only while `running`.
func (s *Store) CommitSwappedApp(ctx context.Context, id, newAppID, detail string) error {
	if !isValidUUID(id) {
		return nil // no such row to update; mirrors the 0-rows-affected no-op below
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET app_id = $2::uuid, state_detail = $3
		WHERE id = $1::uuid AND state = 'running'
	`, id, newAppID, detail)
	if err != nil {
		return fmt.Errorf("commit swapped app: %w", err)
	}
	return nil
}

// ReapHost fails every non-terminal session on a host, releasing each
// reservation, in one statement. The load-bearing failure edge (schema.md
// invariant #3): when an agent connection drops, the control plane is the
// authority of last resort and must not depend on a callback a dead agent
// cannot send. state_detail = 'host_lost' so the client can say so. Returns the
// number failed.
func (s *Store) ReapHost(ctx context.Context, hostID, reason string) (int64, error) {
	if !isValidUUID(hostID) {
		return 0, nil // no such host's sessions to reap
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions SET
		    state         = 'failed',
		    state_detail  = 'host_lost',
		    error_message = $2,
		    ended_at      = now()
		WHERE host_id = $1::uuid
		  AND state NOT IN ('stopped','failed')
	`, hostID, reason)
	if err != nil {
		return 0, fmt.Errorf("reap host sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Host mirrors the schema.md `hosts` columns; the response DTO is built from it.
type Host struct {
	ID             string
	NodeName       string
	Status         string // online|offline|draining
	AgentVersion   *string
	CPUCores       *int32
	MemMB          *int32
	LastRegistered *time.Time
	LastHeartbeat  *time.Time
}

// GetHost reads a host row. Returns ErrNotFound if absent.
func (s *Store) GetHost(ctx context.Context, hostID string) (Host, error) {
	if !isValidUUID(hostID) {
		return Host{}, ErrNotFound
	}
	var h Host
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, node_name, status, agent_version, cpu_cores, mem_mb,
		       last_registered_at, last_heartbeat_at
		FROM hosts WHERE id = $1::uuid
	`, hostID).Scan(&h.ID, &h.NodeName, &h.Status, &h.AgentVersion,
		&h.CPUCores, &h.MemMB, &h.LastRegistered, &h.LastHeartbeat)
	if errors.Is(err, pgx.ErrNoRows) {
		return Host{}, ErrNotFound
	}
	if err != nil {
		return Host{}, fmt.Errorf("get host: %w", err)
	}
	return h, nil
}

// SetHostStatus is the unconditional write; the caller validates the transition.
func (s *Store) SetHostStatus(ctx context.Context, hostID, status string) error {
	if !isValidUUID(hostID) {
		return nil // no such host row to update
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE hosts SET status = $2 WHERE id = $1::uuid`, hostID, status,
	); err != nil {
		return fmt.Errorf("set host status: %w", err)
	}
	return nil
}

// NonTerminalSessionIDsOnHost is the set a force-drain stops.
func (s *Store) NonTerminalSessionIDsOnHost(ctx context.Context, hostID string) ([]string, error) {
	if !isValidUUID(hostID) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text FROM sessions
		WHERE host_id = $1::uuid AND state NOT IN ('stopped','failed')
	`, hostID)
	if err != nil {
		return nil, fmt.Errorf("list host sessions: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan host session id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate host sessions: %w", err)
	}
	return ids, nil
}

// liveHomeSessionSQL asks whether this user already has a live session backed by
// this home. One statement shared by the LAUNCH guard (scheduler.go guard 2b) and
// the SWAP guard (HasLiveUserAppSession), so the two paths into the same
// corruption cannot disagree. Two load-bearing substitutions (§2):
//
//  1. Keys on `COALESCE(a.parent_app_id, a.id)` — homeAppID in SQL — never on
//     s.app_id, or a tile and its parent are different ids, the guard never fires
//     between them, and two Steam clients run on one steamapps tree (a mid-write
//     death marks the install unclean and forces a full checksum re-verify).
//  2. Compares the FAMILY: both sides are homeAppIDs, so tile-vs-sibling collides
//     as tile-vs-parent does. `s.app_id = $2` would catch only the parent.
//
// It returns the conflicting SESSION ID, not a count (§2.1's 409 body carries
// it). $3 (excludeID) is "" at the launch call site, so the exclusion must stay
// a short-circuited text comparison: an unconditional `::uuid` cast would error
// on the empty string instead of being the always-true no-op that path needs.
const liveHomeSessionSQL = `
	SELECT s.id::text
	FROM sessions s
	JOIN apps a ON a.id = s.app_id
	WHERE s.user_id = $1::uuid
	  AND COALESCE(a.parent_app_id, a.id) = $2::uuid
	  AND ($3 = '' OR s.id != $3::uuid)
	  AND s.state IN ('pending','assigned','starting','running')
	LIMIT 1`

// HasLiveUserAppSession is the swap-path single-writer guard (P5-04): the id of
// an active session the user already has ON THE SAME HOME as homeAppID, other
// than excludeID, or "". excludeID is the session being swapped, so a same-app
// swap of the only session is never blocked. Keyed on the home-owning app (see
// liveHomeSessionSQL), so a swap into a tile collides with its parent and its
// siblings.
func (s *Store) HasLiveUserAppSession(ctx context.Context, userID, homeAppID, excludeID string) (string, error) {
	// The same per-user advisory lock the scheduler holds for launches. Without
	// it, a swap-into-C racing a launch-of-C could both pass and produce two home
	// writers. Xact-scoped, so this serializes against any in-flight
	// scheduleAttempt for the user.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin swap-guard tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock($1, hashtext($2::text))`, lockNamespaceUser, userID); err != nil {
		return "", fmt.Errorf("swap-guard lock: %w", err)
	}

	var conflictID string
	err = tx.QueryRow(ctx, liveHomeSessionSQL, userID, homeAppID, excludeID).Scan(&conflictID)
	if errors.Is(err, pgx.ErrNoRows) {
		conflictID = ""
	} else if err != nil {
		return "", fmt.Errorf("has live user app session: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit swap-guard tx: %w", err)
	}
	return conflictID, nil
}

// homeHostSQL resolves the host holding a user's live (non-tombstoned) home for
// an app: §5's hard placement pin, and the same subquery policyOrderSQL uses for
// locality ordering, tie-break included. `ORDER BY last_used_at DESC` is
// load-bearing — after a locality miss a (user, app) can hold homes on two
// hosts, and the tile must land on the one with the current install.
const homeHostSQL = `
	SELECT host_id::text FROM user_homes
	WHERE user_id = $1::uuid AND app_id = $2::uuid AND gc_after IS NULL AND host_id IS NOT NULL
	ORDER BY last_used_at DESC
	LIMIT 1`

// HomeHostForApp returns the host id holding userID's live home for homeAppID, or
// "" when there is none.
//
// It is the pre-schedule half of §5: a derived tile provisions nothing, so a host
// with no home for (user, parent) has literally nothing to mount, and placing it
// anywhere else is not a degraded outcome but a guaranteed failure. "" here means
// the launch must be refused BEFORE placement with 409 home_not_provisioned —
// never fall back to letting the scheduler pick.
func (s *Store) HomeHostForApp(ctx context.Context, userID, homeAppID string) (string, error) {
	var hostID string
	err := s.pool.QueryRow(ctx, homeHostSQL, userID, homeAppID).Scan(&hostID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve home host: %w", err)
	}
	return hostID, nil
}

// probeMaxAgeDays is the staleness cut for user_devices probes (AS-02). A probe
// whose measured_at is more than this many days old is treated as absent.
const probeMaxAgeDays = 30

// DeviceProbe is the parsed capability probe from user_devices.capabilities
// (schema.md §user_devices). Zero fields mean the capability was not measured.
type DeviceProbe struct {
	BandwidthKbps    int32
	RTTMs            int32
	MaxDecodeHeight  int32
	DisplayRefreshHz float64
	// ClientType is the native-client identity tag (capabilities.client_type),
	// "" for the web probe. Never an eligibility gate for the tier ladder; read
	// only by nativeHighEligible.
	ClientType string
	// H264DecodeProfiles is capabilities.decode.h264.profiles, empty when the
	// decode matrix is absent. Used solely by nativeHighEligible.
	H264DecodeProfiles []string
	// HEVC / AV1 are capabilities.codecs.{hevc,av1}: did the device advertise
	// hardware decode. Both false on an absent or stale probe, and the resolver
	// hard-gates those codecs on them being true (clamps 2+3).
	HEVC bool
	AV1  bool
}

// LatestProbe is the parsed probe from the user's most recently seen
// user_devices row, or nil when none is fresh (measured_at within
// probeMaxAgeDays). Missing, stale and unparseable all return (nil, nil); the
// caller falls back to the default tier.
func (s *Store) LatestProbe(ctx context.Context, userID string) (*DeviceProbe, error) {
	var rawCaps []byte
	err := s.pool.QueryRow(ctx, `
		SELECT capabilities
		FROM user_devices
		WHERE user_id = $1::uuid
		ORDER BY last_seen_at DESC
		LIMIT 1
	`, userID).Scan(&rawCaps)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // no device row at all → no probe
	}
	if err != nil {
		return nil, fmt.Errorf("query latest probe: %w", err)
	}

	var caps struct {
		BandwidthKbps   *float64 `json:"bandwidth_kbps"`
		RTTMs           *float64 `json:"rtt_ms"`
		MaxDecodeHeight *float64 `json:"max_decode_height"`
		MeasuredAt      string   `json:"measured_at"`
		Display         struct {
			RefreshHz *float64 `json:"refresh_hz"`
		} `json:"display"`
		// Optional native-client fields; the web probe omits them. Parsed
		// leniently: any absent or odd shape yields the zero value, not an error.
		ClientType string `json:"client_type"`
		Decode     struct {
			H264 struct {
				Profiles []string `json:"profiles"`
			} `json:"h264"`
		} `json:"decode"`
		// The probe uses the browser's "hevc" naming, not the wire "h265";
		// codec.go bridges the two vocabularies.
		Codecs struct {
			HEVC bool `json:"hevc"`
			AV1  bool `json:"av1"`
		} `json:"codecs"`
	}
	if err := json.Unmarshal(rawCaps, &caps); err != nil {
		// Malformed JSON: an absent probe, not an error worth propagating.
		return nil, nil
	}

	// measured_at is server-stamped RFC3339; empty or unparseable counts as stale.
	if caps.MeasuredAt == "" {
		return nil, nil
	}
	measuredAt, err := time.Parse(time.RFC3339, caps.MeasuredAt)
	if err != nil {
		return nil, nil
	}
	if time.Since(measuredAt) > time.Duration(probeMaxAgeDays)*24*time.Hour {
		return nil, nil // stale probe → no-op → default tier
	}

	p := &DeviceProbe{}
	if caps.Display.RefreshHz != nil && *caps.Display.RefreshHz > 0 && *caps.Display.RefreshHz <= 1000 {
		p.DisplayRefreshHz = *caps.Display.RefreshHz
	}
	if caps.BandwidthKbps != nil {
		p.BandwidthKbps = int32(*caps.BandwidthKbps)
	}
	// Anything below 100 kbps is a probe artifact, not a measurement: no client
	// that can run WebRTC sits there (#146). Treat it as unmeasured so the ladder
	// falls back to the default instead of floor-tiering the user.
	if p.BandwidthKbps > 0 && p.BandwidthKbps < 100 {
		p.BandwidthKbps = 0
	}
	if caps.RTTMs != nil {
		p.RTTMs = int32(*caps.RTTMs)
	}
	if caps.MaxDecodeHeight != nil {
		p.MaxDecodeHeight = int32(*caps.MaxDecodeHeight)
	}
	// client_type and the H.264 decode profiles, for native-high eligibility only.
	// Never affects freshness or the tier gate above.
	p.ClientType = caps.ClientType
	p.H264DecodeProfiles = caps.Decode.H264.Profiles
	p.HEVC = caps.Codecs.HEVC
	p.AV1 = caps.Codecs.AV1
	return p, nil
}

// HostCodecs is the wire codec set the host's encoder path can produce
// (hosts.codecs, §3.1.2). A missing row, empty column or parse failure degrades
// to ["h264"], so the resolver never over-clamps a launch to nothing.
func (s *Store) HostCodecs(ctx context.Context, hostID string) ([]string, error) {
	if !isValidUUID(hostID) {
		return []string{wireCodecH264}, nil
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT codecs FROM hosts WHERE id = $1::uuid`, hostID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return []string{wireCodecH264}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query host codecs: %w", err)
	}
	if len(raw) == 0 {
		return []string{wireCodecH264}, nil // NULL column ⇒ h264-only default
	}
	var codecs []string
	if err := json.Unmarshal(raw, &codecs); err != nil || len(codecs) == 0 {
		return []string{wireCodecH264}, nil
	}
	return codecs, nil
}

// HostCodecPixelRates is per-codec sustained encode throughput in Mpix/s, keyed
// by wire codec (#506, hosts.codec_pixel_rates from agent-api.md
// `capacity.codec_throughput`).
//
// Every degradation path returns nil, meaning unknown, gating nothing — the
// opposite posture from HostCodecs, where an unreadable codec SET must degrade
// to the h264 floor because dispatching an unencodable codec is a dead session,
// while an unreadable throughput hint can only cost a tier. It parses per codec
// rather than expecting a flat number, because the agent sends
// `{"h265": {"max_pixel_rate_mpix_s": 395}}` and the store keeps that verbatim.
func (s *Store) HostCodecPixelRates(ctx context.Context, hostID string) (map[string]float64, error) {
	if !isValidUUID(hostID) {
		return nil, nil
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT codec_pixel_rates FROM hosts WHERE id = $1::uuid`, hostID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query host codec pixel rates: %w", err)
	}
	return parseCodecPixelRates(raw), nil
}

// parseCodecPixelRates projects `capacity.codec_throughput` down to the one
// number the launch decision uses, split from the query so the tolerance rules
// are testable without a database. Tolerant per ENTRY, not per document: one
// malformed codec drops that codec to unknown and leaves the rest usable, rather
// than discarding a host's hints over a codec this control plane cannot parse.
func parseCodecPixelRates(raw []byte) map[string]float64 {
	if len(raw) == 0 {
		return nil
	}
	var doc map[string]struct {
		MaxPixelRateMpixS float64 `json:"max_pixel_rate_mpix_s"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) == 0 {
		return nil
	}
	out := make(map[string]float64, len(doc))
	for codec, entry := range doc {
		// A non-positive rate means the agent could not vouch for this, never that
		// the encoder does zero pixels/s: dropping it keeps a zero-valued entry
		// from rejecting every rung of every chain.
		if entry.MaxPixelRateMpixS > 0 {
			out[codec] = entry.MaxPixelRateMpixS
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// HostHardwareEncoder is clamp 5's input: whether the placed host has a hardware
// encoder, and whether that is known. There is no dedicated column — the signal
// is the agent-reported `encoder` in hosts.effective_settings, openh264
// (software) or va | nvenc | vulkan (hardware).
//
// known=false until the agent reports effective settings, and the caller then
// SKIPS clamp 5 (unknown allows). Rejecting on unknown would break every
// hardware-required launch against a host that has simply not reported yet.
func (s *Store) HostHardwareEncoder(ctx context.Context, hostID string) (known bool, hardware bool, err error) {
	if !isValidUUID(hostID) {
		return false, false, nil
	}
	var enc *string
	qErr := s.pool.QueryRow(ctx,
		`SELECT effective_settings ->> 'encoder' FROM hosts WHERE id = $1::uuid`, hostID).Scan(&enc)
	if errors.Is(qErr, pgx.ErrNoRows) {
		return false, false, nil
	}
	if qErr != nil {
		return false, false, fmt.Errorf("query host encoder: %w", qErr)
	}
	if enc == nil || *enc == "" {
		return false, false, nil
	}
	return true, *enc != "openh264", nil
}

// UpdateSessionCodec persists the resolved codec after placement, when
// resolution lifts a session above the inserted h264 floor. The caller writes
// only on a change, so the common h264 case issues no UPDATE.
func (s *Store) UpdateSessionCodec(ctx context.Context, id, codec string) error {
	if !isValidUUID(id) {
		return nil // no such row to update
	}
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET codec = $2 WHERE id = $1::uuid`, id, codec)
	if err != nil {
		return fmt.Errorf("update session codec: %w", err)
	}
	return nil
}

// UpdateSessionNegotiatedCodec records the codec the BROWSER reports decoding
// (migration 0038). Untrusted telemetry input: the value must already have
// passed normaliseNegotiatedCodec. Guarded on a NON-TERMINAL session, as
// UpdateHealthState is — a terminal row is the historical record and a late POST
// from a browser flushing its buffer must not rewrite it. Callers write only on
// a change.
func (s *Store) UpdateSessionNegotiatedCodec(ctx context.Context, id, codec string) error {
	if !isValidUUID(id) {
		return nil // no such row to update
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET negotiated_codec = $2
		WHERE id = $1::uuid AND state NOT IN ('stopped','failed')
	`, id, codec)
	if err != nil {
		return fmt.Errorf("update session negotiated codec: %w", err)
	}
	return nil
}

// sessionCols is the column list shared by every session read and by INSERT ...
// RETURNING, kept in lockstep with scanSessionRow's Scan order.
const sessionCols = `id::text, user_id::text, app_id::text, host_id::text, gpu_id::text,
	state, state_detail, error_message,
	failure_code, app_log_tail,
	width, height, fps, bitrate_kbps, h264_profile,
	codec,
	profile_id,
	stream_profile_id,
	codec_decision, negotiated_codec,
	mic,
	reserved_vram_mb, reserved_encode_slots,
	playout0_ms,
	health_state, health_state_reason, health_state_changed_at,
	created_at, assigned_at, started_at, ended_at`

// selectSessionSQL is the column list + FROM shared by all session reads.
const selectSessionSQL = `SELECT ` + sessionCols + ` FROM sessions`

// row is the common interface of pgx.Row and a row from pgx.Rows.
type row interface {
	Scan(dest ...any) error
}

func scanSession(r row) (Session, error) {
	sess, err := scanSessionRow(r)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return sess, err
}

// LiveSessions backs the host-settings restart guard: a restart-class knob
// change is blocked unless confirmed while live sessions would be interrupted. A
// query error returns 0, fail-open, so a DB blip does not wedge admin edits.
func (s *Store) LiveSessions(hostID string) int {
	if !isValidUUID(hostID) {
		return 0
	}
	var n int
	err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sessions WHERE host_id = $1::uuid AND state IN `+activeStatesSQL,
		hostID).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

// scanSessionRow scans sessionCols positionally, in lockstep with that constant.
// `extra` takes destinations a caller appended AFTER the shared list (today only
// ListAll's LEFT-JOINed names); keeping it variadic is what lets that query reuse
// selectSessionSQL verbatim rather than fork a drift-prone second column list.
func scanSessionRow(r row, extra ...any) (Session, error) {
	var s Session
	var st string
	var hs string
	dest := []any{
		&s.ID, &s.UserID, &s.AppID, &s.HostID, &s.GPUID,
		&st, &s.StateDetail, &s.ErrorMessage,
		&s.FailureCode, &s.AppLogTail,
		&s.Width, &s.Height, &s.FPS, &s.BitrateKbps, &s.H264Profile,
		&s.Codec,
		&s.ProfileID,
		&s.StreamProfileID,
		&s.CodecDecision, &s.NegotiatedCodec,
		&s.Mic,
		&s.ReservedVram, &s.ReservedSlots,
		&s.Playout0Ms,
		&hs, &s.HealthReason, &s.HealthChangedAt,
		&s.CreatedAt, &s.AssignedAt, &s.StartedAt, &s.EndedAt,
	}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return Session{}, err
	}
	s.State = State(st)
	s.HealthState = HealthState(hs)
	return s, nil
}

// UpdateHealthState stamps health_state_changed_at with an optional reason.
// Applies only while non-terminal: a terminal row is immutable, and the
// unsustainable→failed transition goes through the lifecycle path. Best-effort,
// decoupled by callers from the lifecycle transaction.
func (s *Store) UpdateHealthState(ctx context.Context, id string, hs HealthState, reason *string) error {
	if !isValidUUID(id) {
		return nil // no such row to update
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET
			health_state = $2,
			health_state_reason = $3,
			health_state_changed_at = now()
		WHERE id = $1::uuid AND state NOT IN ('stopped','failed')
	`, id, string(hs), reason)
	if err != nil {
		return fmt.Errorf("update health state: %w", err)
	}
	return nil
}
