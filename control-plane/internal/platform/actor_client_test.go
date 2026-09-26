package platform

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
	"github.com/accreleus/quasar/control-plane/internal/updater"
)

func updaterResult(reason string) updater.Result {
	return updater.Result{State: updater.StateFailed, Reason: &reason, Restored: true, Output: "container exited"}
}

// The owned control plane's step, over a REAL unix socket speaking the control
// socket's shapes (testdata/recovery/socket), to a fake recovery actor.

type fakeActor struct {
	mu       sync.Mutex
	accepted []actorsocket.Request
	reject   *actorsocket.Rejection
	result   *actorsocket.Result
	// silent answers every status read 503: an actor that is not answering.
	silent bool
}

func (a *fakeActor) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/submit", func(w http.ResponseWriter, r *http.Request) {
		var req actorsocket.Request
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.reject != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(a.reject)
			return
		}
		a.accepted = append(a.accepted, req)
		w.WriteHeader(http.StatusAccepted)
		old := "sha256:old"
		_ = json.NewEncoder(w).Encode(actorsocket.Accepted{RequestID: req.RequestID, Previous: []actorsocket.Previous{
			{Name: ComponentRecovery, Digest: &old}, {Name: ComponentControlPlane, Digest: &old},
		}})
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.silent {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		st := actorsocket.Status{Role: actorsocket.RoleCombined, Database: actorsocket.DatabaseOwned}
		if a.result != nil && a.result.RequestID == r.URL.Query().Get("request_id") {
			res := *a.result
			st.Result = &res
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	return mux
}

func (a *fakeActor) set(f func(*fakeActor)) {
	a.mu.Lock()
	f(a)
	a.mu.Unlock()
}

func serveActor(t *testing.T, a *fakeActor) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: a.handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func actorResult(requestID string, state actorsocket.State, reason actorsocket.Reason, restored bool) *actorsocket.Result {
	r := &actorsocket.Result{RequestID: requestID, State: state, Restored: restored, Output: "the previous container was put back and is running"}
	if reason != "" {
		r.Reason = &reason
	}
	return r
}

func ownedAttempt(state string) Attempt {
	a := controlPlaneAttempt(state)
	a.RequestedDigests = []ComponentDigest{
		{Name: ComponentRecovery, Image: "ghcr.io/x/quasar-recovery", Digest: "sha256:actor"},
		{Name: ComponentControlPlane, Image: "ghcr.io/x/quasar-control-plane", Digest: "sha256:new"},
	}
	return a
}

// The request the control plane sends for its own step is exactly the shared
// fixture the Rust actor decodes.
func TestTheControlPlanesStepIsTheSharedFixtureRequest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "recovery", "socket", "request-replace-control-plane-actor-first.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Body any `json:"body"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	version := "0.4.0"
	req := ActorRequest(SelfRequest{
		RequestID: "3c8e2a61-5b7d-4e19-a0f4-6d2b9c1e7f30",
		Components: []ComponentDigest{
			{Name: ComponentRecovery, Image: "ghcr.io/accreleus/quasar/quasar-recovery", Digest: "sha256:cc33000000000000000000000000000000000000000000000000000000000000"},
			{Name: ComponentControlPlane, Image: "ghcr.io/accreleus/quasar/quasar-control-plane", Digest: "sha256:aa11000000000000000000000000000000000000000000000000000000000000"},
		},
		Release:       ReleaseRef{ID: "8f0c7d52-1e3a-4b6c-9d2f-5a7e1b3c9d04", Version: &version, SourceCommit: "cccccccccccccccccccccccccccccccccccccccc"},
		SchemaVersion: 96,
	})
	encoded, _ := json.Marshal(req)
	var got any
	_ = json.Unmarshal(encoded, &got)
	if !reflect.DeepEqual(got, fixture.Body) {
		t.Fatalf("sent %s\nfixture %v", encoded, fixture.Body)
	}
}

func TestAnOwnedSelfApplySubmitsInOrderAndRelaysTheActorsRestore(t *testing.T) {
	store := newFakeStore(ownedAttempt(AttemptQueued))
	actor := &fakeActor{}
	self := testSelfApplier(t, store, NewActorClient(serveActor(t, actor)))
	self.Identity = func() buildinfo.Identity { return buildinfo.Identity{Version: "dev", SchemaVersion: 75} }

	done := make(chan struct{})
	go func() {
		defer close(done)
		self.Apply(context.Background(), ownedAttempt(AttemptQueued))
	}()
	waitFor(t, "the submit", func() bool {
		actor.mu.Lock()
		defer actor.mu.Unlock()
		return len(actor.accepted) == 1
	})
	actor.mu.Lock()
	sent := actor.accepted[0]
	actor.mu.Unlock()
	if sent.Kind != actorsocket.KindReplace || len(sent.Components) != 2 ||
		sent.Components[0].Name != ComponentRecovery || sent.Components[1].Name != ComponentControlPlane {
		t.Fatalf("sent %+v, want [recovery-actor, control-plane]", sent)
	}
	if sent.Migrates {
		t.Fatal("a release at this binary's schema was sent as migrating")
	}
	actor.set(func(a *fakeActor) {
		a.result = actorResult(sent.RequestID, actorsocket.StateFailed, actorsocket.ReasonUnhealthy, true)
	})
	<-done
	got := store.snapshot("cp-1")
	if got.State != AttemptFailed || got.Reason == nil || *got.Reason != ReasonUnhealthy {
		t.Fatalf("state=%q reason=%v, want failed/unhealthy", got.State, got.Reason)
	}
	if got.Output == "" || len(got.PreviousDigests) != 2 {
		t.Fatalf("output=%q previous=%+v, want the actor's account and both previous digests", got.Output, got.PreviousDigests)
	}
}

func TestAnOwnedSelfApplyRecordsTheActorsRefusal(t *testing.T) {
	store := newFakeStore(ownedAttempt(AttemptQueued))
	actor := &fakeActor{reject: &actorsocket.Rejection{Reason: actorsocket.ReasonInvalid, Message: "arrives with RH06-12"}}
	self := testSelfApplier(t, store, NewActorClient(serveActor(t, actor)))

	self.Apply(context.Background(), ownedAttempt(AttemptQueued))

	a := store.snapshot("cp-1")
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonInvalid || a.Output != "arrives with RH06-12" {
		t.Fatalf("state=%q reason=%v output=%q, want failed/invalid with the actor's message", a.State, a.Reason, a.Output)
	}
}

func TestAnOwnedSelfApplyWithNoControlSocketNamesIt(t *testing.T) {
	store := newFakeStore(ownedAttempt(AttemptQueued))
	sock := filepath.Join(t.TempDir(), "absent.sock")
	self := testSelfApplier(t, store, NewActorClient(sock))

	self.Apply(context.Background(), ownedAttempt(AttemptQueued))

	a := store.snapshot("cp-1")
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonUpdaterAbsentFailure {
		t.Fatalf("state=%q reason=%v, want failed/updater_absent", a.State, a.Reason)
	}
}

// On an owned machine the booted binary is the evidence only once the actor
// has verified it: a control plane that boots, then never passes its health
// check, is put back, and the row must say failed, not succeeded.
func TestOwnedAdoptWaitsForTheActorsVerdictBeforeTrustingTheBootedBinary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		final *actorsocket.Result
		want  string
	}{
		{"verified", nil, AttemptSucceeded},
		{"restored", &actorsocket.Result{}, AttemptFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			open := ownedAttempt(AttemptVerifying)
			store := newFakeStore(open)
			id, err := store.MintRequestID(context.Background(), "cp-1")
			if err != nil {
				t.Fatal(err)
			}
			actor := &fakeActor{result: actorResult(id, actorsocket.StateVerifying, "", false)}
			self := testSelfApplier(t, store, NewActorClient(serveActor(t, actor)))
			self.Identity = func() buildinfo.Identity {
				return buildinfo.Identity{Version: "0.9.0", SourceCommit: strPtr(testCommit)}
			}
			resolved := make(chan bool, 1)
			go func() { resolved <- self.Adopt(context.Background(), open, testCommit) }()

			time.Sleep(30 * time.Millisecond)
			if got := store.snapshot("cp-1").State; got != AttemptVerifying {
				t.Fatalf("state = %q while the actor is still verifying, want it left open", got)
			}
			actor.set(func(a *fakeActor) {
				if tc.final == nil {
					a.result = actorResult(id, actorsocket.StateSucceeded, "", false)
				} else {
					a.result = actorResult(id, actorsocket.StateFailed, actorsocket.ReasonUnhealthy, true)
				}
			})
			if !<-resolved {
				t.Fatal("Adopt reported the attempt unresolved")
			}
			if got := store.snapshot("cp-1").State; got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

// adoptingOwned is a booted control plane on the release's commit adopting an
// owned attempt the actor is verifying, with compressed clocks.
func adoptingOwned(t *testing.T, actor *fakeActor) (*fakeStore, *SelfApplier, Attempt, string) {
	t.Helper()
	open := ownedAttempt(AttemptVerifying)
	store := newFakeStore(open)
	id, err := store.MintRequestID(context.Background(), "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	self := testSelfApplier(t, store, NewActorClient(serveActor(t, actor)))
	self.Deadline = 30 * time.Millisecond
	self.VerdictSilence = 30 * time.Millisecond
	self.Identity = func() buildinfo.Identity {
		return buildinfo.Identity{Version: "0.9.0", SourceCommit: strPtr(testCommit)}
	}
	return store, self, open, id
}

// An actor that never answers for the request does not strand the row, and
// never makes an unverified build a success: past the deadline it times out.
func TestOwnedAdoptTimesOutFailClosedWhenTheActorIsSilent(t *testing.T) {
	store, self, open, _ := adoptingOwned(t, &fakeActor{silent: true})
	if !self.Adopt(context.Background(), open, testCommit) {
		t.Fatal("Adopt reported the attempt unresolved")
	}
	a := store.snapshot("cp-1")
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonTimeout {
		t.Fatalf("state=%q reason=%v, want failed/timeout", a.State, a.Reason)
	}
}

// No request id means no verdict to wait for: the booted binary is never taken
// as success on its own, and the caller re-drives the attempt.
func TestOwnedAdoptWithNoRequestIDDecidesNothing(t *testing.T) {
	open := ownedAttempt(AttemptQueued)
	store := newFakeStore(open)
	self := testSelfApplier(t, store, NewActorClient(serveActor(t, &fakeActor{})))
	self.Identity = func() buildinfo.Identity {
		return buildinfo.Identity{Version: "0.9.0", SourceCommit: strPtr(testCommit)}
	}
	if self.Adopt(context.Background(), open, testCommit) {
		t.Fatal("Adopt resolved an owned attempt with no request id")
	}
	if got := store.snapshot("cp-1").State; got != AttemptQueued {
		t.Fatalf("state = %q, want it left for the re-drive", got)
	}
}

// A slow link: the actor is still verifying past the apply deadline. It keeps
// answering, so it is waited for, and its verdict decides.
func TestOwnedAdoptWaitsPastTheDeadlineWhileTheActorIsStillVerifying(t *testing.T) {
	actor := &fakeActor{}
	store, self, open, id := adoptingOwned(t, actor)
	actor.set(func(a *fakeActor) { a.result = actorResult(id, actorsocket.StateVerifying, "", false) })
	resolved := make(chan bool, 1)
	go func() { resolved <- self.Adopt(context.Background(), open, testCommit) }()

	time.Sleep(200 * time.Millisecond) // well past Deadline + VerdictSilence
	if got := store.snapshot("cp-1").State; got != AttemptVerifying {
		t.Fatalf("state = %q while the actor still answers verifying, want it left open", got)
	}
	actor.set(func(a *fakeActor) {
		a.result = actorResult(id, actorsocket.StateFailed, actorsocket.ReasonUnhealthy, true)
	})
	if !<-resolved {
		t.Fatal("Adopt reported the attempt unresolved")
	}
	if a := store.snapshot("cp-1"); a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonUnhealthy {
		t.Fatalf("state=%q reason=%v, want the actor's failed/unhealthy", a.State, a.Reason)
	}
}

// The actor stops this control plane to put the old one back before any
// verdict exists: shutting down writes nothing, so the restored control plane
// records the actor's verdict on its next boot.
func TestOwnedAdoptShuttingDownLeavesTheRowForTheNextBoot(t *testing.T) {
	actor := &fakeActor{}
	store, self, open, id := adoptingOwned(t, actor)
	self.Deadline = time.Hour
	actor.set(func(a *fakeActor) { a.result = actorResult(id, actorsocket.StateVerifying, "", false) })
	ctx, cancel := context.WithCancel(context.Background())
	resolved := make(chan bool, 1)
	go func() { resolved <- self.Adopt(ctx, open, testCommit) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	if !<-resolved {
		t.Fatal("Adopt asked to be re-driven while shutting down")
	}
	if got := store.snapshot("cp-1").State; got != AttemptVerifying {
		t.Fatalf("state = %q after shutdown, want verifying left for the next boot", got)
	}

	// The restored old control plane boots on the previous commit and reads it.
	actor.set(func(a *fakeActor) {
		a.result = actorResult(id, actorsocket.StateFailed, actorsocket.ReasonUnhealthy, true)
	})
	if !self.Adopt(context.Background(), open, testCommit+"-no") {
		t.Fatal("the restored control plane's Adopt reported the attempt unresolved")
	}
	a := store.snapshot("cp-1")
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonUnhealthy || a.Output == "" {
		t.Fatalf("state=%q reason=%v output=%q, want the actor's verdict", a.State, a.Reason, a.Output)
	}
}

// A restored control plane that boots after the apply deadline still records the
// verdict already there, rather than timing out on its first look.
func TestAdoptPastTheDeadlineReadsAnAlreadyTerminalResult(t *testing.T) {
	open := controlPlaneAttempt(AttemptRecreating)
	open.CreatedAt = time.Now().Add(-time.Hour)
	store := newFakeStore(open)
	if _, err := store.MintRequestID(context.Background(), "cp-1"); err != nil {
		t.Fatal(err)
	}
	up := &fakeUpdater{}
	reason := ReasonNeverStarted
	up.setResult(updaterResult(reason))
	self := testSelfApplier(t, store, NewUpdaterClient(serveUpdater(t, up)))
	if !self.Adopt(context.Background(), open, testCommit) {
		t.Fatal("Adopt reported the attempt unresolved")
	}
	if a := store.snapshot("cp-1"); a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonNeverStarted {
		t.Fatalf("state=%q reason=%v, want failed/never_started, not a timeout", a.State, a.Reason)
	}
}
