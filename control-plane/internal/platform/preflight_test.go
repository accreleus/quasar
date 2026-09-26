package platform

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The preflight decision, one row per rule of control-api.md §"Self-update
// hardening". No I/O: facts in, a Preflight out.

func checkByID(t *testing.T, p Preflight, id string) PreflightCheck {
	t.Helper()
	for _, c := range p.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no %s check in %+v", id, p.Checks)
	return PreflightCheck{}
}

// A control plane with no recovery actor (not installed with the seed) cannot be
// updated from the console; its check says so and names no Compose command.
func TestControlPlanePreflightWithNoRecoveryActor(t *testing.T) {
	p := PlanPreflight(TargetControlPlane, PreflightFacts{NoRecoveryActor: true, Image: &ImageFact{}})
	c := checkByID(t, p, CheckUpdaterSocket)
	if c.Status != CheckFail || !strings.Contains(c.Detail, "seed") || strings.Contains(c.Detail, "docker compose") {
		t.Fatalf("updater_socket = %+v, want a fail naming the seed and no compose command", c)
	}
	if p.State != PreflightBlocked {
		t.Fatalf("state = %s, want blocked", p.State)
	}
	assertNoRetiredChecks(t, p)
}

// updater_stack_dir and updater_overlays are retired: no target evaluates or
// emits them (control-api.md "RH06 contract step").
func assertNoRetiredChecks(t *testing.T, p Preflight) {
	t.Helper()
	for _, c := range p.Checks {
		if c.ID == "updater_stack_dir" || c.ID == "updater_overlays" {
			t.Fatalf("retired check %s emitted: %+v", c.ID, p.Checks)
		}
	}
}

func TestHostPreflightReadsTheAgentsOwnChecks(t *testing.T) {
	connected := true
	readiness := json.RawMessage(`[
	  {"id":"updater_socket","status":"warn","summary":"the recovery actor answered slowly","remediation":"x"},
	  {"id":"updater_stack_dir","status":"pass","summary":"/srv/quasar/deploy","remediation":""},
	  {"id":"updater_overlays","status":"warn","summary":"could not compare","remediation":"x"},
	  {"id":"health_addr_bindable","status":"fail","summary":"127.0.0.1:9091 is answered by node gpu-01 pid 4121, not this agent","remediation":"free the port (ss -ltnp | grep 9091) or set QUASAR_HEALTH_ADDR"},
	  {"id":"render_node","status":"fail","summary":"unrelated","remediation":""}
	]`)
	at := time.Date(2026, 9, 11, 9, 12, 44, 0, time.UTC)
	h := HostIdentity{HostID: "h1", AgentConnected: &connected, Readiness: readiness, ReadinessReportedAt: &at}
	p := PlanPreflight(TargetHost, HostPreflightFacts(h, &ImageFact{}))

	if p.State != PreflightBlocked {
		t.Fatalf("state = %s, want blocked: %+v", p.State, p.Checks)
	}
	if p.CheckedAt == nil || *p.CheckedAt != "2026-09-11T09:12:44Z" {
		t.Fatalf("checked_at = %v, want the readiness report time", p.CheckedAt)
	}
	bind := checkByID(t, p, CheckHealthAddrBindable)
	if bind.Status != CheckFail || !strings.Contains(bind.Detail, "pid 4121") || !strings.Contains(bind.Detail, "QUASAR_HEALTH_ADDR") {
		t.Fatalf("health_addr_bindable = %+v, want fail carrying the summary AND the remediation", bind)
	}
	if c := checkByID(t, p, CheckUpdaterSocket); c.Status != CheckPass {
		t.Fatalf("warn must read as an advisory pass, got %+v", c)
	}
	// An older agent still reporting the retired Compose checks: they are not
	// lifted, and neither is an unrelated readiness failure (a GPU check).
	assertNoRetiredChecks(t, p)
	for _, c := range p.Checks {
		if c.ID == "render_node" {
			t.Fatal("render_node leaked into preflight")
		}
	}
}

func TestHostPreflightUnknownNeverBlocks(t *testing.T) {
	// An agent that predates the checks reports none of the ids: every one is
	// unknown and the host stays eligible. This is what lets a fleet install
	// the build that adds the checks.
	connected := true
	h := HostIdentity{HostID: "h1", AgentConnected: &connected,
		Readiness: json.RawMessage(`[{"id":"render_node","status":"pass","summary":"","remediation":""}]`)}
	p := PlanPreflight(TargetHost, HostPreflightFacts(h, &ImageFact{}))
	if p.State != PreflightUnknown || p.Blocked() {
		t.Fatalf("state = %s, want unknown", p.State)
	}
	// A malformed report reads the same way, never as blocked.
	h.Readiness = json.RawMessage(`{"not":"an array"}`)
	if p := PlanPreflight(TargetHost, HostPreflightFacts(h, nil)); p.State != PreflightUnknown {
		t.Fatalf("malformed readiness → %s, want unknown", p.State)
	}
	// And a disconnected agent is a fail on its own check.
	off := false
	h.AgentConnected = &off
	if c := checkByID(t, PlanPreflight(TargetHost, HostPreflightFacts(h, nil)), CheckAgentConnected); c.Status != CheckFail {
		t.Fatalf("disconnected = %+v, want fail", c)
	}
}

func TestPreflightImageCheckIsCopiedOntoEveryTarget(t *testing.T) {
	bad := &ImageFact{Err: "node-agent: ghcr.io answered 404 for sha256:abc"}
	for _, kind := range []string{TargetControlPlane, TargetHost} {
		p := PlanPreflight(kind, PreflightFacts{Image: bad})
		if c := checkByID(t, p, CheckImageResolvable); c.Status != CheckFail || c.Detail != bad.Err {
			t.Fatalf("%s image check = %+v", kind, c)
		}
		if !p.Blocked() {
			t.Fatalf("%s must be blocked by an unresolvable image", kind)
		}
	}
	if c := checkByID(t, PlanPreflight(TargetHost, PreflightFacts{}), CheckImageResolvable); c.Status != CheckUnknown {
		t.Fatalf("no release → %+v, want unknown", c)
	}
}

func TestFoldPreflight(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{CheckPass, CheckPass}, PreflightOK},
		{[]string{CheckPass, CheckUnknown}, PreflightUnknown},
		{[]string{CheckUnknown, CheckFail, CheckPass}, PreflightBlocked},
		{[]string{}, PreflightOK},
		{[]string{"something-new"}, PreflightUnknown},
	}
	for _, tc := range cases {
		checks := make([]PreflightCheck, len(tc.in))
		for i, s := range tc.in {
			checks[i] = PreflightCheck{ID: "x", Status: s}
		}
		if got := foldPreflight(checks); got != tc.want {
			t.Fatalf("fold(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
