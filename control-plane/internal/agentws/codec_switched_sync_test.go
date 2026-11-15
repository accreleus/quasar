package agentws

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
)

type recordingTraceEvents struct {
	noopEvents
	mu     sync.Mutex
	events []string
}

func (r *recordingTraceEvents) AgentTraceEvent(_ context.Context, _ string, m SessionTraceEventMsg) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, m.Event+" "+string(m.Payload))
}

func (r *recordingTraceEvents) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// #1324: codec.switched moves Session.pyrowave, so it is never coalesced or dropped. A
// blocked diagnostic queue (the state a slow store leaves it in) must not hold it back,
// and an "off" must not replace the "on" that set `used`.
func TestCodecSwitchedBypassesTheDiagnosticQueue(t *testing.T) {
	pool := testPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	events := &recordingTraceEvents{}
	h := NewHandler(pool, log, nil, events, nil, hostcfg.NewStore(pool), nil)
	t.Cleanup(h.Close)
	h.diagnostics.close()
	blocked := &blockingDiagnosticEvents{gate: make(chan struct{})}
	h.diagnostics = newDiagnosticQueue(blocked, log)
	t.Cleanup(func() { close(blocked.gate) })

	agent, _ := connectTypedAgent(t, pool, h, hostcfg.NextSessionPolicyGroups(), nil)
	const session = "aaaaaaaa-0000-0000-0000-000000000001"
	for _, to := range []string{"pyrowave", "h264"} {
		agent.send(t, map[string]any{
			"type": "session_trace_event", "session_id": session, "ts_unix_ms": 1,
			"event": "codec.switched", "payload": map[string]any{"to": to, "reason": "launch"},
		})
	}
	waitUntil(t, "both switches delivered while the queue is blocked", func() bool { return len(events.seen()) == 2 })
	got := events.seen()
	if !strings.Contains(got[0], `"to":"pyrowave"`) || !strings.Contains(got[1], `"to":"h264"`) {
		t.Fatalf("switches arrived out of order or merged: %v", got)
	}
}
