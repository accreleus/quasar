package agentws

import (
	"io"
	"log/slog"
	"testing"
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
