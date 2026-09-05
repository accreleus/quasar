package platform

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The sequencer's ordering rules, with fake drivers: no database, no updater,
// no agent. Every case here is one sentence of control-api.md
// §"Platform-release apply".

const testRunID = "33333333-3333-4333-8333-333333333333"

// The two reasons amendment 2 appends, at the END of the precedence order.
func TestPlanReportsRunActiveAndTheControlPlanesOwnUpdater(t *testing.T) {
	newest := Release{ID: "r1", Channel: ChannelStable, SourceCommit: commitC,
		BuiltAt: at(3), SchemaVersion: 74, Manifest: []byte(`{}`)}
	in := PlanInputs{
		Channel:        ChannelStable,
		ControlPlane:   cp(commitA, 74),
		Hosts:          []HostIdentity{host("h1", "gpu-01", commitB)},
		Releases:       []Release{newest},
		UpdaterPresent: true,
	}

	// With no updater beside it, this control plane cannot be moved at all.
	noUpdater := in
	noUpdater.UpdaterPresent = false
	if got := targetReason(PlanRelease(noUpdater), TargetControlPlane); got != ReasonUpdaterAbsent {
		t.Fatalf("control plane reason = %q, want updater_absent", got)
	}

	// An active run owns the fleet, so no standalone apply may start on any
	// target — the last reason on the list, because it is the most transient.
	withRun := in
	withRun.ActiveRun = &ApplyRun{ID: testRunID, State: RunRunning}
	v := PlanRelease(withRun)
	if got := targetReason(v, TargetControlPlane); got != ReasonRunActive {
		t.Fatalf("control plane reason = %q, want run_active", got)
	}
	if v.Targets[1].Reason == nil || *v.Targets[1].Reason != ReasonControlPlaneNotFirst {
		t.Fatalf("host reason = %v, want the durable control_plane_not_first to outrank run_active", v.Targets[1].Reason)
	}
	if v.ActiveApply == nil || v.ActiveApply.Run == nil {
		t.Fatal("active_apply.run must carry the run that owns the fleet")
	}
}

// fakeFleetStore is fleetStore in maps. It enforces the two rules the real
// store's SQL does and the sequencer depends on: a terminal row never changes
// again, and one open attempt per target.
type fakeFleetStore struct {
	mu       sync.Mutex
	run      ApplyRun
	attempts []*Attempt
	release  Release
	next     int
	inFlight map[string]bool
}

func newFakeFleetStore(force bool) *fakeFleetStore {
	return &fakeFleetStore{
		run:      ApplyRun{ID: testRunID, ReleaseID: testReleaseID, State: RunPending, Force: force},
		release:  Release{ID: testReleaseID, SourceCommit: testCommit, SchemaVersion: 75, Manifest: applyManifest(testCommit, 75)},
		inFlight: map[string]bool{},
	}
}

func (f *fakeFleetStore) Run(context.Context, string) (ApplyRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.run, nil
}

func (f *fakeFleetStore) ActiveRun(context.Context) (*ApplyRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if TerminalRunState(f.run.State) {
		return nil, nil
	}
	r := f.run
	return &r, nil
}

func (f *fakeFleetStore) RunAttempts(context.Context, string) ([]Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Attempt, 0, len(f.attempts))
	for _, a := range f.attempts {
		out = append(out, *a)
	}
	return out, nil
}

func (f *fakeFleetStore) SetRunTarget(_ context.Context, _, target string, hostID *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if TerminalRunState(f.run.State) {
		return nil
	}
	f.run.State = RunRunning
	t := target
	f.run.CurrentTarget = &t
	f.run.CurrentHostID = hostID
	return nil
}

func (f *fakeFleetStore) FinishRun(_ context.Context, _, state, errText string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if TerminalRunState(f.run.State) {
		return nil
	}
	f.run.State = state
	if errText != "" {
		f.run.Error = &errText
	}
	f.run.CurrentTarget, f.run.CurrentHostID = nil, nil
	return nil
}

func (f *fakeFleetStore) Attempt(_ context.Context, id string) (Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.attempts {
		if a.ID == id {
			return *a, nil
		}
	}
	return Attempt{}, ErrAttemptNotFound
}

func (f *fakeFleetStore) add(a Attempt) (Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := ""
	if a.HostID != nil {
		key = *a.HostID
	}
	if f.inFlight[key] {
		return Attempt{}, ErrAttemptInFlight
	}
	f.inFlight[key] = true
	f.next++
	a.ID = "att-" + string(rune('a'+f.next))
	a.CreatedAt = time.Now()
	f.attempts = append(f.attempts, &a)
	return a, nil
}

func (f *fakeFleetStore) CreateHostAttempt(_ context.Context, in NewHostAttempt) (Attempt, error) {
	hostID := in.HostID
	return f.add(Attempt{
		RunID: in.RunID, Kind: in.Kind, Target: TargetHost, HostID: &hostID,
		ReleaseID: in.ReleaseID, RequestedDigests: in.Requested, PreviousDigests: in.Previous,
		State: AttemptQueued, Force: in.Force,
	})
}

func (f *fakeFleetStore) CreateControlPlaneAttempt(_ context.Context, in NewControlPlaneAttempt) (Attempt, error) {
	return f.add(Attempt{
		RunID: in.RunID, Kind: KindApply, Target: TargetControlPlane,
		ReleaseID: in.ReleaseID, RequestedDigests: in.Requested, PreviousDigests: in.Previous,
		State: AttemptQueued,
	})
}

func (f *fakeFleetStore) LastSucceededDigests(context.Context, string) ([]ComponentDigest, error) {
	return nil, nil
}

func (f *fakeFleetStore) LastSucceededControlPlaneDigests(context.Context) ([]ComponentDigest, error) {
	return nil, nil
}

func (f *fakeFleetStore) Release(context.Context, string) (Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.release, nil
}

func (f *fakeFleetStore) NonTerminalSessions(context.Context, string) (int, error) { return 0, nil }

func (f *fakeFleetStore) SetWaitingSessions(context.Context, string, int) error { return nil }

// resolve settles one attempt, as the per-target machine would.
func (f *fakeFleetStore) resolve(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.attempts {
		if a.ID != id || TerminalAttemptState(a.State) {
			continue
		}
		a.State = state
		if state == AttemptFailed {
			reason := ReasonPullFailed
			a.Reason = &reason
		}
		key := ""
		if a.HostID != nil {
			key = *a.HostID
		}
		f.inFlight[key] = false
	}
}

func (f *fakeFleetStore) requestCancel() {
	f.mu.Lock()
	f.run.CancelRequested = true
	f.mu.Unlock()
}

// fakeDrivers resolve each attempt with the outcome the test configured, which
// is what makes the ORDER the thing under test.
type fakeDrivers struct {
	store *fakeFleetStore
	mu    sync.Mutex
	order []string
	// host id → the state its attempt resolves to. "" means succeeded.
	outcome map[string]string
	// the control-plane attempt's outcome; "" means succeeded.
	cpOutcome string
	cpApplied bool
}

func (d *fakeDrivers) Start(a Attempt) {
	d.mu.Lock()
	d.order = append(d.order, *a.HostID)
	state := d.outcome[*a.HostID]
	d.mu.Unlock()
	if state == "" {
		state = AttemptSucceeded
	}
	d.store.resolve(a.ID, state)
}

func (d *fakeDrivers) UpdaterPresent() bool { return true }

func (d *fakeDrivers) Apply(_ context.Context, a Attempt) {
	d.mu.Lock()
	d.order = append(d.order, TargetControlPlane)
	d.cpApplied = true
	state := d.cpOutcome
	d.mu.Unlock()
	if state == "" {
		state = AttemptSucceeded
	}
	d.store.resolve(a.ID, state)
}

func (d *fakeDrivers) Adopt(ctx context.Context, a Attempt, _ string) bool {
	d.Apply(ctx, a)
	return true
}

func (d *fakeDrivers) steps() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.order))
	copy(out, d.order)
	return out
}

// fleetView builds the view the sequencer reads: the control plane behind, and
// one target per host with the reason the test wants.
func fleetView(cpReason string, hosts ...Target) func(context.Context) (View, error) {
	return func(context.Context) (View, error) {
		v := View{Channel: ChannelStable}
		v.Targets = append(v.Targets, target(TargetControlPlane, nil, nil, cpReason))
		v.Targets = append(v.Targets, hosts...)
		return v, nil
	}
}

func hostTarget(id, name, reason string) Target {
	hostID, nodeName := id, name
	return target(TargetHost, &hostID, &nodeName, reason)
}

func testFleet(t *testing.T, store *fakeFleetStore, d *fakeDrivers, view func(context.Context) (View, error)) *FleetRunner {
	t.Helper()
	f := NewFleetRunner(store, d, d, ManifestOrEdge{}, view, testLogger())
	f.PollWait = time.Millisecond
	t.Cleanup(f.Close)
	return f
}

func runToEnd(t *testing.T, f *FleetRunner, store *fakeFleetStore) ApplyRun {
	t.Helper()
	f.Start(store.run)
	waitFor(t, "the run to finish", func() bool {
		r, _ := store.Run(context.Background(), testRunID)
		return TerminalRunState(r.State)
	})
	r, _ := store.Run(context.Background(), testRunID)
	return r
}

func TestFleetAppliesTheControlPlaneThenEachHostInOrder(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("",
		hostTarget("h1", "gpu-01", ""), hostTarget("h2", "gpu-02", "")))

	run := runToEnd(t, f, store)

	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", run.State)
	}
	want := []string{TargetControlPlane, "h1", "h2"}
	got := d.steps()
	if len(got) != len(want) {
		t.Fatalf("targets reached = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets reached = %v, want %v", got, want)
		}
	}
}

func TestFleetSkipsAnUpToDateControlPlane(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView(ReasonUpToDate, hostTarget("h1", "gpu-01", "")))

	run := runToEnd(t, f, store)

	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", run.State)
	}
	if d.cpApplied {
		t.Fatal("the control plane was applied while already on the release")
	}
	if got := d.steps(); len(got) != 1 || got[0] != "h1" {
		t.Fatalf("targets reached = %v, want just the host", got)
	}
}

func TestFleetStopsAtTheFirstFailedTarget(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{"h1": AttemptFailed}}
	f := testFleet(t, store, d, fleetView("",
		hostTarget("h1", "gpu-01", ""), hostTarget("h2", "gpu-02", "")))

	run := runToEnd(t, f, store)

	if run.State != RunFailed {
		t.Fatalf("run state = %q, want failed", run.State)
	}
	// h2 must never be reached: past a failed host, continuing would march a
	// known-bad digest set across the fleet.
	for _, step := range d.steps() {
		if step == "h2" {
			t.Fatal("the run continued past a failed target")
		}
	}
}

func TestFleetFailsWhenTheControlPlaneCannotTakeIt(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView(ReasonUpdaterAbsent, hostTarget("h1", "gpu-01", "")))

	run := runToEnd(t, f, store)

	if run.State != RunFailed {
		t.Fatalf("run state = %q, want failed", run.State)
	}
	if len(d.steps()) != 0 {
		t.Fatalf("targets reached = %v, want none: ADR 0002 puts the control plane first", d.steps())
	}
	if run.Error == nil {
		t.Fatal("a run-level failure belonging to no attempt must carry prose")
	}
}

func TestFleetReportsSkippedHostsAndSucceeds(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("",
		hostTarget("h1", "gpu-01", ReasonHostOffline),
		hostTarget("h2", "gpu-02", ""),
		hostTarget("h3", "gpu-03", ReasonInstallModeSource)))

	run := runToEnd(t, f, store)

	// An ineligibility is not a failure: a run must not go failed because a
	// host happened to be offline.
	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", run.State)
	}
	skips := f.Skips(testRunID)
	if len(skips) != 2 {
		t.Fatalf("skipped = %+v, want two", skips)
	}
	if skips[0].HostID != "h1" || skips[0].Reason != ReasonHostOffline || skips[0].NodeName != "gpu-01" {
		t.Fatalf("skip[0] = %+v", skips[0])
	}
	if skips[1].Reason != ReasonInstallModeSource {
		t.Fatalf("skip[1] = %+v", skips[1])
	}
}

// run_active is this run: a target must not be skipped because the run holding
// the fleet is the one asking.
func TestFleetIgnoresItsOwnRunActiveReason(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView(ReasonRunActive, hostTarget("h1", "gpu-01", ReasonRunActive)))

	run := runToEnd(t, f, store)

	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", run.State)
	}
	if len(f.Skips(testRunID)) != 0 {
		t.Fatalf("skipped = %+v, want none", f.Skips(testRunID))
	}
}

func TestFleetCancelStopsBeforeTheNextTarget(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	// The cancel arrives while the first host is being applied, so it is read
	// between targets and h2 is never started.
	d.outcome["h1"] = AttemptSucceeded
	f := testFleet(t, store, d, fleetView(ReasonUpToDate,
		hostTarget("h1", "gpu-01", ""), hostTarget("h2", "gpu-02", "")))

	store.requestCancel()
	run := runToEnd(t, f, store)

	if run.State != RunCancelled {
		t.Fatalf("run state = %q, want cancelled", run.State)
	}
	if len(d.steps()) != 0 {
		t.Fatalf("targets reached = %v, want none after a cancel between targets", d.steps())
	}
}

func TestFleetCopiesForceOntoEveryHostAttempt(t *testing.T) {
	store := newFakeFleetStore(true)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView(ReasonUpToDate, hostTarget("h1", "gpu-01", "")))

	runToEnd(t, f, store)

	attempts, _ := store.RunAttempts(context.Background(), testRunID)
	if len(attempts) != 1 || !attempts[0].Force {
		t.Fatalf("attempts = %+v, want one carrying the run's force", attempts)
	}
}

// A run resumed after a restart re-drives the control-plane attempt it left
// open, then carries on with the hosts.
func TestFleetResumesAnOpenControlPlaneAttempt(t *testing.T) {
	store := newFakeFleetStore(false)
	store.run.State = RunRunning
	cp := TargetControlPlane
	store.run.CurrentTarget = &cp
	if _, err := store.CreateControlPlaneAttempt(context.Background(), NewControlPlaneAttempt{
		RunID: &store.run.ID, ReleaseID: &store.release.ID,
	}); err != nil {
		t.Fatal(err)
	}
	store.attempts[0].State = AttemptRecreating

	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))

	run := runToEnd(t, f, store)

	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", run.State)
	}
	if !d.cpApplied {
		t.Fatal("the open control-plane attempt was not re-adopted")
	}
	if got := d.steps(); len(got) != 2 || got[1] != "h1" {
		t.Fatalf("targets reached = %v, want the control plane then the host", got)
	}
}
