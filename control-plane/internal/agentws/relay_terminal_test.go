package agentws

import (
	"testing"
	"time"
)

// #93 — Forget must TELL the attached socket, not just delete it.
//
// Forget is the coordinator's terminal hook (#402): after it runs the bus can
// never deliver another agent frame for that session. It used to drop the
// registration silently, leaving a live, deaf browser socket that the control
// plane went on pinging; the client learned of the teardown only when its own
// transport died, which reads as a close with no closing handshake.
func TestRelayForgetSignalsTerminal(t *testing.T) {
	b := NewRelayBus(relayTestLogger())
	ch := make(chan []byte, 1)
	signals := b.Register("s1", ch)

	select {
	case <-signals.Terminal:
		t.Fatal("Terminal closed before the session ended")
	default:
	}

	b.Forget("s1")

	select {
	case <-signals.Terminal:
	case <-time.After(time.Second):
		t.Fatal("Terminal was not closed by Forget")
	}
	select {
	case <-signals.Displaced:
		t.Fatal("Forget closed Displaced too; the socket cannot then tell a takeover from a teardown")
	default:
	}
}

// A socket leaving on its own must not be told the session ended — and a second
// Forget must not double-close the channel (the coordinator fires it on every
// terminal transition, and a session can reach one by more than one route).
func TestRelayUnregisterDoesNotSignalTerminal(t *testing.T) {
	b := NewRelayBus(relayTestLogger())
	ch := make(chan []byte, 1)
	signals := b.Register("s1", ch)

	b.Unregister("s1", ch)
	select {
	case <-signals.Terminal:
		t.Fatal("Unregister signalled Terminal; only a terminal session may")
	default:
	}

	b.Forget("s1")
	b.Forget("s1")
	select {
	case <-signals.Terminal:
		t.Fatal("Forget signalled a registration Unregister had already dropped")
	default:
	}
}

// A displaced registration is superseded, not ended: closing Terminal for it as
// well would tell the loser the session is over while it is still running for
// the winner.
func TestRelayForgetSignalsOnlyTheCurrentRegistration(t *testing.T) {
	b := NewRelayBus(relayTestLogger())
	chA := make(chan []byte, 1)
	chB := make(chan []byte, 1)
	a := b.Register("s1", chA)
	bb := b.Register("s1", chB)

	b.Forget("s1")

	select {
	case <-a.Terminal:
		t.Fatal("Forget signalled Terminal on the displaced registration")
	default:
	}
	select {
	case <-bb.Terminal:
	case <-time.After(time.Second):
		t.Fatal("Terminal was not closed on the current registration")
	}
}
