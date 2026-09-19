// Package readiness turns a host's stored readiness report plus the admin
// overrides on it into the scheduling verdict: which scopes are blocked, which
// checks are behind that, and which overrides lapsed or are inert.
//
// Pure — no database, no other internal package. The same function writes the
// derived scheduling columns (in the transaction that stores a report or
// changes an override) and computes the host body's `readiness_gate` at read
// time, so the two can never disagree.
//
// semantics: control-api.md "Evidence-gated readiness", ADR 0005.
package readiness

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"time"
)

// Gate states, as served in `readiness_gate.state`.
const (
	StateActive     = "active"
	StateAbstaining = "abstaining"
)

// Only these two statuses matter. Every other value — `warn`, `skip`,
// `provisioning`, `unknown`, and anything unrecognized — neither blocks nor
// lapses an override.
const (
	statusFail = "fail"
	statusPass = "pass"
)

const (
	scopeHost  = "host"
	scopeHomes = "homes"
	scopeGPU   = "gpu"
)

// enforcedByAgent marks the agent's own safety states: it refuses those
// launches itself, so no override lifts one.
const enforcedByAgent = "agent"

// Blocking is one entry of the served `readiness_gate.blocking` array.
type Blocking struct {
	CheckID    string `json:"check_id"`
	Scope      string `json:"scope"`
	GPUIndex   *int   `json:"gpu_index"`
	EnforcedBy string `json:"enforced_by"`
	Overridden bool   `json:"overridden"`
}

// Verdict is one evaluation of (report, overrides). The first three fields are
// the derived scheduling columns; Blocking is what the console shows; Lapsed
// and Inert are the override lifecycle.
type Verdict struct {
	BlockHost  bool
	BlockHomes bool
	// BlockedGPUs are `blocks.gpu_index` values, sorted and deduplicated. An
	// index the host does not have is not filtered here — the writer's UPDATE
	// ignores it by construction.
	BlockedGPUs []int
	// Blocking lists every failing check carrying `blocks`, in report order,
	// including overridden ones and ones whose scope or gpu index is unusable:
	// they are facts for the console and block nothing. Never nil.
	Blocking []Blocking
	// Lapsed: overridden ids the report now shows as `pass`. Inert: overridden
	// ids the report does not contain at all.
	Lapsed []string
	Inert  []string
}

// GateState is the freshness half of the gate. Stale is strictly older than the
// window, matching the admission SQL; a timestamp in the future (clock skew
// between pool connections) is fresh.
func GateState(reportedAt *time.Time, now time.Time, window time.Duration) string {
	if reportedAt == nil || now.Sub(*reportedAt) > window {
		return StateAbstaining
	}
	return StateActive
}

// Evaluate is the whole decision. It never mutates its inputs and is
// deterministic: it runs on every stored report and every host read.
func Evaluate(report json.RawMessage, overrides []string) Verdict {
	v := Verdict{Blocking: []Blocking{}}
	checks := decodeReport(report)

	overridden := make(map[string]bool, len(overrides))
	for _, id := range overrides {
		overridden[id] = true
	}

	gpus := map[int]bool{}
	for _, c := range checks {
		if !c.hasBlocks || c.status != statusFail {
			continue
		}
		lifted := overridden[c.id] && c.enforcedBy != enforcedByAgent
		v.Blocking = append(v.Blocking, Blocking{
			CheckID:    c.id,
			Scope:      c.scope,
			GPUIndex:   c.gpuIndex,
			EnforcedBy: c.enforcedBy,
			Overridden: lifted,
		})
		if lifted {
			continue
		}
		switch c.scope {
		case scopeHost:
			v.BlockHost = true
		case scopeHomes:
			v.BlockHomes = true
		case scopeGPU:
			if c.gpuIndex != nil {
				gpus[*c.gpuIndex] = true
			}
		}
	}
	for i := range gpus {
		v.BlockedGPUs = append(v.BlockedGPUs, i)
	}
	sort.Ints(v.BlockedGPUs)

	present := make(map[string]bool, len(checks))
	passed := make(map[string]bool, len(checks))
	for _, c := range checks {
		present[c.id] = true
		if c.status == statusPass {
			passed[c.id] = true
		}
	}
	seen := make(map[string]bool, len(overrides))
	for _, id := range overrides {
		if seen[id] {
			continue
		}
		seen[id] = true
		switch {
		case passed[id]:
			v.Lapsed = append(v.Lapsed, id)
		case !present[id]:
			v.Inert = append(v.Inert, id)
		}
	}
	return v
}

// check is one decoded report entry.
type check struct {
	id         string
	status     string
	hasBlocks  bool
	scope      string
	enforcedBy string
	gpuIndex   *int
}

// decodeReport decodes field by field rather than into a struct: the report is
// agent-owned and forward-compatible, so one odd value must make its own entry
// inert without voiding the well-formed checks beside it.
func decodeReport(raw json.RawMessage) []check {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]check, 0, len(items))
	for _, item := range items {
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) != nil {
			continue
		}
		id, ok := jsonString(fields["id"])
		if !ok || id == "" {
			continue
		}
		c := check{id: id}
		c.status, _ = jsonString(fields["status"])
		var blocks map[string]json.RawMessage
		if json.Unmarshal(fields["blocks"], &blocks) == nil && blocks != nil {
			c.hasBlocks = true
			c.scope, _ = jsonString(blocks["scope"])
			c.enforcedBy, _ = jsonString(blocks["enforced_by"])
			c.gpuIndex = jsonIndex(blocks["gpu_index"])
		}
		out = append(out, c)
	}
	return out
}

func jsonString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// jsonIndex accepts only a non-negative JSON integer; "1", 1.5 and null are not
// a GPU index and make the check block nothing. It decodes through `any` with
// UseNumber because unmarshalling the JSON string "1" straight into a
// json.Number succeeds — the quotes would otherwise be invisible here.
func jsonIndex(raw json.RawMessage) *int {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	n, ok := v.(json.Number)
	if !ok {
		return nil
	}
	i, err := strconv.Atoi(n.String())
	if err != nil || i < 0 {
		return nil
	}
	return &i
}
