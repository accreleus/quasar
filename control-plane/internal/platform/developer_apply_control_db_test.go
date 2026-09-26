// A developer apply to an owned control plane (control-api.md §"Developer
// apply", #363): the route on a real Postgres behind RequireAuth→RequireAdmin,
// and the standalone control-plane attempt's host holds on the real admission
// table.
package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
	"github.com/accreleus/quasar/control-plane/internal/admission"
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

type recordingSelfDev struct {
	mu      sync.Mutex
	started []Attempt
}

func (r *recordingSelfDev) Start(a Attempt) {
	r.mu.Lock()
	r.started = append(r.started, a)
	r.mu.Unlock()
}

func (r *recordingSelfDev) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started)
}

// newControlDevHarness is an owned combined control plane whose own agent is
// the harness host, with the control-plane developer apply driver recorded.
func newControlDevHarness(t *testing.T) (*devHarness, *recordingSelfDev) {
	t.Helper()
	images := &fakeDevImages{commit: commitB}
	rec := &recordingSelfDev{}
	h := newApplyHarness(t, func(_ *applyHarness, handler *ApplyHandler) {
		handler.WithDeveloperApply(images, []string{"registry.example.invalid/dev"}).
			WithOwnMachine(ownedMachine(actorsocket.RoleCombined, "gpu-01")).
			WithMachineShape(MachineShape{Role: MachineRoleCombined, NodeName: "gpu-01"}).
			WithSelfDeveloper(rec)
	})
	mustExec(t, h.pool, `UPDATE hosts SET install_mode = 'owned' WHERE id = $1::uuid`, h.hostID)
	return &devHarness{applyHarness: h, images: images}, rec
}

func controlBody(components ...ComponentDigest) map[string]any {
	return map[string]any{"target": "control_plane", "components": components}
}

func TestADeveloperApplyToAnOwnedControlPlaneIsAControlPlaneAttemptActorFirst(t *testing.T) {
	h, rec := newControlDevHarness(t)
	code, out := h.post(t, devURL, h.adminToken, controlBody(cpComponent(), actorComponent()))
	if code != http.StatusAccepted {
		t.Fatalf("control plane = %d %s, want 202", code, out)
	}
	var env AttemptEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	a := env.Attempt
	if a.Kind != KindDeveloperApply || a.Target != TargetControlPlane || a.HostID != nil || a.ReleaseID != nil || a.RunID != nil {
		t.Fatalf("attempt = %+v, want a standalone control-plane developer_apply naming no release", a)
	}
	if len(a.RequestedDigests) != 2 || a.RequestedDigests[0].Name != ComponentRecovery || a.RequestedDigests[1].Name != ComponentControlPlane {
		t.Fatalf("requested = %+v, want [recovery-actor, control-plane]", a.RequestedDigests)
	}
	if rec.count() != 1 {
		t.Fatalf("driver started %d attempts, want 1", rec.count())
	}
	// One control-plane attempt at a time.
	if code, out := h.post(t, devURL, h.adminToken, controlBody(cpComponent())); code != http.StatusConflict || errCode(t, out) != CodeAttemptInFlight {
		t.Fatalf("second = %d %s, want 409 attempt_in_flight", code, out)
	}
	if code, _ := h.post(t, devURL, h.userToken, controlBody(cpComponent())); code != http.StatusForbidden {
		t.Fatalf("a user = %d, want 403", code)
	}
}

// ADR 0002 on the image's own schema: below the database is refused, above it
// migrates, which on an owned machine arrives with the pre-update dump (#364).
func TestADeveloperApplyToAnOwnedControlPlaneIsRefusedAcrossASchemaChange(t *testing.T) {
	installed := buildinfo.Get().SchemaVersion
	for _, tc := range []struct {
		schema   int
		wantCode int
		wantErr  string
	}{
		{installed + 1, http.StatusNotImplemented, CodeApplyUnsupported},
		{installed - 1, http.StatusUnprocessableEntity, CodeReleaseBelowSchemaVersion},
	} {
		h, rec := newControlDevHarness(t)
		h.images.schema = tc.schema
		code, out := h.post(t, devURL, h.adminToken, controlBody(cpComponent()))
		if code != tc.wantCode || errCode(t, out) != tc.wantErr {
			t.Fatalf("schema %d = %d %s, want %d %s", tc.schema, code, out, tc.wantCode, tc.wantErr)
		}
		open, err := h.store.OpenAttempts(context.Background())
		if err != nil || len(open) != 0 || rec.count() != 0 {
			t.Fatalf("schema %d: open=%v err=%v started=%d, want nothing created", tc.schema, open, err, rec.count())
		}
	}
}

func TestADeveloperApplyToAnOwnedControlPlaneKeepsToTheAllowlist(t *testing.T) {
	h, rec := newControlDevHarness(t)
	outside := cpComponent()
	outside.Image = "registry.example.invalid/elsewhere/quasar-control-plane"
	code, out := h.post(t, devURL, h.adminToken, controlBody(outside))
	if code != http.StatusConflict || errCode(t, out) != CodeNamespaceRejected {
		t.Fatalf("outside = %d %s, want 409 namespace_rejected", code, out)
	}
	if h.images.count() != 0 || rec.count() != 0 {
		t.Fatal("a registry outside the allowlist was contacted, or an attempt started")
	}
}

// fakeSelf stands for the self-applier: Apply resolves the attempt as told.
type fakeSelf struct {
	store      *Store
	during     func()
	succeed    bool
	adopted    []string
	adoptEnds  bool
	adoptMutex sync.Mutex
}

func (f *fakeSelf) UpdaterPresent() bool { return true }

func (f *fakeSelf) Apply(ctx context.Context, a Attempt) {
	if f.during != nil {
		f.during()
	}
	if f.succeed {
		_, _ = f.store.SucceedAttempt(ctx, a.ID)
		return
	}
	_ = f.store.FailAttempt(ctx, a.ID, ReasonUnhealthy, "the previous container was put back and is running")
}

func (f *fakeSelf) Adopt(ctx context.Context, a Attempt, commit string) bool {
	f.adoptMutex.Lock()
	f.adopted = append(f.adopted, commit)
	f.adoptMutex.Unlock()
	if f.adoptEnds {
		_, _ = f.store.SucceedAttempt(ctx, a.ID)
	}
	return f.adoptEnds
}

func holdsFor(t *testing.T, h *devHarness) []admission.Restriction {
	t.Helper()
	rs, err := admission.NewStore(h.pool).List(context.Background(), h.hostID)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func devCordons(h *devHarness) FleetCordons {
	holds := admission.NewStore(h.pool)
	return FleetCordons{
		AcquireOwned: func(ctx context.Context, ownerID, hostID string) error {
			_, err := holds.Acquire(ctx, hostID, admission.Owner{Kind: admission.Platform, ID: ownerID}, "Platform apply")
			return err
		},
		ReleaseOwned: func(ctx context.Context, ownerID, hostID string) error {
			_, err := holds.Release(ctx, hostID, admission.Owner{Kind: admission.Platform, ID: ownerID}, true)
			return err
		},
	}
}

func newDevControlAttempt(t *testing.T, h *devHarness) Attempt {
	t.Helper()
	a, err := h.store.CreateControlPlaneAttempt(context.Background(), NewControlPlaneAttempt{
		Kind: KindDeveloperApply, Requested: []ComponentDigest{actorComponent(), cpComponent()},
		Previous: unknownPrevious([]ComponentDigest{actorComponent(), cpComponent()}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAControlPlaneDeveloperApplyHoldsEveryHostUntilItEnds(t *testing.T) {
	for _, succeed := range []bool{true, false} {
		h, _ := newControlDevHarness(t)
		a := newDevControlAttempt(t, h)
		held := make(chan bool, 1)
		self := &fakeSelf{store: h.store, succeed: succeed, during: func() {
			rs := holdsFor(t, h)
			held <- len(rs) == 1 && rs[0].OwnerKind == admission.Platform
		}}
		r := NewSelfDeveloperRunner(h.store, self, devCordons(h), nil, testLogger())
		r.InFlightSettle = 10 * time.Millisecond
		t.Cleanup(r.Close)

		r.Start(a)
		if !<-held {
			t.Fatalf("succeed=%v: the host was not held while the control plane was replaced", succeed)
		}
		waitFor(t, "the hold to be released", func() bool { return len(holdsFor(t, h)) == 0 })
		cur, err := h.store.Attempt(context.Background(), a.ID)
		if err != nil || !TerminalAttemptState(cur.State) {
			t.Fatalf("succeed=%v: attempt %+v err=%v, want terminal", succeed, cur, err)
		}
	}
}

// The normal path: the replacement ends the process that took the holds, and
// the booted control plane resolves the attempt and releases them.
func TestABootResolvesAControlPlaneDeveloperApplyAndReleasesItsHolds(t *testing.T) {
	h, _ := newControlDevHarness(t)
	ctx := context.Background()
	open := newDevControlAttempt(t, h)
	cordons := devCordons(h)
	if err := cordons.AcquireOwned(ctx, open.ID, h.hostID); err != nil {
		t.Fatal(err)
	}
	self := &fakeSelf{store: h.store, adoptEnds: true}
	r := NewSelfDeveloperRunner(h.store, self, cordons,
		func(context.Context, []ComponentDigest) (string, error) { return commitB, nil }, testLogger())
	t.Cleanup(r.Close)

	r.Adopt(ctx)
	waitFor(t, "the adopted attempt to resolve and release", func() bool {
		cur, err := h.store.Attempt(ctx, open.ID)
		return err == nil && cur.State == AttemptSucceeded && len(holdsFor(t, h)) == 0
	})
	self.adoptMutex.Lock()
	defer self.adoptMutex.Unlock()
	if len(self.adopted) != 1 || self.adopted[0] != commitB {
		t.Fatalf("adopted with %v, want the images' commit", self.adopted)
	}
}

func TestABootReleasesTheHoldsOfAControlPlaneDeveloperApplyThatAlreadyEnded(t *testing.T) {
	h, _ := newControlDevHarness(t)
	ctx := context.Background()
	ended := newDevControlAttempt(t, h)
	cordons := devCordons(h)
	if err := cordons.AcquireOwned(ctx, ended.ID, h.hostID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.FailAttempt(ctx, ended.ID, ReasonUnhealthy, ""); err != nil {
		t.Fatal(err)
	}
	holds, err := h.store.TerminalStandaloneControlPlaneHolds(ctx)
	if err != nil || len(holds) != 1 || holds[0].AttemptID != ended.ID || holds[0].HostID != h.hostID {
		t.Fatalf("holds = %+v err=%v, want the ended attempt's one", holds, err)
	}
	r := NewSelfDeveloperRunner(h.store, &fakeSelf{store: h.store}, cordons, nil, testLogger())
	t.Cleanup(r.Close)
	r.Adopt(ctx)
	if rs := holdsFor(t, h); len(rs) != 0 {
		t.Fatalf("holds after boot = %+v, want none", rs)
	}
}
