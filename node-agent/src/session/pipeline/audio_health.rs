//! Always-on health of the stream-audio capture (#351).
//!
//! `audiobasesrc` loses audio quietly: a capture thread that falls a ring buffer behind
//! skips to the newest segment (logged only at `GST_DEBUG>=2`), and a monitor that delivers
//! late makes the clock slaving jump the timestamps (not logged at all). The browser hears
//! either as concealment. This logs both, with the capture thread's CPU and run-queue
//! share, so one report from a slow host says where the time went. Probes and a bus sync
//! handler only; it never touches a buffer.

use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use gstreamer as gst;
use gstreamer::prelude::*;

/// How often a summary is logged, and the unit every rate below is measured over.
const WINDOW: Duration = Duration::from_secs(60);
/// A buffer whose newest sample is older than this when it leaves the encoder is late
/// enough that the capture ring buffer (200 ms) is close to overrunning.
const LATE_WARN: Duration = Duration::from_millis(100);
/// A timestamp step this much larger than the previous buffer's duration is a gap, not
/// rounding (one 10 ms buffer is 480 samples; rounding moves it by a nanosecond).
const GAP_TOLERANCE_NS: u64 = 1_000_000;

/// One capture thread's scheduler counters, from `/proc/thread-self/schedstat`: time on a
/// CPU, time runnable but waiting for one, in nanoseconds.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(super) struct SchedStat {
    pub cpu_ns: u64,
    pub wait_ns: u64,
}

/// Parse `/proc/<pid>/task/<tid>/schedstat` (`<cpu ns> <runqueue wait ns> <slices>`).
pub(super) fn parse_schedstat(text: &str) -> Option<SchedStat> {
    let mut it = text.split_whitespace();
    let cpu_ns = it.next()?.parse().ok()?;
    let wait_ns = it.next()?.parse().ok()?;
    Some(SchedStat { cpu_ns, wait_ns })
}

fn read_thread_schedstat() -> Option<SchedStat> {
    parse_schedstat(&std::fs::read_to_string("/proc/thread-self/schedstat").ok()?)
}

/// A finished window, ready to log.
#[derive(Debug, Clone, PartialEq)]
pub(super) struct Report {
    pub window_ms: u64,
    /// Buffers captured, and the audio they carried.
    pub buffers: u64,
    pub captured_ms: u64,
    /// Audio missing between consecutive buffers (timestamp steps), and how many steps.
    pub gap_ms: u64,
    pub gaps: u64,
    /// Buffers `audiobasesrc` flagged DISCONT (its "can't record fast enough" drops).
    pub disconts: u64,
    /// RTP packets the send queue dropped because `webrtcbin` did not take them in time.
    pub send_drops: u64,
    /// Oldest-sample age when a buffer left the capture element, and when it left the
    /// encoder, worst in the window.
    pub max_capture_late_ms: u64,
    pub max_encoded_late_ms: u64,
    /// Share of the window the capture thread spent on a CPU, and runnable but waiting
    /// for one. `None` when the counters were unavailable or the thread changed.
    pub thread_cpu_pct: Option<f64>,
    pub thread_wait_pct: Option<f64>,
}

impl Report {
    /// Whether this window lost audio or came close to it.
    pub fn is_degraded(&self) -> bool {
        self.gap_ms > 0
            || self.disconts > 0
            || self.send_drops > 0
            || self.max_encoded_late_ms >= LATE_WARN.as_millis() as u64
    }
}

/// The per-window accumulator. Pure, so the arithmetic is testable without a pipeline.
#[derive(Debug)]
pub(super) struct Window {
    started: Instant,
    sched_at_start: Option<SchedStat>,
    next_pts_ns: Option<u64>,
    buffers: u64,
    captured_ns: u64,
    gap_ns: u64,
    gaps: u64,
    disconts: u64,
    send_drops: u64,
    max_capture_late_ns: u64,
    max_encoded_late_ns: u64,
}

impl Window {
    pub fn new(now: Instant, sched: Option<SchedStat>) -> Self {
        Self {
            started: now,
            sched_at_start: sched,
            next_pts_ns: None,
            buffers: 0,
            captured_ns: 0,
            gap_ns: 0,
            gaps: 0,
            disconts: 0,
            send_drops: 0,
            max_capture_late_ns: 0,
            max_encoded_late_ns: 0,
        }
    }

    /// A buffer leaving the capture element. Returns the gap before it, if any.
    pub fn captured(
        &mut self,
        pts_ns: u64,
        duration_ns: u64,
        discont: bool,
        late_ns: u64,
    ) -> Option<u64> {
        self.buffers += 1;
        self.captured_ns += duration_ns;
        if discont {
            self.disconts += 1;
        }
        self.max_capture_late_ns = self.max_capture_late_ns.max(late_ns);
        let gap = self
            .next_pts_ns
            .and_then(|expected| pts_ns.checked_sub(expected))
            .filter(|step| *step > GAP_TOLERANCE_NS);
        if let Some(step) = gap {
            self.gap_ns += step;
            self.gaps += 1;
        }
        self.next_pts_ns = Some(pts_ns + duration_ns);
        gap
    }

    /// The same audio leaving the encoder, as an RTP packet.
    pub fn encoded(&mut self, late_ns: u64) {
        self.max_encoded_late_ns = self.max_encoded_late_ns.max(late_ns);
    }

    /// The send queue dropped a packet.
    pub fn send_dropped(&mut self) {
        self.send_drops += 1;
    }

    /// Close the window if it has run its length: the report, and a fresh window that
    /// continues the timestamp chain.
    pub fn roll(&mut self, now: Instant, sched: Option<SchedStat>) -> Option<Report> {
        let elapsed = now.saturating_duration_since(self.started);
        if elapsed < WINDOW {
            return None;
        }
        let elapsed_ns = elapsed.as_nanos().max(1) as f64;
        let share = |f: fn(&SchedStat) -> u64| match (self.sched_at_start, sched) {
            (Some(a), Some(b)) if f(&b) >= f(&a) => {
                Some(100.0 * (f(&b) - f(&a)) as f64 / elapsed_ns)
            }
            _ => None,
        };
        let ms = |ns: u64| ns / 1_000_000;
        let report = Report {
            window_ms: elapsed.as_millis() as u64,
            buffers: self.buffers,
            captured_ms: ms(self.captured_ns),
            gap_ms: ms(self.gap_ns),
            gaps: self.gaps,
            disconts: self.disconts,
            send_drops: self.send_drops,
            max_capture_late_ms: ms(self.max_capture_late_ns),
            max_encoded_late_ms: ms(self.max_encoded_late_ns),
            thread_cpu_pct: share(|s| s.cpu_ns),
            thread_wait_pct: share(|s| s.wait_ns),
        };
        let next_pts_ns = self.next_pts_ns;
        *self = Window::new(now, sched);
        self.next_pts_ns = next_pts_ns;
        Some(report)
    }
}

fn log_report(r: &Report, session_id: &str) {
    let pct = |v: Option<f64>| v.map_or_else(|| "n/a".to_string(), |v| format!("{v:.1}%"));
    if r.is_degraded() {
        tracing::warn!(
            token = "audio-capture-degraded",
            session_id = %session_id,
            "stream audio lost {} ms in {} gaps over the last {} ms ({} buffers, {} ms captured, \
             {} capture drops, {} packets dropped waiting to send); oldest sample at capture {} ms, after encode {} ms; capture \
             thread on CPU {}, waiting for a CPU {}",
            r.gap_ms,
            r.gaps,
            r.window_ms,
            r.buffers,
            r.captured_ms,
            r.disconts,
            r.send_drops,
            r.max_capture_late_ms,
            r.max_encoded_late_ms,
            pct(r.thread_cpu_pct),
            pct(r.thread_wait_pct),
        );
    } else {
        tracing::info!(
            token = "audio-capture-health",
            session_id = %session_id,
            "stream audio healthy over the last {} ms: {} buffers, {} ms captured; oldest \
             sample at capture {} ms, after encode {} ms; capture thread on CPU {}, waiting \
             for a CPU {}",
            r.window_ms,
            r.buffers,
            r.captured_ms,
            r.max_capture_late_ms,
            r.max_encoded_late_ms,
            pct(r.thread_cpu_pct),
            pct(r.thread_wait_pct),
        );
    }
}

/// Age of a buffer's newest sample against the pipeline clock, in ns (0 if not yet due or
/// unknowable).
fn lateness_ns(element: &gst::Element, buffer: &gst::BufferRef) -> u64 {
    let (Some(clock), Some(pts)) = (element.clock(), buffer.pts()) else {
        return 0;
    };
    let Some(base) = element.base_time() else {
        return 0;
    };
    let now = clock.time();
    let end = pts + buffer.duration().unwrap_or(gst::ClockTime::ZERO);
    now.checked_sub(base)
        .and_then(|running| running.checked_sub(end))
        .map_or(0, |late| late.nseconds())
}

/// Watch the capture: `capture` is the audio source element, `encoded` the pad the RTP
/// packets leave the encode chain on, `send_queue` the leaky queue in front of
/// `webrtcbin`. The probes and the queue's `overrun` run on the capture's streaming thread.
pub(super) fn attach(
    capture: &gst::Element,
    encoded: &gst::Pad,
    send_queue: &gst::Element,
    session_id: &str,
) {
    let Some(capture_pad) = capture.static_pad("src") else {
        tracing::warn!(
            token = "audio-health-no-src-pad",
            "audio capture has no src pad; capture health is not reported"
        );
        return;
    };
    let window = Arc::new(Mutex::new(Window::new(Instant::now(), None)));
    let session_gap_logged = Arc::new(std::sync::atomic::AtomicBool::new(false));

    let weak = capture.downgrade();
    let w = window.clone();
    let session_id: Arc<str> = Arc::from(session_id);
    capture_pad.add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
        let (Some(element), Some(buffer)) = (weak.upgrade(), info.buffer()) else {
            return gst::PadProbeReturn::Ok;
        };
        let Some(pts) = buffer.pts() else {
            return gst::PadProbeReturn::Ok;
        };
        let duration = buffer.duration().map_or(0, |d| d.nseconds());
        let discont = buffer.flags().contains(gst::BufferFlags::DISCONT);
        let late = lateness_ns(&element, buffer);
        let Ok(mut win) = w.lock() else {
            return gst::PadProbeReturn::Ok;
        };
        // The first buffer after start is DISCONT by definition; it opens the chain.
        let first = win.buffers == 0 && win.next_pts_ns.is_none();
        if first {
            *win = Window::new(Instant::now(), read_thread_schedstat());
        }
        let gap = win.captured(pts.nseconds(), duration, discont && !first, late);
        if let Some(gap_ns) = gap {
            // The first gap of a session is logged when it happens; later ones only in
            // the window summary, so a failing host logs once a minute, not per buffer.
            if !session_gap_logged.swap(true, std::sync::atomic::Ordering::Relaxed) {
                tracing::warn!(
                    token = "audio-capture-gap",
                    session_id = %session_id,
                    "stream audio skipped {} ms at capture (discont={discont}); further gaps \
                     are summarised once a minute",
                    gap_ns / 1_000_000
                );
            }
        }
        if let Some(report) = win.roll(Instant::now(), read_thread_schedstat()) {
            drop(win);
            log_report(&report, &session_id);
        }
        gst::PadProbeReturn::Ok
    });

    // The queue's first push blocks until the audio PeerConnection is negotiated, so it
    // fills and leaks at every session start. Count drops only once a second packet has
    // left it, which means the first push returned.
    let sent = Arc::new(std::sync::atomic::AtomicU64::new(0));
    if let Some(queue_src) = send_queue.static_pad("src") {
        let sent = sent.clone();
        queue_src.add_probe(gst::PadProbeType::BUFFER, move |_pad, _info| {
            sent.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            gst::PadProbeReturn::Ok
        });
    }
    let w = window.clone();
    send_queue.connect("overrun", false, move |_| {
        if sent.load(std::sync::atomic::Ordering::Relaxed) >= 2 {
            if let Ok(mut win) = w.lock() {
                win.send_dropped();
            }
        }
        None
    });

    let weak = capture.downgrade();
    encoded.add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
        if let (Some(element), Some(buffer)) = (weak.upgrade(), info.buffer()) {
            let late = lateness_ns(&element, buffer);
            if let Ok(mut win) = window.lock() {
                win.encoded(late);
            }
        }
        gst::PadProbeReturn::Ok
    });
}

/// Log the audio pipeline's warnings and errors and drop every message. Nothing else reads
/// this bus: unhandled, its messages are kept for the whole session and `audiobasesrc`'s
/// "Can't record audio fast enough" is never seen.
pub(super) fn watch_bus(pipeline: &gst::Pipeline, session_id: &str) {
    let Some(bus) = pipeline.bus() else {
        return;
    };
    let session_id = session_id.to_string();
    let last = Mutex::new(None::<Instant>);
    let suppressed = std::sync::atomic::AtomicU64::new(0);
    bus.set_sync_handler(move |_bus, msg| {
        let (level, text) = match msg.view() {
            gst::MessageView::Warning(w) => ("warning", format!("{} ({:?})", w.error(), w.debug())),
            gst::MessageView::Error(e) => ("error", format!("{} ({:?})", e.error(), e.debug())),
            _ => return gst::BusSyncReply::Drop,
        };
        let src = msg
            .src()
            .map(|s| s.path_string().to_string())
            .unwrap_or_default();
        let now = Instant::now();
        let mut last = last.lock().unwrap_or_else(|e| e.into_inner());
        if last.is_some_and(|t| now.duration_since(t) < WINDOW) {
            suppressed.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            return gst::BusSyncReply::Drop;
        }
        *last = Some(now);
        let earlier = suppressed.swap(0, std::sync::atomic::Ordering::Relaxed);
        tracing::warn!(
            token = "audio-pipeline-message",
            session_id = %session_id,
            "audio pipeline {level} from {src}: {text} ({earlier} more warnings or errors \
             since the last one logged)"
        );
        gst::BusSyncReply::Drop
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    const MS: u64 = 1_000_000;

    #[test]
    fn schedstat_parses_the_proc_format() {
        assert_eq!(
            parse_schedstat("123456 7890 42\n"),
            Some(SchedStat {
                cpu_ns: 123456,
                wait_ns: 7890
            })
        );
        assert_eq!(parse_schedstat(""), None);
        assert_eq!(parse_schedstat("x y z"), None);
    }

    #[test]
    fn contiguous_buffers_are_not_gaps() {
        let t0 = Instant::now();
        let mut w = Window::new(t0, None);
        for i in 0..100 {
            assert_eq!(w.captured(i * 10 * MS, 10 * MS, false, 12 * MS), None);
        }
        let r = w.roll(t0 + WINDOW, None).expect("window closes");
        assert_eq!(
            (r.buffers, r.captured_ms, r.gap_ms, r.gaps),
            (100, 1000, 0, 0)
        );
        assert!(!r.is_degraded());
    }

    #[test]
    fn a_timestamp_step_counts_the_missing_audio() {
        // The #351 signature: 70 ms of audio, then a 200 ms ring-buffer skip, repeated.
        let t0 = Instant::now();
        let mut w = Window::new(t0, None);
        let mut pts = 0;
        for _cycle in 0..3 {
            for _ in 0..7 {
                w.captured(pts, 10 * MS, false, 0);
                pts += 10 * MS;
            }
            pts += 200 * MS;
            assert_eq!(w.captured(pts, 10 * MS, true, 0), Some(200 * MS));
            pts += 10 * MS;
        }
        let r = w.roll(t0 + WINDOW, None).unwrap();
        assert_eq!((r.gaps, r.gap_ms, r.disconts), (3, 600, 3));
        assert!(r.is_degraded());
    }

    #[test]
    fn a_window_keeps_the_timestamp_chain_across_the_roll() {
        let t0 = Instant::now();
        let mut w = Window::new(t0, None);
        w.captured(0, 10 * MS, false, 0);
        w.roll(t0 + WINDOW, None).unwrap();
        assert_eq!(w.captured(50 * MS, 10 * MS, false, 0), Some(40 * MS));
    }

    #[test]
    fn a_window_does_not_close_early() {
        let t0 = Instant::now();
        let mut w = Window::new(t0, None);
        assert_eq!(w.roll(t0 + WINDOW - Duration::from_millis(1), None), None);
    }

    #[test]
    fn thread_shares_come_from_the_counter_deltas() {
        let t0 = Instant::now();
        let a = SchedStat {
            cpu_ns: 1_000 * MS,
            wait_ns: 500 * MS,
        };
        let b = SchedStat {
            cpu_ns: 7_000 * MS,
            wait_ns: 30_500 * MS,
        };
        let mut w = Window::new(t0, Some(a));
        let r = w.roll(t0 + WINDOW, Some(b)).unwrap();
        assert_eq!(r.thread_cpu_pct.map(|v| v.round()), Some(10.0));
        assert_eq!(r.thread_wait_pct.map(|v| v.round()), Some(50.0));
        // A counter that went backwards is another thread: no share rather than a wrong one.
        let mut w = Window::new(t0, Some(b));
        let r = w.roll(t0 + WINDOW, Some(a)).unwrap();
        assert_eq!((r.thread_cpu_pct, r.thread_wait_pct), (None, None));
    }

    #[test]
    fn a_send_drop_alone_is_degraded() {
        let t0 = Instant::now();
        let mut w = Window::new(t0, None);
        w.captured(0, 10 * MS, false, 0);
        w.send_dropped();
        let r = w.roll(t0 + WINDOW, None).unwrap();
        assert_eq!((r.send_drops, r.gap_ms), (1, 0));
        assert!(r.is_degraded());
    }

    #[test]
    fn a_late_encode_alone_is_degraded() {
        let t0 = Instant::now();
        let mut w = Window::new(t0, None);
        w.captured(0, 10 * MS, false, 20 * MS);
        w.encoded(150 * MS);
        let r = w.roll(t0 + WINDOW, None).unwrap();
        assert_eq!(r.max_encoded_late_ms, 150);
        assert!(r.is_degraded());
    }
}
