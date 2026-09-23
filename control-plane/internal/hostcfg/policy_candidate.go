package hostcfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// groupCandidate is one next-session group's desired content, ready to offer.
// Prerequisites holds only the deployment_baseline fact; the caller adds the
// active-snapshot fact it has from journal inventory.
type groupCandidate struct {
	Group         string
	Scope         string
	Revision      int64
	Settings      map[string]PolicyChoice
	Resolved      map[string]any
	Digest        string
	Prerequisites []any
}

// resolveGroupCandidate resolves a next-session group from saved choices (an
// absent choice is deployment) and the agent's current-connection baseline.
// A non-empty reason means no candidate exists yet: `baseline_unavailable`
// when a needed deployment value is missing or indeterminate, or
// `unsupported_source`. semantics: agent-api.md §Offer, result, revocation.
func resolveGroupCandidate(group string, revision int64, choices map[string]PolicyChoice, baseline map[string]any) (*groupCandidate, string, error) {
	scope, ok := PolicyGroupScope(group)
	if !ok {
		return nil, "", fmt.Errorf("unknown policy group %q", group)
	}
	if scope != "next_session" {
		return nil, "", fmt.Errorf("policy group %q is %s scope", group, scope)
	}
	settings := map[string]PolicyChoice{}
	resolved := map[string]any{}
	deployed := map[string]any{}
	for _, key := range PolicyGroupKeys(group) {
		choice, ok := choices[key]
		if !ok {
			choice = PolicyChoice{Source: "deployment"}
		}
		switch choice.Source {
		case "explicit":
			resolved[key] = choice.Value
		case "deployment":
			value, present := baseline[key]
			if baseline == nil || !present || (value == nil && !byKey()[key].Nullable) {
				return nil, "baseline_unavailable", nil
			}
			if value != nil {
				if err := validateValue(byKey()[key], value); err != nil {
					return nil, "baseline_unavailable", nil
				}
			}
			resolved[key] = value
			deployed[key] = value
		default:
			return nil, "unsupported_source", nil
		}
		settings[key] = choice
	}
	candidate := &groupCandidate{Group: group, Scope: scope, Revision: revision, Settings: settings, Resolved: resolved, Prerequisites: []any{}}
	var err error
	if len(deployed) > 0 {
		fact, err := digestJSON(deployed)
		if err != nil {
			return nil, "", err
		}
		candidate.Prerequisites = append(candidate.Prerequisites, map[string]any{"kind": "deployment_baseline", "id": fact})
	}
	candidate.Digest, err = digestJSON(map[string]any{
		"group": group, "scope": scope, "revision": strconv.FormatInt(revision, 10),
		"settings": settings, "resolved_settings": resolved,
	})
	if err != nil {
		return nil, "", err
	}
	return candidate, "", nil
}

// canonicalJSON is RFC 8785 serialization for the catalog value space: sorted
// object keys, ES6 number form (Go's encoder already matches it) and no HTML
// escaping. The agent's twin is policy_catalog::canonical_json.
func canonicalJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
