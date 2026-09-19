package readiness

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// The rule (control-api.md "Evidence-gated readiness", ADR 0005): a readiness
// check blocks only when it carries `blocks` and its status is `fail`. These
// tables are the whole decision; admission never parses a report, it reads the
// columns this verdict is written to.

func ip(i int) *int { return &i }

type chk struct {
	ID     string `json:"id"`
	Status any    `json:"status"`
	Blocks any    `json:"blocks,omitempty"`
}

func blk(scope string, enforcedBy string) map[string]any {
	return map[string]any{"scope": scope, "enforced_by": enforcedBy}
}

func gpuBlk(index any) map[string]any {
	return map[string]any{"scope": "gpu", "gpu_index": index, "enforced_by": "control_plane"}
}

func report(t *testing.T, checks ...chk) json.RawMessage {
	t.Helper()
	if checks == nil {
		checks = []chk{}
	}
	raw, err := json.Marshal(checks)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type scopes struct {
	host, homes bool
	gpus        []int
}

func scopesOf(v Verdict) scopes {
	g := v.BlockedGPUs
	if len(g) == 0 {
		g = nil
	}
	return scopes{v.BlockHost, v.BlockHomes, g}
}

// TestVerdictScopeTable: every row of the contract's scope table, and the rows
// the agent actually emits for them.
func TestVerdictScopeTable(t *testing.T) {
	cases := []struct {
		name  string
		check chk
		want  scopes
	}{
		{"media probe fails on gpu 1", chk{"media_probe_gpu1", "fail", gpuBlk(1)}, scopes{gpus: []int{1}}},
		{"application gpu probe fails on gpu 0", chk{"application_gpu_probe_gpu0", "fail", gpuBlk(0)}, scopes{gpus: []int{0}}},
		{"input probe fails", chk{"input_probe", "fail", blk("host", "control_plane")}, scopes{host: true}},
		{"audio probe fails", chk{"audio_probe", "fail", blk("host", "control_plane")}, scopes{host: true}},
		{"homes root unwritable", chk{"homes_root_writable", "fail", blk("homes", "control_plane")}, scopes{homes: true}},
		{"homes storage exhausted", chk{"homes_free_space", "fail", blk("homes", "control_plane")}, scopes{homes: true}},
		{"runtime unreachable (agent-enforced)", chk{"runtime_endpoint", "fail", blk("host", "agent")}, scopes{host: true}},
		{"startup cleanup unresolved (agent-enforced)", chk{"startup_cleanup", "fail", blk("host", "agent")}, scopes{host: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Evaluate(report(t, tc.check), nil)
			if got := scopesOf(v); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("blocked scopes = %+v, want %+v", got, tc.want)
			}
			if len(v.Blocking) != 1 || v.Blocking[0].CheckID != tc.check.ID || v.Blocking[0].Overridden {
				t.Fatalf("blocking = %+v, want the one failing check, not overridden", v.Blocking)
			}
		})
	}
}

// TestVerdictInertCases: everything that must never block.
func TestVerdictInertCases(t *testing.T) {
	host := blk("host", "control_plane")
	cases := []struct {
		name   string
		report json.RawMessage
	}{
		{"never reported (NULL)", nil},
		{"JSON null", json.RawMessage(`null`)},
		{"explicit empty report", json.RawMessage(`[]`)},
		{"not an array", json.RawMessage(`{"id":"x","status":"fail","blocks":{"scope":"host","enforced_by":"control_plane"}}`)},
		{"not JSON", json.RawMessage(`[{"id":`)},
		{"proxy check fails (no blocks)", report(t, chk{"render_node", "fail", nil})},
		{"every proxy fails", report(t, chk{"uinput", "fail", nil}, chk{"media_reachability", "fail", nil}, chk{"user_namespaces", "fail", nil})},
		{"evidence check passes", report(t, chk{"input_probe", "pass", host})},
		{"evidence check unknown", report(t, chk{"input_probe", "unknown", host})},
		{"evidence check warn", report(t, chk{"homes_free_space", "warn", blk("homes", "control_plane")})},
		{"evidence check skip", report(t, chk{"media_probe_gpu0", "skip", gpuBlk(0)})},
		{"evidence check provisioning", report(t, chk{"input_probe", "provisioning", host})},
		{"unrecognised status", report(t, chk{"input_probe", "degraded", host})},
		{"status is not exactly fail", report(t, chk{"input_probe", "FAIL", host}, chk{"audio_probe", " fail", host}, chk{"x", "failed", host})},
		{"status is not a string", report(t, chk{"input_probe", true, host}, chk{"audio_probe", 1, host}, chk{"x", nil, host})},
		{"unrecognised scope", report(t, chk{"rack_probe", "fail", blk("rack", "control_plane")})},
		{"scope is not a string", report(t, chk{"x", "fail", map[string]any{"scope": 7, "enforced_by": "control_plane"}})},
		{"blocks is not an object", report(t, chk{"x", "fail", "host"}, chk{"y", "fail", true}, chk{"z", "fail", []string{"host"}})},
		{"blocks is an empty object", report(t, chk{"x", "fail", map[string]any{}})},
		{"gpu scope without an index", report(t, chk{"media_probe", "fail", map[string]any{"scope": "gpu", "enforced_by": "control_plane"}})},
		{"gpu scope with a non-integer index", report(t, chk{"a", "fail", gpuBlk("1")}, chk{"b", "fail", gpuBlk(1.5)}, chk{"c", "fail", gpuBlk(nil)})},
		{"gpu scope with a negative index", report(t, chk{"a", "fail", gpuBlk(-1)})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Evaluate(tc.report, nil)
			if got := scopesOf(v); !reflect.DeepEqual(got, scopes{}) {
				t.Fatalf("blocked scopes = %+v, want nothing blocked", got)
			}
		})
	}
}

// TestVerdictOneBadCheckDoesNotHideAnother: a malformed entry is inert on its
// own; it must not swallow a well-formed failing check beside it.
func TestVerdictOneBadCheckDoesNotHideAnother(t *testing.T) {
	raw := json.RawMessage(`[
		{"id":"weird","status":"fail","blocks":"host"},
		{"id":"also_weird","status":7,"blocks":{"scope":"host","enforced_by":"control_plane"}},
		{"id":"audio_probe","status":"fail","summary":"s","remediation":"r","future_field":{"a":1},
		 "blocks":{"scope":"host","enforced_by":"control_plane","future":true}}
	]`)
	v := Evaluate(raw, nil)
	if !v.BlockHost {
		t.Fatalf("a malformed neighbour hid a failing evidence check: %+v", v)
	}
}

// TestVerdictCollapse: several checks on one scope collapse, and a scope stays
// blocked until EVERY unoverridden failing check naming it is gone.
func TestVerdictCollapse(t *testing.T) {
	host := blk("host", "control_plane")
	raw := report(t,
		chk{"input_probe", "fail", host},
		chk{"audio_probe", "fail", host},
		chk{"media_probe_gpu1", "fail", gpuBlk(1)},
		chk{"application_gpu_probe_gpu1", "fail", gpuBlk(1)},
		chk{"media_probe_gpu0", "fail", gpuBlk(0)},
		chk{"homes_root_writable", "pass", blk("homes", "control_plane")},
	)

	v := Evaluate(raw, nil)
	if got, want := scopesOf(v), (scopes{host: true, gpus: []int{0, 1}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("no overrides: %+v, want %+v (GPU indices sorted and deduplicated)", got, want)
	}
	if len(v.Blocking) != 5 {
		t.Fatalf("blocking lists %d entries, want all 5 failing evidence checks in report order: %+v", len(v.Blocking), v.Blocking)
	}
	for i, id := range []string{"input_probe", "audio_probe", "media_probe_gpu1", "application_gpu_probe_gpu1", "media_probe_gpu0"} {
		if v.Blocking[i].CheckID != id {
			t.Fatalf("blocking[%d] = %s, want %s (report order)", i, v.Blocking[i].CheckID, id)
		}
	}

	// Overriding one of two checks on a scope changes that entry and nothing else.
	v = Evaluate(raw, []string{"input_probe", "media_probe_gpu1"})
	if got, want := scopesOf(v), (scopes{host: true, gpus: []int{0, 1}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("one of two overridden: %+v, want %+v", got, want)
	}
	for _, b := range v.Blocking {
		want := b.CheckID == "input_probe" || b.CheckID == "media_probe_gpu1"
		if b.Overridden != want {
			t.Fatalf("%s overridden=%v, want %v", b.CheckID, b.Overridden, want)
		}
	}

	// Overriding every check on a scope unblocks exactly that scope.
	v = Evaluate(raw, []string{"input_probe", "audio_probe", "media_probe_gpu1", "application_gpu_probe_gpu1"})
	if got, want := scopesOf(v), (scopes{gpus: []int{0}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("host and gpu1 fully overridden: %+v, want %+v", got, want)
	}
	if len(v.Blocking) != 5 {
		t.Fatalf("an override must never hide a failing check: blocking=%+v", v.Blocking)
	}
}

// TestVerdictBlockingEntryShape: the served readiness_gate.blocking entry.
func TestVerdictBlockingEntryShape(t *testing.T) {
	v := Evaluate(report(t,
		chk{"media_probe_gpu1", "fail", gpuBlk(1)},
		chk{"runtime_endpoint", "fail", blk("host", "agent")},
	), []string{"media_probe_gpu1"})
	want := []Blocking{
		{CheckID: "media_probe_gpu1", Scope: "gpu", GPUIndex: ip(1), EnforcedBy: "control_plane", Overridden: true},
		{CheckID: "runtime_endpoint", Scope: "host", GPUIndex: nil, EnforcedBy: "agent", Overridden: false},
	}
	if !reflect.DeepEqual(v.Blocking, want) {
		t.Fatalf("blocking = %+v, want %+v", v.Blocking, want)
	}
	raw, err := json.Marshal(v.Blocking)
	if err != nil {
		t.Fatal(err)
	}
	const wire = `[{"check_id":"media_probe_gpu1","scope":"gpu","gpu_index":1,"enforced_by":"control_plane","overridden":true},` +
		`{"check_id":"runtime_endpoint","scope":"host","gpu_index":null,"enforced_by":"agent","overridden":false}]`
	if string(raw) != wire {
		t.Fatalf("wire shape:\n got %s\nwant %s", raw, wire)
	}

	// Nothing blocking serialises as [], never null: the field is always served.
	empty, _ := json.Marshal(Evaluate(json.RawMessage(`[]`), nil).Blocking)
	if string(empty) != `[]` {
		t.Fatalf("empty blocking serialises as %s, want []", empty)
	}
	never, _ := json.Marshal(Evaluate(nil, nil).Blocking)
	if string(never) != `[]` {
		t.Fatalf("never-reported blocking serialises as %s, want []", never)
	}
}

// TestVerdictAgentEnforcedIsNeverLifted: the agent refuses those launches
// itself, so an override row for such a check (however it got there) lifts
// nothing and is not shown as overriding.
func TestVerdictAgentEnforcedIsNeverLifted(t *testing.T) {
	v := Evaluate(report(t, chk{"startup_cleanup", "fail", blk("host", "agent")}), []string{"startup_cleanup"})
	if !v.BlockHost {
		t.Fatal("an override lifted an agent-enforced block")
	}
	if v.Blocking[0].Overridden {
		t.Fatal("an agent-enforced check reads as overridden")
	}
}

// TestVerdictOverrideLifecycle: lapse on pass and only on pass; inert when the
// id is gone. Decided here so the report write and the override write cannot
// disagree about it.
func TestVerdictOverrideLifecycle(t *testing.T) {
	host := blk("host", "control_plane")
	cases := []struct {
		name       string
		report     json.RawMessage
		wantLapsed []string
		wantInert  []string
		wantHost   bool
	}{
		{"still failing: held", report(t, chk{"audio_probe", "fail", host}), nil, nil, false},
		{"passes: lapses", report(t, chk{"audio_probe", "pass", host}), []string{"audio_probe"}, nil, false},
		{"unknown: held", report(t, chk{"audio_probe", "unknown", host}), nil, nil, false},
		{"warn: held", report(t, chk{"audio_probe", "warn", host}), nil, nil, false},
		{"skip: held", report(t, chk{"audio_probe", "skip", host}), nil, nil, false},
		{"id vanished: inert, not lapsed", report(t, chk{"input_probe", "fail", host}), nil, []string{"audio_probe"}, true},
		{"explicit empty report: inert, not lapsed", json.RawMessage(`[]`), nil, []string{"audio_probe"}, false},
		{"never reported: inert, not lapsed", nil, nil, []string{"audio_probe"}, false},
		{"malformed report: inert, not lapsed", json.RawMessage(`{`), nil, []string{"audio_probe"}, false},
		{"passes but no longer carries blocks: still a pass, lapses", report(t, chk{"audio_probe", "pass", nil}), []string{"audio_probe"}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Evaluate(tc.report, []string{"audio_probe"})
			if !reflect.DeepEqual(nilIfEmpty(v.Lapsed), tc.wantLapsed) {
				t.Errorf("lapsed = %v, want %v", v.Lapsed, tc.wantLapsed)
			}
			if !reflect.DeepEqual(nilIfEmpty(v.Inert), tc.wantInert) {
				t.Errorf("inert = %v, want %v", v.Inert, tc.wantInert)
			}
			if v.BlockHost != tc.wantHost {
				t.Errorf("BlockHost = %v, want %v", v.BlockHost, tc.wantHost)
			}
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// TestVerdictIsDeterministic: same inputs, same output, inputs untouched — it
// runs on every report and every host read.
func TestVerdictIsDeterministic(t *testing.T) {
	raw := report(t, chk{"media_probe_gpu2", "fail", gpuBlk(2)}, chk{"media_probe_gpu0", "fail", gpuBlk(0)})
	before := string(raw)
	overrides := []string{"zzz", "media_probe_gpu0"}
	a, b := Evaluate(raw, overrides), Evaluate(raw, overrides)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("two evaluations differ:\n%+v\n%+v", a, b)
	}
	if string(raw) != before || !reflect.DeepEqual(overrides, []string{"zzz", "media_probe_gpu0"}) {
		t.Fatal("Evaluate mutated its inputs")
	}
}

// TestGateState: the gate abstains on a never-reported or stale report, and
// only then. Stale is strictly older than the window, matching the SQL filter.
func TestGateState(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	window := 60 * time.Second
	at := func(age time.Duration) *time.Time { ts := now.Add(-age); return &ts }
	cases := []struct {
		name       string
		reportedAt *time.Time
		want       string
	}{
		{"never reported", nil, StateAbstaining},
		{"just reported", at(0), StateActive},
		{"one report interval old", at(15 * time.Second), StateActive},
		{"exactly the window", at(window), StateActive},
		{"one millisecond past the window", at(window + time.Millisecond), StateAbstaining},
		{"an hour old", at(time.Hour), StateAbstaining},
		{"stamped slightly in the future (clock skew between pool connections)", at(-2 * time.Second), StateActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GateState(tc.reportedAt, now, window); got != tc.want {
				t.Fatalf("GateState = %q, want %q", got, tc.want)
			}
		})
	}
	if StateActive != "active" || StateAbstaining != "abstaining" {
		t.Fatalf("wire values changed: %q %q", StateActive, StateAbstaining)
	}
}

// TestFindBlocking: the override write path's only view of a report.
func TestFindBlocking(t *testing.T) {
	r := report(t,
		chk{ID: "audio_probe", Status: "fail", Blocks: blk("host", "control_plane")},
		chk{ID: "startup_cleanup", Status: "fail", Blocks: blk("host", "agent")},
		chk{ID: "render_node", Status: "fail"},
		chk{ID: "input_probe", Status: "pass", Blocks: blk("host", "control_plane")},
	)
	cases := []struct {
		name       string
		checkID    string
		want       bool
		enforcedBy string
	}{
		{"blocking and control-plane enforced", "audio_probe", true, "control_plane"},
		{"blocking but agent enforced", "startup_cleanup", true, "agent"},
		{"no blocks (a proxy)", "render_node", false, ""},
		{"passing", "input_probe", false, ""},
		{"absent", "no_such_check", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FindBlocking(r, tc.checkID)
			if ok != tc.want {
				t.Fatalf("FindBlocking(%q) ok = %v, want %v", tc.checkID, ok, tc.want)
			}
			if ok && got.EnforcedBy != tc.enforcedBy {
				t.Fatalf("FindBlocking(%q).EnforcedBy = %q, want %q", tc.checkID, got.EnforcedBy, tc.enforcedBy)
			}
		})
	}
}
