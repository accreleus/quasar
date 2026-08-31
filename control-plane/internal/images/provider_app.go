package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

// EnsureProviderApp materializes the provider app the P5 chain (#456) never
// did: installing a provider image (EnsureProviders) and its preset (preset.go)
// left the `apps` table empty, so the operator had nothing launchable. Lives on
// Store, not a handler, because both the setup wizard and the app.go reconciler
// (startup + post-sync) need the same guarantee.
//
// Field notes:
//   - library_provider: apps_library_provider_ck (migration 0044) restricts it to
//     known providers; the DB, not this file, is the gate on provider names.
//   - kind='launcher': presentation only (0044), no server path branches on it.
//   - cover_url: only an absolute https URL on the image-source allowlist
//     (QUASAR_IMAGE_REGISTRY_HOSTS) — see artworkCoverURL.
//   - runtime_spec carries gpu/no_new_privileges/systempaths_unconfined (#432:
//     Steam needs no_new_privileges=false; desktop images need
//     systempaths_unconfined=true for flatpak's bwrap to remount /proc).
//   - image is set on the app only when the manifest has no runtime block (no
//     preset); otherwise the preset supplies it at launch, so an image update
//     reaches this app with no app edit.
//   - managed_home mirrors the manifest's; mergeManagedHome also ORs in the
//     preset's, but library-discovery enqueue keys off user_homes rows that
//     only exist for a managed home.
//
// Never modifies an existing provider app (name/preset/runtime_spec/enabled) —
// operator edits are theirs. Re-enabling one DisableProviderApps disabled is a
// separate step (EnableProviders); a reconcile must never undo that.

// providerRuntimeExtras: manifest runtime keys that ride apps.runtime_spec
// rather than runtime_presets columns (preset.go's "not mapped" list, inverse view).
type providerRuntimeExtras struct {
	GPU                   *bool `json:"gpu"`
	NoNewPrivileges       *bool `json:"no_new_privileges"`
	SystempathsUnconfined *bool `json:"systempaths_unconfined"`
	ManagedHome           bool  `json:"managed_home"`
}

// catalogArtwork: only `tile` maps to an app column (cover_url, migration 0039).
type catalogArtwork struct {
	Tile string `json:"tile"`
}

// EnsureProviderApp creates the provider app for an installed provider image if
// none claims that provider yet; returns true when it created one. No-op when
// an app already has this library_provider (any enabled state — operator owns
// it) or the image has no adoption/preset yet (a later reconcile picks it up).
//
// On create, grants the all-users entitlement (subject_type='all',
// granted_by='provider') inline in the same transaction — closing the #456
// follow-on where enabling Steam produced an app nobody could see. Only fires
// on created==true: an operator who later deletes the entitlement (leaving the
// app) has that respected forever, same as every other operator edit.
//
// No per-subject mode parameter: a create-time choice can't serve a wizard step
// that runs later against an app that already exists (#465 will need its own
// call that changes an existing app's entitlements).
//
// Provider apps created before this entitlement grant existed are NOT
// backfilled — a migration once tried, keyed on "zero entitlement rows", but
// that's ambiguous between "never granted" and "operator revoked everything"
// and must fail closed, not open. Restoring one is an explicit admin action via
// POST /v1/admin/apps/{id}/entitlements.
func (s *Store) EnsureProviderApp(ctx context.Context, imageID, provider string) (bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return false, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin ensure provider app: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// apps has no unique index on library_provider (it's operator-settable), so the
	// exists-check + insert are made atomic with an advisory lock instead, since
	// startup/post-sync/settings-enable reconciles can race.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('quasar_provider_app:' || $1)::bigint)`, provider); err != nil {
		return false, fmt.Errorf("lock provider app %q: %w", provider, err)
	}

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM apps WHERE library_provider = $1)`, provider).Scan(&exists); err != nil {
		return false, fmt.Errorf("check provider app %q: %w", provider, err)
	}
	if exists {
		return false, nil
	}

	var (
		presetID              *string
		registryRef, localTag string
	)
	err = tx.QueryRow(ctx,
		`SELECT runtime_preset_id::text, registry_ref, local_tag FROM installed_images WHERE image_id = $1`, imageID).
		Scan(&presetID, &registryRef, &localTag)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // not installed yet (#442): nothing launchable to point at
	}
	if err != nil {
		return false, fmt.Errorf("read adoption for provider app %q: %w", imageID, err)
	}

	var displayName, description string
	var artworkRaw, runtimeRaw []byte
	if err := tx.QueryRow(ctx,
		`SELECT display_name, description, artwork, runtime FROM image_catalog WHERE id = $1`, imageID).
		Scan(&displayName, &description, &artworkRaw, &runtimeRaw); err != nil {
		return false, fmt.Errorf("read catalog entry for provider app %q: %w", imageID, err)
	}

	spec, managedHome, err := providerRuntimeSpec(runtimeRaw, adoptedImageRef(registryRef, localTag), presetID == nil)
	if err != nil {
		return false, fmt.Errorf("build provider app runtime_spec for %q: %w", imageID, err)
	}

	name := strings.TrimSpace(displayName)
	if name == "" {
		name = imageID
	}

	var appID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO apps (name, description, cover_url, kind, enabled,
		                  library_provider, runtime_spec, runtime_preset_id, managed_home)
		VALUES ($1, $2, $3, 'launcher', true, $4, $5::jsonb, $6::uuid, $7)
		RETURNING id::text
	`, name, description, artworkCoverURL(s.artworkHosts(), artworkRaw), provider, spec, presetID, managedHome).
		Scan(&appID); err != nil {
		return false, fmt.Errorf("insert provider app for %q: %w", imageID, err)
	}

	if err := insertProviderAppAllEntitlement(ctx, tx, appID, provider); err != nil {
		return false, fmt.Errorf("entitle provider app for %q: %w", imageID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit provider app for %q: %w", imageID, err)
	}
	return true, nil
}

// insertProviderAppAllEntitlement writes the create-time subject_type='all'
// grant. ON CONFLICT DO NOTHING makes it idempotent against a concurrent admin
// grant, same pattern as grantAllOnCreate and the library-scan writer.
func insertProviderAppAllEntitlement(ctx context.Context, tx dbExecutor, appID, provider string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, source_ref)
		VALUES ('all', NULL, $1::uuid, 'provider', $2)
		ON CONFLICT DO NOTHING`, appID, "provider-app-ensure:"+provider)
	return err
}

// providerRuntimeSpec builds runtime_spec from the manifest runtime block, plus
// the adopted ref when needsImage (no preset exists, so mergeRuntimePreset
// won't supply one at launch — writing it when a preset exists would freeze
// the app at today's digest).
func providerRuntimeSpec(runtimeRaw []byte, ref string, needsImage bool) (json.RawMessage, bool, error) {
	var rt providerRuntimeExtras
	if hasRuntimeBlock(runtimeRaw) {
		if err := json.Unmarshal(runtimeRaw, &rt); err != nil {
			return nil, false, fmt.Errorf("parse runtime block: %w", err)
		}
	}

	spec := map[string]any{}
	// Defaults true when the manifest is silent: a provider app is a streamed,
	// GPU-composited session by construction.
	gpu := true
	if rt.GPU != nil {
		gpu = *rt.GPU
	}
	spec["gpu"] = gpu
	// Written only when the manifest states it; absent means the agent's hardened
	// default (true). Steam states false: its startup re-escalates via sudo (#432).
	if rt.NoNewPrivileges != nil {
		spec["no_new_privileges"] = *rt.NoNewPrivileges
	}
	// Written only when stated; absent means the agent's default (false). Desktop
	// images (KDE) state true so flatpak's bwrap can remount its own fresh /proc.
	if rt.SystempathsUnconfined != nil {
		spec["systempaths_unconfined"] = *rt.SystempathsUnconfined
	}
	if needsImage && ref != "" {
		spec["image"] = ref
	}

	out, err := json.Marshal(spec)
	if err != nil {
		return nil, false, fmt.Errorf("marshal runtime_spec: %w", err)
	}
	return out, rt.ManagedHome, nil
}

// artworkCoverURL maps the manifest artwork tile onto apps.cover_url, or nil.
//
// The manifest is untrusted and this value renders in an <img src> in an
// operator's browser, so it must pass the same registry-host allowlist
// (QUASAR_IMAGE_REGISTRY_HOSTS, allowedHostsFromEnv in digest.go) images use —
// an unvalidated absolute URL would let a compromised catalog beacon every
// admin's browser. Falls back to the shipped gradient tile (nil) for a
// relative path (the normal case), a non-https URL, or an off-allowlist host.
//
// A nil allowlist means "allow everything", used only in test wiring.
func artworkCoverURL(allow map[string]struct{}, artworkRaw []byte) *string {
	var art catalogArtwork
	if len(artworkRaw) == 0 || json.Unmarshal(artworkRaw, &art) != nil {
		return nil
	}
	tile := strings.TrimSpace(art.Tile)
	if tile == "" {
		return nil
	}
	u, err := url.Parse(tile)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil
	}
	if allow != nil {
		if _, ok := allow[strings.ToLower(u.Hostname())]; !ok {
			return nil
		}
	}
	return &tile
}

// migrateProviderAppOnUpdate keeps a provider app pointed at a launchable image
// across an Update, inside the caller's re-adoption transaction.
//
// A provider app gets its image from either the managed preset or its own
// runtime_spec.image, and an update can move the manifest across that boundary;
// installed_images is re-pointed either way but the app is not, which strands
// it on the old ref:
//   - preset→none: app still links the old preset (stale ref) → clear the link,
//     write the new ref into runtime_spec.image.
//   - none→preset: app's runtime_spec.image (old ref) would override the new
//     preset in mergeRuntimePreset → link the preset, remove the image key.
//   - otherwise: just the preset-less ref refresh, or nothing to do.
//
// Every branch matches on the OLD value, so an app the operator repointed
// themselves matches nothing and is left alone.
func migrateProviderAppOnUpdate(ctx context.Context, tx dbExecutor, provider, oldPresetID, newPresetID, oldRef, newRef string) error {
	if provider == "" {
		return nil
	}
	switch {
	case oldPresetID != "" && newPresetID == "":
		// The manifest DROPPED its runtime block.
		if newRef == "" {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE apps
			   SET runtime_preset_id = NULL,
			       runtime_spec = jsonb_set(runtime_spec, '{image}', to_jsonb($3::text), true)
			 WHERE library_provider = $1
			   AND runtime_preset_id::text = $2
		`, provider, oldPresetID, newRef); err != nil {
			return fmt.Errorf("migrate provider app off a dropped preset for %q: %w", provider, err)
		}
		return nil

	case oldPresetID == "" && newPresetID != "":
		// The manifest GAINED a runtime block.
		if oldRef == "" {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE apps
			   SET runtime_preset_id = $3::uuid,
			       runtime_spec = runtime_spec - 'image'
			 WHERE library_provider = $1
			   AND runtime_preset_id IS NULL
			   AND runtime_spec->>'image' = $2
		`, provider, oldRef, newPresetID); err != nil {
			return fmt.Errorf("migrate provider app onto a new preset for %q: %w", provider, err)
		}
		return nil

	default:
		return refreshProviderAppImage(ctx, tx, provider, oldRef, newRef)
	}
}

// refreshProviderAppImage moves a preset-less provider app from oldRef to
// newRef; no-op if the ref didn't move or the app has no `image` key (inherits
// from a preset).
func refreshProviderAppImage(ctx context.Context, tx dbExecutor, provider, oldRef, newRef string) error {
	if oldRef == "" || newRef == "" || oldRef == newRef {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE apps
		   SET runtime_spec = jsonb_set(runtime_spec, '{image}', to_jsonb($3::text), true)
		 WHERE library_provider = $1
		   AND runtime_preset_id IS NULL
		   AND runtime_spec->>'image' = $2
	`, provider, oldRef, newRef); err != nil {
		return fmt.Errorf("refresh provider app image for %q: %w", provider, err)
	}
	return nil
}

// SuspendProviderApps disables (never deletes) every enabled provider app for
// the library-discovery-off level. Deleting would cascade its derived tiles
// (migration 0044 ON DELETE CASCADE), destroying a library re-enabling could
// otherwise restore.
//
// `WHERE enabled = true` is load-bearing (migration 0060): an app the operator
// already disabled must not be relabelled as ours, or the next enable would
// flip it back on against their wish.
//
// Returns the count; logs the touched app ids/names (#534) so a later
// `404 app not found` on POST /v1/sessions can be matched back to one.
func (s *Store) SuspendProviderApps(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE apps SET enabled = false, library_discovery_suspended = true
		 WHERE library_provider <> '' AND enabled = true
		 RETURNING id::text, name`)
	if err != nil {
		return 0, fmt.Errorf("suspend provider apps: %w", err)
	}
	var suspended []string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan suspended provider app: %w", err)
		}
		suspended = append(suspended, name+" ("+id+")")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("suspend provider apps: %w", err)
	}
	n := len(suspended)
	if n > 0 {
		s.log.Info("library discovery off: provider apps suspended (disabled, not deleted) — they will 404 at launch until library_discovery_enabled is set true",
			"apps", n, "suspended", strings.Join(suspended, ", "))
	}
	return n, nil
}

// RestoreProviderApps is the inverse: re-enables only apps
// SuspendProviderApps disabled (library_discovery_suspended), never one the
// operator disabled themselves. That marker is what makes this safe to run on
// every reconcile pass (level-triggered) instead of only on the settings
// false→true edge.
func (s *Store) RestoreProviderApps(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE apps SET enabled = true, library_discovery_suspended = false
		 WHERE library_discovery_suspended = true`)
	if err != nil {
		return 0, fmt.Errorf("restore suspended provider apps: %w", err)
	}
	n := int(tag.RowsAffected())
	if n > 0 {
		s.log.Info("library discovery on: suspended provider apps restored", "apps", n)
	}
	return n, nil
}
