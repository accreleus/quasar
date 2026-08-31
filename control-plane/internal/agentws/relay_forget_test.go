package agentws

import (
	"io"
	"log/slog"
	"testing"
)

func relayTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestRelayBusForgetEvictsPending: a session whose browser never attached
// buffers frames under pending[sessionID]; Forget (the session-terminal hook,
// #402) must drop them. Before Forget existed the only eviction paths were
// Register/Unregister, both driven by a browser WebSocket that in a headless
// session never exists — so every buffered frame was retained for the life of
// the process.
func TestRelayBusForgetEvictsPending(t *testing.T) {
	b := NewRelayBus(relayTestLogger())

	for i := 0; i < 8; i++ {
		if err := b.Deliver("sess-1", []byte(`{"type":"offer"}`)); err != nil {
			t.Fatalf("deliver: %v", err)
		}
	}
	if err := b.Deliver("sess-2", []byte(`{"type":"offer"}`)); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	b.mu.Lock()
	n := len(b.pending["sess-1"])
	b.mu.Unlock()
	if n != 8 {
		t.Fatalf("pending frames for sess-1 = %d, want 8", n)
	}

	b.Forget("sess-1")

	b.mu.Lock()
	_, still := b.pending["sess-1"]
	other := len(b.pending["sess-2"])
	b.mu.Unlock()
	if still {
		t.Fatalf("pending entry for sess-1 survived Forget")
	}
	if other != 1 {
		t.Fatalf("Forget touched an unrelated session: sess-2 has %d frames, want 1", other)
	}
}

// TestRelayBusForgetUnknownSession: Forget on a session that never buffered
// anything is a no-op (the coordinator fires it on EVERY terminal transition,
// including the overwhelming majority that never relayed a frame).
func TestRelayBusForgetUnknownSession(t *testing.T) {
	b := NewRelayBus(relayTestLogger())
	b.Forget("nope")
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) != 0 || len(b.browsers) != 0 {
		t.Fatalf("Forget on an unknown session created state: pending=%d browsers=%d",
			len(b.pending), len(b.browsers))
	}
}
