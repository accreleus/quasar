package agentws

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// #415 — Unregister used to delete by session id alone, so a stale
// connection's deferred cleanup could evict a newer reconnect's channel.
// Register chA then chB for the same session, Unregister the STALE chA
// (mirroring WS#1's deferred cleanup firing after WS#2 has already taken
// over), Deliver, and assert chB — not chA — receives. Before the #415 fix
// this failed: Unregister(sessionID) deleted chB's registration regardless of
// which channel called it, so the frame landed back in the pending buffer
// (or was lost to whichever channel happened to still be registered) instead
// of reaching the live browser.
func TestRelayUnregisterIsIdentityAware(t *testing.T) {
	bus := NewRelayBus(slog.New(slog.NewTextHandler(io.Discard, nil)))
	chA := make(chan []byte, 1)
	chB := make(chan []byte, 1)

	bus.Register("s1", chA)
	bus.Register("s1", chB) // reconnect: chB supersedes chA

	// Stale connection's deferred cleanup — must be a no-op, not evict chB.
	bus.Unregister("s1", chA)

	want := []byte("ice-candidate")
	if err := bus.Deliver("s1", want); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	select {
	case got := <-chB:
		if string(got) != string(want) {
			t.Fatalf("chB got %q want %q", got, want)
		}
	default:
		t.Fatalf("chB (the live registration) received nothing — frame was lost or misrouted")
	}
	select {
	case got := <-chA:
		t.Fatalf("chA (displaced) unexpectedly received %q", got)
	default:
	}
}

// The mirror case: Unregister the CURRENT registration and confirm the
// session goes back to "no browser" (frames buffer in pending, matching
// Forget/never-registered behaviour), rather than deleting nothing because
// the check landed on the wrong entry.
func TestRelayUnregisterCurrentRemovesIt(t *testing.T) {
	bus := NewRelayBus(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ch := make(chan []byte, 1)
	bus.Register("s1", ch)
	bus.Unregister("s1", ch)

	if err := bus.Deliver("s1", []byte("buffered")); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	select {
	case got := <-ch:
		t.Fatalf("unregistered channel unexpectedly received %q", got)
	default:
	}
}

// #415 — the displaced signal Register returns must close when a later
// Register call for the same session supersedes it, so the superseded
// connection can tear itself down immediately instead of staying deaf to
// agent frames until its read deadline expires (up to browserReadTimeout).
func TestRelayRegisterSignalsDisplacement(t *testing.T) {
	bus := NewRelayBus(slog.New(slog.NewTextHandler(io.Discard, nil)))
	chA := make(chan []byte, 1)
	chB := make(chan []byte, 1)

	displacedA := bus.Register("s1", chA).Displaced
	select {
	case <-displacedA:
		t.Fatalf("displacedA closed before any reconnect")
	default:
	}

	bus.Register("s1", chB)

	select {
	case <-displacedA:
		// expected — chA was superseded
	case <-time.After(time.Second):
		t.Fatalf("displacedA was not closed after chB superseded it")
	}
}
