package platform

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
)

var socketFixtureDir = filepath.Join("..", "..", "..", "testdata", "recovery", "socket")

// fixtureBody is a status fixture's "body" member, verbatim.
func fixtureBody(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(socketFixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.Body
}

// shortSocketPath stays under the 108-byte sun_path limit t.TempDir can exceed.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "om")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "control.sock")
}

// serveStatus is a recovery actor answering GET /v1/status with body.
func serveStatus(t *testing.T, body []byte) (string, *atomic.Int32) {
	t.Helper()
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/status" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "close")
		_, _ = w.Write(body)
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return path, &hits
}

func TestOwnMachineReadsACombinedHost(t *testing.T) {
	path, _ := serveStatus(t, fixtureBody(t, "status-combined-idle.json"))
	m, ok := NewOwnMachineReader(path).Read(context.Background())
	if !ok {
		t.Fatal("the actor answered, but the read failed")
	}
	id := m.Identity
	for name, got := range map[string]*string{
		"install_mode":                 id.InstallMode,
		"recovery_actor_version":       id.RecoveryActorVersion,
		"recovery_actor_source_commit": id.RecoveryActorSourceCommit,
		"seed_version":                 id.SeedVersion,
		"database_mode":                id.DatabaseMode,
	} {
		want := map[string]string{
			"install_mode":                 "owned",
			"recovery_actor_version":       "0.4.0",
			"recovery_actor_source_commit": strings.Repeat("c", 40),
			"seed_version":                 "0.3.0",
			"database_mode":                "owned",
		}[name]
		if got == nil || *got != want {
			t.Errorf("%s = %v, want %q", name, got, want)
		}
	}
}

func TestOwnMachineReadsAControlOnlyHost(t *testing.T) {
	path, _ := serveStatus(t, fixtureBody(t, "status-control-only-stale.json"))
	m, ok := NewOwnMachineReader(path).Read(context.Background())
	if !ok {
		t.Fatal("a stale answer is still an answer")
	}
	id := m.Identity
	if id.InstallMode == nil || *id.InstallMode != InstallOwned {
		t.Errorf("install_mode = %v, want owned", id.InstallMode)
	}
	if id.SeedVersion != nil {
		t.Errorf("seed_version = %q, want null for a null seed", *id.SeedVersion)
	}
	if id.DatabaseMode == nil || *id.DatabaseMode != DatabaseModeExternal {
		t.Errorf("database_mode = %v, want external", id.DatabaseMode)
	}
}

func TestOwnMachineFailuresReadNull(t *testing.T) {
	garbage, _ := serveStatus(t, []byte("{not json"))
	for name, r := range map[string]*OwnMachineReader{
		"garbage body":   NewOwnMachineReader(garbage),
		"missing socket": NewOwnMachineReader(shortSocketPath(t)),
		"not configured": NewOwnMachineReader(""),
	} {
		m, ok := r.Read(context.Background())
		if ok || m.Identity != (MachineIdentity{}) {
			t.Errorf("%s: ok=%v identity=%+v, want a failed read with every field null", name, ok, m.Identity)
		}
	}
}

func TestOwnMachineFromStatusNullsWhatTheContractCannotUse(t *testing.T) {
	m := OwnMachineFromStatus(actorsocket.Status{
		Actor:    actorsocket.ActorIdentity{Version: "v0.4", Commit: "NOT-HEX"},
		Seed:     &actorsocket.SeedIdentity{Version: ""},
		Database: actorsocket.DatabaseNone,
	})
	id := m.Identity
	if id.InstallMode == nil || *id.InstallMode != InstallOwned {
		t.Fatal("an answer is owned whatever it carries")
	}
	if id.RecoveryActorVersion != nil || id.RecoveryActorSourceCommit != nil || id.SeedVersion != nil || id.DatabaseMode != nil {
		t.Fatalf("identity = %+v, want the unusable values null", id)
	}
}

func TestOwnMachineAnswerIsReusedUntilInvalidated(t *testing.T) {
	path, hits := serveStatus(t, fixtureBody(t, "status-combined-idle.json"))
	r := NewOwnMachineReader(path)
	ctx := context.Background()
	r.Read(ctx)
	r.Read(ctx)
	r.PreflightFacts(ctx)
	if n := hits.Load(); n != 1 {
		t.Fatalf("socket reads = %d within the TTL, want 1", n)
	}
	r.Invalidate()
	r.Read(ctx)
	if n := hits.Load(); n != 2 {
		t.Fatalf("socket reads = %d after Invalidate, want 2", n)
	}
}

// The answer is shared for TTL, so a caller that has already gone away must not
// leave "not answering" cached for every reader after it.
func TestACancelledRequestIsNotCachedAsASilentActor(t *testing.T) {
	path, hits := serveStatus(t, fixtureBody(t, "status-combined-idle.json"))
	r := NewOwnMachineReader(path)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := r.Read(gone); !ok {
		t.Fatal("a cancelled caller's read failed; its cancellation reached the actor read")
	}
	if _, ok := r.Read(context.Background()); !ok {
		t.Fatal("the next reader sees a failed read")
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("actor asked %d times, want once (the answer is reused)", n)
	}
}

// control-api.md amendment 14 §"Preflight": no Compose checks on an owned
// target, updater_socket names no Compose command, and a Quasar-owned database
// adds backup_space.
func TestOwnedControlPlanePreflight(t *testing.T) {
	ids := func(p Preflight) []string {
		var out []string
		for _, c := range p.Checks {
			out = append(out, c.ID)
		}
		return out
	}
	path, _ := serveStatus(t, fixtureBody(t, "status-combined-idle.json"))
	p := PlanPreflight(TargetControlPlane, NewOwnMachineReader(path).PreflightFacts(context.Background()))
	if got := strings.Join(ids(p), ","); got != CheckUpdaterSocket+","+CheckOwnerConflict+","+CheckImageResolvable+","+CheckBackupSpace {
		t.Fatalf("checks = %s, want updater_socket,owner_conflict,image_resolvable,backup_space", got)
	}
	// The fixture's leftover Compose control plane (#366).
	if p.Checks[1].Status != CheckFail || !strings.Contains(p.Checks[1].Detail, "deploy-quasar-control-plane-1") {
		t.Errorf("owner_conflict = %+v, want a fail naming the fixture's conflict", p.Checks[1])
	}
	if p.Checks[0].Status != CheckPass || !strings.Contains(p.Checks[0].Detail, "0.4.0") {
		t.Errorf("answered socket = %+v, want a pass naming the actor", p.Checks[0])
	}
	if p.CheckedAt == nil {
		t.Error("checked_at is null on a read that happened")
	}

	p = PlanPreflight(TargetControlPlane, NewOwnMachineReader(shortSocketPath(t)).PreflightFacts(context.Background()))
	c := p.Checks[0]
	if c.ID != CheckUpdaterSocket || c.Status != CheckFail || !p.Blocked() {
		t.Fatalf("silent actor = %+v (%s), want updater_socket fail, blocked", c, p.State)
	}
	if strings.Contains(c.Detail, "compose") || !strings.Contains(c.Detail, "docker logs quasar-recovery") {
		t.Errorf("detail = %q, want no Compose command and the recovery actor's log named", c.Detail)
	}

	// Not an owned machine: today's four checks.
	if got := ids(PlanPreflight(TargetControlPlane, NewOwnMachineReader("").PreflightFacts(context.Background()))); len(got) != 4 {
		t.Fatalf("unowned checks = %v, want today's four", got)
	}
}

var machineKeys = []string{"install_mode", "recovery_actor_version", "recovery_actor_source_commit", "seed_version", "database_mode", "machine_role", "machine_node_name"}

func TestIdentityServesTheOwnMachineFields(t *testing.T) {
	path, _ := serveStatus(t, fixtureBody(t, "status-combined-idle.json"))
	reader := NewOwnMachineReader(path)
	for name, h := range map[string]*Handler{
		"owned":     NewHandler(&Deps{ControlPlaneMachine: reader.Identity}, nil),
		"no deps":   NewHandler(nil, nil),
		"not owned": NewHandler(&Deps{}, nil),
	} {
		rec := httptest.NewRecorder()
		h.handleIdentity(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/platform/identity", nil))
		var body struct {
			Identity map[string]any `json:"identity"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, k := range append([]string{"version", "schema_version"}, machineKeys...) {
			if _, ok := body.Identity[k]; !ok {
				t.Errorf("%s: identity lacks %q: %s", name, k, rec.Body.String())
			}
		}
		if name == "owned" {
			if body.Identity["install_mode"] != "owned" || body.Identity["database_mode"] != "owned" {
				t.Errorf("owned identity = %s", rec.Body.String())
			}
		} else {
			for _, k := range machineKeys {
				if body.Identity[k] != nil {
					t.Errorf("%s: %s = %v, want null", name, k, body.Identity[k])
				}
			}
		}
	}
}

func TestReleaseViewCarriesTheOwnMachineOnInstalledControlPlane(t *testing.T) {
	owned := InstallOwned
	v := PlanRelease(PlanInputs{Channel: ChannelStable, ControlPlaneMachine: MachineIdentity{InstallMode: &owned}})
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Installed struct {
			ControlPlane map[string]any `json:"control_plane"`
			Hosts        []any          `json:"hosts"`
		} `json:"installed"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	cp := body.Installed.ControlPlane
	for _, k := range append([]string{"version", "source_commit", "built_at", "schema_version"}, machineKeys...) {
		if _, ok := cp[k]; !ok {
			t.Errorf("installed.control_plane lacks %q: %s", k, raw)
		}
	}
	if cp["install_mode"] != "owned" {
		t.Errorf("install_mode = %v, want owned", cp["install_mode"])
	}
	if !strings.Contains(string(raw), `"hosts":`) {
		t.Errorf("installed.hosts dropped: %s", raw)
	}
}

// machine_role / machine_node_name come from the control plane's own
// configuration, so they are served with the recovery actor silent.
func TestIdentityServesTheMachineShapeWhetherOrNotTheActorAnswers(t *testing.T) {
	silent := NewOwnMachineReader(filepath.Join(t.TempDir(), "absent.sock"))
	h := NewHandler(&Deps{
		ControlPlaneMachine: silent.Identity,
		MachineShape:        MachineShape{Role: MachineRoleControlOnly, NodeName: "attic-server"},
	}, nil)
	rec := httptest.NewRecorder()
	h.handleIdentity(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/platform/identity", nil))
	var body struct {
		Identity map[string]any `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Identity["machine_role"] != "control_only" || body.Identity["machine_node_name"] != "attic-server" {
		t.Errorf("identity = %s, want the configured shape", rec.Body.String())
	}
	if body.Identity["install_mode"] != nil {
		t.Errorf("install_mode = %v with the actor silent, want null", body.Identity["install_mode"])
	}
	// Half a shape is no shape.
	if got := (MachineShape{Role: MachineRoleCombined}).Apply(MachineIdentity{}); got.MachineRole != nil {
		t.Errorf("a role without a node name was served: %+v", got)
	}
}
