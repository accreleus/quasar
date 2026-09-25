// POST /v1/admin/platform/developer-apply against a real Postgres, behind the
// real RequireAuth→RequireAdmin chain (#360; control-api.md §"Developer apply").
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	devAgentImage = "registry.example.invalid/dev/quasar-node-agent"
	devActorImage = "registry.example.invalid/dev/quasar-recovery"
)

var (
	devAgentDigest = "sha256:" + strings.Repeat("d", 64)
	devActorDigest = "sha256:" + strings.Repeat("e", 64)
)

// fakeDevImages answers a fixed commit, or an error, and counts reads.
type fakeDevImages struct {
	mu     sync.Mutex
	commit string
	err    error
	reads  int
}

func (f *fakeDevImages) Commit(context.Context, []ComponentDigest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.commit, f.err
}

func (f *fakeDevImages) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

type devHarness struct {
	*applyHarness
	images *fakeDevImages
}

// newDevHarness is the apply harness with the developer route wired, the
// dev namespace allowed, and its host installed the owned way. The images
// carry the control plane's own commit unless a test says otherwise.
func newDevHarness(t *testing.T) *devHarness {
	t.Helper()
	images := &fakeDevImages{commit: commitB}
	h := newApplyHarness(t, func(_ *applyHarness, handler *ApplyHandler) {
		handler.WithDeveloperApply(images, []string{"registry.example.invalid/dev"})
	})
	mustExec(t, h.pool, `UPDATE hosts SET install_mode = 'owned' WHERE id = $1::uuid`, h.hostID)
	return &devHarness{applyHarness: h, images: images}
}

func (h *devHarness) body(components ...ComponentDigest) map[string]any {
	return map[string]any{"target": "host", "host_id": h.hostID, "components": components}
}

func agentComponent() ComponentDigest {
	return ComponentDigest{Name: ComponentNodeAgent, Image: devAgentImage, Digest: devAgentDigest}
}

func actorComponent() ComponentDigest {
	return ComponentDigest{Name: ComponentRecovery, Image: devActorImage, Digest: devActorDigest}
}

const devURL = "/v1/admin/platform/developer-apply"

func TestDeveloperApplyIsAdminOnly(t *testing.T) {
	h := newDevHarness(t)
	if code, _ := h.post(t, devURL, "", h.body(agentComponent())); code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", code)
	}
	if code, _ := h.post(t, devURL, h.userToken, h.body(agentComponent())); code != http.StatusForbidden {
		t.Errorf("non-admin = %d, want 403", code)
	}
	if h.agent.sentCount() != 0 || h.images.count() != 0 {
		t.Fatal("a refused request reached the registry or the host")
	}
}

func TestDeveloperApplyRecordsAnAttemptAndSendsTheDigestsActorFirst(t *testing.T) {
	h := newDevHarness(t)
	// Order in the request is not significant.
	code, body := h.post(t, devURL, h.adminToken, h.body(agentComponent(), actorComponent()))
	if code != http.StatusAccepted {
		t.Fatalf("developer apply = %d (%s), want 202", code, body)
	}
	var env AttemptEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	a := env.Attempt
	if a.Kind != KindDeveloperApply || a.Target != TargetHost || a.ReleaseID != nil || a.RunID != nil {
		t.Fatalf("attempt = %+v, want a standalone developer_apply with no release", a)
	}
	if len(a.RequestedDigests) != 2 || a.RequestedDigests[0].Name != ComponentRecovery || a.RequestedDigests[1].Name != ComponentNodeAgent {
		t.Fatalf("requested_digests = %+v, want the recovery actor first", a.RequestedDigests)
	}

	waitFor(t, "release_apply", func() bool { return h.agent.sentCount() == 1 })
	sent := h.agent.sent[0]
	if sent.Release.ID != "" || sent.Release.Version != nil || sent.Release.SourceCommit != commitB {
		t.Errorf("release = %+v, want id \"\", version null and the images' commit", sent.Release)
	}
	if len(sent.Components) != 2 || sent.Components[0].Digest != devActorDigest || sent.Components[1].Digest != devAgentDigest {
		t.Errorf("components = %+v", sent.Components)
	}

	var action, details string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT action, details::text FROM admin_activity WHERE action = 'platform.apply.developer'`).Scan(&action, &details); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	for _, want := range []string{a.ID, devAgentDigest, devActorDigest, `"force": false`, `"external_backup_confirmed": false`} {
		if !strings.Contains(details, want) {
			t.Errorf("audit details %s lack %s", details, want)
		}
	}

	// It is history like any attempt, and never a release.
	code, body = get(t, h.base+"/v1/admin/platform/attempts?host_id="+h.hostID, h.adminToken)
	if code != http.StatusOK || !strings.Contains(string(body), `"developer_apply"`) {
		t.Errorf("history = %d %s", code, body)
	}
	var releases int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM platform_releases`).Scan(&releases); err != nil || releases != 1 {
		t.Errorf("platform_releases rows = %d (%v), want only the seeded release", releases, err)
	}
}

func TestDeveloperApplySucceedsOnTheRegisterCarryingTheImagesCommit(t *testing.T) {
	h := newDevHarness(t)
	code, body := h.post(t, devURL, h.adminToken, h.body(agentComponent()))
	if code != http.StatusAccepted {
		t.Fatalf("developer apply = %d (%s)", code, body)
	}
	var env AttemptEnvelope
	_ = json.Unmarshal(body, &env)
	waitFor(t, "release_apply", func() bool { return h.agent.sentCount() == 1 })

	ctx := context.Background()
	old := commitA
	h.runner.HandleRegister(ctx, h.hostID, &old)
	if a, _ := h.store.Attempt(ctx, env.Attempt.ID); TerminalAttemptState(a.State) {
		t.Fatalf("a register on the old commit resolved the attempt: %+v", a)
	}
	want := commitB
	h.runner.HandleRegister(ctx, h.hostID, &want)
	waitFor(t, "success", func() bool {
		a, _ := h.store.Attempt(ctx, env.Attempt.ID)
		return a.State == AttemptSucceeded
	})
}

// After a control-plane restart the commit is read again off the digests.
func TestAReAdoptedDeveloperApplyReadsItsCommitAgain(t *testing.T) {
	h := newDevHarness(t)
	ctx := context.Background()
	attempt, err := h.store.CreateHostAttempt(ctx, NewHostAttempt{
		Kind: KindDeveloperApply, HostID: h.hostID,
		Requested: []ComponentDigest{agentComponent()},
		Previous:  unknownPrevious([]ComponentDigest{agentComponent()}),
		Force:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := h.agent.deps()
	deps.DeveloperCommit = h.images.Commit
	fresh := testRunner(h.store, deps)
	t.Cleanup(fresh.Close)
	fresh.Adopt(ctx)
	waitFor(t, "release_apply", func() bool { return h.agent.sentCount() == 1 })
	if got := h.agent.sent[0].Release.SourceCommit; got != commitB {
		t.Fatalf("provenance commit = %q, want the images' %q", got, commitB)
	}
	want := commitB
	fresh.HandleRegister(ctx, h.hostID, &want)
	waitFor(t, "success", func() bool {
		a, _ := h.store.Attempt(ctx, attempt.ID)
		return a.State == AttemptSucceeded
	})
}

func TestARestoredDeveloperApplyIsRecordedWithItsAutoRevertAndIsARevertSourceWhenItSucceeds(t *testing.T) {
	h := newDevHarness(t)
	ctx := context.Background()
	code, body := h.post(t, devURL, h.adminToken, h.body(agentComponent()))
	if code != http.StatusAccepted {
		t.Fatalf("developer apply = %d (%s)", code, body)
	}
	waitFor(t, "release_apply", func() bool { return h.agent.sentCount() == 1 })
	reqID := h.agent.sent[0].RequestID
	prev := "sha256:" + strings.Repeat("a", 64)
	reason := ReasonUnhealthy
	h.runner.HandleReleaseState(ctx, h.hostID, ReleaseStateReport{
		RequestID: reqID, State: AttemptFailed, Reason: &reason, Restored: true,
		Previous: []PreviousDigest{{Name: ComponentNodeAgent, Digest: &prev}},
		Output:   "the new container did not verify; the previous container was put back and is running",
	})
	attempts, err := h.store.ListAttempts(ctx, h.hostID, 10)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, a := range attempts {
		kinds[a.Kind] = a.State
	}
	if kinds[KindDeveloperApply] != AttemptFailed || kinds[KindAutoRevert] != AttemptSucceeded {
		t.Fatalf("history = %v, want the failed developer apply and its auto_revert", kinds)
	}

	// A second developer apply that succeeds is what Revert goes back from.
	code, body = h.post(t, devURL, h.adminToken, h.body(agentComponent()))
	if code != http.StatusAccepted {
		t.Fatalf("second developer apply = %d (%s)", code, body)
	}
	waitFor(t, "release_apply", func() bool { return h.agent.sentCount() == 2 })
	h.runner.HandleReleaseState(ctx, h.hostID, ReleaseStateReport{
		RequestID: h.agent.sent[1].RequestID, State: AttemptSucceeded,
		Previous: []PreviousDigest{{Name: ComponentNodeAgent, Digest: &prev}},
	})
	code, body = h.post(t, h.revertURL(), h.adminToken, map[string]any{"force": true})
	if code != http.StatusAccepted {
		t.Fatalf("revert = %d (%s), want 202", code, body)
	}
	var env AttemptEnvelope
	_ = json.Unmarshal(body, &env)
	if env.Attempt.RequestedDigests[0].Digest != prev {
		t.Fatalf("revert requested %+v, want the digest before the developer apply", env.Attempt.RequestedDigests)
	}
}

func TestDeveloperApplyRefusals(t *testing.T) {
	h := newDevHarness(t)
	type want struct {
		status int
		code   string
		reason string
	}
	check := func(name string, body any, w want) {
		t.Helper()
		status, out := h.post(t, devURL, h.adminToken, body)
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(out, &e)
		if status != w.status || e.Error.Code != w.code || e.Reason != w.reason {
			t.Errorf("%s = %d %s/%s (%s), want %d %s/%s", name, status, e.Error.Code, e.Reason, out, w.status, w.code, w.reason)
		}
	}
	bad := func(mut func(*ComponentDigest)) ComponentDigest {
		c := agentComponent()
		mut(&c)
		return c
	}
	v400 := want{http.StatusBadRequest, "validation_failed", ""}

	check("tag", h.body(bad(func(c *ComponentDigest) { c.Image += ":latest" })), v400)
	check("digest in image", h.body(bad(func(c *ComponentDigest) { c.Image += "@" + devAgentDigest })), v400)
	check("malformed digest", h.body(bad(func(c *ComponentDigest) { c.Digest = "sha256:XYZ" })), v400)
	check("control plane to a host", h.body(bad(func(c *ComponentDigest) { c.Name = ComponentControlPlane })), v400)
	check("unknown component", h.body(bad(func(c *ComponentDigest) { c.Name = "postgres" })), v400)
	check("twice", h.body(agentComponent(), agentComponent()), v400)
	check("empty", h.body(), v400)
	check("no host_id", map[string]any{"target": "host", "components": []ComponentDigest{agentComponent()}}, v400)
	check("host_id on control_plane", map[string]any{"target": "control_plane", "host_id": h.hostID,
		"components": []ComponentDigest{{Name: ComponentControlPlane, Image: devAgentImage, Digest: devAgentDigest}}}, v400)
	check("actor alone on the control plane", map[string]any{"target": "control_plane",
		"components": []ComponentDigest{actorComponent()}}, v400)
	check("unknown field", map[string]any{"target": "host", "host_id": h.hostID, "release_id": h.release.ID,
		"components": []ComponentDigest{agentComponent()}}, v400)

	check("control-plane target", map[string]any{"target": "control_plane",
		"components": []ComponentDigest{{Name: ComponentControlPlane, Image: devAgentImage, Digest: devAgentDigest}}},
		want{http.StatusConflict, CodeTargetNotOwned, ""})
	check("no such host", map[string]any{"target": "host", "host_id": "00000000-0000-4000-8000-000000000000",
		"components": []ComponentDigest{agentComponent()}}, want{http.StatusNotFound, "not_found", ""})

	check("outside the allowlist", h.body(bad(func(c *ComponentDigest) { c.Image = "ghcr.io/someone/quasar-node-agent" })),
		want{http.StatusConflict, CodeNamespaceRejected, ""})
	if h.images.count() != 0 {
		t.Errorf("the registry was read %d times for requests refused before it", h.images.count())
	}

	h.images.err = errors.New("manifest unknown")
	check("unresolvable", h.body(agentComponent()), want{http.StatusConflict, CodeImageUnresolvable, ""})
	h.images.err = nil

	// A commit neither this control plane's nor a known release's.
	h.images.commit = strings.Repeat("f", 40)
	check("unknown commit", h.body(agentComponent()), want{http.StatusConflict, CodeHostNotEligible, ReasonReleaseAboveControlPlane})
	// A known release below the control plane is fine.
	older := seedRelease(t, h.store, commitA, 1, func(r *Release) { r.BuiltAt = at(1) })
	h.images.commit = older.SourceCommit
	if status, out := h.post(t, devURL, h.adminToken, h.body(agentComponent())); status != http.StatusAccepted {
		t.Fatalf("a known older release's commit = %d (%s), want 202", status, out)
	}
	check("second while one is in flight", h.body(agentComponent()), want{http.StatusConflict, CodeAttemptInFlight, ""})
	if h.agent.sentCount() > 1 {
		t.Errorf("sent %d release_apply, want at most the one accepted", h.agent.sentCount())
	}

	// A registry host is never given one.
	h2 := newDevHarness(t)
	mustExec(t, h2.pool, `UPDATE hosts SET install_mode = 'registry' WHERE id = $1::uuid`, h2.hostID)
	if status, out := h2.post(t, devURL, h2.adminToken, h2.body(agentComponent())); status != http.StatusConflict || errCode(t, out) != CodeTargetNotOwned {
		t.Errorf("registry host = %d %s, want 409 target_not_owned", status, out)
	}
	mustExec(t, h2.pool, `UPDATE hosts SET install_mode = 'owned', status = 'offline' WHERE id = $1::uuid`, h2.hostID)
	if status, out := h2.post(t, devURL, h2.adminToken, h2.body(agentComponent())); status != http.StatusConflict {
		t.Errorf("offline host = %d %s, want 409", status, out)
	}
}

// The attempt kind the route writes is admitted by migration 0096's CHECK.
func TestDeveloperApplyKindIsStorable(t *testing.T) {
	h := newDevHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.store.CreateHostAttempt(ctx, NewHostAttempt{
		Kind: KindDeveloperApply, HostID: h.hostID, Requested: []ComponentDigest{agentComponent()},
		Previous: unknownPrevious([]ComponentDigest{agentComponent()}),
	}); err != nil {
		t.Fatalf("developer_apply row: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `UPDATE platform_apply_attempts SET kind = 'something_else'`); err == nil {
		t.Fatal("the kind CHECK admitted an unknown kind")
	}
}
