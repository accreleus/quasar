package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fake docker is a real shell script run through the real exec path, not a
// Go stub behind the interface: the argv the executor builds is the thing most
// likely to be wrong, and a stub would never notice a quoting or ordering bug.
//
// It records every invocation in argv.log and answers from files in its own
// directory: `<verb>.out` / `<verb>.code`, or `<verb>.<n>.out` / `<verb>.<n>.code`
// for the nth call of that verb (1-based), which is how the restore's second
// `up` differs from the first.
const fakeDocker = `#!/bin/sh
d="$QF_DIR"
printf '%s\n' "$*" >> "$d/argv.log"
verb=""
for a in "$@"; do
  case "$a" in
    pull|up|ps|inspect|config|logs) verb="$a"; break ;;
  esac
done
[ -n "$verb" ] || { echo "fake docker: no verb in: $*" >&2; exit 99; }
n=1
[ -f "$d/$verb.n" ] && n=$(cat "$d/$verb.n")
echo $((n + 1)) > "$d/$verb.n"
out="$d/$verb.$n.out"; [ -f "$out" ] || out="$d/$verb.out"
code="$d/$verb.$n.code"; [ -f "$code" ] || code="$d/$verb.code"
[ -f "$out" ] && cat "$out"
[ -f "$code" ] && exit "$(cat "$code")"
exit 0
`

type fakeEnv struct {
	t       *testing.T
	dir     string // fake docker's state
	stack   string // the stack directory (holds .env)
	store   *Store
	docker  Docker
	envPath string
}

func newFakeEnv(t *testing.T, initialEnv string) *fakeEnv {
	t.Helper()
	root := t.TempDir()
	fd := filepath.Join(root, "fake")
	stack := filepath.Join(root, "stack")
	results := filepath.Join(root, "results")
	for _, d := range []string{fd, stack} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(fd, "docker")
	if err := os.WriteFile(bin, []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QF_DIR", fd)
	envPath := filepath.Join(stack, ".env")
	if err := os.WriteFile(envPath, []byte(initialEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(results)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeEnv{t: t, dir: fd, stack: stack, store: store, docker: CLI{Bin: bin}, envPath: envPath}
}

func (f *fakeEnv) canned(name, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeEnv) argv() string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "argv.log"))
	return string(b)
}

func (f *fakeEnv) cfg() Config {
	return Config{
		Project:           "deploy",
		WorkingDir:        f.stack,
		ConfigFiles:       []string{filepath.Join(f.stack, "docker-compose.yml")},
		AllowedNamespaces: []string{"ghcr.io/accreleus/quasar"},
		WaitTimeoutS:      60,
	}
}

func (f *fakeEnv) apply(req ApplyRequest) *Result {
	f.t.Helper()
	priorEnv, err := os.ReadFile(f.envPath)
	if err != nil {
		f.t.Fatal(err)
	}
	cfg := f.cfg()
	plan, rej := Plan(req, string(priorEnv), cfg)
	if rej != nil {
		f.t.Fatalf("unexpected rejection: %v", rej)
	}
	if err := f.store.Claim(req.RequestID, &Accepted{RequestID: req.RequestID}); err != nil {
		f.t.Fatal(err)
	}
	e := &Executor{Store: f.store, Docker: f.docker, Cfg: cfg, EnvPath: f.envPath}
	e.Apply(context.Background(), req, plan, string(priorEnv))
	res, err := f.store.Read(req.RequestID)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func psJSON(service, id, state, health string) string {
	b, _ := json.Marshal(composePS{ID: id, Name: "deploy-" + service + "-1", Service: service, State: state, Health: health})
	return string(b) + "\n"
}

func TestExecutorSucceeds(t *testing.T) {
	f := newFakeEnv(t, "QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@"+prevDigest+"\n")
	f.canned("ps.out", psJSON("quasar-node-agent", "abc123", "running", ""))

	res := f.apply(agentReq())
	if res.State != StateSucceeded || res.Reason != nil {
		t.Fatalf("state=%s reason=%v output=%s", res.State, res.Reason, res.Output)
	}
	if res.FinishedAt == nil {
		t.Fatal("a terminal result must carry finished_at")
	}
	if res.Previous[0].Digest == nil || *res.Previous[0].Digest != prevDigest {
		t.Fatalf("previous = %+v — it must be present in EVERY state, not only on failure", res.Previous[0])
	}
	got, _ := os.ReadFile(f.envPath)
	if !strings.Contains(string(got), goodDigest) {
		t.Fatalf(".env was not rewritten: %s", got)
	}
	prev, err := os.ReadFile(f.envPath + ".prev")
	if err != nil || !strings.Contains(string(prev), prevDigest) {
		t.Fatalf(".env.prev must hold the previous file verbatim: %s (%v)", prev, err)
	}
	log := f.argv()
	for _, want := range []string{"--no-deps", "--force-recreate", "--wait-timeout 60", "--project-directory " + f.stack} {
		if !strings.Contains(log, want) {
			t.Fatalf("argv log missing %q:\n%s", want, log)
		}
	}
}

func TestExecutorPullFailureDoesNotRecreate(t *testing.T) {
	f := newFakeEnv(t, "")
	f.canned("pull.code", "1")
	f.canned("pull.out", "mismatched image rootfs and manifest layers\n")

	res := f.apply(agentReq())
	if res.State != StateFailed || res.Reason == nil || *res.Reason != ReasonPullFailed {
		t.Fatalf("state=%s reason=%v", res.State, res.Reason)
	}
	if strings.Contains(f.argv(), " up ") {
		t.Fatalf("a failed pull must not be followed by a recreate:\n%s", f.argv())
	}
	if !strings.Contains(res.Output, "mismatched image rootfs") {
		t.Fatalf("output = %q", res.Output)
	}
	// Nothing was recreated, so the old container still runs and `.env` must
	// not go on naming an image that never arrived.
	got, _ := os.ReadFile(f.envPath)
	if strings.Contains(string(got), goodDigest) {
		t.Fatalf(".env must be put back after a failed pull: %s", got)
	}
	if !strings.Contains(res.Output, ".env restored") {
		t.Fatalf("output must say the file was put back: %q", res.Output)
	}
}

// With no container at all there is nothing to read logs from, and the restore
// still runs: the old agent is what an operator wants back.
func TestExecutorRecreateFailureWhenNoContainerExists(t *testing.T) {
	f := newFakeEnv(t, "QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@"+prevDigest+"\n")
	f.canned("up.1.code", "1")
	f.canned("up.1.out", "service quasar-node-agent: no such image\n")
	f.canned("ps.out", "")

	res := f.apply(agentReq())
	if res.State != StateFailed || *res.Reason != ReasonRecreateFailed {
		t.Fatalf("state=%s reason=%v", res.State, res.Reason)
	}
	if !res.Restored {
		t.Fatalf("a node-agent recreate failure is restored (ADR 0004); output=%s", res.Output)
	}
}

// StartedAt zero is what proves no migration can have run (ADR 0002), which is
// what makes the control-plane restore safe.
func TestExecutorNeverStartedControlPlaneIsRestored(t *testing.T) {
	f := newFakeEnv(t, "QUASAR_CONTROL_IMAGE=ghcr.io/accreleus/quasar/quasar-control-plane@"+prevDigest+"\n")
	f.canned("up.1.code", "1")
	f.canned("up.1.out", "container deploy-quasar-control-plane-1 exited\n")
	f.canned("ps.out", psJSON("quasar-control-plane", "cp1", "created", ""))
	f.canned("inspect.out", zeroStartedAt+"\n")

	req := agentReq()
	req.Components = []Component{{Name: "control-plane", Image: "ghcr.io/accreleus/quasar/quasar-control-plane", Digest: goodDigest}}
	res := f.apply(req)

	if res.State != StateFailed || *res.Reason != ReasonNeverStarted {
		t.Fatalf("state=%s reason=%v", res.State, res.Reason)
	}
	if !res.Restored {
		t.Fatalf("a never-started control-plane apply must be restored; output=%s", res.Output)
	}
	got, _ := os.ReadFile(f.envPath)
	if !strings.Contains(string(got), prevDigest) || strings.Contains(string(got), goodDigest) {
		t.Fatalf(".env was not restored from .env.prev: %s", got)
	}
}

// ADR 0004: a node agent whose new container never started is restored too —
// it carries no migration, and the agent that would carry an operator's revert
// is the one that is down.
func TestExecutorNeverStartedAgentIsRestored(t *testing.T) {
	f := newFakeEnv(t, "QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@"+prevDigest+"\n")
	f.canned("up.1.code", "1")
	f.canned("ps.out", psJSON("quasar-node-agent", "na1", "created", ""))
	f.canned("inspect.out", zeroStartedAt+"\n")

	res := f.apply(agentReq())
	if *res.Reason != ReasonNeverStarted {
		t.Fatalf("reason = %v", res.Reason)
	}
	if !res.Restored {
		t.Fatalf("a never-started node-agent apply is restored; output=%s", res.Output)
	}
	got, _ := os.ReadFile(f.envPath)
	if !strings.Contains(string(got), prevDigest) || strings.Contains(string(got), goodDigest) {
		t.Fatalf(".env was not restored from .env.prev: %s", got)
	}
}

// The #152 field case: the new agent exits because its health port is taken.
// The updater restores the old one, and the result carries the failed
// container's own last lines — which is where `health-bind-failed` is — so the
// card can say why, and `restored: true` so the control plane can record it.
func TestExecutorUnhealthyAgentIsRestoredWithItsLogTail(t *testing.T) {
	f := newFakeEnv(t, "QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@"+prevDigest+"\n")
	f.canned("up.1.code", "1")
	f.canned("up.1.out", "container deploy-quasar-node-agent-1 is unhealthy\n")
	f.canned("ps.out", psJSON("quasar-node-agent", "na1", "exited", ""))
	f.canned("inspect.out", "2026-09-05T11:04:31Z\n")
	f.canned("logs.out", "ERROR health-bind-failed: cannot bind 127.0.0.1:9091: address in use\n")

	res := f.apply(agentReq())
	if res.State != StateFailed || *res.Reason != ReasonRecreateFailed {
		t.Fatalf("state=%s reason=%v", res.State, res.Reason)
	}
	if !res.Restored {
		t.Fatalf("restored=false; output=%s", res.Output)
	}
	for _, want := range []string{"health-bind-failed", "previous digest brought back up", "last lines of the failed container"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output lacks %q:\n%s", want, res.Output)
		}
	}
	// The log read names the failed container, and the restore is a second up.
	log := f.argv()
	if !strings.Contains(log, "logs --tail 40 -- na1") || strings.Count(log, " up ") != 2 {
		t.Fatalf("argv:\n%s", log)
	}
	got, _ := os.ReadFile(f.envPath)
	if !strings.Contains(string(got), prevDigest) {
		t.Fatalf(".env was not restored: %s", got)
	}
}

// A control plane that STARTED may have migrated: never restored (ADR 0002).
func TestExecutorUnhealthyControlPlaneIsNotRestored(t *testing.T) {
	f := newFakeEnv(t, "QUASAR_CONTROL_IMAGE=ghcr.io/accreleus/quasar/quasar-control-plane@"+prevDigest+"\n")
	f.canned("ps.out", psJSON("quasar-control-plane", "cp1", "running", "unhealthy"))
	f.canned("inspect.out", "2026-09-05T11:04:31Z\n")

	req := agentReq()
	req.Components = []Component{{Name: "control-plane", Image: "ghcr.io/accreleus/quasar/quasar-control-plane", Digest: goodDigest}}
	res := f.apply(req)
	if *res.Reason != ReasonUnhealthy || res.Restored {
		t.Fatalf("reason=%v restored=%v", res.Reason, res.Restored)
	}
	if strings.Count(f.argv(), " up ") != 1 {
		t.Fatalf("a started control plane must not be re-upped:\n%s", f.argv())
	}
}

// If the restore itself fails, both failures are in the output and the host is
// where a failed recreate always left it.
func TestExecutorRestoreFailureSurfacesBoth(t *testing.T) {
	f := newFakeEnv(t, "QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@"+prevDigest+"\n")
	f.canned("up.code", "1")
	f.canned("up.out", "container exited\n")
	f.canned("ps.out", psJSON("quasar-node-agent", "na1", "exited", ""))
	f.canned("inspect.out", "2026-09-05T11:04:31Z\n")

	res := f.apply(agentReq())
	if res.Restored {
		t.Fatal("the second up failed too, so nothing was restored")
	}
	if !strings.Contains(res.Output, "restore ALSO failed") {
		t.Fatalf("output = %q", res.Output)
	}
}

func TestRestoreWorthy(t *testing.T) {
	agent := []Component{{Name: "node-agent"}}
	cp := []Component{{Name: "control-plane"}}
	cases := []struct {
		reason string
		comps  []Component
		want   bool
	}{
		{ReasonNeverStarted, agent, true}, {ReasonNeverStarted, cp, true},
		{ReasonRecreateFailed, agent, true}, {ReasonRecreateFailed, cp, false},
		{ReasonUnhealthy, agent, true}, {ReasonUnhealthy, cp, false},
		{ReasonPullFailed, agent, false}, {ReasonInvalid, agent, false},
	}
	for _, tc := range cases {
		if got := restoreWorthy(tc.reason, tc.comps); got != tc.want {
			t.Fatalf("restoreWorthy(%s, %s) = %v, want %v", tc.reason, tc.comps[0].Name, got, tc.want)
		}
	}
}

// `up -d` returns 0 for a container that starts and then dies, so the verdict
// comes from post-state, not the exit code.
func TestExecutorUnhealthyDespiteAZeroExit(t *testing.T) {
	f := newFakeEnv(t, "")
	f.canned("ps.out", psJSON("quasar-node-agent", "na1", "running", "unhealthy"))
	f.canned("inspect.out", "2026-09-05T11:04:31Z\n")

	res := f.apply(agentReq())
	if res.State != StateFailed || *res.Reason != ReasonUnhealthy {
		t.Fatalf("state=%s reason=%v", res.State, res.Reason)
	}
}

func TestExecutorHealthlessRunningServiceSucceeds(t *testing.T) {
	f := newFakeEnv(t, "")
	f.canned("ps.out", psJSON("quasar-node-agent", "na1", "running", ""))
	if res := f.apply(agentReq()); res.State != StateSucceeded {
		t.Fatalf("state=%s reason=%v output=%s", res.State, res.Reason, res.Output)
	}
}

// `compose config`, not the compose file: an unset ${QUASAR_CONTROL_IMAGE} must
// come back as the default tag and an .env pin as a digest reference, which is
// what makes the answer usable for classifying install mode.
func TestEffectiveImagesReportsWhatComposeResolves(t *testing.T) {
	f := newFakeEnv(t, "")
	f.canned("config.out", `{"services":{
	  "quasar-control-plane":{"image":"quasar-control-plane:latest"},
	  "quasar-node-agent":{"image":"ghcr.io/accreleus/quasar/quasar-node-agent@`+prevDigest+`"},
	  "quasar-postgres":{"image":"postgres:16-alpine"}}}`)
	e := &Executor{Store: f.store, Docker: f.docker, Cfg: f.cfg(), EnvPath: f.envPath}

	got := e.EffectiveImages(context.Background())
	if got["control-plane"] == nil || *got["control-plane"] != "quasar-control-plane:latest" {
		t.Fatalf("control-plane = %v", got["control-plane"])
	}
	if got["node-agent"] == nil || *got["node-agent"] != "ghcr.io/accreleus/quasar/quasar-node-agent@"+prevDigest {
		t.Fatalf("node-agent = %v", got["node-agent"])
	}
	if _, ok := got["postgres"]; ok {
		t.Fatal("only the two components this updater can move belong in images")
	}
	if !strings.Contains(f.argv(), "config --format json") {
		t.Fatalf("argv log:\n%s", f.argv())
	}
}

// An agentless stack (deploy/overlays/docker-compose.local.yml) has no
// node-agent service: null, which is not the same as an empty string.
func TestEffectiveImagesReportsNullForAnAbsentService(t *testing.T) {
	f := newFakeEnv(t, "")
	f.canned("config.out", `{"services":{"quasar-control-plane":{"image":"cp:latest"}}}`)
	e := &Executor{Store: f.store, Docker: f.docker, Cfg: f.cfg(), EnvPath: f.envPath}

	got := e.EffectiveImages(context.Background())
	v, present := got["node-agent"]
	if !present || v != nil {
		t.Fatalf("node-agent = %v (present=%v), want a present null", v, present)
	}
}

// A compose that cannot be read is null for BOTH, never a guess.
func TestEffectiveImagesIsNullWhenComposeFails(t *testing.T) {
	f := newFakeEnv(t, "")
	f.canned("config.code", "1")
	f.canned("config.out", "no such file\n")
	e := &Executor{Store: f.store, Docker: f.docker, Cfg: f.cfg(), EnvPath: f.envPath}

	for name, ref := range e.EffectiveImages(context.Background()) {
		if ref != nil {
			t.Fatalf("%s = %q, want null", name, *ref)
		}
	}
}

func TestParseComposePSAcceptsBothShapes(t *testing.T) {
	arr := `[{"ID":"a","Service":"s","State":"running","Health":""}]`
	nd := "{\"ID\":\"a\",\"Service\":\"s\",\"State\":\"running\"}\n{\"ID\":\"b\",\"Service\":\"t\",\"State\":\"exited\"}\n"
	if got := parseComposePS(arr); len(got) != 1 || got[0].Service != "s" {
		t.Fatalf("array form: %+v", got)
	}
	if got := parseComposePS(nd); len(got) != 2 || got[1].Service != "t" {
		t.Fatalf("ndjson form: %+v", got)
	}
}

// The reader is a different container than the writer and is normally being
// replaced while the write happens: a half-written file must be unrepresentable.
func TestResultWritesAreAtomicAndLeaveNoTemporaries(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &Result{RequestID: reqID, State: StatePending}
	for i := 0; i < 5; i++ {
		r.State = StatePulling
		if err := s.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != reqID+".json" {
		t.Fatalf("results dir = %v, want exactly the one result file", names(entries))
	}
	back, err := s.Read(reqID)
	if err != nil || back.State != StatePulling {
		t.Fatalf("read back %+v (%v)", back, err)
	}
	if _, err := s.Read("../../etc/passwd"); err == nil {
		t.Fatal("a request id that is not a uuid must never become a path")
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestTailOutputKeepsTheEnd(t *testing.T) {
	body := ""
	for i := 0; i < 500; i++ {
		body += fmt.Sprintf("line %d\n", i)
	}
	got := TailOutput(body, 100)
	if len(got) > 100 || !strings.HasSuffix(got, "line 499\n") {
		t.Fatalf("got %q", got)
	}
	if strings.HasPrefix(got, "ine") {
		t.Fatal("the tail must start at a line boundary")
	}
}
