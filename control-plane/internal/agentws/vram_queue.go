package agentws

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// vramQueueMax bounds the pending batches. One batch per host, so the cap is a
// fleet-size backstop, not a rate limit — coalescing (below) already collapses a
// host's heartbeat burst to its newest sample.
const vramQueueMax = 256

// vramSampleBatch is one heartbeat's worth of live VRAM telemetry for one host.
type vramSampleBatch struct {
	hostID  string
	agentMs int64
	samples []GPUVramSample
}

// vramQueue keeps live-VRAM persistence off the agent WebSocket reader
// (#383 §3.3): the sample UPDATE contends with the scheduler's `FOR UPDATE OF
// h, g` row lock, and a stall past the read deadline would mark the host
// offline and reap its sessions — optional telemetry must never do that.
// Same shape as diagnosticQueue: bounded, latest-value coalescing per host,
// one drain goroutine; the store's monotonic guard makes out-of-order
// delivery harmless.
type vramQueue struct {
	store *agentStore
	log   *slog.Logger
	wake  chan struct{}

	mu      sync.Mutex
	pending map[string]vramSampleBatch

	dropped   atomic.Uint64
	coalesced atomic.Uint64
	processed atomic.Uint64

	done      chan struct{}
	closeOnce sync.Once
}

func newVramQueue(store *agentStore, log *slog.Logger) *vramQueue {
	q := &vramQueue{
		store:   store,
		log:     log,
		wake:    make(chan struct{}, 1),
		pending: make(map[string]vramSampleBatch),
		done:    make(chan struct{}),
	}
	go q.run()
	return q
}

func (q *vramQueue) enqueue(b vramSampleBatch) {
	if len(b.samples) == 0 {
		return
	}
	q.mu.Lock()
	if _, ok := q.pending[b.hostID]; ok {
		q.pending[b.hostID] = b
		n := q.coalesced.Add(1)
		q.mu.Unlock()
		q.logAtPowerOfTwo("vram sample queue coalesced", n)
		return
	}
	if len(q.pending) >= vramQueueMax {
		n := q.dropped.Add(1)
		q.mu.Unlock()
		q.logAtPowerOfTwo("vram sample queue dropped", n)
		return
	}
	q.pending[b.hostID] = b
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Bounds one batch's UPDATE: a single wedged `FOR UPDATE` holder would
// otherwise block the one drain goroutine and freeze VRAM telemetry
// fleet-wide. A sample is worthless long before this fires; stale telemetry
// makes the veto abstain, which is fail-open.
const vramWriteTimeout = 5 * time.Second

func (q *vramQueue) run() {
	defer close(q.done)
	for range q.wake {
		for {
			var b vramSampleBatch
			found := false
			q.mu.Lock()
			for key, candidate := range q.pending {
				b = candidate
				delete(q.pending, key)
				found = true
				break
			}
			q.mu.Unlock()
			// Derive "drained" from whether the map yielded anything, NOT from
			// b.hostID == "": an empty host id would otherwise be consumed AND
			// truncate the drain loop, silently stranding the rest of the batch.
			if !found {
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), vramWriteTimeout)
			err := q.store.applyVramSamples(ctx, b.hostID, b.agentMs, b.samples)
			cancel()
			if err != nil {
				// Never fatal: a failed sample write just leaves the previous (or
				// absent) reading in place, and the veto abstains once it goes stale.
				q.log.Warn("vram sample persist failed", "host_id", b.hostID, "err", err)
			}
			q.processed.Add(1)
		}
	}
}

// close stops the drain goroutine and waits for the in-flight batch to finish.
// Pending batches are intentionally discarded: they are point-in-time telemetry
// and the next heartbeat replaces them. Without this the goroutine outlives its
// Handler — one leak per Handler, which matters most in tests, where the suite
// builds many.
func (q *vramQueue) close() {
	q.closeOnce.Do(func() {
		close(q.wake)
		<-q.done
	})
}

func (q *vramQueue) logAtPowerOfTwo(message string, n uint64) {
	if n != 0 && n&(n-1) == 0 {
		q.log.Warn(message, "count", n, "capacity", vramQueueMax)
	}
}

func (q *vramQueue) stats() (pending int, dropped, coalesced, processed uint64) {
	q.mu.Lock()
	pending = len(q.pending)
	q.mu.Unlock()
	return pending, q.dropped.Load(), q.coalesced.Load(), q.processed.Load()
}
