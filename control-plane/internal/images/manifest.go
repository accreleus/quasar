// Package images implements the app-image catalog: manifest
// fetch/validate/cache, the image_catalog table, install/update/pin, ensure
// orchestration, and the admin endpoints (control-api.md "App-image catalog +
// management").
package images

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// SupportedManifestVersion is the only manifest_version this build
// understands; anything else is refused outright, never partially applied.
const SupportedManifestVersion = 1

// Manifest is the top-level shape of the quasar-images catalog manifest.
// Unknown top-level and per-image fields are ignored rather than rejected —
// the manifest is owned by a separate repo and may carry authoring metadata
// Quasar doesn't need.
type Manifest struct {
	ManifestVersion int             `json:"manifest_version"`
	Images          []ManifestImage `json:"images"`
}

// ManifestImage is one catalog entry. Runtime is kept as raw JSON — a superset
// object (preset_name/gpu/no_new_privileges/systempaths_unconfined
// /managed_home/home_container_path/args/env/mounts) acted on at install, not here.
//
// Raw holds the entire manifest entry exactly as fetched, including fields
// this struct doesn't declare, populated by Parse from the original bytes,
// never re-marshaled from the typed fields. image_catalog.raw persists this
// verbatim (store.go) — what makes forward-compatibility with unrecognized
// fields (migration 0054) actually true.
type ManifestImage struct {
	ID               string          `json:"id"`
	DisplayName      string          `json:"display_name"`
	Description      string          `json:"description"`
	Kind             string          `json:"kind"` // "prebuilt" | "template"
	Version          string          `json:"version"`
	RegistryRef      string          `json:"registry_ref"` // prebuilt only
	Dockerfile       string          `json:"dockerfile"`   // template only
	BuildArgs        json.RawMessage `json:"build_args"`
	Artwork          json.RawMessage `json:"artwork"`
	Runtime          json.RawMessage `json:"runtime"`
	LibraryProvider  string          `json:"library_provider"`
	MinQuasarVersion string          `json:"min_quasar_version"`
	Raw              json.RawMessage `json:"-"`
}

const (
	KindPrebuilt = "prebuilt"
	KindTemplate = "template"
)

// Parse decodes a manifest document. Does not validate — call Validate
// separately so a caller can distinguish "not JSON" from "JSON but refused".
//
// images decodes in two stages on purpose:
//  1. As json.RawMessage first, so missing/`null` is distinguishable from an
//     explicit `[]` and refused — decoding straight into a slice would let a
//     plausible publishing mistake ({"manifest_version":1}) read as an empty
//     manifest, and Store.upsert's deletion reconciliation would then wipe
//     the entire cached catalog. An explicit `[]` stays valid.
//  2. Each entry decodes twice: once to json.RawMessage (ManifestImage.Raw)
//     and once to the typed struct, so a field the struct doesn't declare is
//     never silently dropped.
func Parse(data []byte) (*Manifest, error) {
	var envelope struct {
		ManifestVersion int             `json:"manifest_version"`
		Images          json.RawMessage `json:"images"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	trimmed := bytes.TrimSpace(envelope.Images)
	switch {
	case len(trimmed) == 0:
		return nil, fmt.Errorf("parse manifest: images field is missing — refusing (an absent catalog is not an empty catalog)")
	case string(trimmed) == "null":
		return nil, fmt.Errorf("parse manifest: images is null — refusing (a null catalog is not an empty catalog)")
	case trimmed[0] != '[':
		return nil, fmt.Errorf("parse manifest: images must be a JSON array")
	}

	var rawImages []json.RawMessage
	if err := json.Unmarshal(trimmed, &rawImages); err != nil {
		return nil, fmt.Errorf("parse manifest images: %w", err)
	}

	m := &Manifest{ManifestVersion: envelope.ManifestVersion}
	for i, rawImg := range rawImages {
		var img ManifestImage
		if err := json.Unmarshal(rawImg, &img); err != nil {
			return nil, fmt.Errorf("parse manifest images[%d]: %w", i, err)
		}
		img.Raw = rawImg
		m.Images = append(m.Images, img)
	}
	return m, nil
}

// Validate refuses an unknown manifest_version and any entry missing a
// required field; Store.Sync must not partially apply a manifest that fails.
func Validate(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("manifest is nil")
	}
	if m.ManifestVersion != SupportedManifestVersion {
		return fmt.Errorf("unsupported manifest_version %d: this build only understands version %d; refusing rather than partially applying",
			m.ManifestVersion, SupportedManifestVersion)
	}
	seen := make(map[string]bool, len(m.Images))
	for i, img := range m.Images {
		if img.ID == "" {
			return fmt.Errorf("images[%d]: id is required", i)
		}
		if seen[img.ID] {
			return fmt.Errorf("images[%d] (id=%q): duplicate id in manifest", i, img.ID)
		}
		seen[img.ID] = true
		if img.DisplayName == "" {
			return fmt.Errorf("images[%d] (id=%q): display_name is required", i, img.ID)
		}
		if img.Version == "" {
			return fmt.Errorf("images[%d] (id=%q): version is required", i, img.ID)
		}
		switch img.Kind {
		case KindPrebuilt:
			if img.RegistryRef == "" {
				return fmt.Errorf("images[%d] (id=%q): kind=prebuilt requires registry_ref", i, img.ID)
			}
		case KindTemplate:
			if img.Dockerfile == "" {
				return fmt.Errorf("images[%d] (id=%q): kind=template requires dockerfile", i, img.ID)
			}
		default:
			return fmt.Errorf("images[%d] (id=%q): kind must be \"prebuilt\" or \"template\", got %q", i, img.ID, img.Kind)
		}
		// A non-string value is an error, not a silent drop: dispatching argless
		// would build the wrong image, and the frozen adoption would carry it forward.
		if err := validateBuildArgs(img.BuildArgs); err != nil {
			return fmt.Errorf("images[%d] (id=%q): %w", i, img.ID, err)
		}
	}
	return nil
}

// validateBuildArgs confirms build_args, when present, is a flat JSON object
// whose every value is a string (docker --build-arg KEY=VALUE). Absent/empty/
// null is valid "no args".
func validateBuildArgs(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	// Decode values as RawMessage, not string: unmarshaling a JSON null into a Go
	// string doesn't error, it silently becomes "" — requiring a JSON string
	// literal is the actual check.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return fmt.Errorf("build_args must be a flat object of string values: %w", err)
	}
	for k, v := range m {
		vt := bytes.TrimSpace(v)
		if len(vt) == 0 || vt[0] != '"' {
			return fmt.Errorf("build_args[%q] must be a string, got %s", k, string(vt))
		}
	}
	return nil
}

// ParseAndValidate is the entry point Store.Sync uses: parse then validate.
func ParseAndValidate(data []byte) (*Manifest, error) {
	m, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := Validate(m); err != nil {
		return nil, err
	}
	return m, nil
}
