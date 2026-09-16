package agentws

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRemoveReportsCurrent: remove() reports whether the connection was still the
// registered one. This gates the host-disconnect reaper (P2-06): a connection
// displaced by a reconnect must NOT trigger a reap, or a stale teardown would
// fail the reconnect's live sessions.
func TestRemoveReportsCurrent(t *testing.T) {
	r := NewRegistry(quietLogger())

	// A single connection: removing it reports current=true (its teardown should
	// reap the host's sessions).
	c1 := newConn("host-1", nil)
	r.add(c1)
	if got := r.remove(c1); !got {
		t.Fatal("remove of the sole connection: got false, want true (current)")
	}

	// Reconnect: c2 displaces c1. c1's later teardown must report current=false
	// (it was displaced) so it does NOT reap c2's sessions; c2 reports true.
	c2 := newConn("host-1", nil)
	c3 := newConn("host-1", nil)
	r.add(c2)
	r.add(c3) // c3 displaces c2
	if got := r.remove(c2); got {
		t.Fatal("remove of a displaced connection: got true, want false")
	}
	if got := r.remove(c3); !got {
		t.Fatal("remove of the current connection: got false, want true")
	}
}

func waitFor(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitDone(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitLifecycleUsers observes that a caller has acquired a reference before it
// can wait on the per-host mutex. It makes queueing assertions deterministic
// without adding a production-only test hook.
func waitLifecycleUsers(t *testing.T, r *Registry, hostID string, want int) {
	t.Helper()
	waitFor(t, "lifecycle waiter", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		gate := r.lifecycles[hostID]
		return gate != nil && gate.users >= want
	})
}

func TestLifecycleGateSerializesReplacementRemovalAndDisplacedCallbacks(t *testing.T) {
	r := NewRegistry(quietLogger())
	one := newConn("host-1", nil)
	two := newConn("host-1", nil)
	three := newConn("host-1", nil)
	other := newConn("host-2", nil)
	r.add(one)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() { r.withCurrent(one, func() { close(entered); <-release }); close(done) }()
	waitDone(t, "first callback", entered)
	replaced := make(chan struct{})
	go func() { r.add(two); close(replaced) }()
	waitLifecycleUsers(t, r, "host-1", 2)
	select {
	case <-replaced:
		t.Fatal("replacement bypassed active host lifecycle callback")
	default:
	}
	// A different host never waits behind host-1's lifecycle work.
	otherDone := make(chan struct{})
	go func() { r.add(other); close(otherDone) }()
	waitDone(t, "unrelated host registration", otherDone)
	close(release)
	waitDone(t, "first callback completion", done)
	waitDone(t, "replacement", replaced)
	if r.withCurrent(one, func() { t.Fatal("displaced callback ran") }) {
		t.Fatal("displaced connection accepted")
	}
	if !r.withCurrent(two, func() {
		if err := r.Send("host-1", map[string]string{"type": "test"}); err != nil {
			t.Fatalf("callback Send: %v", err)
		}
	}) {
		t.Fatal("replacement rejected")
	}

	// Hold a current callback while removal queues. Once removal has become the
	// current lifecycle operation, it holds the same gate while its disconnect
	// callback runs; the third registration must wait there. The reference count
	// prevents remove from deleting the gate between the queued operations.
	active := make(chan struct{})
	releaseActive := make(chan struct{})
	activeDone := make(chan struct{})
	go func() { r.withCurrent(two, func() { close(active); <-releaseActive }); close(activeDone) }()
	waitDone(t, "second callback", active)

	removed := make(chan struct{})
	removalEntered := make(chan struct{})
	releaseRemoval := make(chan struct{})
	disconnected := 0
	go func() {
		r.removeWithLifecycle(two, func() {
			disconnected++
			close(removalEntered)
			<-releaseRemoval
		})
		close(removed)
	}()
	waitLifecycleUsers(t, r, "host-1", 2)
	close(releaseActive)
	waitDone(t, "second callback completion", activeDone)
	waitDone(t, "disconnect callback", removalEntered)

	third := make(chan struct{})
	go func() { r.add(three); close(third) }()
	waitLifecycleUsers(t, r, "host-1", 2)
	select {
	case <-third:
		t.Fatal("third registration bypassed active disconnect callback")
	default:
	}
	close(releaseRemoval)
	waitDone(t, "current removal", removed)
	waitDone(t, "third registration", third)
	if disconnected != 1 {
		t.Fatalf("HostDisconnected callbacks = %d, want 1", disconnected)
	}
	if r.withCurrent(two, func() { t.Fatal("removed callback ran") }) {
		t.Fatal("removed connection accepted")
	}
	if !r.withCurrent(three, func() {}) {
		t.Fatal("third registration rejected after queued removal")
	}
}
