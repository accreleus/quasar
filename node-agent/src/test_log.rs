//! Shared test-only log capture for the node-agent lib test binary.
//!
//! Every test in this binary used to install its own scoped `tracing`
//! subscriber (`set_default` / `with_default`) and then call
//! `rebuild_interest_cache`. `tracing` caches whether a callsite is enabled,
//! and under the parallel runner a scoped dispatcher one test thread installed
//! raced the interest cache another thread was rebuilding, leaving some
//! callsites cached as disabled for the rest of the process (tracing#523): a
//! test that captured its logs one run captured nothing the next.
//!
//! This module installs ONE process-wide subscriber once (a `Once` +
//! `try_init`) and never recreates it, so the callsite interest is stable. It
//! has three layers:
//!
//! * a routed `fmt` layer that writes each formatted line to the calling
//!   thread's sink when one is set — this is what [`logged`] and [`begin`]
//!   capture;
//! * the browser-host stderr layer (`quasar_node_agent=info`), which the
//!   takeover tests used to keep `webrtcbin`'s span-less threads visible; and
//! * a [`WarningLog`] layer that keeps every WARN/ERROR line in [`warnings`].
//!
//! No test installs a scoped subscriber or calls `rebuild_interest_cache`
//! anymore, so no thread can rebuild the interest cache out from under another.

use std::cell::RefCell;
use std::sync::{Arc, Mutex, Once};

use tracing_subscriber::prelude::*;

// The bytes one capture collects for the calling thread (and any thread it
// spawns, which share this thread-local sink).
thread_local! {
    static SINK: RefCell<Option<Arc<Mutex<Vec<u8>>>>> = const {
        RefCell::new(None)
    };
}

/// Writes each formatted line to the calling thread's `SINK`, or drops it when
/// no capture is active on this thread.
struct Routed;

impl std::io::Write for Routed {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        SINK.with(|s| {
            if let Some(arc) = s.borrow().clone() {
                arc.lock().unwrap().extend_from_slice(buf);
            }
        });
        Ok(buf.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// Every WARN and ERROR line this process has logged, as `field=value ` pairs.
///
/// Process-wide: `webrtcbin`'s threads carry no span, so a subscriber set for
/// one test's thread never sees what they log — the global subscriber does.
static WARNINGS: Mutex<Vec<String>> = Mutex::new(Vec::new());

struct WarningLog;

impl<S: tracing::Subscriber> tracing_subscriber::Layer<S> for WarningLog {
    fn on_event(&self, event: &tracing::Event<'_>, _: tracing_subscriber::layer::Context<'_, S>) {
        struct Line(String);
        impl tracing::field::Visit for Line {
            fn record_debug(&mut self, field: &tracing::field::Field, value: &dyn std::fmt::Debug) {
                self.0 += &format!("{}={value:?} ", field.name());
            }
        }
        if *event.metadata().level() <= tracing::Level::WARN {
            let mut line = Line(String::new());
            event.record(&mut line);
            WARNINGS.lock().unwrap().push(line.0);
        }
    }
}

/// A snapshot of every WARN/ERROR line logged so far, for the takeover tests'
/// fingerprint checks.
pub fn warnings() -> Vec<String> {
    WARNINGS.lock().unwrap().clone()
}

fn install() {
    static ONCE: Once = Once::new();
    ONCE.call_once(|| {
        // `try_init`, not `set_global_default`: a product `init_subscriber`
        // exists for the real binary, and if something already set a global
        // subscriber here this is a no-op rather than a panic. The single
        // `Once` installs the full registry (routed + stderr + WarningLog)
        // exactly once, so the routed layer is always present and the callsite
        // interest it pins is never rebuilt away.
        let _ = tracing_subscriber::registry()
            .with(
                tracing_subscriber::fmt::layer()
                    .with_ansi(false)
                    .with_writer(|| Routed)
                    // DEBUG, not INFO: one capture asserts on a debug-level
                    // log line (a refused hello's bounded log). INFO and above
                    // is what the stderr and WarningLog layers keep.
                    .with_filter(tracing_subscriber::filter::LevelFilter::DEBUG),
            )
            .with(
                tracing_subscriber::fmt::layer()
                    .with_test_writer()
                    .with_filter(tracing_subscriber::EnvFilter::new("quasar_node_agent=info")),
            )
            .with(WarningLog)
            .try_init();
    });
}

/// Stops the active capture on this thread when dropped.
pub struct EndCapture;

impl Drop for EndCapture {
    fn drop(&mut self) {
        SINK.with(|s| {
            *s.borrow_mut() = None;
        });
    }
}

/// The lines `run` logs (DEBUG and above) on this thread, one per element.
///
/// Scoped to this thread and every thread it spawns: the sink is thread-local,
/// so a task spawned inside `run` writes to the same sink. (A per-test scoped
/// subscriber had exactly this scope; nothing reaches deeper.)
pub fn logged(run: impl FnOnce()) -> Vec<String> {
    let sink = Arc::new(Mutex::new(Vec::new()));
    let _guard = begin(&sink);
    run();
    let bytes = sink.lock().unwrap();
    String::from_utf8_lossy(&bytes)
        .lines()
        .map(String::from)
        .collect()
}

/// Sets this thread's sink to `sink` until the returned guard drops, so a
/// capture handle can be read mid-flight (e.g. while a session is still
/// running). Installs the shared subscriber if it is not already installed.
pub fn begin(sink: &Arc<Mutex<Vec<u8>>>) -> EndCapture {
    install();
    SINK.with(|s| {
        *s.borrow_mut() = Some(sink.clone());
    });
    EndCapture
}

/// Agent log lines on stderr (for the browser host) plus warnings in
/// [`warnings`]. One global subscriber, installed once: safe to call from any
/// test, any thread. Replaces the takeover tests' per-call `try_init`.
pub fn trace_to_stderr() {
    install();
}
