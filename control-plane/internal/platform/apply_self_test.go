package platform

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
	"github.com/accreleus/quasar/control-plane/internal/updater"
)

// The control-plane target, over a REAL unix socket: the one thing a fake
// cannot check is that the client actually speaks the local API.

// AttemptRequestID completes selfStore on the runner tests' fake.
func (f *fakeStore) AttemptRequestID(_ context.Context, attemptID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for req, id := range f.requests {
		if id == attemptID {
			return req, nil
		}
	}
	return "", nil
}

// fakeUpdater is the socket's four routes, minus everything an apply does.
type fakeUpdater struct {
	mu        sync.Mutex
	accepted  []updater.ApplyRequest
	result    *updater.Result
	reject    *updaterError
	self      *UpdaterSelf
	selfReads int
}

func (u *fakeUpdater) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/apply", func(w http.ResponseWriter, r *http.Request) {
		var req updater.ApplyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.reject != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(u.reject)
			return
		}
		u.accepted = append(u.accepted, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(updater.Accepted{
			RequestID: req.RequestID,
			Previous:  []updater.PreviousComponent{{Name: ComponentControlPlane, Digest: strPtr("sha256:old")}},
		})
	})
	mux.HandleFunc("GET /v1/self", func(w http.ResponseWriter, _ *http.Request) {
		u.mu.Lock()
		self := u.self
		u.mu.Unlock()
		if self == nil {
			self = &UpdaterSelf{}
		}
		_ = json.NewEncoder(w).Encode(self)
	})
	mux.HandleFunc("GET /v1/results/{request_id}", func(w http.ResponseWriter, _ *http.Request) {
		u.mu.Lock()
		res := u.result
		u.mu.Unlock()
		if res == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(updaterError{Reason: "invalid", Message: "no result"})
			return
		}
		_ = json.NewEncoder(w).Encode(res)
	})
	return mux
}

func (u *fakeUpdater) setResult(r updater.Result) {
	u.mu.Lock()
	u.result = &r
	u.mu.Unlock()
}

// serveUpdater listens on a temp socket and returns its path.
func serveUpdater(t *testing.T, u *fakeUpdater) string {
	t.Helper()
	// A unix socket path is bounded at ~104 bytes; t.TempDir() under /tmp fits.
	sock := filepath.Join(t.TempDir(), "u.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: u.handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func controlPlaneAttempt(state string) Attempt {
	return Attempt{
		ID: "cp-1", Kind: KindApply, Target: TargetControlPlane, State: state,
		ReleaseID: strPtr(testReleaseID),
		RequestedDigests: []ComponentDigest{{
			Name: ComponentControlPlane, Image: "ghcr.io/x/quasar-control-plane", Digest: "sha256:new",
		}},
		PreviousDigests: []PreviousDigest{}, CreatedAt: time.Now(),
	}
}

func testSelfApplier(t *testing.T, store selfStore, up UpdaterAPI) *SelfApplier {
	t.Helper()
	s := NewSelfApplier(store, up, testLogger())
	s.PollInterval = 5 * time.Millisecond
	s.Deadline = 2 * time.Second
	s.Identity = func() buildinfo.Identity { return buildinfo.Identity{Version: "dev"} }
	return s
}

func TestSelfApplyRelaysTheUpdatersProgress(t *testing.T) {
	store := newFakeStore(controlPlaneAttempt(AttemptQueued))
	up := &fakeUpdater{}
	client := NewUpdaterClient(serveUpdater(t, up))
	self := testSelfApplier(t, store, client)

	up.setResult(updater.Result{State: updater.StatePulling})
	done := make(chan struct{})
	go func() {
		defer close(done)
		self.Apply(context.Background(), controlPlaneAttempt(AttemptQueued))
	}()

	// The relayed state reaches the row, and the 202's previous digests are
	// recorded before anything could need them for a restore.
	waitFor(t, "the pulling state and the previous digests to be recorded", func() bool {
		a := store.snapshot("cp-1")
		return a.State == AttemptPulling && len(a.PreviousDigests) == 1
	})

	up.setResult(updater.Result{State: updater.StateSucceeded})
	<-done
	if got := store.snapshot("cp-1").State; got != AttemptSucceeded {
		t.Fatalf("state = %q, want succeeded", got)
	}
	if len(up.accepted) != 1 {
		t.Fatalf("updater received %d applies, want 1", len(up.accepted))
	}
	if up.accepted[0].Components[0].Name != ComponentControlPlane {
		t.Fatalf("sent component %q, want the control plane's", up.accepted[0].Components[0].Name)
	}
}

func TestSelfApplyRefusesWithNoUpdaterSocket(t *testing.T) {
	store := newFakeStore(controlPlaneAttempt(AttemptQueued))
	self := testSelfApplier(t, store, NewUpdaterClient(filepath.Join(t.TempDir(), "absent.sock")))

	self.Apply(context.Background(), controlPlaneAttempt(AttemptQueued))

	a := store.snapshot("cp-1")
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonUpdaterAbsentFailure {
		t.Fatalf("state=%q reason=%v, want failed/updater_absent", a.State, a.Reason)
	}
}

func TestSelfApplyRecordsTheUpdatersRefusal(t *testing.T) {
	store := newFakeStore(controlPlaneAttempt(AttemptQueued))
	up := &fakeUpdater{reject: &updaterError{Reason: ReasonNamespaceRejected, Message: "outside the namespace"}}
	self := testSelfApplier(t, store, NewUpdaterClient(serveUpdater(t, up)))

	self.Apply(context.Background(), controlPlaneAttempt(AttemptQueued))

	a := store.snapshot("cp-1")
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonNamespaceRejected {
		t.Fatalf("state=%q reason=%v, want failed/namespace_rejected", a.State, a.Reason)
	}
}

// The load-bearing case: the process that sent the apply is gone, and the
// binary that boots is the evidence. Its own commit, not the result file, is
// what resolves the row.
func TestAdoptSucceedsWhenThisBuildIsServingTheReleasesCommit(t *testing.T) {
	open := controlPlaneAttempt(AttemptRecreating)
	store := newFakeStore(open)
	up := &fakeUpdater{}
	// A late result file still says recreating; it must not be believed over
	// this binary's own liveness.
	up.setResult(updater.Result{State: updater.StateRecreating})
	self := testSelfApplier(t, store, NewUpdaterClient(serveUpdater(t, up)))
	self.Identity = func() buildinfo.Identity {
		return buildinfo.Identity{Version: "0.9.0", SourceCommit: strPtr(testCommit)}
	}

	if !self.Adopt(context.Background(), open, testCommit) {
		t.Fatal("Adopt reported the attempt unresolved")
	}
	if got := store.snapshot("cp-1").State; got != AttemptSucceeded {
		t.Fatalf("state = %q, want succeeded", got)
	}
}

// The never-started auto-restore: the updater brought the OLD build back, so
// the identity does not match and the result file is what decides.
func TestAdoptRecordsARestoredFailure(t *testing.T) {
	open := controlPlaneAttempt(AttemptRecreating)
	store := newFakeStore(open)
	// A request id, as the row would carry after the send.
	if _, err := store.MintRequestID(context.Background(), "cp-1"); err != nil {
		t.Fatal(err)
	}
	up := &fakeUpdater{}
	reason := ReasonNeverStarted
	up.setResult(updater.Result{
		State: updater.StateFailed, Reason: &reason, Restored: true,
		Output:   "container exited",
		Previous: []updater.PreviousComponent{{Name: ComponentControlPlane, Digest: strPtr("sha256:old")}},
	})
	self := testSelfApplier(t, store, NewUpdaterClient(serveUpdater(t, up)))

	if !self.Adopt(context.Background(), open, testCommit) {
		t.Fatal("Adopt reported the attempt unresolved")
	}
	a := store.snapshot("cp-1")
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonNeverStarted {
		t.Fatalf("state=%q reason=%v, want failed/never_started", a.State, a.Reason)
	}
	if len(a.PreviousDigests) != 1 || a.PreviousDigests[0].Digest == nil {
		t.Fatalf("previous digests = %+v, want the restore recipe", a.PreviousDigests)
	}
}

// An attempt the restart caught before it was sent carries no request id; the
// run re-drives it rather than adopting a poll with nothing to poll.
func TestAdoptLeavesAnUnsentAttemptToTheRun(t *testing.T) {
	open := controlPlaneAttempt(AttemptQueued)
	store := newFakeStore(open)
	self := testSelfApplier(t, store, &fakeUpdater{})

	if self.Adopt(context.Background(), open, testCommit) {
		t.Fatal("Adopt claimed an unsent attempt")
	}
	if got := store.snapshot("cp-1").State; got != AttemptQueued {
		t.Fatalf("state = %q, want it left queued", got)
	}
}

// A source-built control plane must never be offered the registry image: the
// two are different builds with different uids, and the swap leaves a container
// that starts and then cannot write its own TLS volume.
func TestClassifyImageRefMatchesTheAgentsRule(t *testing.T) {
	cases := []struct{ ref, want string }{
		{"ghcr.io/accreleus/quasar/quasar-control-plane:v0.9.0", InstallRegistry},
		{"ghcr.io/accreleus/quasar/quasar-control-plane@sha256:" + hex64, InstallRegistry},
		{"localhost:5000/quasar-control-plane:latest", InstallRegistry},
		{"registry.example.com/quasar/quasar-control-plane:latest", InstallRegistry},
		{"myorg/quasar-control-plane:latest", InstallSource},
		{"quasar-control-plane:latest", InstallSource},
		{"quasar-control-plane", InstallSource},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := ClassifyImageRef(c.ref); got != c.want {
			t.Errorf("ClassifyImageRef(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}

func TestInstallModeComesFromTheUpdater(t *testing.T) {
	tests := []struct {
		name  string
		self  *UpdaterSelf
		want  string
		known bool
	}{
		{"registry", &UpdaterSelf{Images: map[string]string{
			ComponentControlPlane: "ghcr.io/accreleus/quasar/quasar-control-plane:v1"}}, InstallRegistry, true},
		{"source", &UpdaterSelf{Images: map[string]string{
			ComponentControlPlane: "quasar-control-plane:latest"}}, InstallSource, true},
		// An updater that predates the `images` object says nothing, and
		// nothing is never read as registry.
		{"older updater", &UpdaterSelf{}, "", false},
		{"unreachable", nil, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeUpdater{self: tc.self}
			self := testSelfApplier(t, newFakeStore(controlPlaneAttempt(AttemptQueued)), up)
			got := self.InstallMode()
			if !tc.known {
				if got != nil {
					t.Fatalf("install mode = %q, want unknown", *got)
				}
				return
			}
			if got == nil || *got != tc.want {
				t.Fatalf("install mode = %v, want %q", got, tc.want)
			}
			// Read once and reused: the answer is cached for its TTL.
			_ = self.InstallMode()
			if up.selfReads != 1 {
				t.Fatalf("the updater was read %d times, want the answer cached", up.selfReads)
			}
		})
	}
}

// fakeUpdater doubles as an UpdaterAPI for the cases that need no socket.
func (u *fakeUpdater) Present() bool { return true }

func (u *fakeUpdater) Self(context.Context) (UpdaterSelf, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.selfReads++
	if u.self == nil {
		return UpdaterSelf{}, ErrNoResult
	}
	return *u.self, nil
}

func (u *fakeUpdater) Apply(context.Context, updater.ApplyRequest) (updater.Accepted, error) {
	return updater.Accepted{}, nil
}

func (u *fakeUpdater) Result(_ context.Context, _ string) (updater.Result, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.result == nil {
		return updater.Result{}, ErrNoResult
	}
	return *u.result, nil
}
