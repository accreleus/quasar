package agentws

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

const diagnosticQueueMax = 256

type diagnosticEvent struct {
	hostID string
	metric *SessionMetricsMsg
	trace  *SessionTraceEventMsg
}

func (e diagnosticEvent) key() string {
	if e.metric != nil {
		return "metric:" + e.hostID + ":" + e.metric.SessionID
	}
	return fmt.Sprintf("trace:%s:%s:%s", e.hostID, e.trace.SessionID, e.trace.Event)
}

// diagnosticQueue removes telemetry persistence from the agent WebSocket reader.
// It is bounded and latest-value coalescing so slow storage cannot block lifecycle.
type diagnosticQueue struct {
	events Events
	log    *slog.Logger
	wake   chan struct{}

	mu      sync.Mutex
	pending map[string]diagnosticEvent

	dropped   atomic.Uint64
	coalesced atomic.Uint64
	processed atomic.Uint64

	done      chan struct{}
	closeOnce sync.Once
}

func newDiagnosticQueue(events Events, log *slog.Logger) *diagnosticQueue {
	q := &diagnosticQueue{
		events:  events,
		log:     log,
		wake:    make(chan struct{}, 1),
		pending: make(map[string]diagnosticEvent),
		done:    make(chan struct{}),
	}
	go q.run()
	return q
}

func (q *diagnosticQueue) enqueue(e diagnosticEvent) {
	key := e.key()
	q.mu.Lock()
	if _, ok := q.pending[key]; ok {
		q.pending[key] = e
		n := q.coalesced.Add(1)
		q.mu.Unlock()
		q.logAtPowerOfTwo("diagnostic queue coalesced", n)
		return
	}
	if len(q.pending) >= diagnosticQueueMax {
		n := q.dropped.Add(1)
		q.mu.Unlock()
		q.logAtPowerOfTwo("diagnostic queue dropped", n)
		return
	}
	q.pending[key] = e
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *diagnosticQueue) run() {
	defer close(q.done)
	for range q.wake {
		for {
			var e diagnosticEvent
			q.mu.Lock()
			for key, candidate := range q.pending {
				e = candidate
				delete(q.pending, key)
				break
			}
			empty := e.metric == nil && e.trace == nil
			q.mu.Unlock()
			if empty {
				break
			}
			if e.metric != nil {
				q.events.AgentMetrics(context.Background(), e.hostID, *e.metric)
			} else {
				q.events.AgentTraceEvent(context.Background(), e.hostID, *e.trace)
			}
			q.processed.Add(1)
		}
	}
}

// close stops the drain goroutine and waits for the in-flight event (mirrors
// vramQueue.close). Pending events are discarded — point-in-time telemetry.
// Without this the goroutine leaks once per Handler, which would force a
// package-wide goleak ignore that masks every future regression.
func (q *diagnosticQueue) close() {
	q.closeOnce.Do(func() {
		close(q.wake)
		<-q.done
	})
}

func (q *diagnosticQueue) logAtPowerOfTwo(message string, n uint64) {
	if n != 0 && n&(n-1) == 0 {
		q.log.Warn(message, "count", n, "capacity", diagnosticQueueMax)
	}
}

func (q *diagnosticQueue) stats() (pending int, dropped, coalesced, processed uint64) {
	q.mu.Lock()
	pending = len(q.pending)
	q.mu.Unlock()
	return pending, q.dropped.Load(), q.coalesced.Load(), q.processed.Load()
}
