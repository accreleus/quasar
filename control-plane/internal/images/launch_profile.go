// launch_profile.go — an image's declared launch profile, applied to an app's
// runtime_spec (#171).
//
// Three runtime_spec keys are facts about whether an image can start at all,
// not per-app preferences: `gpu` (a desktop image's own preflight refuses a
// software Vulkan renderer), `no_new_privileges` (Steam re-escalates via sudo,
// #432) and `systempaths_unconfined` (KDE's flatpak remounts /proc). The
// manifest declares them in its `runtime` block; they have no runtime_presets
// column (preset.go's "not mapped" list), so the only way they reach a launch
// is ON THE APP ROW. providerRuntimeSpec has always written them there for a
// provider-created app. Until #171 nothing did for an app an admin created in
// the console against the same managed preset: the editor wrote gpu:false,
// believing it inert, and every such desktop app failed with
// `software Vulkan renderer detected`.
//
// ApplyLaunchProfile is the one rule both paths now share. It is applied at
// WRITE time (internal/crud create/patch) and by migration 0082 for existing
// rows — never at dispatch: schema.md forbids reading the live catalog at
// launch (a sync rewrites `runtime` and deletes withdrawn rows), and
// control-api.md promises that `gpu` passes through mergeRuntimePreset
// untouched.
//
// The manifest value wins over whatever the app spec says. A console-created
// app has no control for these keys, and an explicit contrary value is never a
// working configuration on the image that declares them. Host-side hardening
// remains available (QUASAR_APP_PRIVILEGE_OPTOUT, container.rs).
package images

import (
	"encoding/json"
	"fmt"
)

// ApplyLaunchProfile returns spec with the manifest runtime block's launch
// profile applied. Rules, identical to providerRuntimeSpec's:
//
//   - gpu: the manifest's value; TRUE when the manifest is silent. An app on a
//     managed preset is a streamed, GPU-composited session by construction.
//   - no_new_privileges, systempaths_unconfined: written only when the manifest
//     states them; absent stays absent (the agent's hardened default applies).
//   - every other key in spec is preserved.
//
// An empty spec is `{}`. The result is re-encoded (key order is not
// preserved); callers write it to JSONB, which normalises anyway.
func ApplyLaunchProfile(spec json.RawMessage, runtimeRaw []byte) (json.RawMessage, error) {
	out := map[string]any{}
	if len(spec) > 0 {
		if err := json.Unmarshal(spec, &out); err != nil {
			return nil, fmt.Errorf("parse runtime_spec: %w", err)
		}
	}
	var rt providerRuntimeExtras
	if hasRuntimeBlock(runtimeRaw) {
		if err := json.Unmarshal(runtimeRaw, &rt); err != nil {
			return nil, fmt.Errorf("parse runtime block: %w", err)
		}
	}
	applyLaunchProfile(out, rt)
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal runtime_spec: %w", err)
	}
	return b, nil
}

// applyLaunchProfile writes the three profile keys into spec.
func applyLaunchProfile(spec map[string]any, rt providerRuntimeExtras) {
	gpu := true
	if rt.GPU != nil {
		gpu = *rt.GPU
	}
	spec["gpu"] = gpu
	if rt.NoNewPrivileges != nil {
		spec["no_new_privileges"] = *rt.NoNewPrivileges
	}
	if rt.SystempathsUnconfined != nil {
		spec["systempaths_unconfined"] = *rt.SystempathsUnconfined
	}
}
