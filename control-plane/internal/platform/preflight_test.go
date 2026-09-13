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

func healthySelf() *UpdaterSelfFacts {
	return &UpdaterSelfFacts{
		Version:     "0.2.5",
		StackDir:    "/srv/quasar/deploy",
		ConfigFiles: []string{"/srv/quasar/deploy/docker-compose.yml"},
		ServiceConfigFiles: map[string][]string{
			"quasar-control-plane": {"/srv/quasar/deploy/docker-compose.yml"},
			"quasar-node-agent":    {"/srv/quasar/deploy/docker-compose.yml"},
		},
	}
}

func TestControlPlanePreflightSocketThreeWay(t *testing.T) {
	// #184: three situations used to read as one "not installed".
	cases := []struct {
		name   string
		socket *SocketState
		self   *UpdaterSelfFacts
		status string
		wants  string
	}{
		{"no mount directory", &SocketState{}, nil, CheckFail, "recreate"},
		{"directory but no socket", &SocketState{DirExists: true}, nil, CheckFail, "quasar-updater"},
		{"socket but no answer", &SocketState{DirExists: true, SocketExists: true}, &UpdaterSelfFacts{Err: "dial: refused"}, CheckFail, "did not answer"},
		{"answered", &SocketState{DirExists: true, SocketExists: true}, healthySelf(), CheckPass, "0.2.5"},
		{"nobody looked", nil, nil, CheckUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := PlanPreflight(TargetControlPlane, PreflightFacts{Socket: tc.socket, Self: tc.self})
			c := checkByID(t, p, CheckUpdaterSocket)
			if c.Status != tc.status || !strings.Contains(c.Detail, tc.wants) {
				t.Fatalf("updater_socket = %+v, want %s containing %q", c, tc.status, tc.wants)
			}
		})
	}
	// The mount-directory case must NAME THE FIX: it is the one an operator
	// cannot guess from "not installed".
	p := PlanPreflight(TargetControlPlane, PreflightFacts{Socket: &SocketState{}})
	if d := checkByID(t, p, CheckUpdaterSocket).Detail; !strings.Contains(d, "--force-recreate") {
		t.Fatalf("no-mount detail must carry the recreate command, got %q", d)
	}
}

func TestControlPlanePreflightStackDirAndOverlays(t *testing.T) {
	ok := PlanPreflight(TargetControlPlane, PreflightFacts{
		Socket: &SocketState{true, true}, Self: healthySelf(), Image: &ImageFact{}})
	if ok.State != PreflightOK {
		t.Fatalf("state = %s, want ok: %+v", ok.State, ok.Checks)
	}

	// An overlay the control plane was started with and the updater was not.
	drift := healthySelf()
	drift.ServiceConfigFiles["quasar-control-plane"] = []string{
		"/srv/quasar/deploy/docker-compose.yml", "/srv/quasar/deploy/overlays/docker-compose.dev.yml"}
	p := PlanPreflight(TargetControlPlane, PreflightFacts{Socket: &SocketState{true, true}, Self: drift, Image: &ImageFact{}})
	c := checkByID(t, p, CheckUpdaterOverlays)
	if c.Status != CheckFail || !strings.Contains(c.Detail, "quasar-control-plane") || !strings.Contains(c.Detail, "docker-compose.dev.yml") {
		t.Fatalf("overlay drift = %+v, want fail naming the service and the overlay", c)
	}
	if p.State != PreflightBlocked {
		t.Fatalf("state = %s, want blocked", p.State)
	}

	// A service with no container is nothing to compare, not a mismatch.
	absent := healthySelf()
	absent.ServiceConfigFiles["quasar-node-agent"] = nil
	if c := checkByID(t, PlanPreflight(TargetControlPlane, PreflightFacts{Socket: &SocketState{true, true}, Self: absent}), CheckUpdaterOverlays); c.Status != CheckPass {
		t.Fatalf("absent service = %+v, want pass", c)
	}

	// An updater that predates the overlay report: unknown, never blocked.
	old := healthySelf()
	old.ServiceConfigFiles = nil
	p = PlanPreflight(TargetControlPlane, PreflightFacts{Socket: &SocketState{true, true}, Self: old, Image: &ImageFact{}})
	if c := checkByID(t, p, CheckUpdaterOverlays); c.Status != CheckUnknown {
		t.Fatalf("old updater = %+v, want unknown", c)
	}
	if p.State != PreflightUnknown {
		t.Fatalf("state = %s, want unknown", p.State)
	}

	// No discovered stack at all.
	none := &UpdaterSelfFacts{Version: "0.2.5"}
	if c := checkByID(t, PlanPreflight(TargetControlPlane, PreflightFacts{Socket: &SocketState{true, true}, Self: none}), CheckUpdaterStackDir); c.Status != CheckFail || !strings.Contains(c.Detail, "QUASAR_STACK_DIR") {
		t.Fatalf("undiscovered stack = %+v, want fail naming QUASAR_STACK_DIR", c)
	}
}

func TestHostPreflightReadsTheAgentsOwnChecks(t *testing.T) {
	connected := true
	readiness := json.RawMessage(`[
	  {"id":"updater_socket","status":"pass","summary":"updater 0.2.5 answered","remediation":""},
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
	if c := checkByID(t, p, CheckUpdaterOverlays); c.Status != CheckPass {
		t.Fatalf("warn must read as an advisory pass, got %+v", c)
	}
	// An unrelated readiness failure (a GPU check) is not a preflight fact.
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
