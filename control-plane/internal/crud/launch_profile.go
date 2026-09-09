// launch_profile.go — stamp a managed image's launch profile onto an app row
// at write time (#171).
//
// An app whose runtime preset is IMAGE-MANAGED (runtime_presets.managed_image_id
// set) launches the image that manifest describes, so the manifest's declared
// `gpu` / `no_new_privileges` / `systempaths_unconfined` are stamped onto the
// app's runtime_spec on every create/patch that touches the spec or the
// preset. Provider-created apps have always had this (images.providerRuntimeSpec);
// console-created apps on the same presets did not, and every desktop app made
// in the console failed with `software Vulkan renderer detected`.
//
// Write time, not launch time: schema.md forbids dispatch reading the live
// catalog (a sync rewrites `runtime` and deletes withdrawn rows), and
// control-api.md promises `gpu` passes through the launch merge untouched.
// Existing rows were backfilled by migration 0082 with the same rule.
//
// A derived tile (parent_app_id set) is never stamped: apps_derived_shape_ck
// requires its runtime_spec to stay `{}`, and it launches its parent's spec.
// A provider-created app (library_provider set) is never stamped either: the
// provider copied the values at install, against the image version it adopted,
// and owns them through image update (migrateProviderAppOnUpdate); re-stamping
// from the live catalog on an admin edit could move it to a newer version than
// the image it runs. Migration 0082 draws the same two lines.
package crud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/accreleus/quasar/control-plane/internal/images"
	"github.com/jackc/pgx/v5"
)

// managedLaunchProfile returns the manifest runtime block of the image that
// manages presetID, and whether there is one. A preset that is absent or not
// image-managed yields (nil, false, nil): nothing to stamp.
func (s *store) managedLaunchProfile(ctx context.Context, presetID string) ([]byte, bool, error) {
	var runtime []byte
	err := s.pool.QueryRow(ctx, `
		SELECT ic.runtime
		FROM runtime_presets rp
		JOIN image_catalog ic ON ic.id = rp.managed_image_id
		WHERE rp.id::text = $1
	`, presetID).Scan(&runtime)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read managed launch profile for preset %s: %w", presetID, err)
	}
	return runtime, true, nil
}

// stampLaunchProfile applies the managed image's profile to spec when presetID
// names a managed preset. ok reports whether anything was stamped; when false,
// spec is returned untouched (byte-identical).
func (s *store) stampLaunchProfile(ctx context.Context, spec json.RawMessage, presetID string) (json.RawMessage, bool, error) {
	runtime, managed, err := s.managedLaunchProfile(ctx, presetID)
	if err != nil || !managed {
		return spec, false, err
	}
	out, err := images.ApplyLaunchProfile(spec, runtime)
	if err != nil {
		return spec, false, fmt.Errorf("apply launch profile of preset %s: %w", presetID, err)
	}
	return out, true, nil
}

// appRuntimeRefs is the stored runtime shape of one app, read by the PATCH path
// so a partial request can be evaluated against the effective preset and spec.
type appRuntimeRefs struct {
	RuntimeSpec     json.RawMessage
	RuntimePresetID *string
	ParentAppID     *string
	LibraryProvider string
}

func (s *store) appRuntimeRefs(ctx context.Context, id string) (appRuntimeRefs, error) {
	var r appRuntimeRefs
	err := s.pool.QueryRow(ctx, `
		SELECT runtime_spec, runtime_preset_id::text, parent_app_id::text, library_provider
		FROM apps WHERE id::text = $1
	`, id).Scan(&r.RuntimeSpec, &r.RuntimePresetID, &r.ParentAppID, &r.LibraryProvider)
	if errors.Is(err, pgx.ErrNoRows) {
		return appRuntimeRefs{}, ErrNotFound
	}
	if err != nil {
		return appRuntimeRefs{}, fmt.Errorf("read app runtime refs: %w", err)
	}
	return r, nil
}
