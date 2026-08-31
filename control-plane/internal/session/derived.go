// derived.go — derived launch tiles: an apps row with parent_app_id set
// (migration 0044). A tile carries identity and presentation only and borrows
// everything executable from its parent, contributing one thing to execution:
// STEAM_STARTUP_FLAGS, merged over the parent's runtime_spec.env at dispatch.
//
// The rule that must not be broken (§2) is homeAppID below: every guard, lock and
// placement decision keyed on an app's identity FOR STORAGE must key on the
// home-owning app, not the tile. The seven sites:
//
//	1 scheduler.go     guard 2b, the managed-home single-writer count
//	2 store.go         HasLiveUserAppSession, the swap-path guard
//	3 storage.go       hasLiveSessionForHome, the tombstone guard
//	4 placement.go     policyOrderSQL's locality subquery
//	5 home.go          resolveHomeSpec → RequireHome
//	6 storage.go       TouchUsed at session end
//	7 storage.go       ReportBytesUsed
//
// Sites 3, 6 and 7 are pure SQL with no LaunchApp in hand and apply the same rule
// as `COALESCE(a.parent_app_id, a.id)`. Sites 1 and 2 must additionally widen
// from "this app" to this app FAMILY: a tile colliding only with its parent and
// not its siblings is the same corruption reached one click differently.
package session

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// homeAppID is the identity a home is provisioned, locked and placed under; a
// derived tile borrows its parent's. The only definition — never inline
// `if x.ParentAppID != ""` at a call site, or the seven sites stop being greppable.
func homeAppID(a LaunchApp) string {
	if a.ParentAppID != "" {
		return a.ParentAppID
	}
	return a.ID
}

func (a LaunchApp) IsDerived() bool { return a.ParentAppID != "" }

// steamStartupFlagsKey is the env var the quasar-steam entrypoint reads. Its
// value replaces the client's default flags wholesale and is word-split by
// `read -r -a`.
const steamStartupFlagsKey = "STEAM_STARTUP_FLAGS"

// steamAppIDPattern is the appid grammar: a bare positive integer, no leading
// zero, sign, whitespace or separators. Same regex as
// internal/crud.steamAppIDPattern (the HTTP write gate) and apps_external_id_ck
// (the storage backstop, migration 0042).
var steamAppIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// composeSteamFlags renders the STEAM_STARTUP_FLAGS value for a derived tile.
//
// An argument-injection boundary, not a formatter (§10): the result is word-split
// by `read -r -a` in the entrypoint, so every space becomes an argument to the
// Steam client, and the value comes from a background job parsing a file on disk.
// So the template is FIXED with only a validated integer interpolated, never
// concatenated free text, and an unvalidated appid returns an ERROR rather than a
// truncated, sanitized or empty flag string — all three silently produce "asked
// for a game, got the client". Not operator-editable: an "extra Steam flags"
// setting would put arbitrary text into the same word-split.
func composeSteamFlags(externalID string) (string, error) {
	if !steamAppIDPattern.MatchString(externalID) {
		// The value is not echoed: a malformed appid is a hand-edited row or a
		// parser defect, and neither justifies arbitrary stored bytes in a log line.
		return "", fmt.Errorf("derived tile external_id is not a Steam appid (a bare positive integer, no leading zero)")
	}
	return "-bigpicture -applaunch " + externalID, nil
}

// injectSteamFlags merges the tile's STEAM_STARTUP_FLAGS over the effective
// runtime spec's `env`: parent first, tile second, tile wins, the same rule
// mergeRuntimePreset uses for preset-vs-app.
//
// It touches `env` and nothing else, and only ever runs for a derived app, so
// the byte-identical guarantee in runtime_preset.go is unaffected.
func injectSteamFlags(runtimeSpec []byte, flags string) ([]byte, error) {
	spec := map[string]any{}
	if len(runtimeSpec) > 0 {
		if err := json.Unmarshal(runtimeSpec, &spec); err != nil {
			return nil, fmt.Errorf("parse runtime_spec: %w", err)
		}
	}
	env, err := specObject(spec, "env")
	if err != nil {
		return nil, err
	}
	merged := make(map[string]any, len(env)+1)
	for k, v := range env {
		merged[k] = v
	}
	merged[steamStartupFlagsKey] = flags
	spec["env"] = merged

	out, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal runtime_spec: %w", err)
	}
	return out, nil
}
