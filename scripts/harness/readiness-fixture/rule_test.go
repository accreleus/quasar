package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuleValidate(t *testing.T) {
	cases := []struct {
		name    string
		rule    Rule
		wantErr bool
	}{
		{"off", Rule{Mode: "off"}, false},
		{"inject ok", Rule{Mode: "inject", Check: &Check{ID: "harness_synthetic_gate", Status: "fail"}}, false},
		{"inject missing check", Rule{Mode: "inject"}, true},
		{"inject bad prefix", Rule{Mode: "inject", Check: &Check{ID: "not_synthetic", Status: "fail"}}, true},
		{"unknown mode", Rule{Mode: "bogus"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestRewriteCapacityModeOff(t *testing.T) {
	frame := []byte(`{"type":"capacity","readiness":[{"id":"x","status":"pass"}]}`)
	out, rewritten, err := rewriteCapacity(frame, Rule{Mode: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if rewritten {
		t.Fatal("mode off must never rewrite")
	}
	if string(out) != string(frame) {
		t.Fatalf("mode off must forward verbatim, got %s", out)
	}
}

func TestRewriteCapacityNonCapacityUntouched(t *testing.T) {
	frame := []byte(`{"type":"heartbeat","running_sessions":[]}`)
	rule := Rule{Mode: "inject", Check: &Check{ID: "harness_synthetic_gate", Status: "fail"}}
	out, rewritten, err := rewriteCapacity(frame, rule)
	if err != nil {
		t.Fatal(err)
	}
	if rewritten {
		t.Fatal("non-capacity message must never be rewritten")
	}
	if string(out) != string(frame) {
		t.Fatalf("non-capacity must forward verbatim, got %s", out)
	}
}

func TestRewriteCapacityWithoutReadinessUntouched(t *testing.T) {
	frame := []byte(`{"type":"capacity","host":{"cpu_cores":16}}`)
	rule := Rule{Mode: "inject", Check: &Check{ID: "harness_synthetic_gate", Status: "fail"}}
	out, rewritten, err := rewriteCapacity(frame, rule)
	if err != nil {
		t.Fatal(err)
	}
	if rewritten {
		t.Fatal("capacity without readiness must never be rewritten")
	}
	if string(out) != string(frame) {
		t.Fatalf("must forward verbatim, got %s", out)
	}
}

func TestRewriteCapacityInjectsAndPreservesUnknownFields(t *testing.T) {
	nowRFC3339 = func() string { return "2026-09-19T00:00:00Z" }
	defer func() { nowRFC3339 = func() string { return "" } }()

	frame := []byte(`{
		"type":"capacity",
		"host":{"cpu_cores":16,"weird_future_field":{"nested":true}},
		"codecs":["h264"],
		"readiness":[
			{"id":"nvidia_egl_vendor_json","status":"pass","summary":"ok","remediation":"","future_key":"kept"},
			{"id":"harness_synthetic_old","status":"pass","summary":"stale","remediation":""}
		]
	}`)
	rule := Rule{Mode: "inject", Check: &Check{
		ID: "harness_synthetic_gate", Status: "fail", Summary: "injected", Remediation: "clear the rule",
		Source: "host_probe", Blocks: &Blocks{Scope: "host", EnforcedBy: "control_plane"},
	}}

	out, rewritten, err := rewriteCapacity(frame, rule)
	if err != nil {
		t.Fatal(err)
	}
	if !rewritten {
		t.Fatal("expected a rewrite")
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}

	// Unknown/other fields survive untouched.
	var host map[string]json.RawMessage
	if err := json.Unmarshal(top["host"], &host); err != nil {
		t.Fatal(err)
	}
	if string(host["weird_future_field"]) != `{"nested":true}` {
		t.Fatalf("unrelated field mutated: %s", host["weird_future_field"])
	}
	if string(top["codecs"]) != `["h264"]` {
		t.Fatalf("unrelated top-level field mutated: %s", top["codecs"])
	}

	var checks []map[string]json.RawMessage
	if err := json.Unmarshal(top["readiness"], &checks); err != nil {
		t.Fatal(err)
	}
	if len(checks) != 2 {
		t.Fatalf("want 2 checks (1 kept + 1 injected), got %d: %v", len(checks), checks)
	}

	var keptID string
	json.Unmarshal(checks[0]["id"], &keptID)
	if keptID != "nvidia_egl_vendor_json" {
		t.Fatalf("expected the non-synthetic check preserved first, got %s", keptID)
	}
	if string(checks[0]["future_key"]) != `"kept"` {
		t.Fatalf("preserved check lost an unknown field: %v", checks[0])
	}

	var injectedID, injectedObservedAt string
	json.Unmarshal(checks[1]["id"], &injectedID)
	json.Unmarshal(checks[1]["observed_at"], &injectedObservedAt)
	if injectedID != "harness_synthetic_gate" {
		t.Fatalf("expected injected check, got %s", injectedID)
	}
	if injectedObservedAt != "2026-09-19T00:00:00Z" {
		t.Fatalf("expected observed_at to be stamped to now, got %s", injectedObservedAt)
	}
	if !strings.Contains(string(checks[1]["blocks"]), `"scope":"host"`) {
		t.Fatalf("blocks not carried through: %s", checks[1]["blocks"])
	}
}

func TestRewriteCapacityReplacesOnlySyntheticPrefixedChecks(t *testing.T) {
	frame := []byte(`{"type":"capacity","readiness":[
		{"id":"harness_synthetic_gate","status":"pass"},
		{"id":"harness_synthetic_gate_v2","status":"pass"},
		{"id":"input_probe","status":"pass"}
	]}`)
	rule := Rule{Mode: "inject", Check: &Check{ID: "harness_synthetic_gate", Status: "fail"}}
	out, _, err := rewriteCapacity(frame, rule)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	json.Unmarshal(out, &top)
	var checks []map[string]json.RawMessage
	json.Unmarshal(top["readiness"], &checks)
	if len(checks) != 2 {
		t.Fatalf("expected both harness_synthetic_-prefixed checks removed and one re-added, got %d: %v", len(checks), checks)
	}
	var ids []string
	for _, c := range checks {
		var id string
		json.Unmarshal(c["id"], &id)
		ids = append(ids, id)
	}
	if ids[0] != "input_probe" || ids[1] != "harness_synthetic_gate" {
		t.Fatalf("unexpected ids: %v", ids)
	}
}
