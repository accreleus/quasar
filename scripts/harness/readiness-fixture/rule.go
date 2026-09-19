package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// syntheticPrefix is the only id prefix a rule's injected check may use — the
// harness's proof that production code never speaks this vocabulary keys on it
// (docs/superpowers/plans/2026-09-19-rh02-264-harness-matrix.md "Why the
// synthetic check cannot reach production").
const syntheticPrefix = "harness_synthetic_"

// Blocks mirrors protocol/agent-api.md capacity.readiness[].blocks verbatim.
type Blocks struct {
	Scope      string `json:"scope"`
	GPUIndex   *int   `json:"gpu_index,omitempty"`
	EnforcedBy string `json:"enforced_by"`
}

// Check mirrors one protocol/agent-api.md capacity.readiness[] entry. ObservedAt
// and Source are amendment-11 optional fields; Blocks is present only on a
// check that rests on evidence.
type Check struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	Summary     string  `json:"summary"`
	Remediation string  `json:"remediation"`
	ObservedAt  string  `json:"observed_at,omitempty"`
	Source      string  `json:"source,omitempty"`
	Blocks      *Blocks `json:"blocks,omitempty"`
}

// Rule is the relay's current instruction, set over the loopback control HTTP
// port. Mode "off" forwards every capacity message untouched; "inject" rewrites
// the readiness array of every upstream capacity message that carries one.
type Rule struct {
	Mode  string `json:"mode"`
	Check *Check `json:"check,omitempty"`
}

// Validate enforces the one rule invariant that matters for the "cannot reach
// production" proof: an injected check id must start with syntheticPrefix.
func (r Rule) Validate() error {
	switch r.Mode {
	case "off":
		return nil
	case "inject":
		if r.Check == nil {
			return errors.New("inject mode requires a check")
		}
		if !strings.HasPrefix(r.Check.ID, syntheticPrefix) {
			return fmt.Errorf("check id %q must start with %q", r.Check.ID, syntheticPrefix)
		}
		return nil
	default:
		return fmt.Errorf("unknown mode %q, want \"off\" or \"inject\"", r.Mode)
	}
}

// defaultRule is what a fresh relay starts with — never rewrites anything.
func defaultRule() Rule { return Rule{Mode: "off"} }

// nowRFC3339 is the injected check's observed_at clock — a package var so tests
// can pin it.
var nowRFC3339 = func() string { return time.Now().UTC().Format(time.RFC3339) }

// rewriteCapacity applies rule to one upstream (agent -> control) WS text frame.
// It returns the frame unchanged (same slice) unless a rewrite actually applies,
// so "verbatim forwarding" is not just meaning-preserving but byte-identical in
// every case that doesn't call for a rewrite.
func rewriteCapacity(frame []byte, rule Rule) (out []byte, rewritten bool, err error) {
	if rule.Mode != "inject" {
		return frame, false, nil
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(frame, &top); err != nil {
		// Not a JSON object at all — not our concern, forward verbatim.
		return frame, false, nil
	}
	typeRaw, ok := top["type"]
	if !ok {
		return frame, false, nil
	}
	var typ string
	if err := json.Unmarshal(typeRaw, &typ); err != nil || typ != "capacity" {
		return frame, false, nil
	}
	readinessRaw, ok := top["readiness"]
	if !ok || string(readinessRaw) == "null" {
		return frame, false, nil
	}

	var checks []map[string]json.RawMessage
	if err := json.Unmarshal(readinessRaw, &checks); err != nil {
		return nil, false, fmt.Errorf("capacity.readiness is not an array of objects: %w", err)
	}

	kept := checks[:0:0]
	for _, c := range checks {
		if idMatchesSyntheticPrefix(c) {
			continue
		}
		kept = append(kept, c)
	}

	injected, err := checkToRawObject(*rule.Check, nowRFC3339())
	if err != nil {
		return nil, false, err
	}
	kept = append(kept, injected)

	newReadiness, err := json.Marshal(kept)
	if err != nil {
		return nil, false, err
	}
	top["readiness"] = newReadiness

	out, err = json.Marshal(top)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func idMatchesSyntheticPrefix(c map[string]json.RawMessage) bool {
	idRaw, ok := c["id"]
	if !ok {
		return false
	}
	var id string
	if err := json.Unmarshal(idRaw, &id); err != nil {
		return false
	}
	return strings.HasPrefix(id, syntheticPrefix)
}

// checkToRawObject marshals a Check into the map[string]json.RawMessage shape
// rewriteCapacity assembles its array from, with observed_at forced to now —
// the rule's own observed_at, if any, is not the wire truth once it crosses
// the relay.
func checkToRawObject(c Check, observedAt string) (map[string]json.RawMessage, error) {
	c.ObservedAt = observedAt
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
