// Add host after a developer apply moved the control plane's own machine (#385):
// /enroll-host.sh names the images that machine runs, below an override and the
// installed release, above the install-time fallback. The developer apply runs through
// the real self-applier and actor client against a recovery actor on a unix socket
// speaking testdata/recovery/socket's shapes. TEST_DATABASE_URL-gated; `make test-db`.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
	"github.com/accreleus/quasar/control-plane/internal/enrollscript"
	"github.com/accreleus/quasar/control-plane/internal/platform"
)

// fakeRecoveryActor serves GET /v1/status and POST /v1/submit. A submit succeeds at
// once and moves the actor to the recovery-actor component it names, as a verified
// hand-over leaves the machine.
type fakeRecoveryActor struct {
	mu     sync.Mutex
	status actorsocket.Status
	result *actorsocket.Result
}

func (f *fakeRecoveryActor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Connection", "close")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/status":
		st := f.status
		if id := r.URL.Query().Get("request_id"); id != "" && f.result != nil && f.result.RequestID == id {
			st.Result = f.result
		}
		_ = json.NewEncoder(w).Encode(st)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/submit":
		var req actorsocket.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(actorsocket.Rejection{Reason: actorsocket.ReasonInvalid, Message: err.Error()})
			return
		}
		previous := make([]actorsocket.Previous, 0, len(req.Components))
		for _, c := range req.Components {
			p := actorsocket.Previous{Name: c.Name}
			if c.Name == platform.ComponentRecovery {
				p.Digest = f.status.Actor.Digest
				digest := c.Digest
				f.status.Actor.Image, f.status.Actor.Digest = c.Image, &digest
			}
			previous = append(previous, p)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		f.result = &actorsocket.Result{RequestID: req.RequestID, State: actorsocket.StateSucceeded,
			Components: req.Components, Previous: previous, StartedAt: now, UpdatedAt: now, FinishedAt: &now,
			Release: req.Release}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(actorsocket.Accepted{RequestID: req.RequestID, Previous: previous})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRecoveryActor) runAgent(image, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.status.Services {
		if s.Role == platform.ComponentNodeAgent {
			d := digest
			f.status.Services[i].Image, f.status.Services[i].Digest, f.status.Services[i].State = image, &d, "running"
		}
	}
}

// serveRecoveryActor starts f on a socket path under the 108-byte sun_path limit.
func serveRecoveryActor(t *testing.T, f *fakeRecoveryActor) (string, *httptest.Server) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ra")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(f)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return path, srv
}

func statusFixture(t *testing.T, name string) actorsocket.Status {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "recovery", "socket", name))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Body actorsocket.Status `json:"body"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.Body
}

func serves(t *testing.T, body, seed, agent string) {
	t.Helper()
	if !strings.Contains(body, "\nPINNED_SEED_IMAGE='"+seed+"'\n") {
		t.Errorf("served seed is not %s:\n%s", seed, pinnedLines(body))
	}
	if !strings.Contains(body, "\nPINNED_AGENT_IMAGE='"+agent+"'\n") {
		t.Errorf("served agent is not %s:\n%s", agent, pinnedLines(body))
	}
}

func pinnedLines(body string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "PINNED_") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestAddHostServesTheSeedTheControlPlanesMachineRunsAfterADeveloperApply(t *testing.T) {
	pool := jobsTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE platform_releases, platform_apply_attempts CASCADE`); err != nil {
		t.Fatal(err)
	}
	store := platform.NewStore(pool)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Installed at A: the combined host at rest, its agent stopped.
	actor := &fakeRecoveryActor{status: statusFixture(t, "status-combined-idle.json")}
	socket, srv := serveRecoveryActor(t, actor)
	seedA := actor.status.Actor.Image + "@" + *actor.status.Actor.Digest
	installTime := enrollscript.Pins{
		SeedImage:  seedA,
		AgentImage: "ghcr.io/accreleus/quasar/quasar-node-agent@sha256:" + strings.Repeat("2", 64),
	}
	reader := platform.NewOwnMachineReader(socket)
	reader.TTL = 0
	reader.Log = quiet
	unreleased := strings.Repeat("0", 40)
	pins := installedEnrollPins(enrollscript.Pins{}, installTime, store, &unreleased, reader, quiet)
	serves(t, servedPins(t, pins), seedA, installTime.AgentImage)

	// A developer apply moves the actor to B.
	seedB := "registry.example.invalid/dev/quasar-recovery@sha256:" + strings.Repeat("b", 64)
	repo, digest, _ := strings.Cut(seedB, "@")
	requested := []platform.ComponentDigest{{Name: platform.ComponentRecovery, Image: repo, Digest: digest}}
	attempt, err := store.CreateControlPlaneAttempt(ctx, platform.NewControlPlaneAttempt{
		Kind: platform.KindDeveloperApply, Requested: requested,
		Previous: []platform.PreviousDigest{{Name: platform.ComponentRecovery}},
	})
	if err != nil {
		t.Fatal(err)
	}
	applier := platform.NewSelfApplier(store, platform.NewActorClient(socket), quiet)
	applier.PollInterval = 10 * time.Millisecond
	applier.DeveloperCommit = func(context.Context, []platform.ComponentDigest) (string, error) {
		return strings.Repeat("b", 40), nil
	}
	applier.Apply(ctx, attempt)
	if got, err := store.Attempt(ctx, attempt.ID); err != nil || got.State != platform.AttemptSucceeded {
		t.Fatalf("developer apply = %+v err=%v, want succeeded", got, err)
	}
	serves(t, servedPins(t, pins), seedB, installTime.AgentImage)

	// The machine's own agent, once it runs, is Add host's agent too.
	agentD := "registry.example.invalid/dev/quasar-node-agent@sha256:" + strings.Repeat("d", 64)
	agentRepo, agentDigest, _ := strings.Cut(agentD, "@")
	actor.runAgent(agentRepo, agentDigest)
	serves(t, servedPins(t, pins), seedB, agentD)

	// An operator override still wins, field by field.
	override := enrollscript.Pins{SeedImage: "registry.example.invalid/pinned/quasar-recovery@sha256:" + strings.Repeat("e", 64)}
	serves(t, servedPins(t, installedEnrollPins(override, installTime, store, &unreleased, reader, quiet)), override.SeedImage, agentD)

	// So does the installed release.
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "release", "platform-release-manifest.v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := platform.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertRelease(ctx, platform.Release{
		Channel: platform.ChannelStable, Version: &m.Version, SourceCommit: m.SourceCommit,
		BuiltAt: m.BuiltAtTime(), SchemaVersion: m.SchemaVersion, Manifest: json.RawMessage(raw),
	}); err != nil {
		t.Fatal(err)
	}
	releaseSeed, releaseAgent, _ := platform.EnrollImagesOf(m)
	released := m.SourceCommit
	serves(t, servedPins(t, installedEnrollPins(enrollscript.Pins{}, installTime, store, &released, reader, quiet)), releaseSeed, releaseAgent)

	// An actor that stops answering leaves the install-time images.
	srv.Close()
	serves(t, servedPins(t, pins), installTime.SeedImage, installTime.AgentImage)
}
