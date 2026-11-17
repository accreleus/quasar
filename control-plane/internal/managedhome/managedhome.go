// Package managedhome owns the ONE rule that decides whether a launch of an app
// gets a managed per-user home and where it is mounted.
//
// Two readers must agree on it: the launch path (internal/session, which mounts
// the home) and the admin app shape (internal/crud, which reports the merged
// result as `effective_managed_home` / `effective_home_container_path`,
// control-api.md amendment 82). It lives here because neither may import the
// other.
//
// The rule (control-api.md, Runtime presets → merge rules):
//
//   - managed_home: the preset provides the default, the app may turn one ON.
//     apps.managed_home is NOT NULL DEFAULT false with no "unset" to tell from
//     an explicit false, so an app cannot turn a preset's managed home OFF — a
//     preset that provisions a per-user home is a storage guarantee for every
//     app inheriting it.
//   - home_container_path: matters only when a managed home is on. The app's
//     path wins unless it is still the schema default, which expresses no
//     preference and takes the preset's path.
package managedhome

// DefaultContainerPath is the schema default for apps.home_container_path
// (migration 0008) and runtime_presets.home_container_path (migration 0035).
const DefaultContainerPath = "/home/quasar"

// Merge applies the rule. presetManaged/presetPath are the governing preset's
// own values — for a derived tile, its PARENT's preset, which is the same
// preset the launch binds the home against. Pass false/"" for "no preset".
func Merge(appManaged bool, appPath string, presetManaged bool, presetPath string) (managed bool, path string) {
	managed = appManaged || presetManaged

	path = appPath
	if appPath == "" || appPath == DefaultContainerPath {
		if presetPath != "" {
			path = presetPath
		}
	}
	if path == "" {
		path = DefaultContainerPath
	}
	return managed, path
}
