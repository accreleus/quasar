package hostcfg

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// PolicyKeySpec is one row of the frozen RH05 source/effect/evidence matrix
// (docs/design/rh05-contract-proposal.md). Group names follow agent-api.md:
// `hardware` owns encoder/render_node/cuda_device, every other key is its own
// group. Link names keys whose values are validated together.
type PolicyKeySpec struct {
	Key       string
	Group     string
	Scope     string
	Automatic bool
	Link      string
}

const (
	linkABRMode       = "abr_mode"
	linkResHysteresis = "res_hysteresis"
	linkHomeStorage   = "home_storage"
)

var policyMatrix = func() []PolicyKeySpec {
	specs := make([]PolicyKeySpec, 0, 39)
	for _, knob := range Catalog() {
		spec := PolicyKeySpec{Key: knob.Key, Group: knob.Key, Scope: "next_session"}
		switch knob.Key {
		case "encoder", "render_node":
			spec.Group, spec.Scope, spec.Automatic = "hardware", "restart", true
		case "cuda_device":
			spec.Group, spec.Scope = "hardware", "restart"
		case "abr_enabled", "abr_mode":
			spec.Link = linkABRMode
		case "abr_ladder_res_engage_frac", "abr_ladder_res_recover_frac":
			spec.Link = linkResHysteresis
		case "home_root":
			spec.Link = linkHomeStorage
		}
		specs = append(specs, spec)
	}
	return specs
}()

// PolicyKeySpecs returns the matrix in catalog order.
func PolicyKeySpecs() []PolicyKeySpec { return append([]PolicyKeySpec(nil), policyMatrix...) }

// PolicySpec returns key's matrix row.
func PolicySpec(key string) (PolicyKeySpec, bool) {
	for _, spec := range policyMatrix {
		if spec.Key == key {
			return spec, true
		}
	}
	return PolicyKeySpec{}, false
}

// PolicyGroupKeys returns a group's catalog keys, sorted.
func PolicyGroupKeys(group string) []string {
	var keys []string
	for _, spec := range policyMatrix {
		if spec.Group == group {
			keys = append(keys, spec.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

// IsPolicyGroup reports whether group is a known wire group name.
func IsPolicyGroup(group string) bool { return len(PolicyGroupKeys(group)) > 0 }

// PolicyGroupScope returns a known group's scope.
func PolicyGroupScope(group string) (string, bool) {
	for _, spec := range policyMatrix {
		if spec.Group == group {
			return spec.Scope, true
		}
	}
	return "", false
}

// NextSessionPolicyGroups returns every next-session group, sorted. These are
// the groups a version 2 agent may typed-own without idle_apply.
func NextSessionPolicyGroups() []string {
	seen := map[string]bool{}
	var groups []string
	for _, spec := range policyMatrix {
		if spec.Scope == "next_session" && !seen[spec.Group] {
			seen[spec.Group] = true
			groups = append(groups, spec.Group)
		}
	}
	sort.Strings(groups)
	return groups
}

// PolicyValidationError is a whole-request rejection. Code is the typed error
// from control-api.md amendment 13 (`validation_failed`, `unsupported_source`,
// `home_conflict`); Keys names every setting involved.
type PolicyValidationError struct {
	Code    string
	Keys    []string
	Message string
}

func (e *PolicyValidationError) Error() string { return e.Message }

func policyInvalid(code, message string, keys ...string) *PolicyValidationError {
	sort.Strings(keys)
	return &PolicyValidationError{Code: code, Keys: keys, Message: message}
}

// PolicyEditContext is what a policy edit is validated against. Baseline is the
// agent's last reported deployment baseline (nil when unknown); a deployment
// choice resolves only from it, never from a catalog default.
type PolicyEditContext struct {
	Current          map[string]PolicyChoice
	Baseline         map[string]any
	MountedHomeRoot  string
	ExistingHomeRefs []string
}

// ValidatePolicyEdit validates a whole edit before any persistence: keys,
// sources, catalog types and bounds, then cross-key rules over the resulting
// choices. One bad key rejects the edit.
func ValidatePolicyEdit(changes map[string]PolicyChoice, ctx PolicyEditContext) error {
	if len(changes) == 0 {
		return policyInvalid("validation_failed", "changes must not be empty")
	}
	keys := make([]string, 0, len(changes))
	for key := range changes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := validatePolicyChoice(key, changes[key]); err != nil {
			return err
		}
	}
	merged := map[string]PolicyChoice{}
	for key, choice := range ctx.Current {
		merged[key] = choice
	}
	for key, choice := range changes {
		merged[key] = choice
	}
	if err := validateResHysteresis(merged, ctx.Baseline); err != nil {
		return err
	}
	if choice, ok := changes["home_root"]; ok && choice.Source == "explicit" {
		if err := validateHomeRootChoice(choice.Value.(string), ctx); err != nil {
			return err
		}
	}
	return nil
}

func validatePolicyChoice(key string, choice PolicyChoice) error {
	spec, ok := PolicySpec(key)
	if !ok {
		return policyInvalid("validation_failed", fmt.Sprintf("unknown setting %q", key), key)
	}
	switch choice.Source {
	case "explicit":
		if choice.Value == nil {
			return policyInvalid("validation_failed", fmt.Sprintf("%q requires a value", key), key)
		}
		if err := validateValue(byKey()[key], choice.Value); err != nil {
			return policyInvalid("validation_failed", err.Error(), key)
		}
	case "deployment":
		if choice.Value != nil {
			return policyInvalid("validation_failed", fmt.Sprintf("%q deployment forbids a value", key), key)
		}
	case "automatic":
		if choice.Value != nil {
			return policyInvalid("validation_failed", fmt.Sprintf("%q automatic forbids a value", key), key)
		}
		if !spec.Automatic {
			return policyInvalid("unsupported_source", fmt.Sprintf("%q does not support automatic", key), key)
		}
	default:
		return policyInvalid("validation_failed", fmt.Sprintf("%q has invalid source", key), key)
	}
	return nil
}

// resolvedPolicyValue returns key's value when the control plane knows it: an
// explicit choice, or a deployment choice with a reported baseline value.
func resolvedPolicyValue(key string, choices map[string]PolicyChoice, baseline map[string]any) (any, bool) {
	choice, ok := choices[key]
	if ok && choice.Source == "explicit" {
		return choice.Value, true
	}
	if (!ok || choice.Source == "deployment") && baseline != nil {
		value, ok := baseline[key]
		return value, ok && value != nil
	}
	return nil, false
}

func validateResHysteresis(choices map[string]PolicyChoice, baseline map[string]any) error {
	engageRaw, eok := resolvedPolicyValue("abr_ladder_res_engage_frac", choices, baseline)
	recoverRaw, rok := resolvedPolicyValue("abr_ladder_res_recover_frac", choices, baseline)
	if !eok || !rok {
		return nil
	}
	if err := ValidateResolved(map[string]any{"abr_ladder_res_engage_frac": engageRaw, "abr_ladder_res_recover_frac": recoverRaw}); err != nil {
		return policyInvalid("validation_failed", err.Error(), "abr_ladder_res_engage_frac", "abr_ladder_res_recover_frac")
	}
	return nil
}

func validateHomeRootChoice(candidate string, ctx PolicyEditContext) error {
	if candidate != "" {
		if err := ValidateHomeRootUnder(candidate, ctx.MountedHomeRoot); err != nil {
			return policyInvalid("validation_failed", err.Error(), "home_root")
		}
	}
	var stranded []string
	root := path.Clean(candidate)
	for _, ref := range ctx.ExistingHomeRefs {
		clean := path.Clean(ref)
		if candidate == "" || (clean != root && !strings.HasPrefix(clean, root+"/")) {
			stranded = append(stranded, ref)
		}
	}
	if len(stranded) > 0 {
		return policyInvalid("home_conflict", fmt.Sprintf(
			"home_root %q would leave %d existing managed home(s) on this host outside the storage root; "+
				"keep a root that contains them or move those homes first", candidate, len(stranded)), "home_root")
	}
	return nil
}
