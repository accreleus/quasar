// Package storage is the P5-02 storage-provider abstraction. A "home" is the
// per-(user, app) read-write state directory mounted into the game container.
// The control plane synthesizes the mount string and keeps the user_homes row;
// it never touches a host filesystem (invariant #1 — provisioning is
// agent-side). One driver (#473 removed docker volumes — no host path for
// library discovery to walk; "volume" is rejected in internal/settings and
// again in resolveDriver for the env-sourced path):
//
//   - local: `{root}/{user}/{app}:{containerPath}:rw`; the agent ensures the
//     directory pre-launch, {root} is the host's effective home root.
//
// Provider ('auto'|'local') and per-host root are resolved per EnsureHome call
// (settings PATCH → host home_root → QUASAR_HOME_ROOT), so an admin flip
// applies on the next launch without restart. The per-host storage root is the
// control: 'auto' and 'local' both mean "under this host's root", and no root
// is a loud launch error naming the remedy (resolveDriver).
//
// The wire is untouched: the string rides in the opaque `app.mounts` array of
// session_assign / session_swap_app (agent-api.md, P5-01 amendment).
package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Home is one user_homes bookkeeping row. The id and name fields are pointers
// because a home outlives the user/app/host it belonged to — and that orphaned
// row is exactly what an admin opened the storage page to find. The ids remain
// the authoritative key.
type Home struct {
	ID         string
	UserID     *string
	AppID      *string
	HostID     *string
	Username   *string
	AppName    *string
	HostName   *string
	Provider   string
	Ref        string
	BytesUsed  int64
	CreatedAt  time.Time
	LastUsedAt time.Time
	GCAfter    *time.Time
}

// ErrHomeNotFound is returned when a user_homes row with the given ID is absent.
var ErrHomeNotFound = fmt.Errorf("home not found")

// ListHomesOpts filters for ListHomes.
type ListHomesOpts struct {
	UserID    string // filter to homes belonging to this user (UUID string)
	AppID     string // filter to homes for this app (UUID string)
	PendingGC *bool  // true = only tombstoned; false = only live
	Cursor    string // opaque offset cursor
}

// MyStorageItem is one row of the user's own storage summary (P5-05).
type MyStorageItem struct {
	AppID      string     `json:"app_id"`
	AppName    string     `json:"app_name"`
	BytesUsed  int64      `json:"bytes_used"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// homeKey carries everything a driver may use to synthesize a locator: the
// stable UUIDs plus operator-readable slugs derived from the current username /
// app name.
type homeKey struct {
	userID, appID     string
	userSlug, appSlug string
}

// driver synthesizes the provider-specific locator for a (user, app) home.
// The schema CHECK on user_homes.provider still lists "volume"
// (protocol/schema.md, historical rows), but no driver here can produce it.
type driver interface {
	// name is the user_homes.provider value.
	name() string
	// ref returns the provider-scoped locator (host path).
	ref(k homeKey) string
}

type localDriver struct{ root string }

func (localDriver) name() string { return "local" }
func (d localDriver) ref(k homeKey) string {
	// Human-navigable layout: {root}/{username}/{app-name-slug}. The path is
	// synthesized only when a (user, app, host) home is first created —
	// EnsureHome reuses the stored ref afterwards, so a later username/app
	// rename cannot orphan existing data (it keeps its original path).
	return path.Join(d.root, k.userSlug, k.appSlug)
}

// slugRe matches characters allowed verbatim in a home path segment (a subset
// of validateMount's alphabet, minus '/' and leading-dot hazards).
var slugRe = regexp.MustCompile(`[^a-z0-9_-]+`)

// slugify turns a display name into a filesystem-safe path segment. Lossless
// names (already clean after lowercasing) are used as-is; anything that needed
// altering gets a short-UUID suffix so two names that sanitize identically
// ("Mike!" / "Mike?") cannot collide onto one directory. An empty result falls
// back to the UUID.
func slugify(name, id string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	clean := strings.Trim(slugRe.ReplaceAllString(lower, "-"), "-.")
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	switch {
	case clean == "":
		return id
	case clean == lower:
		return clean
	default:
		return clean + "-" + short
	}
}

// SettingsReader supplies the instance-wide storage_provider choice
// ('auto'|'local'), read fresh on every EnsureHome so an admin's PATCH
// takes effect on the next launch (storage-config). Implemented by
// internal/settings.Store.StorageProvider.
type SettingsReader interface {
	StorageProvider(ctx context.Context) (string, error)
}

// HostRootResolver resolves a session host's *effective* managed-home root
// (override → agent effective_settings → control-plane env → ""), per host, so
// different hosts may use different roots (storage-config). Implemented by
// internal/hostcfg.Store.HomeRoot (env-bound via HostRootResolverFunc in app.go).
type HostRootResolver interface {
	HomeRoot(ctx context.Context, hostID string) (string, error)
}

// HostRootResolverFunc adapts a plain function to HostRootResolver — the wiring
// point that binds hostcfg.Store.HomeRoot's env fallback (app.go).
type HostRootResolverFunc func(ctx context.Context, hostID string) (string, error)

// HomeRoot implements HostRootResolver.
func (f HostRootResolverFunc) HomeRoot(ctx context.Context, hostID string) (string, error) {
	return f(ctx, hostID)
}

// fixedProvider is a SettingsReader that always returns the same provider — used
// by the env/test constructors (NewFromEnv / NewLocal).
type fixedProvider string

func (p fixedProvider) StorageProvider(context.Context) (string, error) { return string(p), nil }

// fixedRoot is a HostRootResolver that always returns the same root regardless of
// host — used by the env/test constructors (v1 uniform-root behaviour).
type fixedRoot string

func (r fixedRoot) HomeRoot(context.Context, string) (string, error) { return string(r), nil }

// Manager synthesizes home mounts and owns the user_homes bookkeeping. The
// managed-home provider is resolved PER EnsureHome call (not fixed at
// construction): the instance-wide storage_provider (settings) is layered over the
// session host's effective home root (roots) — storage-config. This is what lets an
// admin flip the provider in the UI and different hosts use different roots.
type Manager struct {
	pool     *pgxpool.Pool
	settings SettingsReader
	roots    HostRootResolver
}

// New builds the runtime Manager wired to the live settings + host-root resolvers
// (app.go). Every EnsureHome consults these fresh, so provider/root changes apply on
// the next launch with no restart.
func New(pool *pgxpool.Pool, settings SettingsReader, roots HostRootResolver) *Manager {
	return &Manager{pool: pool, settings: settings, roots: roots}
}

// NewFromEnv builds a Manager whose provider + root are fixed from
// QUASAR_STORAGE_PROVIDER / QUASAR_HOME_ROOT (tests, or a deployment without
// the admin surface wired); resolution still flows per-call, the values are
// simply constant. "volume" is rejected (#473).
func NewFromEnv(pool *pgxpool.Pool) (*Manager, error) {
	prov := strings.TrimSpace(os.Getenv("QUASAR_STORAGE_PROVIDER"))
	if prov == "" {
		prov = "auto"
	}
	switch prov {
	case "auto", "local":
	case "volume":
		return nil, fmt.Errorf("%w", ErrVolumeDriverRemoved)
	default:
		return nil, fmt.Errorf("unknown QUASAR_STORAGE_PROVIDER %q (auto|local)", prov)
	}
	root := strings.TrimSpace(os.Getenv("QUASAR_HOME_ROOT"))
	if root != "" && !path.IsAbs(root) {
		return nil, fmt.Errorf("QUASAR_HOME_ROOT must be absolute (got %q)", root)
	}
	return &Manager{pool: pool, settings: fixedProvider(prov), roots: fixedRoot(root)}, nil
}

// NewLocal returns a local-driver Manager rooted at root (tests / explicit wiring).
func NewLocal(pool *pgxpool.Pool, root string) *Manager {
	return &Manager{pool: pool, settings: fixedProvider("local"), roots: fixedRoot(path.Clean(root))}
}

// ErrNoHomeRoot lets callers that ask about resolution rather than perform it
// (library.Handler's inertReason, via ResolvedDriverName) tell "not configured
// yet" apart from a genuinely broken read.
var ErrNoHomeRoot = errors.New("host has no storage root")

// ErrVolumeDriverRemoved backs the "storage_provider=volume" rejection (#473).
// internal/settings.ValidStorageProvider is the primary gate; resolveDriver
// checks again for the env-sourced NewFromEnv path and pre-upgrade rows.
var ErrVolumeDriverRemoved = errors.New("the docker-volume home driver was removed; use a mount path (QUASAR_HOME_ROOT)")

// noHomeRootError builds the operator-facing launch failure for a host with no
// effective home root: it names the host the way the operator named it, says
// what will not happen, and says where to fix it — never an internal knob or a
// bare UUID.
func (m *Manager) noHomeRootError(ctx context.Context, hostID string) error {
	return fmt.Errorf("%w: no managed-home storage root is set for host %s, so its games have nowhere to keep their save data and the session cannot start. "+
		"Set a storage root for this host under Admin → Hosts (or in the setup wizard's host-check step)",
		ErrNoHomeRoot, m.hostLabel(ctx, hostID))
}

// hostLabel renders a host for an operator: its node_name plus the short id, or
// the raw id when the name cannot be read. Best-effort by construction — this
// only ever decorates an error that is already being returned, so a failed
// lookup must not replace the real failure with a database one.
func (m *Manager) hostLabel(ctx context.Context, hostID string) string {
	if m.pool == nil {
		return hostID
	}
	var name string
	if err := m.pool.QueryRow(ctx,
		`SELECT node_name FROM hosts WHERE id::text = $1`, hostID).Scan(&name); err != nil || name == "" {
		return hostID
	}
	short := hostID
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("%q (%s)", name, short)
}

// ResolvedDriverName returns the driver name EnsureHome would actually use for
// this host right now — the resolved driver, not the raw stored setting (#472
// was exactly that mismatch going unnoticed). Exported for library.Handler's
// inertReason. On a rootless host it returns ErrNoHomeRoot; question-asking
// callers should treat that as "not local" (the launch it predicts would fail).
func (m *Manager) ResolvedDriverName(ctx context.Context, hostID string) (string, error) {
	drv, err := m.resolveDriver(ctx, hostID)
	if err != nil {
		return "", err
	}
	return drv.name(), nil
}

func (m *Manager) resolveDriver(ctx context.Context, hostID string) (driver, error) {
	prov, err := m.settings.StorageProvider(ctx)
	if err != nil {
		return nil, fmt.Errorf("read storage_provider: %w", err)
	}
	if prov == "" {
		prov = "auto"
	}
	root := ""
	if m.roots != nil {
		root, err = m.roots.HomeRoot(ctx, hostID)
		if err != nil {
			return nil, fmt.Errorf("resolve home root for host %s: %w", hostID, err)
		}
	}
	root = strings.TrimSpace(root)
	if root != "" && !path.IsAbs(root) {
		return nil, fmt.Errorf("effective home root %q for host %s is not absolute", root, hostID)
	}
	switch prov {
	case "volume":
		return nil, ErrVolumeDriverRemoved
	case "local", "auto":
		// One branch for both, per the package comment: 'auto' only ever
		// differed by silently downgrading to the removed volume driver.
		if root == "" {
			return nil, m.noHomeRootError(ctx, hostID)
		}
		return localDriver{root: path.Clean(root)}, nil
	default:
		return nil, fmt.Errorf("unknown storage_provider %q (auto|local)", prov)
	}
}

// mountPattern is the shape every synthesized mount string must match before it
// is handed to the dispatch path: source:containerPath:rw where source is a
// volume name or absolute host path and containerPath is absolute. Nothing in
// the pipeline validates mounts otherwise (the survey finding — a malformed
// entry surfaces as an opaque `container run failed`), so the synthesizer is
// the validation point.
var mountPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-/]+:/[A-Za-z0-9_.\-/]+:rw$`)

func validateMount(m string) error {
	if !mountPattern.MatchString(m) {
		return fmt.Errorf("synthesized mount %q does not match the required shape", m)
	}
	if strings.Contains(m, "..") {
		return fmt.Errorf("synthesized mount %q contains a path traversal", m)
	}
	return nil
}

// EnsureHome synthesizes the home mount string for (user, app) on host and
// upserts the bookkeeping row (clearing any pending tombstone — launching into
// a home un-marks it for GC). Returns the validated mount string.
//
// containerPath comes from apps.home_container_path and must be absolute.
func (m *Manager) EnsureHome(ctx context.Context, userID, appID, hostID, containerPath string) (string, error) {
	if !path.IsAbs(containerPath) {
		return "", fmt.Errorf("home_container_path %q is not absolute", containerPath)
	}
	// Read fresh per launch so an admin PATCH / per-host home_root applies on
	// the next launch with no restart.
	drv, err := m.resolveDriver(ctx, hostID)
	if err != nil {
		return "", err
	}
	key := homeKey{userID: userID, appID: appID, userSlug: userID, appSlug: appID}
	// Resolve display names for the human-navigable local layout; a lookup miss
	// (test fixtures, deleted rows) falls back to the UUIDs.
	var uname, aname string
	if err := m.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT username FROM users WHERE id = $1::uuid), ''),
		       COALESCE((SELECT name     FROM apps  WHERE id = $2::uuid), '')
	`, userID, appID).Scan(&uname, &aname); err == nil {
		if uname != "" {
			key.userSlug = slugify(uname, userID)
		}
		if aname != "" {
			key.appSlug = slugify(aname, appID)
		}
	}
	ref := drv.ref(key)
	// Sticky ref: an existing row keeps its stored ref, so renames never
	// re-point the mount away from the data. Only a provider change replaces
	// it — the row must describe the home actually mounted, or GC/admin views
	// reap the wrong backing store. The previous driver's store then becomes
	// invisible to bookkeeping (accepted: driver switches are rare, operator-
	// initiated).
	var provider, storedRef string
	if err := m.pool.QueryRow(ctx, `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5)
		ON CONFLICT (user_id, app_id, host_id) DO UPDATE
		    SET provider = EXCLUDED.provider,
		        ref = CASE WHEN user_homes.provider = EXCLUDED.provider
		                   THEN user_homes.ref ELSE EXCLUDED.ref END,
		        last_used_at = now(), gc_after = NULL
		RETURNING provider, ref
	`, userID, appID, hostID, drv.name(), ref).Scan(&provider, &storedRef); err != nil {
		return "", fmt.Errorf("upsert user_home: %w", err)
	}
	mount := fmt.Sprintf("%s:%s:rw", storedRef, path.Clean(containerPath))
	if err := validateMount(mount); err != nil {
		return "", err
	}
	return mount, nil
}

// ErrHomeNotProvisioned is returned by RequireHome when the (user, app, host)
// triple has no live user_homes row. It is the storage half of the session
// package's ErrHomeNotProvisioned → 409 home_not_provisioned mapping.
var ErrHomeNotProvisioned = errors.New("home not provisioned")

// RequireHome is EnsureHome's read-only sibling — resolve the mount for an
// existing home, refuse when there is none. Used by derived tiles
// (steam-library-discovery §3), which must never trigger EnsureHome's two
// side effects: creating on a miss (an empty library that reaches `running`
// looking healthy) and un-tombstoning a home an admin marked for reaping
// (the upsert's gc_after = NULL). It requires `gc_after IS NULL` and its one
// write is the same last_used_at touch EnsureHome applies, keeping GC and
// locality ordering honest for a tile-launched family. The mount is built and
// validated identically to EnsureHome's, so a tile's mount string is
// byte-identical to its parent's — the two apps mount the same directory.
func (m *Manager) RequireHome(ctx context.Context, userID, appID, hostID, containerPath string) (string, error) {
	if !path.IsAbs(containerPath) {
		return "", fmt.Errorf("home_container_path %q is not absolute", containerPath)
	}
	// One statement for the existence check, gc_after guard and touch: split
	// up, a concurrent tombstone could land between them and the caller would
	// mount a home already scheduled for deletion. No resolveDriver / ref
	// synthesis on purpose — the stored ref is authoritative; re-deriving could
	// hand back a path for a provider the data is not stored under.
	var storedRef string
	err := m.pool.QueryRow(ctx, `
		UPDATE user_homes SET last_used_at = now()
		WHERE user_id = $1::uuid AND app_id = $2::uuid AND host_id = $3::uuid
		  AND gc_after IS NULL
		RETURNING ref
	`, userID, appID, hostID).Scan(&storedRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrHomeNotProvisioned
	}
	if err != nil {
		return "", fmt.Errorf("require user_home: %w", err)
	}
	mount := fmt.Sprintf("%s:%s:rw", storedRef, path.Clean(containerPath))
	if err := validateMount(mount); err != nil {
		return "", err
	}
	return mount, nil
}

// TouchUsed stamps last_used_at for the (user, app, host) home on session end;
// best-effort (a miss is harmless — EnsureHome touches on the next launch).
// appID is the session's app id, so the SQL itself resolves a derived tile to
// its parent's home via COALESCE(parent_app_id, id) — without that, a
// tile-launched family would never advance last_used_at on the real home,
// quietly degrading locality/pin ordering and GC candidate ordering.
func (m *Manager) TouchUsed(ctx context.Context, userID, appID, hostID string) error {
	_, err := m.pool.Exec(ctx, `
		UPDATE user_homes SET last_used_at = now()
		WHERE user_id = $1::uuid
		  AND app_id = (SELECT COALESCE(a.parent_app_id, a.id) FROM apps a WHERE a.id = $2::uuid)
		  AND host_id = $3::uuid
	`, userID, appID, hostID)
	if err != nil {
		return fmt.Errorf("touch user_home: %w", err)
	}
	return nil
}

// ReportBytesUsed updates bytes_used for the session's (user, HOME app, host)
// home (P5-03); best-effort from the pre-terminal metrics sample, a miss is a
// no-op. The join must go through apps with COALESCE(parent_app_id, id): a
// derived-tile session has no home row of its own, and joining on s.app_id
// silently discarded the write — no error, no log, a figure that just stops
// moving.
func (m *Manager) ReportBytesUsed(ctx context.Context, sessionID string, bytesUsed int64) error {
	_, err := m.pool.Exec(ctx, `
		UPDATE user_homes uh
		SET bytes_used = $2
		FROM sessions s
		JOIN apps a ON a.id = s.app_id
		WHERE s.id = $1::uuid
		  AND uh.user_id = s.user_id
		  AND uh.app_id  = COALESCE(a.parent_app_id, a.id)
		  AND uh.host_id = s.host_id
		  AND uh.gc_after IS NULL
	`, sessionID, bytesUsed)
	if err != nil {
		return fmt.Errorf("report bytes_used: %w", err)
	}
	return nil
}

// ListHomes returns user_homes rows paginated (admin use). Filters are optional.
func (m *Manager) ListHomes(ctx context.Context, opts ListHomesOpts) ([]Home, string, error) {
	const pageSize = int64(50)
	var offset int64
	fmt.Sscanf(opts.Cursor, "%d", &offset)

	var where []string
	var args []any
	i := 1
	if opts.UserID != "" {
		where = append(where, fmt.Sprintf("uh.user_id::text = $%d", i))
		args = append(args, opts.UserID)
		i++
	}
	if opts.AppID != "" {
		where = append(where, fmt.Sprintf("uh.app_id::text = $%d", i))
		args = append(args, opts.AppID)
		i++
	}
	if opts.PendingGC != nil {
		if *opts.PendingGC {
			where = append(where, "uh.gc_after IS NOT NULL")
		} else {
			where = append(where, "uh.gc_after IS NULL")
		}
	}

	// Must be LEFT joins: the fk columns are ON DELETE SET NULL, and an inner
	// join would silently drop exactly the orphaned rows an admin came to find.
	// Names use the same columns the rest of the admin UI shows.
	q := `SELECT uh.id::text, uh.user_id::text, uh.app_id::text, uh.host_id::text,
	             u.username, a.name, h.node_name,
	             uh.provider, uh.ref, uh.bytes_used, uh.created_at, uh.last_used_at, uh.gc_after
	      FROM user_homes uh
	      LEFT JOIN users u ON u.id = uh.user_id
	      LEFT JOIN apps  a ON a.id = uh.app_id
	      LEFT JOIN hosts h ON h.id = uh.host_id`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	// created_at is qualified — every joined table has one.
	q += fmt.Sprintf(" ORDER BY uh.created_at DESC LIMIT $%d OFFSET $%d", i, i+1)
	args = append(args, pageSize+1, offset)

	rows, err := m.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list homes: %w", err)
	}
	defer rows.Close()

	var out []Home
	for rows.Next() {
		var h Home
		if err := rows.Scan(&h.ID, &h.UserID, &h.AppID, &h.HostID,
			&h.Username, &h.AppName, &h.HostName,
			&h.Provider, &h.Ref, &h.BytesUsed, &h.CreatedAt, &h.LastUsedAt, &h.GCAfter); err != nil {
			return nil, "", fmt.Errorf("scan home: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var next string
	if int64(len(out)) > pageSize {
		out = out[:pageSize]
		next = fmt.Sprintf("%d", offset+pageSize)
	}
	return out, next, nil
}

// TombstonedHome names the (user, app) pair whose home was just tombstoned, so
// the audit row says whose data is scheduled for deletion rather than only
// echoing the home's uuid. AppName is empty when the app row is already gone
// (user_homes.app_id is ON DELETE SET NULL, migration 0009).
type TombstonedHome struct {
	Username string
	AppName  string
}

// TombstoneHome sets gc_after = now() on a home row. Returns ErrHomeNotFound if
// absent or ErrHomeInUse if a live session currently mounts it.
//
// The UPDATE returns the owning username and app name via RETURNING, so the
// caller can audit a destructive action without a second round trip.
func (m *Manager) TombstoneHome(ctx context.Context, id string) (TombstonedHome, error) {
	live, err := m.hasLiveSessionForHome(ctx, id)
	if err != nil {
		return TombstonedHome{}, err
	}
	if live {
		return TombstonedHome{}, ErrHomeInUse
	}
	var out TombstonedHome
	var appName *string
	err = m.pool.QueryRow(ctx, `
		UPDATE user_homes SET gc_after = now() WHERE id::text = $1
		RETURNING
			(SELECT username FROM users WHERE users.id = user_homes.user_id),
			(SELECT name FROM apps WHERE apps.id = user_homes.app_id)
	`, id).Scan(&out.Username, &appName)
	if errors.Is(err, pgx.ErrNoRows) {
		return TombstonedHome{}, ErrHomeNotFound
	}
	if err != nil {
		return TombstonedHome{}, fmt.Errorf("tombstone home: %w", err)
	}
	if appName != nil {
		out.AppName = *appName
	}
	return out, nil
}

// hasLiveSessionForHome reports whether any non-terminal session uses this
// home. It guards a data-destruction path (tombstone → agent-pull reaper
// deletes the backing store), so the join must go through apps with
// COALESCE(parent_app_id, id): a derived-tile session has no home row of its
// own, and joining on s.app_id once let an admin tombstone a live Steam
// library mid-write. Any family member's session counts — they all mount it.
func (m *Manager) hasLiveSessionForHome(ctx context.Context, homeID string) (bool, error) {
	var n int
	err := m.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM sessions s
		JOIN apps a ON a.id = s.app_id
		JOIN user_homes uh ON uh.user_id = s.user_id
		                  AND uh.app_id  = COALESCE(a.parent_app_id, a.id)
		WHERE uh.id::text = $1
		  AND s.state IN ('pending','assigned','starting','running')
	`, homeID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check live session for home: %w", err)
	}
	return n > 0, nil
}

// ErrHomeInUse is returned by TombstoneHome when a live session mounts the home.
var ErrHomeInUse = fmt.Errorf("home in use by a live session")

// ListUserStorage returns per-app storage summaries for a single user.
func (m *Manager) ListUserStorage(ctx context.Context, userID string) ([]MyStorageItem, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT uh.app_id::text, a.name, uh.bytes_used, uh.last_used_at
		FROM user_homes uh
		JOIN apps a ON a.id = uh.app_id
		WHERE uh.user_id = $1::uuid AND uh.gc_after IS NULL
		ORDER BY a.name
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list user storage: %w", err)
	}
	defer rows.Close()

	var out []MyStorageItem
	for rows.Next() {
		var it MyStorageItem
		if err := rows.Scan(&it.AppID, &it.AppName, &it.BytesUsed, &it.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan user storage: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SweepHomes hard-deletes only the tombstoned rows the agent-pull reaper
// (#175) can never claim: past the 24h grace with host_id IS NULL (no
// node-agent owns the backing store, so a row-only delete is all there is).
// Host-pinned tombstoned rows are left for the agent to pull, reap host-side,
// and confirm — the confirm hard-deletes the row after the directory is
// actually gone. The control plane has no host-FS access (invariant #1), so it
// must not row-delete a pinned home out from under its backing store.
// Triggered by the storage.home_janitor job (internal/jobs); the count is the
// run summary.
func (m *Manager) SweepHomes(ctx context.Context, log *slog.Logger) (int64, error) {
	const sweep = `DELETE FROM user_homes
		WHERE gc_after IS NOT NULL AND gc_after + interval '24 hours' < now()
		  AND host_id IS NULL`
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tag, err := m.pool.Exec(cctx, sweep)
	if err != nil {
		log.Warn("home janitor sweep failed", "err", err)
		return 0, err
	}
	if n := tag.RowsAffected(); n > 0 {
		log.Info("home janitor", "deleted_unreapable", n)
	}
	return tag.RowsAffected(), nil
}
