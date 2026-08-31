package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/accreleus/quasar/control-plane/internal/mountpolicy"
	"github.com/accreleus/quasar/control-plane/internal/runtimeconfig"
)

// Runtime-preset materialization (image-management P5). Installing/updating a
// catalog image whose manifest carries a `runtime` block upserts a managed
// runtime_presets row from it and links installed_images.runtime_preset_id —
// the missing link that makes an installed image launchable.
//
// Writes the same runtime_presets columns migration 0035 / internal/crud
// /presets.go / internal/session/runtime_preset.go (mergeRuntimePreset) own:
// preset_name->name, args/env/mounts (JSONB), managed_home,
// home_container_path, network (S2: ''|none|bridge, never 'host').
//
// Deliberately NOT mapped here (ride apps.runtime_spec instead, no
// runtime_presets column exists): no_new_privileges (#432 — Steam
// re-escalates via sudo), gpu, systempaths_unconfined. See
// provider_app.go's providerRuntimeSpec for the one place a manifest value
// reaches an app row.
//
// `image` is written as the ADOPTED ref (#456: installed_images.registry_ref
// or .local_tag, whichever the adoption populated) — never a manifest field.
// Putting it on the preset rather than the app is what makes a provider app
// track image updates for free: Update() re-materializes the same managed row,
// and mergeRuntimePreset only fills `image` when the app itself states none.
//
// The typed runtime->preset mapping duplicates internal/crud/presets.go's
// admin CRUD write of the same columns; not extracted to a shared helper
// (would risk an import cycle, crud sits above images). Keep that file aligned
// if the column shape or string-type rule changes.

// manifestRuntime is the subset of image_catalog.runtime this materialization
// reads. Every other key (no_new_privileges, gpu, systempaths_unconfined, …)
// is ignored here on purpose.
type manifestRuntime struct {
	PresetName        string          `json:"preset_name"`
	Args              json.RawMessage `json:"args"`
	Env               json.RawMessage `json:"env"`
	Mounts            json.RawMessage `json:"mounts"`
	ManagedHome       bool            `json:"managed_home"`
	HomeContainerPath string          `json:"home_container_path"`
	// S2 per-app container-network requirement. Absent/"" means inherit the
	// agent's hardened `none` default. May be `none` or `bridge`, never `host`
	// (runtimeconfig doc) — a manifest is authored elsewhere and must not be
	// able to remove the container's network namespace on every installing host.
	Network string `json:"network"`
}

// hasRuntimeBlock: an empty object, empty bytes, or literal null all mean "no
// runtime block" and materialize nothing.
func hasRuntimeBlock(raw []byte) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return false
	}
	s := string(t)
	return s != "null" && s != "{}"
}

// decodeStringArray strictly decodes a manifest runtime string array (args or
// mounts): empty/absent/null/`[]` is the empty slice; anything else that isn't
// a JSON array of strings is rejected. The runtime block is cached verbatim at
// sync, unvalidated, so this is the one place its shape is defended before
// session.decodeJSONList would break on it at launch — the error here rolls
// back install/update instead.
func decodeStringArray(field string, raw json.RawMessage) ([]string, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal(t, &out); err != nil {
		return nil, fmt.Errorf("runtime %s must be a JSON array of strings: %w", field, err)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// decodeStringMap strictly decodes runtime.env (string=>string), same reason
// as decodeStringArray: env must decode cleanly at launch (session
// .decodeJSONObject).
func decodeStringMap(field string, raw json.RawMessage) (map[string]string, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return map[string]string{}, nil
	}
	out := map[string]string{}
	if err := json.Unmarshal(t, &out); err != nil {
		return nil, fmt.Errorf("runtime %s must be a JSON object of string=>string: %w", field, err)
	}
	return out, nil
}

// validateMounts refuses a manifest whose mounts are an escape on every host. The
// deny list is shared with the admin CRUD door (internal/mountpolicy); what a given
// host actually permits is the node agent's allowlist, not this. A rejection rolls
// back install/update rather than failing silently at launch.
func validateMounts(mounts []string) error {
	if err := mountpolicy.ValidateAll(mounts); err != nil {
		return fmt.Errorf("runtime %w", err)
	}
	return nil
}

// materializePreset upserts the managed runtime_presets row for imageID and
// returns its id, or "" if the image carries no runtime block (caller leaves
// runtime_preset_id NULL). Runs inside the caller's transaction so the preset
// and the link commit atomically. Idempotent: keyed on managed_image_id
// (migration 0058 partial unique index); an admin-authored preset is never
// touched even on a name collision (resolved to a distinct name instead).
// Validates before writing (decodeStringArray/decodeStringMap/validateMounts);
// a rejection rolls back the whole install/update. adoptedRef is the concrete
// ref this adoption put on the fleet, and becomes the preset's `image`.
func materializePreset(ctx context.Context, tx dbExecutor, imageID, displayName, adoptedRef string, runtimeRaw []byte) (string, error) {
	if !hasRuntimeBlock(runtimeRaw) {
		return "", nil
	}
	var rt manifestRuntime
	if err := json.Unmarshal(runtimeRaw, &rt); err != nil {
		return "", fmt.Errorf("parse runtime block for image %q: %w", imageID, err)
	}

	args, err := decodeStringArray("args", rt.Args)
	if err != nil {
		return "", fmt.Errorf("runtime block for image %q: %w", imageID, err)
	}
	env, err := decodeStringMap("env", rt.Env)
	if err != nil {
		return "", fmt.Errorf("runtime block for image %q: %w", imageID, err)
	}
	mounts, err := decodeStringArray("mounts", rt.Mounts)
	if err != nil {
		return "", fmt.Errorf("runtime block for image %q: %w", imageID, err)
	}
	if err := validateMounts(mounts); err != nil {
		return "", fmt.Errorf("runtime block for image %q: %w", imageID, err)
	}
	// Shared vocabulary with the admin CRUD write path: same column, two doors,
	// one list — a second copy here would drift and the laxer door would win.
	if !runtimeconfig.ValidNetwork(rt.Network) {
		return "", fmt.Errorf("runtime block for image %q: network %q rejected — %s",
			imageID, rt.Network, runtimeconfig.NetworkError)
	}

	// Re-marshal the validated typed values so stored == what launch reads.
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("marshal runtime args for image %q: %w", imageID, err)
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("marshal runtime env for image %q: %w", imageID, err)
	}
	mountsJSON, err := json.Marshal(mounts)
	if err != nil {
		return "", fmt.Errorf("marshal runtime mounts for image %q: %w", imageID, err)
	}

	desired := rt.PresetName // preset_name, then display name, then id — must never be blank (0035 unique name)
	if desired == "" {
		desired = displayName
	}
	if desired == "" {
		desired = imageID
	}

	homePath := rt.HomeContainerPath
	if homePath == "" {
		homePath = defaultHomeContainerPath
	}

	return insertManagedPreset(ctx, tx, imageID, desired, adoptedRef, argsJSON, envJSON, mountsJSON, rt.ManagedHome, homePath, rt.Network)
}

const maxPresetNameAttempts = 8

// insertManagedPreset upserts the managed runtime_presets row and returns its
// id. Resolves the name UNIQUE constraint under concurrency with a bounded
// retry rather than a table lock: uniqueManagedPresetName's check-then-insert
// isn't atomic, so a racing insert can commit between our SELECT and INSERT,
// raising SQLSTATE 23505. On that we roll back to a savepoint (a bare failed
// statement would poison the whole transaction) and pick the next
// disambiguated name; the now-committed collision becomes visible to the next
// SELECT, so the retry advances instead of failing with a 500.
//
// ON CONFLICT (managed_image_id) re-materializes this image's own row in
// place; only a different row's name can collide, and an admin-authored
// preset is never overwritten.
func insertManagedPreset(ctx context.Context, tx dbExecutor, imageID, desired, adoptedRef string, argsJSON, envJSON, mountsJSON []byte, managedHome bool, homePath, network string) (string, error) {
	for attempt := 0; attempt < maxPresetNameAttempts; attempt++ {
		name, err := uniqueManagedPresetName(ctx, tx, desired, imageID)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT quasar_p5_preset`); err != nil {
			return "", fmt.Errorf("savepoint before managed preset insert: %w", err)
		}
		var id string
		err = tx.QueryRow(ctx, `
			INSERT INTO runtime_presets
				(name, image, args, env, mounts, managed_home, home_container_path, managed_image_id, network)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (managed_image_id) WHERE managed_image_id IS NOT NULL
			DO UPDATE SET
				name                = EXCLUDED.name,
				image               = EXCLUDED.image,
				args                = EXCLUDED.args,
				env                 = EXCLUDED.env,
				mounts              = EXCLUDED.mounts,
				managed_home        = EXCLUDED.managed_home,
				home_container_path = EXCLUDED.home_container_path,
				-- Re-materialization must be able to CLEAR the network too: an
				-- image whose manifest drops the network field reverts the managed
				-- preset to '' (inherit), rather than keeping a stale bridge.
				network             = EXCLUDED.network
			RETURNING id::text
		`, name, adoptedRef, argsJSON, envJSON, mountsJSON, managedHome, homePath, imageID, network).Scan(&id)
		if err == nil {
			if _, relErr := tx.Exec(ctx, `RELEASE SAVEPOINT quasar_p5_preset`); relErr != nil {
				return "", fmt.Errorf("release savepoint after managed preset insert: %w", relErr)
			}
			return id, nil
		}
		if isUniqueNameViolation(err) {
			if _, rbErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT quasar_p5_preset`); rbErr != nil {
				return "", fmt.Errorf("rollback to savepoint after preset name collision: %w", rbErr)
			}
			continue // pick the next disambiguated name and retry
		}
		return "", fmt.Errorf("upsert managed runtime preset for image %q: %w", imageID, err)
	}
	return "", fmt.Errorf("could not allocate a free managed runtime preset name for image %q after %d attempts", imageID, maxPresetNameAttempts)
}

// isUniqueNameViolation: the only unique constraint that can fire on this
// insert is name (managed_image_id is handled by ON CONFLICT), so 23505 here
// is always a name collision to retry.
func isUniqueNameViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// derefID flattens a nullable id column to the "" convention the preset
// helpers use for "no preset".
func derefID(id *string) string {
	if id == nil {
		return ""
	}
	return *id
}

// nullablePresetID maps a preset id to its SQL argument, or SQL NULL when
// materialization produced none.
func nullablePresetID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// defaultHomeContainerPath mirrors runtime_presets.home_container_path's
// schema default (0035) and session.defaultHomeContainerPath.
const defaultHomeContainerPath = "/home/quasar"

// uniqueManagedPresetName returns `desired` if no OTHER preset holds that name
// (excluding this image's own managed row, which the upsert is about to update
// in place), or a disambiguated variant otherwise — never overwrites an
// admin-authored preset's name.

// installedAdoption is what a sync needs about an installed image to detect
// and repair a same-version runtime-block drift (#470): materializePreset only
// fires from an explicit Install/Update, so a manifest change at an unchanged
// version left the preset stale with no repair path except reinstall. A
// version change is already covered by Update()/applyUpdatePolicy, so
// reconcileRuntimeDrift below only covers the same-version case.
type installedAdoption struct {
	version     string
	runtime     []byte
	registryRef string
	localTag    string
	presetID    string
	provider    string
}

// reconcileRuntimeDrift re-materializes an installed image's managed preset
// when its manifest runtime block changed at an unchanged catalog version
// (#470). old is the pre-sync snapshot (installedAdoptions, store.go, read
// before the upsert loop overwrites image_catalog.runtime); ok false means not
// currently installed, so the change stays cosmetic.
//
// A version change is left alone: Update()/applyUpdatePolicy already owns
// that path.
//
// No protection against clobbering an operator's own edits to the managed
// preset — same as Update()'s unconditional ON CONFLICT DO UPDATE already
// has, since this calls the identical insertManagedPreset upsert. The only
// protected case (a name collision with a different admin-authored preset) is
// unchanged.
func reconcileRuntimeDrift(ctx context.Context, tx dbExecutor, log *slog.Logger, imageID string, img ManifestImage, old installedAdoption, ok bool) error {
	if !ok || old.version != img.Version {
		return nil
	}
	newRuntime := normalizeJSONObject(img.Runtime)
	if runtimeBlocksEqual(old.runtime, newRuntime) {
		return nil
	}

	adoptedRef := adoptedImageRef(old.registryRef, old.localTag)
	presetID, err := materializePreset(ctx, tx, imageID, img.DisplayName, adoptedRef, newRuntime)
	if err != nil {
		return fmt.Errorf("materialize runtime preset id=%q: %w", imageID, err)
	}
	// Same always-re-point rule Update() applies: a dropped runtime block
	// materializes nothing, so the link must go NULL.
	if _, err := tx.Exec(ctx,
		`UPDATE installed_images SET runtime_preset_id = $2 WHERE image_id = $1`, imageID, nullablePresetID(presetID)); err != nil {
		return fmt.Errorf("link runtime preset id=%q: %w", imageID, err)
	}

	// #498: a managed-preset rewrite with no admin actor and no audit table is
	// unattributable otherwise. This log line is the attribution.
	log.Info("catalog sync: re-materialized managed preset for a same-version runtime-block change",
		"image_id", imageID, "version", img.Version,
		"image_ref", adoptedRef,
		"old_preset_id", old.presetID, "new_preset_id", presetID,
		"old_runtime", string(old.runtime), "new_runtime", string(newRuntime))

	if old.provider != "" {
		// Same crossing cases Update() guards against: a runtime block
		// gained/dropped at the same version can still move a provider app's
		// image between the managed preset and its own runtime_spec.image.
		if err := migrateProviderAppOnUpdate(ctx, tx, old.provider, old.presetID, presetID, adoptedRef, adoptedRef); err != nil {
			return err
		}
	}
	return nil
}

// runtimeBlocksEqual compares semantically, not by bytes: old comes back as
// canonical jsonb text (reordered keys) while newRuntime is manifest
// formatting, so byte comparison would false-positive on every sync. A decode
// failure is "not equal" — routes through materializePreset's own validation
// rather than a silent skip.
func runtimeBlocksEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func uniqueManagedPresetName(ctx context.Context, tx dbExecutor, desired, imageID string) (string, error) {
	candidate := desired
	for i := 0; i < 100; i++ {
		var taken bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM runtime_presets
				WHERE name = $1 AND managed_image_id IS DISTINCT FROM $2
			)`, candidate, imageID).Scan(&taken); err != nil {
			return "", fmt.Errorf("check runtime preset name collision: %w", err)
		}
		if !taken {
			return candidate, nil
		}
		if i == 0 {
			candidate = fmt.Sprintf("%s (%s)", desired, imageID)
		} else {
			candidate = fmt.Sprintf("%s (%s %d)", desired, imageID, i+1)
		}
	}
	return "", fmt.Errorf("could not find a free runtime preset name for %q (image %q)", desired, imageID)
}
