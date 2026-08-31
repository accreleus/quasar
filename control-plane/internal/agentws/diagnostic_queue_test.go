package agentws

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type blockingDiagnosticEvents struct {
	noopEvents
	gate    chan struct{}
	metrics atomic.Uint64
	traces  atomic.Uint64
}

func (e *blockingDiagnosticEvents) AgentMetrics(context.Context, string, SessionMetricsMsg) {
	<-e.gate
	e.metrics.Add(1)
}

func (e *blockingDiagnosticEvents) AgentTraceEvent(context.Context, string, SessionTraceEventMsg) {
	<-e.gate
	e.traces.Add(1)
}

// TestDiagnosticQueueCloseStopsDrain: close() must stop the drain goroutine and
// be idempotent, mirroring vramQueue.close (#401). The package-level goleak
// guard (goleak_test.go) is the real assertion — this pins the contract close()
// is expected to honour: it returns only once run() has exited.
func TestDiagnosticQueueCloseStopsDrain(t *testing.T) {
	q := newDiagnosticQueue(noopEvents{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m := SessionMetricsMsg{SessionID: "s"}
	q.enqueue(diagnosticEvent{hostID: "host", metric: &m})

	done := make(chan struct{})
	go func() {
		q.close()
		q.close() // idempotent
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("close() did not return: the drain goroutine never exited")
	}
	select {
	case <-q.done:
	default:
		t.Fatal("close() returned before run() exited")
	}
}

func TestDiagnosticQueueBoundedAndCoalescing(t *testing.T) {
	events := &blockingDiagnosticEvents{gate: make(chan struct{})}
	q := newDiagnosticQueue(events, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(q.close) // #401
	for i := 0; i < 10_000; i++ {
		m := SessionMetricsMsg{SessionID: "same-session", TsUnixMs: int64(i)}
		q.enqueue(diagnosticEvent{hostID: "host", metric: &m})
	}
	for i := 0; i < diagnosticQueueMax*2; i++ {
		m := SessionMetricsMsg{SessionID: "session-" + strconv.Itoa(i)}
		q.enqueue(diagnosticEvent{hostID: "host", metric: &m})
	}
	pending, dropped, coalesced, _ := q.stats()
	if pending > diagnosticQueueMax {
		t.Fatalf("pending=%d exceeds cap=%d", pending, diagnosticQueueMax)
	}
	if dropped == 0 || coalesced == 0 {
		t.Fatalf("dropped=%d coalesced=%d, want both observable", dropped, coalesced)
	}
	close(events.gate)
	deadline := time.Now().Add(3 * time.Second)
	for {
		pending, _, _, processed := q.stats()
		if pending == 0 && processed > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue did not drain: pending=%d processed=%d", pending, processed)
		}
		time.Sleep(time.Millisecond)
	}
}
