//! SO-03: micro-benchmark the always-on per-frame encode-metrics hot path.
//!
//! The encoder sink/src pad probes call `record_encode_in` / `record_encode_out`
//! once per encoded frame on every running session (deep trace off). This bench
//! measures that overhead so the SO-03 trim (3→2 mutex acquisitions per frame via
//! one combined `Pending` lock, single clock read) is measured, not asserted.
//!
//! It is a self-contained A/B: `two_mutex_in_out_pair` faithfully replicates the
//! pre-SO-03 pattern (the encode FIFO and the samples vec behind SEPARATE locks, so
//! the src side takes two acquisitions), and `record_encode_in_out_pair` exercises
//! the real merged-lock `SessionMetrics`. The delta between the two in one run is
//! the lock-merge win.
//!
//! Run inside the dev container: `scripts/dev/dev.sh bench node-agent`
//! (or `cargo bench` from `node-agent/`).

use std::collections::VecDeque;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use criterion::{criterion_group, criterion_main, Criterion};
use quasar_node_agent::session::metrics::SessionMetrics;
// criterion 0.8 deprecated its own `black_box` re-export in favour of std's.
use std::hint::black_box;

const PENDING_CAP: usize = 64;

/// Pre-SO-03 replica: the encode FIFO and the samples vec behind SEPARATE locks, so
/// `record_out` takes two acquisitions (pop, then sample push). The atomics mirror
/// the real `frames_out`/`bytes_out` counters so the only difference vs the merged
/// implementation is the lock granularity.
struct TwoMutexPending {
    frames_out: AtomicU64,
    bytes_out: AtomicU64,
    pending: Mutex<VecDeque<Instant>>,
    samples: Mutex<Vec<f64>>,
}

impl TwoMutexPending {
    fn new() -> Self {
        Self {
            frames_out: AtomicU64::new(0),
            bytes_out: AtomicU64::new(0),
            pending: Mutex::new(VecDeque::new()),
            samples: Mutex::new(Vec::new()),
        }
    }
    fn record_in(&self, t: Instant) {
        let mut q = self.pending.lock().unwrap();
        if q.len() >= PENDING_CAP {
            q.clear();
        }
        q.push_back(t);
    }
    fn record_out(&self, t_out: Instant, bytes: u64) {
        self.frames_out.fetch_add(1, Ordering::Relaxed);
        self.bytes_out.fetch_add(bytes, Ordering::Relaxed);
        let popped = self.pending.lock().unwrap().pop_front();
        if let Some(t_in) = popped {
            self.samples
                .lock()
                .unwrap()
                .push(t_out.saturating_duration_since(t_in).as_secs_f64() * 1000.0);
        }
    }
}

fn bench_hot_path(c: &mut Criterion) {
    let base = Instant::now();
    let out_at = base + Duration::from_millis(8);

    // The production tier: lightweight counting + FIFO encode-time pairing. This is
    // exactly what runs per frame (deep trace is now a client-side capability).
    let m = SessionMetrics::new("protective", 60); // SPT-02: mode arg; SPT-03: target fps

    // Sink side in isolation — the FIFO push (+ overflow guard). Naturally bounded:
    // the FIFO self-clears at PENDING_CAP, so memory stays flat across iterations.
    c.bench_function("record_encode_in", |b| {
        b.iter(|| m.record_encode_in(black_box(base)));
    });

    // Full per-frame pair, merged single lock (SO-03).
    c.bench_function("record_encode_in_out_pair", |b| {
        b.iter(|| {
            m.record_encode_in(black_box(base));
            m.record_encode_out(black_box(out_at), black_box(40_000));
        });
    });

    // Full per-frame pair, pre-SO-03 two-mutex pattern (the A/B baseline).
    let old = TwoMutexPending::new();
    c.bench_function("two_mutex_in_out_pair", |b| {
        b.iter(|| {
            old.record_in(black_box(base));
            old.record_out(black_box(out_at), black_box(40_000));
        });
    });
}

criterion_group!(benches, bench_hot_path);
criterion_main!(benches);
