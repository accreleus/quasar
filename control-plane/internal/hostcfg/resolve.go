package hostcfg

import (
	"fmt"
	"path"
	"reflect"
	"strings"
)

// Validate checks a sparse override map against the catalog: keys must exist,
// values must match the knob's type/range/enum. A nil value is allowed only for
// a nullable knob (it clears the override). Returns the first error found.
func Validate(overrides map[string]any) error {
	cat := byKey()
	for key, v := range overrides {
		k, ok := cat[key]
		if !ok {
			return fmt.Errorf("unknown setting %q", key)
		}
		if v == nil {
			if !k.Nullable {
				return fmt.Errorf("%q may not be null", key)
			}
			continue
		}
		if err := validateValue(k, v); err != nil {
			return err
		}
	}
	return nil
}

func validateValue(k Knob, v any) error {
	switch k.Type {
	case TypeBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%q must be a boolean", k.Key)
		}
	case TypeInt, TypeFloat:
		n, ok := v.(float64)
		if !ok {
			return fmt.Errorf("%q must be a number", k.Key)
		}
		if k.Type == TypeInt && n != float64(int64(n)) {
			return fmt.Errorf("%q must be an integer", k.Key)
		}
		if k.Min != nil && n < *k.Min {
			return fmt.Errorf("%q must be >= %v", k.Key, *k.Min)
		}
		if k.Max != nil && n > *k.Max {
			return fmt.Errorf("%q must be <= %v", k.Key, *k.Max)
		}
	case TypeEnum:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%q must be a string", k.Key)
		}
		for _, e := range k.Enum {
			if s == e {
				return nil
			}
		}
		return fmt.Errorf("%q must be one of %v", k.Key, k.Enum)
	case TypeString:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%q must be a string", k.Key)
		}
		if k.AbsPathOrEmpty && s != "" && !path.IsAbs(s) {
			return fmt.Errorf("%q must be empty or an absolute path", k.Key)
		}
	}
	return nil
}

// ValidatePatch is Validate with PATCH semantics: null is allowed for any
// known key (it means "clear this override"), even a non-nullable one.
func ValidatePatch(overrides map[string]any) error {
	cat := byKey()
	for key, v := range overrides {
		if _, ok := cat[key]; !ok {
			return fmt.Errorf("unknown setting %q", key)
		}
		if v == nil {
			continue // null always means "clear"; always allowed in patch context
		}
		if err := validateValue(cat[key], v); err != nil {
			return err
		}
	}
	return nil
}

// MinHysteresisBand is the minimum required gap between the resolution
// ladder's engage and recover fractions (D2 hysteresis rule): a narrower band
// (or an inverted one) lets the ladder pump between rungs.
const MinHysteresisBand = 0.05

// ValidateResolved performs cross-knob checks that span more than one key, so
// it runs on the fully resolved map (after Resolve), unlike
// Validate/ValidatePatch which see only the sparse overrides.
func ValidateResolved(resolved map[string]any) error {
	engage, eok := resolved["abr_ladder_res_engage_frac"].(float64)
	recoverFrac, rok := resolved["abr_ladder_res_recover_frac"].(float64)
	if !eok || !rok {
		return nil // not configured (or not float) — Validate already covered types
	}
	if recoverFrac-engage < MinHysteresisBand {
		return fmt.Errorf(
			"abr_ladder_res_recover_frac (%.3g) must exceed abr_ladder_res_engage_frac (%.3g) "+
				"by at least %.2f, or the resolution ladder will pump between rungs",
			recoverFrac, engage, MinHysteresisBand)
	}
	return nil
}

// Resolve layers validated overrides over the catalog defaults. Callers must
// Validate first. A nil override value falls back to the default (clear). This
// is the **display** view (admin API `resolved`), NOT what is pushed to the agent
// — see AgentOverrides.
func Resolve(overrides map[string]any) map[string]any {
	out := Defaults()
	for key, v := range overrides {
		if v == nil {
			continue // cleared → keep default
		}
		out[key] = v
	}
	return out
}

// AgentOverrides is the sparse map delivered in `config_update` (agent-api.md):
// only explicitly-set overrides, cleared keys stripped. The agent overlays
// these on its env baseline — the #194 fix: the catalog default must never
// clobber a GPU host's QUASAR_ENCODER=nvenc, and a cleared override reverts to
// the agent env, not the catalog default.
func AgentOverrides(overrides map[string]any) map[string]any {
	out := map[string]any{}
	for key, v := range overrides {
		if v == nil {
			continue // cleared → omit → agent reverts to its env baseline
		}
		out[key] = v
	}
	return out
}

// ValidateHomeRootUnder: compose bind-mounts exactly one host path into the
// node-agent (`${QUASAR_HOME_ROOT}:${QUASAR_HOME_ROOT}`), so the only correct
// home_root is agentRoot or a path beneath it — anything else fails silently
// later (phantom dir in the agent, root:root dir on the host, zero bytes-used).
// A write-path constraint only: Store.HomeRoot's resolve ladder is unchanged
// and pre-existing overrides stand. The comparison is path-segment-aware, not
// raw HasPrefix, so "<agentRoot>-evil" is rejected. candidate == "" (clear) is
// always allowed.
func ValidateHomeRootUnder(candidate, agentRoot string) error {
	if candidate == "" {
		return nil
	}
	agentRoot = strings.TrimSpace(agentRoot)
	if agentRoot == "" {
		return fmt.Errorf("this host's agent has not reported a storage root yet, so there is nothing for %q to be a subpath of. "+
			"To give this host a storage root, set QUASAR_HOME_ROOT in deploy/.env for this host and redeploy", candidate)
	}
	root := path.Clean(agentRoot)
	c := path.Clean(candidate)
	if c == root || strings.HasPrefix(c, root+"/") {
		return nil
	}
	return fmt.Errorf("home_root must be this host's reported storage root (%s) or a subdirectory of it, not %q. "+
		"To use a different root entirely, set QUASAR_HOME_ROOT in deploy/.env for this host and redeploy — "+
		"the bind mount has to move with it, which a per-host override here cannot do", root, candidate)
}

// RestartChange reports whether any restart-class knob differs between the old
// and new resolved (or override) maps.
func RestartChange(oldOverrides, newOverrides map[string]any) bool {
	oldR, newR := Resolve(oldOverrides), Resolve(newOverrides)
	for _, k := range Catalog() {
		if k.Class != ClassRestart {
			continue
		}
		if !reflect.DeepEqual(oldR[k.Key], newR[k.Key]) {
			return true
		}
	}
	return false
}
