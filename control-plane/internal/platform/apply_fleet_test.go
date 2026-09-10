package platform

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// The sequencer's ordering rules, with fake drivers: no database, no updater,
// no agent. Every case here is one sentence of control-api.md
// §"Platform-release apply".

const testRunID = "33333333-3333-4333-8333-333333333333"

// The schema version the fake store's release carries. Tests place the
// runner's own version on one side of it or the other to choose whether the
// release runs a migration (#153).
const fakeReleaseSchema = 75

// The two reasons amendment 2 appends, at the END of the precedence order.
func TestPlanReportsRunActiveAndTheControlPlanesOwnUpdater(t *testing.T) {
	newest := Release{ID: "r1", Channel: ChannelStable, SourceCommit: commitC,
		BuiltAt: at(3), SchemaVersion: 74, Manifest: []byte(`{}`)}
	in := PlanInputs{
		Channel:                 ChannelStable,
		ControlPlane:            cp(commitA, 74),
		Hosts:                   []HostIdentity{host("h1", "gpu-01", commitB)},
		Releases:                []Release{newest},
		UpdaterPresent:          true,
		ControlPlaneInstallMode: str(InstallRegistry),
	}

	// With no updater beside it, this control plane cannot be moved at all.
	noUpdater := in
	noUpdater.UpdaterPresent = false
	if got := targetReason(PlanRelease(noUpdater), TargetControlPlane); got != ReasonUpdaterAbsent {
		t.Fatalf("control plane reason = %q, want updater_absent", got)
	}

	// A source-built control plane is never offered a registry image, and an
	// install mode nobody could read is never assumed to be one.
	sourceBuilt := in
	sourceBuilt.ControlPlaneInstallMode = str(InstallSource)
	if got := targetReason(PlanRelease(sourceBuilt), TargetControlPlane); got != ReasonInstallModeSource {
		t.Fatalf("control plane reason = %q, want install_mode_source", got)
	}
	unknownMode := in
	unknownMode.ControlPlaneInstallMode = nil
	if got := targetReason(PlanRelease(unknownMode), TargetControlPlane); got != ReasonIdentityUnknown {
		t.Fatalf("control plane reason = %q, want identity_unknown", got)
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
	// Fleet-wide non-terminal sessions, the subset of those not yet `running`,
	// and the cordon/drain calls the run made.
	sessions   int
	inFlightN  int
	hosts      []HostIdentity
	cordon     []string
	uncordon   []string
	forceDrain []string
	// Every sessions_remaining the run recorded, in order: the final value is a
	// single column, so only the sequence shows what was waited on.
	remainingLog []int
	// The run's persisted record of what it found (migration 0076).
	cordons_ []HostCordon
	// Whether each run's scheduling cleanup was proven done (migration 0083),
	// and faults to inject into recording it and into confirming a host's
	// scheduling state (#176).
	cordonsRestored map[string]bool
	markRestoredErr error
	hostStatusErr   error
	claims          int
	// Agent connectivity as the registry would report it, keyed by host id.
	// Absent = connected: the fake's default host is a live one.
	disconnected map[string]bool
	// Release-read fault injection: reads from the releaseErrFrom'th on fail.
	releaseReads   int
	releaseErrFrom int
	// Session-count fault injection (#175): reads in
	// [sessionsErrFrom, sessionsErrTo] fail, and sessionsErrTo == 0 means "and
	// every read after that". Counting from 1, as releaseErrFrom does.
	sessionsReads   int
	sessionsErrFrom int
	sessionsErrTo   int
}

func newFakeFleetStore(force bool) *fakeFleetStore {
	return &fakeFleetStore{
		run:      ApplyRun{ID: testRunID, ReleaseID: testReleaseID, State: RunPending, Force: force},
		release:  Release{ID: testReleaseID, SourceCommit: testCommit, SchemaVersion: fakeReleaseSchema, Manifest: applyManifest(testCommit, fakeReleaseSchema)},
		inFlight: map[string]bool{},
		hosts: []HostIdentity{
			{HostID: "h1", NodeName: "gpu-01", Status: "online"},
			{HostID: "h2", NodeName: "gpu-02", Status: "online"},
		},
	}
}

func (f *fakeFleetStore) Hosts(context.Context) ([]HostIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]HostIdentity, len(f.hosts))
	copy(out, f.hosts)
	return out, nil
}

func (f *fakeFleetStore) HostStatus(_ context.Context, hostID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hostStatusErr != nil {
		return "", f.hostStatusErr
	}
	for _, h := range f.hosts {
		if h.HostID == hostID {
			return h.Status, nil
		}
	}
	return "", ErrHostNotFound
}

func (f *fakeFleetStore) SetCordonedHosts(_ context.Context, _ string, states []HostCordon) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cordons_ = append([]HostCordon(nil), states...)
	return nil
}

func (f *fakeFleetStore) CordonedHosts(context.Context, string) ([]HostCordon, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HostCordon(nil), f.cordons_...), nil
}

func (f *fakeFleetStore) MarkCordonsRestored(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markRestoredErr != nil {
		return f.markRestoredErr
	}
	if f.cordonsRestored == nil {
		f.cordonsRestored = map[string]bool{}
	}
	f.cordonsRestored[runID] = true
	return nil
}

// ClaimUnrestoredCordons mirrors the real store's predicate: terminal, with
// something recorded in cordoned_hosts, and never stamped. The claim is counted
// so a test can tell a sweep that ran from one that refused.
func (f *fakeFleetStore) ClaimUnrestoredCordons(_ context.Context, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if !TerminalRunState(f.run.State) || len(f.cordons_) == 0 || f.cordonsRestored[f.run.ID] || limit <= 0 {
		return nil, nil
	}
	return []string{f.run.ID}, nil
}

// pending is the recovery requirement as the sweep would find it, WITHOUT
// claiming it — assertions must not consume the queue they are inspecting.
func (f *fakeFleetStore) pending() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !TerminalRunState(f.run.State) || len(f.cordons_) == 0 || f.cordonsRestored[f.run.ID] {
		return nil
	}
	return []string{f.run.ID}
}

// pendingIsOutstanding is pending() as a yes/no, for a fixture guard.
func (f *fakeFleetStore) pendingIsOutstanding() bool { return len(f.pending()) == 1 }

func (f *fakeFleetStore) setRunState(state string) {
	f.mu.Lock()
	f.run.State = state
	f.mu.Unlock()
}

func (f *fakeFleetStore) claimCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims
}

// restoredCordons is whether the run's cleanup was recorded as proven done.
func (f *fakeFleetStore) restoredCordons(runID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cordonsRestored[runID]
}

func (f *fakeFleetStore) FleetNonTerminalSessions(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionsReads++
	if f.sessionsErrFrom > 0 && f.sessionsReads >= f.sessionsErrFrom &&
		(f.sessionsErrTo == 0 || f.sessionsReads <= f.sessionsErrTo) {
		// (0, err), exactly as the real store answers a failed count. The zero
		// IS the defect in #175, so a sentinel here would test a store that
		// does not exist.
		return 0, errors.New("session count failed")
	}
	return f.sessions, nil
}

// countReads is how many times the run has asked for the fleet's session count.
func (f *fakeFleetStore) countReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionsReads
}

func (f *fakeFleetStore) FleetInFlightSessions(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlightN, nil
}

func (f *fakeFleetStore) setSessions(n int) {
	f.mu.Lock()
	f.sessions = n
	f.mu.Unlock()
}

// setInFlight sets the non-terminal-but-not-yet-`running` count — the rows a
// reconnecting agent fails (#153).
func (f *fakeFleetStore) setInFlight(n int) {
	f.mu.Lock()
	f.inFlightN = n
	f.mu.Unlock()
}

// recordedRemaining is every sessions_remaining the run wrote, in order.
func (f *fakeFleetStore) recordedRemaining() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.remainingLog...)
}

// drained is which hosts the run force-drained, in order.
func (f *fakeFleetStore) drained() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forceDrain...)
}

func (f *fakeFleetStore) FailAttempt(_ context.Context, id, reason, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.attempts {
		if a.ID != id || TerminalAttemptState(a.State) {
			continue
		}
		a.State = AttemptFailed
		r := reason
		a.Reason = &r
	}
	return nil
}

// cordons records what the run did to scheduling.
func (f *fakeFleetStore) cordons() FleetCordons {
	return FleetCordons{
		Cordon: func(_ context.Context, hostID string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.cordon = append(f.cordon, hostID)
			f.setStatusLocked(hostID, "draining")
			return nil
		},
		Uncordon: func(_ context.Context, hostID string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.uncordon = append(f.uncordon, hostID)
			f.setStatusLocked(hostID, "online")
			return nil
		},
		// Stands in for coordinator.DrainHost(force=true): the sessions on that
		// host actually end, which is the whole point of the fix — a force that
		// records the count and ends nothing is what #153 had to correct.
		Drain: func(_ context.Context, hostID string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.forceDrain = append(f.forceDrain, hostID)
			f.setStatusLocked(hostID, "draining")
			f.sessions = 0
			f.inFlightN = 0
			return nil
		},
	}
}

func (f *fakeFleetStore) setStatusLocked(hostID, status string) {
	for i := range f.hosts {
		if f.hosts[i].HostID == hostID {
			f.hosts[i].Status = status
		}
	}
}

// setHostStatus and hostStatus read/write the fake's idea of a host's
// scheduling state, which is what a cordon actually changes — `scheduling()`
// below returns the CALL LOG, and a leaked cordon is a state, not a call.
func (f *fakeFleetStore) setHostStatus(hostID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setStatusLocked(hostID, status)
}

func (f *fakeFleetStore) hostStatus(hostID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.hosts {
		if h.HostID == hostID {
			return h.Status
		}
	}
	return ""
}

// draining is every host currently out of scheduling.
func (f *fakeFleetStore) draining() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0)
	for _, h := range f.hosts {
		if h.Status == "draining" {
			out = append(out, h.HostID)
		}
	}
	return out
}

func (f *fakeFleetStore) scheduling() (cordoned, uncordoned []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cordon...), append([]string(nil), f.uncordon...)
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
	f.releaseReads++
	// Fail from the Nth read on, so a test can let the attempt be created and
	// still make the drain decision's own read fail.
	if f.releaseErrFrom > 0 && f.releaseReads >= f.releaseErrFrom {
		return Release{}, errors.New("release read failed")
	}
	return f.release, nil
}

func (f *fakeFleetStore) NonTerminalSessions(context.Context, string) (int, error) { return 0, nil }

func (f *fakeFleetStore) SetWaitingSessions(_ context.Context, id string, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remainingLog = append(f.remainingLog, n)
	for _, a := range f.attempts {
		if a.ID != id || TerminalAttemptState(a.State) {
			continue
		}
		a.State = AttemptWaitingSessions
		remaining := n
		a.SessionsRemaining = &remaining
	}
	return nil
}

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

// planningView builds the view by running the REAL PlanRelease over the fake's
// own host table, so cordoning a host actually moves its status and eligibility
// is re-derived from it. Every other fleet test injects Target rows directly,
// which is why #169 — where the run's own cordon changed what the planner saw —
// could not be expressed before.
func planningView(f *fakeFleetStore, cpSchema int) func(context.Context) (View, error) {
	return func(ctx context.Context) (View, error) {
		hosts, err := f.Hosts(ctx)
		if err != nil {
			return View{}, err
		}
		f.mu.Lock()
		for i := range hosts {
			connected := !f.disconnected[hosts[i].HostID]
			hosts[i].AgentConnected = &connected
			hosts[i].AgentVersion = str("0.1.0")
			hosts[i].SourceCommit = str(commitA)
			hosts[i].BuiltAt = str("2026-09-01T00:00:00Z")
			hosts[i].InstallMode = str(InstallRegistry)
			hosts[i].UpdaterPresent = boolp(true)
		}
		rel := f.release
		f.mu.Unlock()
		// The fake's release carries no channel or version — it is normally fed
		// straight to the sequencer, never through `offerable`, which filters on
		// both. Fill them so the real planner lists it.
		rel.Channel = ChannelStable
		if rel.Version == nil {
			rel.Version = str("0.9.0")
		}
		return PlanRelease(PlanInputs{
			Channel:                 ChannelStable,
			ControlPlane:            cp(testCommit, cpSchema),
			Hosts:                   hosts,
			Releases:                []Release{rel},
			UpdaterPresent:          true,
			ControlPlaneInstallMode: str(InstallRegistry),
		}), nil
	}
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
	f := NewFleetRunner(store, d, d, ManifestOrEdge{}, store.cordons(), view, testLogger())
	f.PollWait = time.Millisecond
	f.Deadline = 2 * time.Second
	f.AdoptSettle = time.Millisecond
	f.InFlightSettle = 500 * time.Millisecond
	// One below the fixture release's schema, so the default fixture is a
	// MIGRATING release and every case that does not care about #153 keeps
	// exercising the drain. A case that wants the holding path raises this to
	// the release's own version.
	f.SchemaVersion = fakeReleaseSchema - 1
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

// A release carrying a migration drains the whole fleet before the
// control-plane step (prepareFleet has the argument).
func TestFleetDrainsEverySessionBeforeAMigratingControlPlaneStep(t *testing.T) {
	store := newFakeFleetStore(false)
	store.setSessions(2)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))

	f.Start(store.run)
	// The count is reported on the CONTROL-PLANE attempt, and nothing is sent
	// while it is above zero.
	waitFor(t, "the fleet-wide session count on the control-plane attempt", func() bool {
		as, _ := store.RunAttempts(context.Background(), testRunID)
		return len(as) == 1 && as[0].Target == TargetControlPlane &&
			as[0].State == AttemptWaitingSessions && as[0].SessionsRemaining != nil &&
			*as[0].SessionsRemaining == 2
	})
	if steps := d.steps(); len(steps) != 0 {
		t.Fatalf("targets reached = %v, want nothing sent while the fleet is busy", steps)
	}
	// Every online host is out of scheduling for the duration.
	cordoned, _ := store.scheduling()
	if len(cordoned) != 2 {
		t.Fatalf("cordoned = %v, want both hosts", cordoned)
	}

	store.setSessions(0)
	waitFor(t, "the run to finish", func() bool {
		r, _ := store.Run(context.Background(), testRunID)
		return TerminalRunState(r.State)
	})
	run, _ := store.Run(context.Background(), testRunID)
	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", run.State)
	}
	// And back into service when the run is done.
	waitFor(t, "both hosts to be restored", func() bool {
		_, uncordoned := store.scheduling()
		return len(uncordoned) == 2
	})
}

// A release that runs no migration takes the control-plane step under live
// sessions: the run finishes with them still running, and still holds the fleet
// out of scheduling.
func TestFleetHoldsSessionsAcrossANonMigratingControlPlaneStep(t *testing.T) {
	store := newFakeFleetStore(false)
	store.setSessions(3)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))
	// Level with the release: it embeds no migration this control plane has
	// not already run.
	f.SchemaVersion = fakeReleaseSchema

	// Nothing ever sets the count to zero, so reaching a terminal state at all
	// is the assertion: the step did not wait.
	run := runToEnd(t, f, store)

	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded without the fleet ever emptying", run.State)
	}
	if got := d.steps(); len(got) != 2 || got[0] != TargetControlPlane || got[1] != "h1" {
		t.Fatalf("targets reached = %v, want the control plane then the host", got)
	}
	if n, _ := store.FleetNonTerminalSessions(context.Background()); n != 3 {
		t.Fatalf("sessions = %d, want the 3 that were running to still be running", n)
	}
	// `sessions_remaining` is what a client tells the operator they are about to
	// lose, so a step that waits on nothing must not report one.
	as, _ := store.RunAttempts(context.Background(), testRunID)
	if len(as) == 0 || as[0].Target != TargetControlPlane {
		t.Fatalf("attempts = %+v, want the control-plane attempt first", as)
	}
	if as[0].SessionsRemaining != nil {
		t.Fatalf("sessions_remaining = %v, want null: the step waited on nothing", *as[0].SessionsRemaining)
	}
	// The fleet is still cordoned for the run's whole life: every host in it is
	// about to be recreated.
	if cordoned, _ := store.scheduling(); len(cordoned) != 2 {
		t.Fatalf("cordoned = %v, want both hosts", cordoned)
	}
}

// A release this process cannot read is treated as migrating. Draining a fleet
// that did not need it costs sessions the operator can see; running a migration
// under live sessions is the failure nobody sees until later.
func TestFleetDrainsWhenTheReleaseCannotBeRead(t *testing.T) {
	store := newFakeFleetStore(false)
	store.setSessions(1)
	// The first read builds the attempt; the drain decision's read fails.
	store.releaseErrFrom = 2
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))
	f.SchemaVersion = fakeReleaseSchema // would otherwise hold

	f.Start(store.run)
	waitFor(t, "the control-plane attempt to wait on the fleet", func() bool {
		as, _ := store.RunAttempts(context.Background(), testRunID)
		return len(as) == 1 && as[0].State == AttemptWaitingSessions
	})
	if steps := d.steps(); len(steps) != 0 {
		t.Fatalf("targets reached = %v, want nothing sent while the fleet is busy", steps)
	}
}

// force on a MIGRATING release ENDS the sessions rather than merely skipping the
// wait. Pre-#128 the recreate ended them as a side effect and skipping was
// enough; it no longer does, so a force that only skipped would run the
// migration under the very sessions it claimed to end (#153).
func TestFleetForceStopsTheSessionsBeforeAMigratingControlPlaneStep(t *testing.T) {
	store := newFakeFleetStore(true)
	store.setSessions(3)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))

	run := runToEnd(t, f, store)

	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded with force", run.State)
	}
	// Every host the run recorded was force-drained, and the fleet reached zero
	// BEFORE the control plane was sent its release.
	if drained := store.drained(); len(drained) != 2 || drained[0] != "h1" || drained[1] != "h2" {
		t.Fatalf("force-drained hosts = %v, want both recorded hosts", drained)
	}
	if n, _ := store.FleetNonTerminalSessions(context.Background()); n != 0 {
		t.Fatalf("fleet sessions after a forced migrating step = %d, want 0", n)
	}
	// force is the operator agreeing to lose them, and the N is recorded BEFORE
	// anything ends it — a watcher that only ever saw the post-drain zero would
	// never learn what the run cost.
	as, _ := store.RunAttempts(context.Background(), testRunID)
	if as[0].SessionsRemaining == nil {
		t.Fatal("sessions_remaining must be recorded on a forced migrating attempt")
	}
	if got := store.recordedRemaining(); len(got) == 0 || got[0] != 3 {
		t.Fatalf("recorded sessions_remaining = %v, want the 3 that were live first", got)
	}
}

// With no Drain wired, force must NOT become "send it anyway": the safe reading
// of "end them and we cannot" is to wait, exactly as an unforced attempt does.
func TestFleetForceWithNoDrainWiredStillWaits(t *testing.T) {
	store := newFakeFleetStore(true)
	store.setSessions(2)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	cordons := store.cordons()
	cordons.Drain = nil
	f := NewFleetRunner(store, d, d, ManifestOrEdge{}, cordons,
		fleetView("", hostTarget("h1", "gpu-01", "")), testLogger())
	f.PollWait = time.Millisecond
	f.Deadline = 300 * time.Millisecond
	f.AdoptSettle = time.Millisecond
	f.InFlightSettle = 50 * time.Millisecond
	f.SchemaVersion = fakeReleaseSchema - 1
	t.Cleanup(f.Close)

	run := runToEnd(t, f, store)

	if run.State != RunFailed {
		t.Fatalf("run state = %q, want failed: nothing ended the sessions, so the wait must time out", run.State)
	}
	if steps := d.steps(); len(steps) != 0 {
		t.Fatalf("targets reached = %v, want nothing sent: the migration must not run over live sessions", steps)
	}
}

// A non-migrating step no longer drains, but it must still let an in-flight
// launch land: those rows are what the reconnecting agent fails (#153, MAJOR 2).
func TestFleetWaitsForInFlightLaunchesOnANonMigratingStep(t *testing.T) {
	store := newFakeFleetStore(false)
	store.setSessions(2)
	store.setInFlight(1)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))
	f.SchemaVersion = fakeReleaseSchema // not migrating

	f.Start(store.run)
	// Nothing is sent while a launch is still in flight, even though no
	// `running` session is at stake.
	waitFor(t, "the fleet to be cordoned", func() bool {
		cordoned, _ := store.scheduling()
		return len(cordoned) == 2
	})
	if steps := d.steps(); len(steps) != 0 {
		t.Fatalf("targets reached = %v, want nothing sent while a launch is in flight", steps)
	}

	// The launch completes: it is `running` now, so it is no longer in flight and
	// it survives the recreate.
	store.setInFlight(0)
	waitFor(t, "the run to finish", func() bool {
		r, _ := store.Run(context.Background(), testRunID)
		return TerminalRunState(r.State)
	})
	run, _ := store.Run(context.Background(), testRunID)

	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", run.State)
	}
	// It waited: the attempt never entered waiting_sessions, because nothing
	// was being lost and no operator consented to anything.
	as, _ := store.RunAttempts(context.Background(), testRunID)
	if as[0].SessionsRemaining != nil {
		t.Fatalf("sessions_remaining = %v, want null on a non-migrating step", as[0].SessionsRemaining)
	}
	if n, _ := store.FleetNonTerminalSessions(context.Background()); n != 2 {
		t.Fatalf("fleet sessions = %d, want the 2 running sessions untouched", n)
	}
}

// The settle is bounded: a wedged in-flight row must not hold a release for
// ever. Expiry proceeds, because the cost of being wrong is a launch that was
// going to fail anyway.
func TestFleetProceedsWhenInFlightLaunchesNeverSettle(t *testing.T) {
	store := newFakeFleetStore(false)
	store.setInFlight(1)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))
	f.SchemaVersion = fakeReleaseSchema // not migrating
	f.InFlightSettle = 20 * time.Millisecond

	run := runToEnd(t, f, store)

	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded past the settle deadline", run.State)
	}
}

// A host an admin had already cordoned stays cordoned; the run restores what it
// found, on every terminal path including a failed one.
func TestFleetRestoresTheCordonItFoundOnFailure(t *testing.T) {
	store := newFakeFleetStore(false)
	store.hosts[1].Status = "draining"
	d := &fakeDrivers{store: store, outcome: map[string]string{"h1": AttemptFailed}}
	f := testFleet(t, store, d, fleetView("",
		hostTarget("h1", "gpu-01", ""), hostTarget("h2", "gpu-02", "")))

	run := runToEnd(t, f, store)

	if run.State != RunFailed {
		t.Fatalf("run state = %q, want failed", run.State)
	}
	waitFor(t, "the run's cordon to be lifted", func() bool {
		_, uncordoned := store.scheduling()
		return len(uncordoned) == 1 && uncordoned[0] == "h1"
	})
	cordoned, _ := store.scheduling()
	// h2 is cordoned once at the start (it was found draining, so untouched)
	// and re-asserted at the end.
	found := 0
	for _, id := range cordoned {
		if id == "h2" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("cordon calls for the admin-cordoned host = %d, want the one restore", found)
	}
}

// The control-plane step is skipped when it is already on the release, and so
// is the fleet drain: nothing is about to be recreated.
func TestFleetDoesNotDrainWhenTheControlPlaneIsUpToDate(t *testing.T) {
	store := newFakeFleetStore(false)
	store.setSessions(5)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView(ReasonUpToDate, hostTarget("h1", "gpu-01", "")))

	run := runToEnd(t, f, store)

	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", run.State)
	}
	if cordoned, _ := store.scheduling(); len(cordoned) != 0 {
		t.Fatalf("cordoned = %v, want none: the per-host machine cordons its own", cordoned)
	}
}

// The run restarts mid-flight, so the fleet cordon is re-established from what
// the new process finds, and released when the adopted run finishes.
func TestFleetReCordonsOnAdoption(t *testing.T) {
	store := newFakeFleetStore(false)
	store.run.State = RunRunning
	if _, err := store.CreateControlPlaneAttempt(context.Background(), NewControlPlaneAttempt{
		RunID: &store.run.ID, ReleaseID: &store.release.ID,
	}); err != nil {
		t.Fatal(err)
	}
	store.attempts[0].State = AttemptRecreating

	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))

	f.Adopt(context.Background())
	waitFor(t, "the adopted run to finish", func() bool {
		r, _ := store.Run(context.Background(), testRunID)
		return TerminalRunState(r.State)
	})

	waitFor(t, "both hosts to be restored when the adopted run finished", func() bool {
		_, uncordoned := store.scheduling()
		return len(uncordoned) == 2
	})
	if cordoned, _ := store.scheduling(); len(cordoned) != 2 {
		t.Fatalf("cordoned = %v, want both hosts re-cordoned on adoption", cordoned)
	}
}

// The live #117 failure: the run was re-adopted, resolved its control-plane
// attempt and reached the first host while every agent was still reconnecting.
func TestFleetSettlesBeforeTheFirstHostOnAdoption(t *testing.T) {
	store := newFakeFleetStore(false)
	store.run.State = RunRunning
	if _, err := store.CreateControlPlaneAttempt(context.Background(), NewControlPlaneAttempt{
		RunID: &store.run.ID, ReleaseID: &store.release.ID,
	}); err != nil {
		t.Fatal(err)
	}
	store.attempts[0].State = AttemptSucceeded

	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))
	f.AdoptSettle = 60 * time.Millisecond

	start := time.Now()
	f.Adopt(context.Background())
	waitFor(t, "the adopted run to finish", func() bool {
		r, _ := store.Run(context.Background(), testRunID)
		return TerminalRunState(r.State)
	})
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("reached the first host after %s, want the settle window observed", elapsed)
	}
}

// The cordon record is persisted, so the restart the run causes cannot lose it.
func TestFleetRestoresCordonsRecordedBeforeTheRestart(t *testing.T) {
	store := newFakeFleetStore(false)
	store.run.State = RunRunning
	// What the previous process wrote before it cordoned and replaced itself.
	if err := store.SetCordonedHosts(context.Background(), testRunID, []HostCordon{
		{HostID: "h1", WasCordoned: false},
		{HostID: "h2", WasCordoned: true},
	}); err != nil {
		t.Fatal(err)
	}
	store.hosts[0].Status = "draining"
	store.hosts[1].Status = "draining"
	if _, err := store.CreateControlPlaneAttempt(context.Background(), NewControlPlaneAttempt{
		RunID: &store.run.ID, ReleaseID: &store.release.ID,
	}); err != nil {
		t.Fatal(err)
	}
	store.attempts[0].State = AttemptSucceeded

	d := &fakeDrivers{store: store, outcome: map[string]string{"h1": AttemptFailed}}
	f := testFleet(t, store, d, fleetView("",
		hostTarget("h1", "gpu-01", ""), hostTarget("h2", "gpu-02", "")))

	f.Adopt(context.Background())
	waitFor(t, "the adopted run to finish", func() bool {
		r, _ := store.Run(context.Background(), testRunID)
		return TerminalRunState(r.State)
	})

	run, _ := store.Run(context.Background(), testRunID)
	if run.State != RunFailed {
		t.Fatalf("run state = %q, want failed", run.State)
	}
	// A failed run must still put the fleet back: h1 was the run's cordon, h2
	// was the operator's. The restore runs after the terminal write.
	waitFor(t, "the run's cordon to be lifted", func() bool {
		_, uncordoned := store.scheduling()
		return len(uncordoned) == 1 && uncordoned[0] == "h1"
	})
	status, _ := store.HostStatus(context.Background(), "h2")
	if status != "draining" {
		t.Fatalf("the operator's cordon = %q, want it left in place", status)
	}
}

// Adoption with a record re-cordons: between the two processes something (an
// agent re-register, an admin) may have lifted the cordon on a host the run
// itself cordoned. The host the record calls the operator's is not touched.
func TestFleetReCordonsRecordedHostsOnAdoption(t *testing.T) {
	store := newFakeFleetStore(false)
	store.run.State = RunRunning
	if err := store.SetCordonedHosts(context.Background(), testRunID, []HostCordon{
		{HostID: "h1", WasCordoned: false},
		{HostID: "h2", WasCordoned: true},
	}); err != nil {
		t.Fatal(err)
	}
	// h1's cordon was lifted underneath the run; h2 is the operator's.
	store.hosts[0].Status = "online"
	store.hosts[1].Status = "draining"
	if _, err := store.CreateControlPlaneAttempt(context.Background(), NewControlPlaneAttempt{
		RunID: &store.run.ID, ReleaseID: &store.release.ID,
	}); err != nil {
		t.Fatal(err)
	}
	store.attempts[0].State = AttemptRecreating

	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("",
		hostTarget("h1", "gpu-01", ""), hostTarget("h2", "gpu-02", "")))

	f.Adopt(context.Background())
	waitFor(t, "the adopted run to finish", func() bool {
		r, _ := store.Run(context.Background(), testRunID)
		return TerminalRunState(r.State)
	})
	waitFor(t, "the run's cordon to be lifted", func() bool {
		_, uncordoned := store.scheduling()
		return len(uncordoned) == 1 && uncordoned[0] == "h1"
	})

	// h1 cordoned on adoption; h2 only ever cordoned by the restore at the end.
	cordoned, _ := store.scheduling()
	if len(cordoned) != 2 || cordoned[0] != "h1" || cordoned[1] != "h2" {
		t.Fatalf("cordon calls = %v, want h1 re-cordoned on adoption then h2 restored", cordoned)
	}
	if status, _ := store.HostStatus(context.Background(), "h2"); status != "draining" {
		t.Fatalf("the operator's cordon = %q, want it left in place", status)
	}
}

// #140: a run started by a control plane older than migration 0076 has no
// record, and every host is draining because THAT process cordoned them.
// Reading those live statuses as the operator's intent re-cordons the whole
// fleet at finish with nothing left to lift it.
func TestFleetAdoptedWithNoRecordTreatsEveryCordonAsItsOwn(t *testing.T) {
	store := newFakeFleetStore(false)
	store.run.State = RunRunning
	store.hosts[0].Status = "draining"
	store.hosts[1].Status = "draining"
	if _, err := store.CreateControlPlaneAttempt(context.Background(), NewControlPlaneAttempt{
		RunID: &store.run.ID, ReleaseID: &store.release.ID,
	}); err != nil {
		t.Fatal(err)
	}
	store.attempts[0].State = AttemptRecreating

	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("",
		hostTarget("h1", "gpu-01", ""), hostTarget("h2", "gpu-02", "")))

	f.Adopt(context.Background())
	waitFor(t, "the adopted run to finish", func() bool {
		r, _ := store.Run(context.Background(), testRunID)
		return TerminalRunState(r.State)
	})
	waitFor(t, "both hosts to be back in scheduling", func() bool {
		one, _ := store.HostStatus(context.Background(), "h1")
		two, _ := store.HostStatus(context.Background(), "h2")
		return one == "online" && two == "online"
	})
	record, err := store.CordonedHosts(context.Background(), testRunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(record) != 2 {
		t.Fatalf("cordon record = %v, want one entry per host", record)
	}
	for _, st := range record {
		if st.WasCordoned {
			t.Fatalf("recorded %s as the operator's cordon, want every cordon read as the run's own", st.HostID)
		}
	}
}

// #169, THE LIVE CASE, through the real planner and a real cordon.
//
// A host whose agent is gone but whose row still says `online` — the normal
// state after any control-plane restart, because nothing corrects an idle host's
// status across one — must be SKIPPED, not attempted. Before the fix the run
// cordoned it (status -> draining, which is not `offline`), found it eligible,
// created an attempt, waited for an agent that never came, failed `timeout`, and
// stopped: every host behind it was never updated.
//
// The existing skip test injects ReasonHostOffline into the view directly, which
// bypasses the cordon that masked it in production. This one does not.
func TestFleetSkipsAHostWhoseAgentIsGoneEvenAfterItCordonsIt(t *testing.T) {
	store := newFakeFleetStore(false)
	// h1's row says online — stale — but no agent is there. h2 is live.
	store.disconnected = map[string]bool{"h1": true}
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, planningView(store, fakeReleaseSchema))

	run := runToEnd(t, f, store)

	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded — a run must not fail because a host happened to be offline", run.State)
	}
	// h2 was updated; h1 was skipped, not attempted.
	if got := d.steps(); len(got) != 1 || got[0] != "h2" {
		t.Fatalf("targets reached = %v, want only h2 — h1 has no agent to send to", got)
	}
	skips := f.Skips(testRunID)
	if len(skips) != 1 || skips[0].HostID != "h1" || skips[0].Reason != ReasonHostOffline {
		t.Fatalf("skipped = %+v, want h1 with %s", skips, ReasonHostOffline)
	}
	// And it is left in service, not cordoned: the run lifts what it imposed.
	waitFor(t, "h1 to be uncordoned", func() bool {
		st, err := store.HostStatus(context.Background(), "h1")
		return err == nil && st != "draining"
	})
}

// The mirror: a cordon on its own is NOT absence. A host the run has cordoned
// whose agent is present must still be applied to — otherwise the fix would skip
// the entire fleet, since a run cordons every host before it starts.
func TestFleetStillAppliesToACordonedHostWhoseAgentIsPresent(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, planningView(store, fakeReleaseSchema))

	run := runToEnd(t, f, store)

	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", run.State)
	}
	if got := d.steps(); len(got) != 2 {
		t.Fatalf("targets reached = %v, want both hosts — cordoned is the condition an apply wants", got)
	}
	if skips := f.Skips(testRunID); len(skips) != 0 {
		t.Fatalf("skipped = %+v, want none", skips)
	}
}

// #170's real bug: only `draining` is a cordon. Recording an OFFLINE host as
// "the admin cordoned it" meant the run never cordoned it — so it could take a
// placement the run would destroy — and then CORDONED it at restore, leaving a
// host nobody cordoned out of scheduling with nothing to lift it.
func TestFleetDoesNotTreatAnOfflineHostAsAnAdminsCordon(t *testing.T) {
	store := newFakeFleetStore(false)
	store.hosts[0].Status = HostOffline
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, planningView(store, fakeReleaseSchema))

	runToEnd(t, f, store)

	recorded, _ := store.CordonedHosts(context.Background(), testRunID)
	for _, st := range recorded {
		if st.HostID == "h1" && st.WasCordoned {
			t.Fatal("an offline host was recorded as the admin's cordon; the run will now cordon it at restore " +
				"and leave a host nobody cordoned out of scheduling")
		}
	}
	// And it must not be left draining.
	if st, err := store.HostStatus(context.Background(), "h1"); err == nil && st == "draining" {
		t.Fatal("an offline host was left draining by the run's own restore")
	}
}

// #175. A migrating control-plane step is the one place a session count is
// load-bearing rather than advisory: past it the database is migrated, and every
// migration in this repo was authored assuming no session was live. The store
// answers a failed count with `(0, error)` and every wait below is written
// against `remaining`, so a read that fails at the wrong moment reads exactly
// like "the fleet has drained".
//
// The four tests below pin the three places that could happen and the one place
// it must not cost a healthy run anything.

// The FIRST count fails. This used to `return true` — "the count is advisory;
// refusing to update over it would be worse" — which sent the release.
func TestFleetMigratingStepWillNotProceedOnAnUnreadableSessionCount(t *testing.T) {
	store := newFakeFleetStore(false)
	store.setSessions(2)
	store.sessionsErrFrom = 1 // every read fails
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))
	f.Deadline = 300 * time.Millisecond

	run := runToEnd(t, f, store)

	if run.State != RunFailed {
		t.Fatalf("run state = %q, want failed: a migration must not run on a count nobody read", run.State)
	}
	if steps := d.steps(); len(steps) != 0 {
		t.Fatalf("targets reached = %v, want nothing sent", steps)
	}
	as, _ := store.RunAttempts(context.Background(), testRunID)
	if len(as) != 1 || as[0].Target != TargetControlPlane {
		t.Fatalf("attempts = %+v, want one control-plane attempt", as)
	}
	if as[0].Reason == nil || *as[0].Reason != ReasonTimeout {
		t.Fatalf("failure reason = %v, want %q", as[0].Reason, ReasonTimeout)
	}
	// Nothing may be reported as remaining: no read ever produced a number.
	if got := store.recordedRemaining(); len(got) != 0 {
		t.Fatalf("recorded sessions_remaining = %v, want none — every read failed", got)
	}
}

// A count that fails DURING the wait must not end it. `remaining, err =
// store.FleetNonTerminalSessions(ctx)` assigned the store's zero before the
// error was even looked at, and `continue` then re-tested `remaining > 0`
// against it — so the second poll of a fleet that never emptied sent the
// release.
func TestFleetMigratingStepKeepsWaitingWhenAPollCannotCount(t *testing.T) {
	store := newFakeFleetStore(false)
	store.setSessions(3)
	store.sessionsErrFrom = 2 // the first read succeeds; every poll after fails
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))
	f.Deadline = 300 * time.Millisecond

	run := runToEnd(t, f, store)

	if run.State != RunFailed {
		t.Fatalf("run state = %q, want failed: the fleet never emptied", run.State)
	}
	if steps := d.steps(); len(steps) != 0 {
		t.Fatalf("targets reached = %v, want nothing sent", steps)
	}
	// The one count that DID read is the one the operator was shown, and it is
	// not zero.
	if got := store.recordedRemaining(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("recorded sessions_remaining = %v, want just the 3 that read", got)
	}
}

// The other half of the same rule: a transient count failure must cost a healthy
// run nothing. Once a read succeeds and says zero, the step proceeds.
func TestFleetMigratingStepProceedsOnceACountActuallyReadsZero(t *testing.T) {
	store := newFakeFleetStore(false)
	store.setSessions(1)
	// The first read is good; the next five fail; then reads work again.
	store.sessionsErrFrom, store.sessionsErrTo = 2, 6
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))

	f.Start(store.run)
	waitFor(t, "the failing polls to be spent", func() bool { return store.countReads() > 6 })
	store.setSessions(0)
	waitFor(t, "the run to finish", func() bool {
		r, _ := store.Run(context.Background(), testRunID)
		return TerminalRunState(r.State)
	})

	run, _ := store.Run(context.Background(), testRunID)
	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded: the count recovered and read zero", run.State)
	}
	if got := d.steps(); len(got) != 2 || got[0] != TargetControlPlane || got[1] != "h1" {
		t.Fatalf("targets reached = %v, want the control plane then the host", got)
	}
}

// force + migrating. `force` is the operator agreeing to lose the sessions, not
// agreeing to migrate without knowing. The recount right after the drain used to
// take the store's zero on failure and skip the wait entirely.
//
// Drain here succeeds without the count having reached zero, which is the
// ordinary shape rather than a contrived one: coordinator.DrainHost ends
// sessions asynchronously, so the rows are still non-terminal when it returns.
func TestFleetForcedMigratingStepDoesNotReadAFailedRecountAsDrained(t *testing.T) {
	store := newFakeFleetStore(true)
	store.setSessions(2)
	// 1 = the count recorded before the drain; 2 = the recount after it, and
	// every poll from there.
	store.sessionsErrFrom = 2
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	cordons := store.cordons()
	cordons.Drain = func(context.Context, string) error { return nil }
	f := NewFleetRunner(store, d, d, ManifestOrEdge{}, cordons,
		fleetView("", hostTarget("h1", "gpu-01", "")), testLogger())
	f.PollWait = time.Millisecond
	f.Deadline = 300 * time.Millisecond
	f.AdoptSettle = time.Millisecond
	f.InFlightSettle = 50 * time.Millisecond
	f.SchemaVersion = fakeReleaseSchema - 1
	t.Cleanup(f.Close)

	run := runToEnd(t, f, store)

	if run.State != RunFailed {
		t.Fatalf("run state = %q, want failed: the recount never proved the fleet empty", run.State)
	}
	if steps := d.steps(); len(steps) != 0 {
		t.Fatalf("targets reached = %v, want nothing sent", steps)
	}
	// What force cost is still recorded, from the one read that worked.
	if got := store.recordedRemaining(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("recorded sessions_remaining = %v, want the 2 that were live", got)
	}
}

// The wait is bounded AND interruptible whatever the count says: an unknown
// count must not make a run impossible to stop. The mechanism is the one the
// loop already had — an attempt resolved from outside, which is how a cancel
// reaches a step that has not been sent — and it still works while every count
// read is failing (#175 acceptance).
func TestFleetAWaitWhoseCountCannotBeReadStillStopsWhenTheAttemptResolves(t *testing.T) {
	store := newFakeFleetStore(false)
	store.setSessions(2)
	store.sessionsErrFrom = 1 // every read fails; the wait cannot end on its own
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", "")))
	f.Deadline = time.Minute // far longer than this test: only the resolution can end it

	f.Start(store.run)
	waitFor(t, "the control-plane attempt to exist", func() bool {
		as, _ := store.RunAttempts(context.Background(), testRunID)
		return len(as) == 1
	})
	as, _ := store.RunAttempts(context.Background(), testRunID)
	if err := store.FailAttempt(context.Background(), as[0].ID, ReasonTimeout, ""); err != nil {
		t.Fatalf("resolve the attempt: %v", err)
	}

	waitFor(t, "the run to finish", func() bool {
		r, _ := store.Run(context.Background(), testRunID)
		return TerminalRunState(r.State)
	})
	if steps := d.steps(); len(steps) != 0 {
		t.Fatalf("targets reached = %v, want nothing sent", steps)
	}
}

// #176. `finish` writes the terminal state and only THEN restores the cordons
// the run imposed, so a restore that fails — or a process that dies in that
// window — leaves a terminal run whose hosts are still `draining`. `ActiveRun`
// selects only NON-terminal runs, so nothing on the adoption path would ever
// look at that run again: the host stays out of scheduling with one ERROR line
// as the whole record.
//
// The recovery requirement is now persisted, and the next start acts on it.

// newRestoreFleet builds a runner over `store` whose Uncordon behaviour the test
// controls, so a cleanup failure can be injected and then lifted — which is what
// "restart in that window" looks like from this package.
func newRestoreFleet(t *testing.T, store *fakeFleetStore, d *fakeDrivers, uncordonFails *bool) *FleetRunner {
	t.Helper()
	cordons := store.cordons()
	realUncordon := cordons.Uncordon
	cordons.Uncordon = func(ctx context.Context, hostID string) error {
		if *uncordonFails {
			// Fails AND leaves the host draining, which is the shape that
			// matters: an uncordon that errors but worked is not a leak.
			return errors.New("uncordon failed")
		}
		return realUncordon(ctx, hostID)
	}
	// The control-plane path, because that is the one that cordons the whole
	// fleet; and a NON-migrating release, so the run does not also wait for a
	// drain this test is not about.
	f := NewFleetRunner(store, d, d, ManifestOrEdge{}, cordons,
		fleetView("", hostTarget("h1", "gpu-01", "")), testLogger())
	f.PollWait = time.Millisecond
	f.Deadline = 2 * time.Second
	f.AdoptSettle = time.Millisecond
	f.InFlightSettle = 50 * time.Millisecond
	f.SchemaVersion = fakeReleaseSchema
	t.Cleanup(f.Close)
	return f
}

// An uncordon that fails after the terminal write leaves a durable recovery
// requirement, not a log line.
func TestFleetRecordsUnfinishedSchedulingCleanup(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	uncordonFails := true
	f := newRestoreFleet(t, store, d, &uncordonFails)

	run := runToEnd(t, f, store)

	if !TerminalRunState(run.State) {
		t.Fatalf("run state = %q, want terminal: the run itself succeeded", run.State)
	}
	if store.restoredCordons(testRunID) {
		t.Fatal("cleanup was recorded as done while every uncordon was failing")
	}
	// And it is findable, which is the whole point: a terminal run is invisible
	// to ActiveRun.
	pending := store.pending()
	if len(pending) != 1 || pending[0] != testRunID {
		t.Fatalf("pending cleanup = %v, want just this run", pending)
	}
	if len(store.draining()) == 0 {
		t.Fatal("the fixture is wrong: this test needs a host actually left draining")
	}
}

// The restart. A fresh runner sweeps the requirement, puts the fleet back, and
// clears it — without re-running anything the update already did.
func TestFleetResumesSchedulingCleanupOnTheNextStart(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	uncordonFails := true
	f := newRestoreFleet(t, store, d, &uncordonFails)
	runToEnd(t, f, store)
	stepsBefore := len(d.steps())

	// The next control-plane start, with whatever broke the uncordon now fixed.
	uncordonFails = false
	f2 := newRestoreFleet(t, store, d, &uncordonFails)
	f2.ResumeCordonRestores(context.Background())

	if !store.restoredCordons(testRunID) {
		t.Fatal("the resumed sweep did not clear the recovery requirement")
	}
	if left := store.draining(); len(left) != 0 {
		t.Fatalf("still draining = %v, want the fleet back in scheduling", left)
	}
	if pending := store.pending(); len(pending) != 0 {
		t.Fatalf("pending cleanup = %v, want none once it is settled", pending)
	}
	// Cleanup is cleanup. It must not re-drive the update itself.
	if got := len(d.steps()); got != stepsBefore {
		t.Fatalf("targets reached = %d, want the %d from the run itself", got, stepsBefore)
	}
}

// An admin's own cordon is not this run's to lift, on the resumed path exactly as
// on the ordinary one (#170's rule, re-pinned here because the sweep is a second
// caller of it).
func TestFleetResumedCleanupPutsBackAnAdminCordonRatherThanLiftingIt(t *testing.T) {
	store := newFakeFleetStore(false)
	// h2 was the operator's before the run ever started.
	store.setHostStatus("h2", "draining")
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	uncordonFails := true
	f := newRestoreFleet(t, store, d, &uncordonFails)
	runToEnd(t, f, store)

	uncordonFails = false
	newRestoreFleet(t, store, d, &uncordonFails).ResumeCordonRestores(context.Background())

	if !store.restoredCordons(testRunID) {
		t.Fatal("the resumed sweep did not settle")
	}
	if got := store.hostStatus("h2"); got != "draining" {
		t.Fatalf("h2 status = %q, want the admin's own cordon left alone", got)
	}
	if got := store.hostStatus("h1"); got != "online" {
		t.Fatalf("h1 status = %q, want the run's own cordon lifted", got)
	}
}

// A failure that persists stays a requirement rather than being swallowed, and
// the sweep does not spin on it: one pass per start.
func TestFleetResumedCleanupThatStillFailsStaysARequirement(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	uncordonFails := true
	f := newRestoreFleet(t, store, d, &uncordonFails)
	runToEnd(t, f, store)

	newRestoreFleet(t, store, d, &uncordonFails).ResumeCordonRestores(context.Background())

	if store.restoredCordons(testRunID) {
		t.Fatal("a cleanup that never succeeded was recorded as done")
	}
	if pending := store.pending(); len(pending) != 1 {
		t.Fatalf("pending cleanup = %v, want it still outstanding for the next start", pending)
	}
}

// The work is done even when recording it is not, so the next start re-runs an
// idempotent restore rather than leaving a host cordoned.
func TestFleetTreatsAnUnrecordedRestoreAsStillOutstanding(t *testing.T) {
	store := newFakeFleetStore(false)
	store.markRestoredErr = errors.New("mark failed")
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	uncordonFails := false
	f := newRestoreFleet(t, store, d, &uncordonFails)

	runToEnd(t, f, store)

	if left := store.draining(); len(left) != 0 {
		t.Fatalf("still draining = %v, want the restore itself to have worked", left)
	}
	if pending := store.pending(); len(pending) != 1 {
		t.Fatalf("pending cleanup = %v, want the unrecorded restore re-checked next start", pending)
	}
}

// The sweep replays a claim about a moment that has passed — "this host was not
// cordoned when I found it" — so it must not run against a fleet a LIVE run has
// deliberately cordoned, or it puts hosts back into scheduling mid-update
// (#176 review, security finding).
func TestFleetDoesNotSweepWhileAFleetRunIsActive(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	uncordonFails := true
	f := newRestoreFleet(t, store, d, &uncordonFails)
	runToEnd(t, f, store)
	if !store.pendingIsOutstanding() {
		t.Fatal("the fixture is wrong: this test needs an outstanding requirement")
	}

	// A new run is in flight on the next start.
	store.setRunState(RunRunning)
	uncordonFails = false
	claimsBefore := store.claimCount()
	newRestoreFleet(t, store, d, &uncordonFails).ResumeCordonRestores(context.Background())

	if store.claimCount() != claimsBefore {
		t.Fatal("the sweep claimed work while a fleet run was active")
	}
	if store.restoredCordons(testRunID) {
		t.Fatal("the sweep settled an old run's cordons underneath a live one")
	}
}

// A `Cordon` that fails after taking effect, and a host deleted since the run
// recorded it, are both settled — not requirements retried on every start for
// the rest of the instance's life (#176 review).
func TestFleetSettlesAnAdminCordonThatIsAlreadyInPlace(t *testing.T) {
	store := newFakeFleetStore(false)
	store.setHostStatus("h2", "draining") // h2 is the operator's
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	cordons := store.cordons()
	realCordon := cordons.Cordon
	cordons.Cordon = func(ctx context.Context, hostID string) error {
		if hostID == "h2" {
			// Takes effect, then loses the answer.
			_ = realCordon(ctx, hostID)
			return errors.New("cordon request failed after it applied")
		}
		return realCordon(ctx, hostID)
	}
	f := NewFleetRunner(store, d, d, ManifestOrEdge{}, cordons,
		fleetView("", hostTarget("h1", "gpu-01", "")), testLogger())
	f.PollWait = time.Millisecond
	f.Deadline = 2 * time.Second
	f.AdoptSettle = time.Millisecond
	f.InFlightSettle = 50 * time.Millisecond
	f.SchemaVersion = fakeReleaseSchema
	t.Cleanup(f.Close)

	runToEnd(t, f, store)

	if !store.restoredCordons(testRunID) {
		t.Fatal("a cordon that is demonstrably in place left an undischargeable requirement")
	}
	if got := store.hostStatus("h2"); got != "draining" {
		t.Fatalf("h2 status = %q, want the admin's own cordon in place", got)
	}
}

// A status read that fails is "could not tell", and could-not-tell is not
// restored — the branch the recovery contract turns on, and the fake could not
// reach it before (#176 review, testing finding).
func TestFleetDoesNotSettleWhenAHostStatusCannotBeRead(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	uncordonFails := false
	f := newRestoreFleet(t, store, d, &uncordonFails)
	store.hostStatusErr = errors.New("status read failed")

	runToEnd(t, f, store)

	if store.restoredCordons(testRunID) {
		t.Fatal("cleanup was recorded as done although no host status could be confirmed")
	}
	if pending := store.pending(); len(pending) != 1 {
		t.Fatalf("pending cleanup = %v, want the unconfirmed run outstanding", pending)
	}
}
