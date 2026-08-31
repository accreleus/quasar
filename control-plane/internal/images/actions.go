package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Install / uninstall / pin / update — the P3 admin actions
// (protocol/control-api.md §"App-image management P3").
//
// Every action is a Store method, not a handler-level orchestration: the auto
// update policy (applyUpdatePolicy, store.go) must apply exactly the same
// re-adopt + re-ensure as an explicit POST .../update, so both call one
// function rather than risk drifting apart.

// Action errors. The handler maps each to its documented status +
// discriminator (control-api.md); nothing else interprets them.
var (
	ErrNotFound = errors.New("image not found in catalog") // 404 not_found
	// 409 already_installed: moving to a newer version is POST .../update,
	// never a re-install (which would silently reset a pin).
	ErrAlreadyInstalled = errors.New("image already installed")
	// 409 digest_unresolved: last sync couldn't resolve registry_digest.
	// Adopting the mutable tag instead is the fleet-splitting behaviour #440
	// exists to prevent, so this refuses rather than degrades.
	ErrDigestUnresolved = errors.New("image digest unresolved")
	// 409 context_unresolved: template analogue of ErrDigestUnresolved, for
	// context_sha.
	ErrContextUnresolved = errors.New("template context sha unresolved")
	// 404 not_installed: no installed_images row, or id absent from the
	// catalog entirely — either way, nothing to uninstall/pin/update.
	ErrNotInstalled = errors.New("image not installed")
	ErrPinned       = errors.New("image is pinned") // 409 conflict
	// 409 provider_enabled (#471): uninstalling now would be silently undone
	// by the next sync's provider auto-ensure (provider.go onSyncSuccess)
	// reinstalling it seconds later. Wrapped by *ProviderEnabledError to name
	// the provider.
	ErrProviderEnabled = errors.New("image is a library provider and discovery is enabled")
)

// ProviderEnabledError wraps ErrProviderEnabled with the provider's catalog
// display name so the 409 the handler writes can name it (#471).
type ProviderEnabledError struct {
	DisplayName string
}

func (e *ProviderEnabledError) Error() string {
	return fmt.Sprintf("%s library discovery is enabled", e.DisplayName)
}

func (e *ProviderEnabledError) Unwrap() error { return ErrProviderEnabled }

// Ensure is the narrow seam onto the ensure orchestrator (*Ensurer in
// production) — a behaviour, not the orchestrator's internals, so a test can
// assert what was dispatched with no fleet. Both methods are non-blocking: a
// pull takes minutes and is reported over image_state, so no admin request
// waits on one.
type Ensure interface {
	EnsureImage(ctx context.Context, imageID string)
	// hostIDs must be captured before the caller deletes host_images rows —
	// reading them here would race the delete and dispatch nothing.
	RemoveImage(ctx context.Context, imageID string, hostIDs []string)
}

// localTag renders the CP-assigned build tag for a template adoption:
// quasar-local/<image_id>:<version>. Never a registry ref.
func localTag(id, version string) string {
	return "quasar-local/" + id + ":" + version
}

// adoptedImageRef collapses the two mutually-exclusive ref columns (registry_ref
// for a prebuilt, local_tag for a template) into the one string that names this
// adoption's image on a host — the key session placement matches against
// (session/placement.go imageReadySQL).
func adoptedImageRef(registryRef, tag string) string {
	if tag != "" {
		return tag
	}
	return registryRef
}

// Install adopts a catalog image and, unless lazy, kicks ensure-everywhere.
// Returns the refreshed catalog entry (the 201 body).
//
// A prebuilt entry adopts the catalog's digest form (never the mutable tag —
// #440) into registry_ref; a template adopts a CP-assigned local_tag. Either
// way s.ensure.EnsureImage is the same call — the Ensurer reads which column
// was populated and sends image_ensure or image_build accordingly.
func (s *Store) Install(ctx context.Context, id string, lazy bool) (CatalogImage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CatalogImage{}, fmt.Errorf("begin install: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var version, kind, digest, contextSHA, dockerfile, displayName string
	var catalogBuildArgs, runtimeRaw []byte
	err = tx.QueryRow(ctx,
		`SELECT version, kind, registry_digest, context_sha, COALESCE(dockerfile, ''), build_args, runtime, display_name FROM image_catalog WHERE id = $1`, id).
		Scan(&version, &kind, &digest, &contextSHA, &dockerfile, &catalogBuildArgs, &runtimeRaw, &displayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return CatalogImage{}, ErrNotFound
	}
	if err != nil {
		return CatalogImage{}, fmt.Errorf("read image_catalog id=%q: %w", id, err)
	}

	// already_installed outranks digest_unresolved/context_unresolved
	// (control-api.md error order): an already-installed image must answer 409
	// already_installed regardless of resolution state. INSERT ... ON CONFLICT
	// below is the authoritative race guard; this is the uncontended-case check.
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM installed_images WHERE image_id = $1)`, id).Scan(&exists); err != nil {
		return CatalogImage{}, fmt.Errorf("check installed_images id=%q: %w", id, err)
	}
	if exists {
		return CatalogImage{}, ErrAlreadyInstalled
	}

	// A template snapshots context_repo/context_sha/dockerfile/build_args from
	// the catalog transactionally with version+local_tag, so a later sync can
	// never rebuild different bits under this adopted version.
	var registryRef, tag, contextRepo, frozenContextSHA, frozenDockerfile string
	frozenBuildArgs := json.RawMessage("{}")
	switch kind {
	case KindTemplate:
		if contextSHA == "" {
			return CatalogImage{}, ErrContextUnresolved
		}
		tag = localTag(id, version)
		contextRepo = s.catalogRepo()
		frozenContextSHA = contextSHA
		frozenDockerfile = dockerfile
		frozenBuildArgs = normalizeJSONObject(catalogBuildArgs)
	default: // KindPrebuilt
		if digest == "" {
			return CatalogImage{}, ErrDigestUnresolved
		}
		registryRef = digest
	}

	// ON CONFLICT DO NOTHING, not SELECT-then-INSERT: two admins clicking
	// Install at once must produce one adoption and a clean 409, not a
	// unique-violation 500.
	cmdTag, err := tx.Exec(ctx, `
		INSERT INTO installed_images
			(image_id, version, registry_ref, local_tag, context_repo, context_sha, dockerfile, build_args, lazy, pinned)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, false)
		ON CONFLICT (image_id) DO NOTHING
	`, id, version, registryRef, tag, contextRepo, frozenContextSHA, frozenDockerfile, frozenBuildArgs, lazy)
	if err != nil {
		return CatalogImage{}, fmt.Errorf("insert installed_images id=%q: %w", id, err)
	}
	if cmdTag.RowsAffected() == 0 {
		return CatalogImage{}, ErrAlreadyInstalled
	}

	// Materialize the managed runtime preset transactionally with the adoption
	// (preset.go), so a runtime-bearing image is launchable the moment install
	// commits. No runtime block -> presetID "" and the link stays NULL.
	presetID, err := materializePreset(ctx, tx, id, displayName, adoptedImageRef(registryRef, tag), runtimeRaw)
	if err != nil {
		return CatalogImage{}, fmt.Errorf("materialize runtime preset id=%q: %w", id, err)
	}
	if presetID != "" {
		if _, err := tx.Exec(ctx,
			`UPDATE installed_images SET runtime_preset_id = $2 WHERE image_id = $1`, id, presetID); err != nil {
			return CatalogImage{}, fmt.Errorf("link runtime preset id=%q: %w", id, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return CatalogImage{}, fmt.Errorf("commit install id=%q: %w", id, err)
	}

	// Dispatch only after commit: an ensure for a row that then failed to commit
	// would put images on hosts this instance has no record of wanting.
	if !lazy && s.ensure != nil {
		s.ensure.EnsureImage(ctx, id)
	}
	return s.ImageByID(ctx, id)
}

// Uninstall drops the adoption: dispatches a best-effort image_remove to every
// connected host with the image, deletes host_images rows, then installed_images.
//
// host_images is deleted here rather than by an FK cascade because those rows
// FK image_catalog, not the adoption row (migration 0055) — the image stays in
// the catalog, only this instance's adoption ends.
func (s *Store) Uninstall(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin uninstall: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Existence check must precede the provider check: a not-actually-installed
	// id must still answer 404 not_installed even while discovery is enabled.
	var libraryProvider, displayName string
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(ic.library_provider, ''), ic.display_name
		FROM installed_images ii
		JOIN image_catalog ic ON ic.id = ii.image_id
		WHERE ii.image_id = $1
	`, id).Scan(&libraryProvider, &displayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotInstalled
	}
	if err != nil {
		return fmt.Errorf("read installed image id=%q: %w", id, err)
	}
	if libraryProvider != "" {
		// #471: refuse rather than let the next sync's provider auto-ensure
		// silently reinstall this seconds later. Checked in the same tx as the
		// delete, though discovery can still flip true between this read and
		// commit — that race is the provider reconciler's job, not this action's.
		enabled, err := s.libraryDiscoveryEnabledTx(ctx, tx)
		if err != nil {
			return err
		}
		if enabled {
			return &ProviderEnabledError{DisplayName: displayName}
		}
	}

	tag, err := tx.Exec(ctx, `DELETE FROM installed_images WHERE image_id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete installed_images id=%q: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotInstalled
	}

	hostIDs, err := imageHostIDs(ctx, tx, id) // captured before the delete removes it
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_images WHERE image_id = $1`, id); err != nil {
		return fmt.Errorf("delete host_images id=%q: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit uninstall id=%q: %w", id, err)
	}

	// After commit, same reason as Install: a rolled-back uninstall must not
	// have told the fleet to delete the image.
	if s.ensure != nil {
		s.ensure.RemoveImage(ctx, id, hostIDs)
	}
	return nil
}

// SetPinned pins or unpins an adoption. Idempotent — setting the value it
// already has is a successful no-op, so the 204 stays honest for a UI toggle
// that may double-fire.
func (s *Store) SetPinned(ctx context.Context, id string, pinned bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE installed_images SET pinned = $2 WHERE image_id = $1`, id, pinned)
	if err != nil {
		return fmt.Errorf("update installed_images pinned id=%q: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotInstalled
	}
	return nil
}

// Update re-adopts the catalog's current version and re-ensures everywhere.
// applied is false with a nil error when the adoption already names the
// catalog version — a no-op is not a failure.
//
// Like Install, a prebuilt re-adopts the catalog's current digest into
// registry_ref; a template re-adopts a freshly rendered local_tag.
func (s *Store) Update(ctx context.Context, id string) (applied bool, img CatalogImage, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, CatalogImage{}, fmt.Errorf("begin update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		installedVersion, catalogVersion, kind, digest, contextSHA, dockerfile, displayName string
		catalogBuildArgs, runtimeRaw                                                        []byte
		pinned                                                                              bool
		// Read so the provider app can be migrated to whatever the re-adoption
		// produces, including across a runtime-block transition (#456 follow-up).
		oldRegistryRef, oldLocalTag string
		oldPresetID                 *string
		provider                    *string
	)
	// FOR UPDATE OF ii: two concurrent updates (an admin's click racing the
	// auto policy) must serialize on the row, not both read+dispatch.
	err = tx.QueryRow(ctx, `
		SELECT ii.version, ii.pinned, ii.registry_ref, ii.local_tag, ii.runtime_preset_id::text,
		       ic.version, ic.kind, ic.registry_digest, ic.context_sha, COALESCE(ic.dockerfile, ''), ic.build_args, ic.runtime, ic.display_name, ic.library_provider
		FROM installed_images ii
		JOIN image_catalog ic ON ic.id = ii.image_id
		WHERE ii.image_id = $1
		FOR UPDATE OF ii
	`, id).Scan(&installedVersion, &pinned, &oldRegistryRef, &oldLocalTag, &oldPresetID,
		&catalogVersion, &kind, &digest, &contextSHA, &dockerfile, &catalogBuildArgs, &runtimeRaw, &displayName, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, CatalogImage{}, ErrNotInstalled
	}
	if err != nil {
		return false, CatalogImage{}, fmt.Errorf("read adoption id=%q: %w", id, err)
	}
	if pinned {
		return false, CatalogImage{}, ErrPinned
	}
	if installedVersion == catalogVersion {
		_ = tx.Rollback(ctx) // already current; nothing written
		img, err = s.ImageByID(ctx, id)
		return false, img, err
	}

	// An update is the ONLY moment a template's
	// context_repo/context_sha/dockerfile/build_args may move to the catalog's
	// current values.
	var registryRef, tag, contextRepo, frozenContextSHA, frozenDockerfile string
	frozenBuildArgs := json.RawMessage("{}")
	switch kind {
	case KindTemplate:
		if contextSHA == "" {
			return false, CatalogImage{}, ErrContextUnresolved
		}
		tag = localTag(id, catalogVersion)
		contextRepo = s.catalogRepo()
		frozenContextSHA = contextSHA
		frozenDockerfile = dockerfile
		frozenBuildArgs = normalizeJSONObject(catalogBuildArgs)
	default: // KindPrebuilt
		if digest == "" {
			return false, CatalogImage{}, ErrDigestUnresolved
		}
		registryRef = digest
	}

	if _, err := tx.Exec(ctx, `
		UPDATE installed_images
		   SET version = $2, registry_ref = $3, local_tag = $4,
		       context_repo = $5, context_sha = $6, dockerfile = $7, build_args = $8,
		       installed_at = now()
		 WHERE image_id = $1
	`, id, catalogVersion, registryRef, tag, contextRepo, frozenContextSHA, frozenDockerfile, frozenBuildArgs); err != nil {
		return false, CatalogImage{}, fmt.Errorf("re-adopt installed_images id=%q: %w", id, err)
	}

	// Re-materializes the SAME managed row (keyed on managed_image_id), in place.
	presetID, err := materializePreset(ctx, tx, id, displayName, adoptedImageRef(registryRef, tag), runtimeRaw)
	if err != nil {
		return false, CatalogImage{}, fmt.Errorf("materialize runtime preset id=%q: %w", id, err)
	}
	// Always re-point the link: a manifest that dropped its runtime block
	// materializes nothing (presetID ""), so the link must go NULL — leaving it
	// pointing at the obsolete preset would launch stale config. The old preset
	// row is left for admin cleanup, matching the uninstall stance.
	if _, err := tx.Exec(ctx,
		`UPDATE installed_images SET runtime_preset_id = $2 WHERE image_id = $1`, id, nullablePresetID(presetID)); err != nil {
		return false, CatalogImage{}, fmt.Errorf("link runtime preset id=%q: %w", id, err)
	}

	// Runs in this transaction so the provider app can never disagree with the
	// adoption it follows. See migrateProviderAppOnUpdate for the three cases.
	if provider != nil && *provider != "" {
		if err := migrateProviderAppOnUpdate(ctx, tx, *provider,
			derefID(oldPresetID), presetID,
			adoptedImageRef(oldRegistryRef, oldLocalTag), adoptedImageRef(registryRef, tag)); err != nil {
			return false, CatalogImage{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, CatalogImage{}, fmt.Errorf("commit update id=%q: %w", id, err)
	}

	// A lazy adoption re-ensures to nothing: EnsureImage reads the non-lazy set,
	// so a lazy image's new version lands at its next launch placement.
	if s.ensure != nil {
		s.ensure.EnsureImage(ctx, id)
	}
	img, err = s.ImageByID(ctx, id)
	return true, img, err
}

// ImageByID returns one entry of the served catalog, via the same Envelope
// path GET /v1/admin/images uses — an action's response can never drift in
// shape from the list.
func (s *Store) ImageByID(ctx context.Context, id string) (CatalogImage, error) {
	env, err := s.Envelope(ctx)
	if err != nil {
		return CatalogImage{}, err
	}
	for _, img := range env.Images {
		if img.ID == id {
			return img, nil
		}
	}
	return CatalogImage{}, ErrNotFound
}

// imageHostIDs lists the hosts holding a host_images row for this image.
func imageHostIDs(ctx context.Context, db dbExecutor, id string) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT host_id::text FROM host_images WHERE image_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("query host_images id=%q: %w", id, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var hostID string
		if err := rows.Scan(&hostID); err != nil {
			return nil, fmt.Errorf("scan host_images host_id: %w", err)
		}
		out = append(out, hostID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate host_images id=%q: %w", id, err)
	}
	return out, nil
}
