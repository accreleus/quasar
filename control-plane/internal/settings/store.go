// Package settings implements the instance_settings singleton (LP-SEC-01 §A.0):
// one global row of admin-settable, instance-wide config. In W1 it holds only
// registration_mode (closed | invite_only | open), the persisted switch that turns the
// invitation system on/off from the admin UI. The REGISTRATION_MODE env var only seeds
// the row on first boot; thereafter the persisted value is authoritative.
package settings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Registration modes (schema.md instance_settings.registration_mode CHECK).
const (
	RegistrationClosed     = "closed"      // invitation system off; register refused (default)
	RegistrationInviteOnly = "invite_only" // register requires a valid invite code
	RegistrationOpen       = "open"        // register works with no invite (solo self-host)
)

// ValidMode reports whether m is one of the three accepted registration modes.
func ValidMode(m string) bool {
	return m == RegistrationClosed || m == RegistrationInviteOnly || m == RegistrationOpen
}

// Storage providers (schema.md instance_settings.storage_provider CHECK) — the
// managed-home selector, resolved per session by internal/storage.Manager.
// "volume" was hard-removed (#473): the wire enum in protocol/schema.md /
// openapi.yaml still lists it (frozen contract) and migration 0068 coerces
// stored 'volume' rows to 'local', but only "auto"/"local" are accepted and
// the two are synonyms (the host's storage root is the control).
const (
	StorageAuto  = "auto"  // homes under the session host's storage root; no root is a launch error (default)
	StorageLocal = "local" // identical to auto — retained because the wire enum and stored rows carry it

	// The removed docker-volume driver's value; ValidStorageProvider rejects it.
	storageVolumeRemoved = "volume"
)

// ValidStorageProvider reports whether p is accepted. "volume" is not (#473) —
// check IsRemovedVolumeProvider first for the specific removal message.
func ValidStorageProvider(p string) bool {
	return p == StorageAuto || p == StorageLocal
}

// IsRemovedVolumeProvider lets the PATCH handler give an admin the specific
// "why" instead of the generic "must be auto or local".
func IsRemovedVolumeProvider(p string) bool {
	return p == storageVolumeRemoved
}

// Shared with internal/storage's defensive resolveDriver check so both
// surfaces say the same thing.
const ErrVolumeDriverRemovedMsg = "the docker-volume home driver was removed; use a mount path (QUASAR_HOME_ROOT)"

// App-image update policies (schema.md instance_settings.image_update_policy
// CHECK, migration 0054; behaviour in control-api.md §Update-policy semantics).
const (
	ImagePolicyManual = "manual" // sync refreshes the catalog and nothing else
	ImagePolicyNotify = "notify" // same effect as manual; the UI surfaces the badge (DEFAULT)
	ImagePolicyAuto   = "auto"   // sync re-adopts + re-ensures every drifted UNPINNED image
)

// ValidImageUpdatePolicy mirrors the instance_settings CHECK so the PATCH
// handler answers 400 validation_failed instead of a database error.
func ValidImageUpdatePolicy(p string) bool {
	return p == ImagePolicyManual || p == ImagePolicyNotify || p == ImagePolicyAuto
}

// Settings is the instance_settings singleton row.
type Settings struct {
	RegistrationMode string `json:"registration_mode"`
	StorageProvider  string `json:"storage_provider"`
	// The Phase 4 discovery master switch, and the only switch (migration
	// 0045): auto-publish is the behaviour, so there is no separate publish
	// toggle. Default false — ship-dark.
	LibraryDiscoveryEnabled bool `json:"library_discovery_enabled"`

	// Database-side values, not the resolved ones: QUASAR_LIBRARY_SCAN_INTERVAL
	// / QUASAR_STEAM_APPDETAILS_LOOKUP override them when set. Resolution lives
	// in internal/library's shared resolver so the status panel and the
	// scheduler cannot disagree; GET /v1/admin/library/status reports the
	// resolved value plus which source won. Migration 0047.
	LibraryDiscoveryIntervalMinutes   int  `json:"library_discovery_interval_minutes"`
	LibraryDiscoveryAppDetailsEnabled bool `json:"library_discovery_appdetails_enabled"`

	// Instance-wide mic-capture gate (migration 0049). Read per launch, never
	// cached at boot. A mic request against a false gate is not an error — the
	// launch succeeds with no microphone. Default false, ship-dark.
	MicCaptureEnabled bool `json:"mic_capture_enabled"`

	// App-image update policy, manual | notify | auto (migration 0054;
	// semantics: control-api.md §Update-policy semantics). DDL default is
	// `notify`, not `manual` — never silently auto-update an unconfigured
	// instance, but do surface the badge.
	ImageUpdatePolicy string `json:"image_update_policy"`

	// Admin-editable signaling origin allow-list (migration 0064) — the
	// database half of a two-source resolution: QUASAR_ALLOWED_ORIGINS, when
	// set, overrides it outright. GET /v1/admin/access-check reports which
	// source won. An empty list is not "deny all": internal/signal still
	// exempts same-origin and no-Origin requests, so a fresh instance keeps
	// working. `*` is refused by the PATCH handler.
	AllowedOrigins []string `json:"allowed_origins"`

	// Which platform releases the admin console is shown, and the branch the
	// edge channel follows (migration 0074; control-api.md §Platform releases).
	// Read per request, never cached at boot: a channel switch takes effect
	// with no restart. The branch is validated by the PATCH handler — a CHECK
	// cannot express a git ref name — and is never cleared by a channel switch.
	ReleaseChannel    string `json:"release_channel"`
	ReleaseEdgeBranch string `json:"release_edge_branch"`

	UpdatedBy *string   `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Library discovery interval bounds (protocol/openapi.yaml SettingsPatch):
// 15 minutes .. 7 days, mirroring the migration-0047 CHECK so the PATCH
// handler answers 400 validation_failed instead of a database error.
const (
	MinLibraryDiscoveryIntervalMinutes = 15
	MaxLibraryDiscoveryIntervalMinutes = 10080
)

// ValidLibraryDiscoveryIntervalMinutes reports whether n is in PATCH bounds.
func ValidLibraryDiscoveryIntervalMinutes(n int) bool {
	return n >= MinLibraryDiscoveryIntervalMinutes && n <= MaxLibraryDiscoveryIntervalMinutes
}

// Platform-release channel + edge branch (schema.md instance_settings,
// migration 0074; semantics: control-api.md §Platform releases).
const (
	ReleaseChannelStable = "stable"
	ReleaseChannelEdge   = "edge"

	DefaultReleaseEdgeBranch = "develop"

	// The migration stores TEXT with no length CHECK; the contract's bound.
	MaxReleaseEdgeBranchLen = 255
)

// ValidReleaseChannel mirrors the instance_settings CHECK so the PATCH handler
// answers 400 validation_failed instead of a database error.
func ValidReleaseChannel(c string) bool {
	return c == ReleaseChannelStable || c == ReleaseChannelEdge
}

// ValidReleaseEdgeBranch enforces the contract's ref-name rule: non-empty, at
// most 255 characters, and a valid git ref name component — no whitespace, no
// "..", no leading "-", no control characters. It is validated whatever the
// channel is, since the branch is stored on both and selects nothing on stable.
func ValidReleaseEdgeBranch(b string) bool {
	if b == "" || len(b) > MaxReleaseEdgeBranchLen {
		return false
	}
	if strings.Contains(b, "..") || strings.HasPrefix(b, "-") {
		return false
	}
	for _, r := range b {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Store is the data-access layer for instance_settings over the pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store from the shared pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Seed inserts the singleton row on first boot, taking registration_mode from
// defaultMode (the REGISTRATION_MODE env, or 'closed'). Idempotent: a no-op once the
// row exists, so the admin-set value is never clobbered on a later boot. Mirrors the
// bootstrap-admin custody model — env seeds first boot only.
func (s *Store) Seed(ctx context.Context, defaultMode string) error {
	if defaultMode == "" {
		defaultMode = RegistrationClosed
	}
	if !ValidMode(defaultMode) {
		return fmt.Errorf("invalid REGISTRATION_MODE %q: must be closed|invite_only|open", defaultMode)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO instance_settings (id, registration_mode)
		VALUES (true, $1)
		ON CONFLICT (id) DO NOTHING
	`, defaultMode)
	if err != nil {
		return fmt.Errorf("seed instance_settings: %w", err)
	}
	return nil
}

// Get returns the singleton settings row. If the row is somehow absent (Seed not yet
// run) it returns the secure default (closed) rather than erroring, so a missing seed
// can never be read as "open".
func (s *Store) Get(ctx context.Context) (Settings, error) {
	st, err := scanSettings(s.pool.QueryRow(ctx,
		`SELECT `+settingsColumns+` FROM instance_settings WHERE id = true`))
	if errors.Is(err, pgx.ErrNoRows) {
		// The secure default for every field — a missing seed can never be read
		// as "open", nor as "walk everybody's home directory".
		return Settings{
			RegistrationMode:                RegistrationClosed,
			StorageProvider:                 StorageAuto,
			LibraryDiscoveryIntervalMinutes: 360,
			// The column default: an unseeded instance must read the same
			// policy a seeded one would.
			ImageUpdatePolicy: ImagePolicyNotify,
			AllowedOrigins:    []string{},
			ReleaseChannel:    ReleaseChannelStable,
			ReleaseEdgeBranch: DefaultReleaseEdgeBranch,
			UpdatedAt:         time.Now().UTC(),
		}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("get instance_settings: %w", err)
	}
	return st, nil
}

// RegistrationMode is the hot-path read used by the register gate. Returns the secure
// default (closed) if the row is missing — never fails open.
func (s *Store) RegistrationMode(ctx context.Context) (string, error) {
	st, err := s.Get(ctx)
	if err != nil {
		return RegistrationClosed, err
	}
	return st.RegistrationMode, nil
}

// StorageProvider is the read used by internal/storage.Manager at dispatch to pick
// the managed-home driver. Returns the default ('auto') if the row is missing.
func (s *Store) StorageProvider(ctx context.Context) (string, error) {
	st, err := s.Get(ctx)
	if err != nil {
		return StorageAuto, err
	}
	return st.StorageProvider, nil
}

// SetupCompleted reports whether the first-run wizard has finished or been
// skipped (setup_completed_at IS NOT NULL, migration 0053); backs GET
// /v1/setup/status. A missing singleton row reads false.
func (s *Store) SetupCompleted(ctx context.Context) (bool, error) {
	var completed bool
	err := s.pool.QueryRow(ctx,
		`SELECT setup_completed_at IS NOT NULL FROM instance_settings WHERE id = true`).Scan(&completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get setup_completed: %w", err)
	}
	return completed, nil
}

// MarkSetupComplete stamps setup_completed_at when NULL; backs POST
// /v1/setup/complete. Idempotent: the COALESCE keeps an already-set timestamp,
// so re-opening a finished wizard never rewrites when setup finished. Upserts
// the singleton so an unseeded instance converges, and never touches
// setup_state (wizard position stays client-side per the contract).
func (s *Store) MarkSetupComplete(ctx context.Context) (bool, error) {
	var completed bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO instance_settings (id, setup_completed_at)
		VALUES (true, now())
		ON CONFLICT (id) DO UPDATE
		    SET setup_completed_at = COALESCE(instance_settings.setup_completed_at, now())
		RETURNING setup_completed_at IS NOT NULL
	`).Scan(&completed)
	if err != nil {
		return false, fmt.Errorf("mark setup complete: %w", err)
	}
	return completed, nil
}

// LibraryDiscoveryEnabled is read per pass, never cached at boot (see
// internal/artwork/provider.go for what happens otherwise). Missing row reads
// false — a feature that walks user home directories never fails open.
func (s *Store) LibraryDiscoveryEnabled(ctx context.Context) (bool, error) {
	st, err := s.Get(ctx)
	if err != nil {
		return false, err
	}
	return st.LibraryDiscoveryEnabled, nil
}

// LibraryDiscoveryIntervalMinutes is the database-side read; internal/library's
// resolver applies the QUASAR_LIBRARY_SCAN_INTERVAL override on top. Missing
// row reads the column default (360).
func (s *Store) LibraryDiscoveryIntervalMinutes(ctx context.Context) (int, error) {
	st, err := s.Get(ctx)
	if err != nil {
		return 360, err
	}
	return st.LibraryDiscoveryIntervalMinutes, nil
}

// LibraryDiscoveryAppDetailsEnabled is the database-side read;
// QUASAR_STEAM_APPDETAILS_LOOKUP overrides it. Missing row reads false — an
// opt-in third-party disclosure never fails open.
func (s *Store) LibraryDiscoveryAppDetailsEnabled(ctx context.Context) (bool, error) {
	st, err := s.Get(ctx)
	if err != nil {
		return false, err
	}
	return st.LibraryDiscoveryAppDetailsEnabled, nil
}

// MicCaptureEnabled is the launcher's per-launch read of the mic gate. Read
// per launch, never cached at boot. Missing row reads false — a capability the
// operator never turned on never fails open.
func (s *Store) MicCaptureEnabled(ctx context.Context) (bool, error) {
	st, err := s.Get(ctx)
	if err != nil {
		return false, err
	}
	return st.MicCaptureEnabled, nil
}

// AllowedOrigins is the database-side read of the signaling origin allow-list;
// QUASAR_ALLOWED_ORIGINS overrides it. Read per handshake (behind a short TTL
// in the signal handler), never cached at boot — an admin adding their proxy's
// origin must not need a restart. Missing row reads an empty list, which the
// same-origin exemption already covers.
func (s *Store) AllowedOrigins(ctx context.Context) ([]string, error) {
	st, err := s.Get(ctx)
	if err != nil {
		return []string{}, err
	}
	return st.AllowedOrigins, nil
}

// ReleaseChannel is the release view's per-request read. A missing row reads
// the column defaults, which is what a fresh instance follows.
func (s *Store) ReleaseChannel(ctx context.Context) (channel, edgeBranch string, err error) {
	st, err := s.Get(ctx)
	if err != nil {
		return ReleaseChannelStable, DefaultReleaseEdgeBranch, err
	}
	return st.ReleaseChannel, st.ReleaseEdgeBranch, nil
}

// --- the single write path ----------------------------------------------------

// settingsColumns is the column list, written once. Adding a column is one
// edit here, one field on Settings, one line in scanSettings, one field on
// Patch.
const settingsColumns = `registration_mode, storage_provider, library_discovery_enabled,
	library_discovery_interval_minutes, library_discovery_appdetails_enabled,
	mic_capture_enabled, image_update_policy, allowed_origins,
	release_channel, release_edge_branch,
	updated_by::text, updated_at`

// scanner is the shared surface of pgx.Row and pgx.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanSettings(row scanner) (Settings, error) {
	var st Settings
	err := row.Scan(&st.RegistrationMode, &st.StorageProvider, &st.LibraryDiscoveryEnabled,
		&st.LibraryDiscoveryIntervalMinutes, &st.LibraryDiscoveryAppDetailsEnabled,
		&st.MicCaptureEnabled, &st.ImageUpdatePolicy, &st.AllowedOrigins,
		&st.ReleaseChannel, &st.ReleaseEdgeBranch,
		&st.UpdatedBy, &st.UpdatedAt)
	if err != nil {
		return Settings{}, err
	}
	if st.AllowedOrigins == nil {
		st.AllowedOrigins = []string{}
	}
	return st, nil
}

// Patch is the set of fields an admin asked to change. A nil pointer means
// UNCHANGED — every field is optional and absence is never "reset to the
// default". AllowedOrigins is a pointer to a slice so that an explicit empty
// list ("clear it") stays distinguishable from absence.
type Patch struct {
	RegistrationMode                  *string
	StorageProvider                   *string
	LibraryDiscoveryEnabled           *bool
	LibraryDiscoveryIntervalMinutes   *int
	LibraryDiscoveryAppDetailsEnabled *bool
	MicCaptureEnabled                 *bool
	ImageUpdatePolicy                 *string
	AllowedOrigins                    *[]string
	ReleaseChannel                    *string
	ReleaseEdgeBranch                 *string
}

// ChangedKeys lists the fields this patch sets, for the audit row. Names only —
// a value never leaves this struct.
func (p Patch) ChangedKeys() []string {
	var keys []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"registration_mode", p.RegistrationMode != nil},
		{"storage_provider", p.StorageProvider != nil},
		{"library_discovery_enabled", p.LibraryDiscoveryEnabled != nil},
		{"library_discovery_interval_minutes", p.LibraryDiscoveryIntervalMinutes != nil},
		{"library_discovery_appdetails_enabled", p.LibraryDiscoveryAppDetailsEnabled != nil},
		{"mic_capture_enabled", p.MicCaptureEnabled != nil},
		{"image_update_policy", p.ImageUpdatePolicy != nil},
		{"allowed_origins", p.AllowedOrigins != nil},
		{"release_channel", p.ReleaseChannel != nil},
		{"release_edge_branch", p.ReleaseEdgeBranch != nil},
	} {
		if f.set {
			keys = append(keys, f.name)
		}
	}
	return keys
}

// Empty reports whether the patch names no known field.
func (p Patch) Empty() bool {
	return p.RegistrationMode == nil && p.StorageProvider == nil && p.LibraryDiscoveryEnabled == nil &&
		p.LibraryDiscoveryIntervalMinutes == nil && p.LibraryDiscoveryAppDetailsEnabled == nil &&
		p.MicCaptureEnabled == nil && p.ImageUpdatePolicy == nil && p.AllowedOrigins == nil &&
		p.ReleaseChannel == nil && p.ReleaseEdgeBranch == nil
}

// Apply writes every provided field in one statement inside one transaction —
// a settings save is a single operator intention and must land or not land as
// one (guarded by TestApplyRollsBackEntirelyOnDatabaseRejection). It also
// returns the previous library_discovery_enabled: the caller needs the
// transition, not the result.
//
// COALESCE($n, existing) is "absent means unchanged" in SQL — a nil pointer
// encodes as NULL. The seed statement names no defaults on purpose: the DDL
// stays the only owner of what a fresh row looks like, so a migration changing
// a default cannot silently diverge from this file.
func (s *Store) Apply(ctx context.Context, p Patch, updatedBy string) (st Settings, wasDiscoveryEnabled bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Settings{}, false, fmt.Errorf("begin update instance_settings: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO instance_settings (id, updated_by) VALUES (true, $1::uuid)
		ON CONFLICT (id) DO NOTHING
	`, updatedBy); err != nil {
		return Settings{}, false, fmt.Errorf("seed instance_settings: %w", err)
	}

	// FOR UPDATE in the same transaction as the write, so two admins saving at
	// once cannot both observe false→true and both trigger a scan pass. The
	// seed above guarantees the row exists.
	err = tx.QueryRow(ctx,
		`SELECT library_discovery_enabled FROM instance_settings WHERE id = true FOR UPDATE`).Scan(&wasDiscoveryEnabled)
	if err != nil {
		return Settings{}, false, fmt.Errorf("read instance_settings: %w", err)
	}

	var origins any
	if p.AllowedOrigins != nil {
		list := *p.AllowedOrigins
		if list == nil {
			list = []string{}
		}
		origins = list
	}

	st, err = scanSettings(tx.QueryRow(ctx, `
		UPDATE instance_settings AS s SET
		    registration_mode                    = COALESCE($1::text,    s.registration_mode),
		    storage_provider                     = COALESCE($2::text,    s.storage_provider),
		    library_discovery_enabled            = COALESCE($3::boolean, s.library_discovery_enabled),
		    library_discovery_interval_minutes   = COALESCE($4::int,     s.library_discovery_interval_minutes),
		    library_discovery_appdetails_enabled = COALESCE($5::boolean, s.library_discovery_appdetails_enabled),
		    mic_capture_enabled                  = COALESCE($6::boolean, s.mic_capture_enabled),
		    image_update_policy                  = COALESCE($7::text,    s.image_update_policy),
		    allowed_origins                      = COALESCE($8::text[],  s.allowed_origins),
		    release_channel                      = COALESCE($9::text,    s.release_channel),
		    release_edge_branch                  = COALESCE($10::text,   s.release_edge_branch),
		    updated_by                           = $11::uuid
		WHERE id = true
		RETURNING `+settingsColumns+`
	`, p.RegistrationMode, p.StorageProvider, p.LibraryDiscoveryEnabled,
		p.LibraryDiscoveryIntervalMinutes, p.LibraryDiscoveryAppDetailsEnabled,
		p.MicCaptureEnabled, p.ImageUpdatePolicy, origins,
		p.ReleaseChannel, p.ReleaseEdgeBranch, updatedBy))
	if err != nil {
		return Settings{}, false, fmt.Errorf("update instance_settings: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Settings{}, false, fmt.Errorf("commit instance_settings: %w", err)
	}
	return st, wasDiscoveryEnabled, nil
}

// --- single-field helpers (thin wrappers over Apply) --------------------------
// These exist for callers that change exactly one thing. They hold NO SQL of
// their own, which is the point: the column list and the write live in Apply.

func (s *Store) UpdateRegistrationMode(ctx context.Context, mode, updatedBy string) (Settings, error) {
	st, _, err := s.Apply(ctx, Patch{RegistrationMode: &mode}, updatedBy)
	return st, err
}

func (s *Store) UpdateStorageProvider(ctx context.Context, provider, updatedBy string) (Settings, error) {
	st, _, err := s.Apply(ctx, Patch{StorageProvider: &provider}, updatedBy)
	return st, err
}

// UpdateLibraryDiscoveryEnabled also returns the PREVIOUS value, because the
// caller needs the TRANSITION: only false→true earns an immediate scan pass.
func (s *Store) UpdateLibraryDiscoveryEnabled(ctx context.Context, enabled bool, updatedBy string) (Settings, bool, error) {
	return s.Apply(ctx, Patch{LibraryDiscoveryEnabled: &enabled}, updatedBy)
}

func (s *Store) UpdateLibraryDiscoveryIntervalMinutes(ctx context.Context, minutes int, updatedBy string) (Settings, error) {
	st, _, err := s.Apply(ctx, Patch{LibraryDiscoveryIntervalMinutes: &minutes}, updatedBy)
	return st, err
}

func (s *Store) UpdateLibraryDiscoveryAppDetailsEnabled(ctx context.Context, enabled bool, updatedBy string) (Settings, error) {
	st, _, err := s.Apply(ctx, Patch{LibraryDiscoveryAppDetailsEnabled: &enabled}, updatedBy)
	return st, err
}

func (s *Store) UpdateMicCaptureEnabled(ctx context.Context, enabled bool, updatedBy string) (Settings, error) {
	st, _, err := s.Apply(ctx, Patch{MicCaptureEnabled: &enabled}, updatedBy)
	return st, err
}

func (s *Store) UpdateImageUpdatePolicy(ctx context.Context, policy, updatedBy string) (Settings, error) {
	st, _, err := s.Apply(ctx, Patch{ImageUpdatePolicy: &policy}, updatedBy)
	return st, err
}

func (s *Store) UpdateAllowedOrigins(ctx context.Context, list []string, updatedBy string) (Settings, error) {
	st, _, err := s.Apply(ctx, Patch{AllowedOrigins: &list}, updatedBy)
	return st, err
}
