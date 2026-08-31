// runtime_preset.go — the server-side merge of a shared runtime preset with an
// app's own runtime configuration.
//
// The merge must stay on the launch path, every launch: flattening a preset into
// apps.runtime_spec at save time would freeze a copy per app, and the point is
// that editing a preset changes the next launch of every app using it. The agent
// sees no change — the same opaque flattened `app` object in session_assign /
// session_swap_app (agent-api.md), with no new wire field.
//
// An app with no preset must dispatch a runtime_spec BYTE-IDENTICAL to what it
// dispatched before, so mergeRuntimePreset runs only when a preset row was
// joined; the no-preset path returns the stored JSONB untouched, without even a
// decode/re-encode round trip (which reorders keys and renormalizes numbers).
// See GetLaunchApp and TestRuntimeSpecUnchangedWithoutPreset.
package session

import (
	"encoding/json"
	"fmt"

	"github.com/accreleus/quasar/control-plane/internal/runtimeconfig"
)

// defaultHomeContainerPath is the schema default for apps.home_container_path
// (migration 0008) and runtime_presets.home_container_path (migration 0035).
const defaultHomeContainerPath = "/home/quasar"

// runtimePreset is a runtime_presets row as loaded at launch. Zero value = no
// preset; callers must not call the merge with one.
type runtimePreset struct {
	ID                string
	Image             string
	Args              json.RawMessage // JSONB array of strings
	Env               json.RawMessage // JSONB object of string -> string
	Mounts            json.RawMessage // JSONB array of strings
	ManagedHome       bool
	HomeContainerPath string
	// Network is the container-network requirement; '' inherits the agent's host
	// default. Constrained to ''|none|bridge|host by runtime_presets_network_ck
	// (migration 0061).
	Network string
}

// mergeRuntimePreset flattens a preset and an app's own runtime_spec into the
// single opaque spec the agent receives.
//
// Merge rules (control-api.md / schema.md amendment):
//
//   - env:    preset first, app overlaid. A key on both takes the app's value.
//   - mounts: appended, preset first, NO dedupe. Two mounts on one container
//     path is a real misconfiguration and must surface; dropping one would hide
//     the mistake and make the container disagree with the config.
//   - args:   appended, preset first. Order is meaningful.
//   - image:  the app wins when set; blank or absent inherits the preset's.
//   - network: same rule as image (§S2). Neither stated ⇒ the key stays ABSENT
//     and the agent applies its own fallback (QUASAR_CONTAINER_NETWORK, else
//     `none`). Never write an explicit "": on the wire it is indistinguishable
//     from an operator choice.
//
// Every other runtime_spec key (`gpu`, anything added later) is carried through
// untouched; the merge owns only the five fields above.
func mergeRuntimePreset(appSpec json.RawMessage, p runtimePreset) (json.RawMessage, error) {
	spec := map[string]any{}
	if len(appSpec) > 0 {
		if err := json.Unmarshal(appSpec, &spec); err != nil {
			return nil, fmt.Errorf("parse runtime_spec: %w", err)
		}
	}

	if !hasNonEmptyString(spec, "image") && p.Image != "" {
		spec["image"] = p.Image
	}

	// Both unset leaves the key absent.
	if !hasNonEmptyString(spec, "network") && p.Network != "" {
		spec["network"] = p.Network
	}

	for _, f := range []struct {
		key    string
		preset json.RawMessage
	}{
		{"args", p.Args},
		{"mounts", p.Mounts},
	} {
		presetList, err := decodeJSONList(f.preset, "runtime preset "+f.key)
		if err != nil {
			return nil, err
		}
		appList, err := specList(spec, f.key)
		if err != nil {
			return nil, err
		}
		if len(presetList) == 0 && appList == nil {
			continue // nothing to add and the app never had the key; leave it absent
		}
		merged := make([]any, 0, len(presetList)+len(appList))
		merged = append(merged, presetList...)
		merged = append(merged, appList...)
		spec[f.key] = merged
	}

	presetEnv, err := decodeJSONObject(p.Env, "runtime preset env")
	if err != nil {
		return nil, err
	}
	appEnv, err := specObject(spec, "env")
	if err != nil {
		return nil, err
	}
	if len(presetEnv) > 0 || appEnv != nil {
		merged := make(map[string]any, len(presetEnv)+len(appEnv))
		for k, v := range presetEnv {
			merged[k] = v
		}
		for k, v := range appEnv { // app value wins on a conflicting key
			merged[k] = v
		}
		spec["env"] = merged
	}

	out, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal merged runtime_spec: %w", err)
	}
	return out, nil
}

// mergeManagedHome is the storage half: the preset provides the default, the app
// may override. apps.managed_home is NOT NULL DEFAULT false, so there is no
// "unset" to tell from an explicit false — an app can turn a managed home ON
// when its preset does not provide one, but cannot turn a preset's OFF, because
// a preset that provisions a per-user home is a storage guarantee for every app
// inheriting it.
//
// home_container_path matters only when managed_home is true; the app's path
// wins unless it is the schema default, which expresses no preference.
func mergeManagedHome(appManagedHome bool, appPath string, p runtimePreset) (bool, string) {
	managed := appManagedHome || p.ManagedHome

	path := appPath
	if appPath == "" || appPath == defaultHomeContainerPath {
		if p.HomeContainerPath != "" {
			path = p.HomeContainerPath
		}
	}
	if path == "" {
		path = defaultHomeContainerPath
	}
	return managed, path
}

// validateRuntimeNetwork checks an app's own `runtime_spec.network` before it is
// inherited over or dispatched. The merge cannot cover it: hasNonEmptyString
// treats a present-but-non-string value as absent, so a preset silently
// overwrites the operator's value and, with no preset, it is dispatched verbatim
// and fails late in the agent's deserialization. It also catches a well-formed
// but disallowed string, which the preset column's CHECK cannot reach because
// apps.runtime_spec is opaque JSONB.
//
// Called on EVERY launch, including the no-preset path, and BEFORE inheritance.
// Read-only, so the byte-identical guarantee holds.
func validateRuntimeNetwork(appSpec json.RawMessage) error {
	if len(appSpec) == 0 {
		return nil
	}
	var probe struct {
		Network any `json:"network"`
	}
	if err := json.Unmarshal(appSpec, &probe); err != nil {
		return fmt.Errorf("parse runtime_spec: %w", err)
	}
	if probe.Network == nil {
		return nil // absent or explicit null; both mean "not stated"
	}
	s, ok := probe.Network.(string)
	if !ok {
		return fmt.Errorf("runtime_spec.network must be a string, got %T", probe.Network)
	}
	if !runtimeconfig.ValidNetwork(s) {
		return fmt.Errorf("runtime_spec.network %q rejected — %s", s, runtimeconfig.NetworkError)
	}
	return nil
}

// hasNonEmptyString reports whether spec[key] is a non-empty JSON string.
func hasNonEmptyString(spec map[string]any, key string) bool {
	s, ok := spec[key].(string)
	return ok && s != ""
}

// specList reads a list-valued runtime_spec key. A missing key yields (nil, nil),
// distinguishable from an explicit empty list (a non-nil empty slice). JSON null
// counts as absent.
func specList(spec map[string]any, key string) ([]any, error) {
	raw, ok := spec[key]
	if !ok || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("runtime_spec.%s is not an array", key)
	}
	if list == nil {
		list = []any{}
	}
	return list, nil
}

// specObject is specList for an object-valued key, same absent-vs-empty rule.
func specObject(spec map[string]any, key string) (map[string]any, error) {
	raw, ok := spec[key]
	if !ok || raw == nil {
		return nil, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("runtime_spec.%s is not an object", key)
	}
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, nil
}

// decodeJSONList decodes a preset's JSONB array column. Empty/NULL bytes give an
// empty list rather than an error: the column is NOT NULL DEFAULT '[]', so this
// is defence against a hand-edited row.
func decodeJSONList(raw json.RawMessage, what string) ([]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var list []any
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", what, err)
	}
	return list, nil
}

// decodeJSONObject decodes a preset's JSONB object column (see decodeJSONList).
func decodeJSONObject(raw json.RawMessage, what string) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", what, err)
	}
	return obj, nil
}
