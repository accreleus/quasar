//! On-demand observation of a live session — the `session_capture` seam.
//!
//! An admin asks a *running* session a question and gets one bounded answer back
//! as a `session_trace_event` with event `diag.<kind>`.
//!
//! Runs entirely on the runner's supervision thread, off its existing 100 ms
//! `POLL` tick — the same access [`runner::effective_media_snapshot`] already
//! performs against a `PLAYING` pipeline. Deliberately: no pad probe (the
//! host-side deep-trace probe was removed in #270, it could crash the stream);
//! no allocation, lock or work of any kind on a streaming thread (only
//! [`SessionMetrics`] is touched, via the same poison-tolerant snapshot locks the
//! heartbeat drain uses); no media content, ever. The one pipeline cost is
//! `pipeline_dot`'s graph walk under the bin lock — the same class of access as
//! `effective_media_snapshot`'s property/caps reads.
//!
//! # Safety envelope (enforced, and tested for the negative cases)
//!
//! A capture may **never** carry pixel, audio or bitstream content; input events or
//! microphone data; the environment wholesale; the node secret or an enrollment
//! token; or file paths outside the session scratch. Concretely:
//!
//! * `pipeline_dot` uses `CAPS_DETAILS | STATES` **only**. `NON_DEFAULT_PARAMS` /
//!   `FULL_PARAMS` print property *values* (e.g. socket/device paths) and are
//!   banned; [`tests::dot_details_never_include_non_default_params`] proves it.
//! * `encoder_props` reads an **allow-list of property names** ([`ENCODER_PROP_ALLOW`]) —
//!   an absent name is never queried. Strings over [`MAX_PROP_STRING`] are elided.
//! * `burst_stats` carries only numbers the metrics drain already computes.
//! * Every payload is capped at `budget.max_bytes` *compressed* and `budget.max_ms`
//!   wall clock.
//!
//! # Single-flight
//!
//! One capture per session. The reservation is an `AtomicBool` shared with the
//! agent loop so the ack (`busy`) is decided synchronously, never queued.

use std::io::Write as _;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use base64::Engine as _;
use gstreamer as gst;
use gstreamer::prelude::*;
use serde_json::{json, Value};

use crate::messages::{CaptureBudget, CaptureKind, CaptureParams};
use crate::session::metrics::{BurstSnapshot, SessionMetrics, Summary};

/// A string property value longer than this is elided rather than emitted. Long
/// strings are where paths, URIs and blobs live; the useful encoder properties are
/// short enums and numbers.
pub const MAX_PROP_STRING: usize = 256;

/// Raw telemetry samples emitted per burst sub-window, per series.
pub const MAX_BURST_SAMPLES: usize = 200;

/// A JSON result at or below this size rides the wire as-is (`encoding: "json"`);
/// anything larger is gzipped and base64'd like the dot dump.
const JSON_INLINE_MAX: usize = 32_768;

/// Hard floor/ceiling on the wall-clock budget, whatever the control plane asks.
const MIN_BUDGET_MS: u64 = 100;
const MAX_BUDGET_MS: u64 = 60_000;
/// Hard floor on the byte budget — below this nothing useful survives truncation.
const MIN_BUDGET_BYTES: usize = 1_024;

/// Encoder properties a capture may read, by NAME. An allow-list, not a deny-list:
/// an absent name is never queried, so a new encoder property cannot leak by default.
///
/// One flat list across every encoder family this agent builds (NVENC, VA, Vulkan,
/// openh264, x264): the union is what a reader wants (the same knob is `gop-size`
/// on one element, `key-int-max` on another), and each name is guarded by
/// `find_property`, so an element simply omits what it lacks.
///
/// **Deliberately absent:** `device-path` and `cuda-device-id` — paths/device ids;
/// `session.effective_media` already reports both once per session.
pub const ENCODER_PROP_ALLOW: &[&str] = &[
    // rate control
    "bitrate",
    "target-bitrate",
    "max-bitrate",
    "rate-control",
    "rc-mode",
    "cpb-size",
    "target-usage",
    "multipass",
    "rc-lookahead",
    "spatial-aq",
    "temporal-aq",
    "aq-strength",
    "mbbrc",
    // quantiser
    "qp-min",
    "qp-max",
    "qp-i",
    "qp-p",
    "qp-b",
    "min-qp",
    "max-qp",
    "init-qp",
    "qos",
    // GOP / reference structure
    "gop-size",
    "key-int-max",
    "idr-period",
    "keyframe-max-dist",
    "ref-frames",
    "num-ref-frames",
    "max-num-references",
    "b-frames",
    "bframes",
    "max-bframes",
    "b-adapt",
    "low-delay-b",
    // bitstream shape
    "profile",
    "level",
    "level-idc",
    "num-slices",
    "slices",
    "num-tile-cols",
    "num-tile-rows",
    "aud",
    "cabac",
    "entropy-mode",
    "trellis",
    "repeat-sequence-header",
    "insert-sps-pps",
    "config-interval",
    // speed / quality posture
    "preset",
    "tune",
    "tuning-info",
    "complexity",
    "deadline",
    "speed-preset",
    "zerolatency",
    "threads",
];

// ── Request / refusal ───────────────────────────────────────────────────────────

/// A capture routed from the agent loop to the runner. `kind` is always a KNOWN
/// kind: [`CaptureKind::Other`] is refused at the ack, before a request is formed.
#[derive(Debug, Clone)]
pub struct CaptureRequest {
    pub capture_id: String,
    pub kind: CaptureKind,
    pub budget: CaptureBudget,
    pub params: CaptureParams,
    /// The reservation the agent loop took to ack this request. It travels WITH the
    /// request so the runner cannot hold a slot it was never handed — and so
    /// [`Capture`] itself needs no per-session wiring beyond the channel.
    pub slot: CaptureSlot,
}

/// Why a `session_capture` was refused. The `as_str` values are the wire `error`
/// strings in the nack ack — the control plane maps them to HTTP status codes.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CaptureRefusal {
    /// A capture is already in flight for this session. Refused, never queued.
    Busy,
    /// The kind is known but impossible here (a local-only console session has no
    /// encode pipeline at all).
    Unsupported,
    /// A kind string this build does not implement.
    UnknownKind,
}

impl CaptureRefusal {
    pub fn as_str(self) -> &'static str {
        match self {
            CaptureRefusal::Busy => "busy",
            CaptureRefusal::Unsupported => "unsupported",
            CaptureRefusal::UnknownKind => "unknown_kind",
        }
    }
}

/// The single-flight reservation, shared between the agent loop (which takes it, so
/// the ack can say `busy` synchronously) and the runner's [`Capture`] (which
/// releases it when the capture finishes, fails, or deadlines).
#[derive(Debug, Clone, Default)]
pub struct CaptureSlot(Arc<AtomicBool>);

impl CaptureSlot {
    pub fn new() -> Self {
        Self::default()
    }

    /// Reserve the slot for one capture. `Err(Busy)` when one is already running.
    ///
    /// Called from the agent loop *before* the request is handed to the runner, so
    /// the ack is decided on the same state the runner will observe.
    pub fn reserve(&self) -> Result<(), CaptureRefusal> {
        self.0
            .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
            .map(|_| ())
            .map_err(|_| CaptureRefusal::Busy)
    }

    /// Give the slot back. Idempotent.
    pub fn release(&self) {
        self.0.store(false, Ordering::Release);
    }

    pub fn is_busy(&self) -> bool {
        self.0.load(Ordering::Acquire)
    }
}

/// Decide a `session_capture` ack without running anything. Pure apart from the
/// slot reservation, which is the point: the agent loop must be able to answer
/// `busy` / `unsupported` / `unknown_kind` before the runner has seen the request.
///
/// `has_encode_pipeline` is false for a local-only console session (no encode
/// pipeline, no encoder, no RTP — nothing any kind can observe).
pub fn admit(
    kind: &CaptureKind,
    has_encode_pipeline: bool,
    slot: &CaptureSlot,
) -> Result<(), CaptureRefusal> {
    if !kind.is_known() {
        return Err(CaptureRefusal::UnknownKind);
    }
    if !has_encode_pipeline {
        return Err(CaptureRefusal::Unsupported);
    }
    slot.reserve()
}

// ── Budget / burst plan ─────────────────────────────────────────────────────────

/// The clamped, runnable form of `params` + `budget` for a `burst_stats` capture.
///
/// Clamping rather than refusing is deliberate: the control plane's job is to say
/// what it wants, the agent's job is to stay inside its own limits. A request for
/// 10 000 × 5 ms produces a legal 40 × 100 ms plan and a capture that returns.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct BurstPlan {
    pub windows: u32,
    pub window_ms: u64,
}

impl BurstPlan {
    pub const DEFAULT_WINDOWS: u32 = 20;
    pub const DEFAULT_WINDOW_MS: u64 = 250;
    pub const MIN_WINDOWS: u32 = 1;
    pub const MAX_WINDOWS: u32 = 40;
    pub const MIN_WINDOW_MS: u64 = 100;
    pub const MAX_WINDOW_MS: u64 = 1_000;

    /// `windows` ∈ 1..=40, `window_ms` ∈ 100..=1000, and `windows × window_ms` no
    /// more than the wall-clock budget (never fewer than one window — a budget too
    /// small for even one sub-window still returns one, and the deadline truncates
    /// it if it has to).
    pub fn resolve(params: CaptureParams, max_ms: u64) -> BurstPlan {
        let window_ms = params
            .window_ms
            .unwrap_or(Self::DEFAULT_WINDOW_MS)
            .clamp(Self::MIN_WINDOW_MS, Self::MAX_WINDOW_MS);
        let asked = params
            .windows
            .unwrap_or(Self::DEFAULT_WINDOWS)
            .clamp(Self::MIN_WINDOWS, Self::MAX_WINDOWS);
        let affordable = (max_ms / window_ms)
            .max(1)
            .min(u64::from(Self::MAX_WINDOWS)) as u32;
        BurstPlan {
            windows: asked.min(affordable).max(Self::MIN_WINDOWS),
            window_ms,
        }
    }

    pub fn total_ms(self) -> u64 {
        u64::from(self.windows) * self.window_ms
    }
}

/// The budget, clamped to what this agent will actually honour.
fn clamp_budget(budget: CaptureBudget) -> CaptureBudget {
    CaptureBudget {
        max_bytes: budget.max_bytes.max(MIN_BUDGET_BYTES),
        max_ms: budget.max_ms.clamp(MIN_BUDGET_MS, MAX_BUDGET_MS),
    }
}

// ── What the runner hands in, and gets back ─────────────────────────────────────

/// Everything a capture may read, handed in by the runner on each poll. Borrowed,
/// not cloned — an owned `Arc` here would risk the ref-cycle leak shape
/// (`.claude/rules/gstreamer-gotchas.md`).
pub struct CaptureCtx<'a> {
    /// The live encode pipeline (`PLAYING`). Read-only.
    pub encode_pipe: &'a gst::Pipeline,
    /// The encoder element, already resolved by the runner (`quasar-video-encoder`
    /// or `quasar-vulkan-encoder`).
    pub encoder: Option<&'a gst::Element>,
    pub metrics: &'a SessionMetrics,
    /// Scale-stage / ring context the runner already holds. Built by the runner so
    /// this module never reaches back into pipeline internals.
    pub stage: Value,
}

/// A finished capture, ready for the reliable lane.
#[derive(Debug, Clone)]
pub struct CaptureReport {
    /// `diag.pipeline_dot` | `diag.encoder_props` | `diag.burst_stats`.
    pub event: &'static str,
    /// The `session_trace_event` payload (spec §"Upstream result").
    pub payload: Value,
    pub capture_id: String,
    pub kind: &'static str,
    /// Compressed (or inline-JSON) byte count — what the cap applies to.
    pub compressed_bytes: usize,
    pub duration_ms: u64,
    pub error: Option<&'static str>,
}

// ── The seam ────────────────────────────────────────────────────────────────────

struct Active {
    req: CaptureRequest,
    started: Instant,
    deadline: Instant,
    budget: CaptureBudget,
    burst: Option<BurstRun>,
}

struct BurstRun {
    plan: BurstPlan,
    next_close: Instant,
    windows: Vec<Value>,
    /// The previous sub-window's counter snapshot, so each window reports a delta.
    prev: BurstSnapshot,
}

/// The observation seam. Owned by the runner; polled once per supervision tick.
#[derive(Default)]
pub struct Capture {
    active: Option<Active>,
}

impl Capture {
    pub fn new() -> Self {
        Capture::default()
    }

    pub fn is_active(&self) -> bool {
        self.active.is_some()
    }

    /// Install a request the agent loop has already admitted (and whose slot it has
    /// already reserved). Returns `Busy` only if the two ever disagree, which would
    /// be a bug — the reservation is what prevents it.
    pub fn arm(
        &mut self,
        req: CaptureRequest,
        metrics: &SessionMetrics,
    ) -> Result<(), CaptureRefusal> {
        if !req.kind.is_known() {
            req.slot.release();
            return Err(CaptureRefusal::UnknownKind);
        }
        if self.active.is_some() {
            return Err(CaptureRefusal::Busy);
        }
        let budget = clamp_budget(req.budget);
        let started = Instant::now();
        let burst = (req.kind == CaptureKind::BurstStats).then(|| {
            let plan = BurstPlan::resolve(req.params, budget.max_ms);
            BurstRun {
                plan,
                next_close: started + Duration::from_millis(plan.window_ms),
                windows: Vec::with_capacity(plan.windows as usize),
                prev: metrics.snapshot_burst(started),
            }
        });
        self.active = Some(Active {
            req,
            started,
            deadline: started + Duration::from_millis(budget.max_ms),
            budget,
            burst,
        });
        Ok(())
    }

    /// Advance the in-flight capture. Returns `Some(report)` on the tick it
    /// completes (or deadlines); `None` while it is still running or idle.
    ///
    /// Called from the runner's supervision loop, never from a streaming thread.
    pub fn poll(&mut self, ctx: &CaptureCtx<'_>) -> Option<CaptureReport> {
        let active = self.active.as_mut()?;
        let now = Instant::now();
        let expired = now >= active.deadline;

        let report = match active.req.kind {
            // Both one-shots complete on the first tick after arm. The deadline can
            // only bite if the graph walk itself overran it, which the elapsed check
            // below still reports honestly rather than silently.
            CaptureKind::PipelineDot => Some(capture_pipeline_dot(active, ctx)),
            CaptureKind::EncoderProps => Some(capture_encoder_props(active, ctx)),
            CaptureKind::BurstStats => {
                let run = active
                    .burst
                    .as_mut()
                    .expect("burst run armed with the kind");
                if now >= run.next_close {
                    let snap = ctx.metrics.snapshot_burst(now);
                    let t_ms = now.saturating_duration_since(active.started).as_millis() as u64;
                    run.windows.push(burst_window(t_ms, &run.prev, &snap));
                    run.prev = snap;
                    run.next_close = now + Duration::from_millis(run.plan.window_ms);
                }
                let done = run.windows.len() >= run.plan.windows as usize;
                (done || expired).then(|| capture_burst_stats(active, expired))
            }
            CaptureKind::Other(_) => {
                // Unreachable: `arm` rejects it. Fail closed rather than spin.
                Some(finish_json(active, json!({}), Some("unknown_kind")))
            }
        };

        if report.is_some() {
            if let Some(done) = self.active.take() {
                done.req.slot.release();
            }
        }
        report
    }
}

/// Free the slot on EVERY runner exit path (~40 return sites) — same shape as the
/// `MetricsHookGuard` in `runner::run_blocking`: a lifetime obligation belongs in
/// `Drop`, not in a list of places to remember.
///
/// Belt-and-braces rather than load-bearing (the slot really lives in the agent's
/// `RunningHandle`, torn down on the same path) but a stuck `busy` is invisible
/// until an admin hits it.
impl Drop for Capture {
    fn drop(&mut self) {
        if let Some(active) = self.active.take() {
            active.req.slot.release();
        }
    }
}

// ── Kinds ───────────────────────────────────────────────────────────────────────

/// The graph dump.
///
/// `CAPS_DETAILS | STATES` and nothing else. `NON_DEFAULT_PARAMS` / `FULL_PARAMS`
/// render element property *values* into the dot label — including string
/// properties like socket paths, device paths and URIs — which is precisely what
/// this surface promises never to emit.
fn capture_pipeline_dot(active: &Active, ctx: &CaptureCtx<'_>) -> CaptureReport {
    let details = gst::DebugGraphDetails::CAPS_DETAILS | gst::DebugGraphDetails::STATES;
    let dot = ctx.encode_pipe.debug_to_dot_data(details).to_string();
    let error = (dot.is_empty()).then_some("empty_graph");
    finish_text(active, dot, "text/vnd.graphviz", error)
}

/// Live, allow-listed encoder configuration plus the caps actually negotiated
/// around the encoder and the payloader.
fn capture_encoder_props(active: &Active, ctx: &CaptureCtx<'_>) -> CaptureReport {
    let Some(encoder) = ctx.encoder else {
        return finish_json(active, json!({}), Some("no_encoder"));
    };
    let factory = encoder.factory().map(|f| f.name().to_string());
    let mut properties = serde_json::Map::new();
    for name in ENCODER_PROP_ALLOW {
        if let Some(value) = read_allowed_property(encoder, name) {
            properties.insert((*name).to_string(), value);
        }
    }
    let payloader_src = find_payloader(ctx.encode_pipe).and_then(|p| pad_caps(&p, "src"));
    let value = json!({
        "encoder_factory": factory,
        "codec": pad_caps(encoder, "src").as_deref().and_then(codec_of_caps),
        "properties": Value::Object(properties),
        "caps": {
            "encoder_sink": pad_caps(encoder, "sink"),
            "encoder_src": pad_caps(encoder, "src"),
            "payloader_src": payloader_src,
        },
        "scale_stage": ctx.stage,
        "ring": {
            // ONE named env var, never a bulk enumeration (tests.rs greps this
            // file for that) — the host-wide Vulkan encode-src ring pin
            // (`pipeline::source_branch::pin_vulkan_encode_ring`), a process-global
            // the pipeline itself cannot report back.
            "wolf_vulkan_ring": std::env::var("WOLF_VULKAN_RING").ok(),
        },
    });
    finish_json(active, value, None)
}

fn capture_burst_stats(active: &mut Active, expired: bool) -> CaptureReport {
    let run = active
        .burst
        .as_ref()
        .expect("burst run armed with the kind");
    let value = json!({
        "window_ms": run.plan.window_ms,
        "windows_requested": run.plan.windows,
        "windows": Value::Array(run.windows.clone()),
    });
    let error = (expired && run.windows.len() < run.plan.windows as usize).then_some("deadline");
    finish_json(active, value, error)
}

/// One burst sub-window: rates as deltas over the elapsed time, latency series as
/// the samples that arrived since the previous snapshot.
fn burst_window(t_ms: u64, prev: &BurstSnapshot, now: &BurstSnapshot) -> Value {
    let secs = now.at.saturating_duration_since(prev.at).as_secs_f64();
    let rate = |d: u64| if secs > 0.0 { d as f64 / secs } else { 0.0 };
    let d_frames = now.frames_out.saturating_sub(prev.frames_out);
    let d_bytes = now.bytes_out.saturating_sub(prev.bytes_out);
    json!({
        "t_ms": t_ms,
        "encode_ms": series(prev.encode_ms.len(), &now.encode_ms),
        "dwell_ms": series(prev.queue_dwell_ms.len(), &now.queue_dwell_ms),
        "fps": rate(d_frames),
        "bitrate_kbps": if secs > 0.0 { (d_bytes as f64 * 8.0) / 1000.0 / secs } else { 0.0 },
        "frames_dropped": now.overflow_drops.saturating_sub(prev.overflow_drops),
        "queue_level_max": now.queue_level_max,
        "abr_setpoint_kbps": now.abr_setpoint_kbps,
        "gcc_estimate_kbps": now.gcc_estimate_kbps,
    })
}

/// Summarize the samples that arrived since `seen`, with up to
/// [`MAX_BURST_SAMPLES`] of the raw values.
///
/// The heartbeat drain is the only thing that empties these vectors, so within one
/// burst the suffix past `seen` is exactly this sub-window's arrivals. If a drain
/// lands mid-burst and shrinks the vector, this reports whatever is present rather
/// than panicking or indexing negative.
fn series(seen: usize, samples: &[f64]) -> Value {
    let window: &[f64] = if samples.len() >= seen {
        &samples[seen..]
    } else {
        samples
    };
    let Some(summary) = Summary::of(window) else {
        return json!({ "n": 0 });
    };
    let raw: Vec<f64> = window.iter().take(MAX_BURST_SAMPLES).copied().collect();
    json!({
        "n": window.len(),
        "p50": summary.p50,
        "p95": summary.p95,
        "max": summary.max,
        "samples": raw,
    })
}

// ── Reading elements safely ─────────────────────────────────────────────────────

/// Read one allow-listed property, GValue-serialized, with long strings elided.
///
/// `find_property` first: setting or reading an absent property on a gst element
/// panics in gstreamer-rs (the enum-property trap the repo rules call out), and a
/// panic here would take the runner thread — and therefore the session — down.
fn read_allowed_property(element: &gst::Element, name: &str) -> Option<Value> {
    if !ENCODER_PROP_ALLOW.contains(&name) {
        return None;
    }
    element.find_property(name)?;
    let rendered = element.property_value(name).serialize().ok()?.to_string();
    Some(elide_long(rendered))
}

/// Keep a short rendered value; replace a long one with its length.
///
/// The encoder knobs worth reading are short — numbers and enum nicknames. A value
/// that runs long is either a path, a URI, or a blob, i.e. exactly what this
/// surface promises not to carry, so the length is reported instead of the value.
fn elide_long(rendered: String) -> Value {
    if rendered.len() > MAX_PROP_STRING {
        Value::String(format!("<elided: {} chars>", rendered.len()))
    } else {
        Value::String(rendered)
    }
}

fn pad_caps(element: &gst::Element, pad: &str) -> Option<String> {
    element
        .static_pad(pad)?
        .current_caps()
        .map(|caps| caps.to_string())
}

fn codec_of_caps(caps: &str) -> Option<&'static str> {
    if caps.contains("x-h265") {
        Some("h265")
    } else if caps.contains("x-av1") {
        Some("av1")
    } else if caps.contains("x-h264") {
        Some("h264")
    } else {
        None
    }
}

/// The RTP payloader in the encode pipeline. It is built unnamed
/// (`pipeline::codec_chain`), so it is found by factory name — `rtph264pay`,
/// `rtph265pay`, `rtpav1pay` — rather than by a name that does not exist.
fn find_payloader(pipeline: &gst::Pipeline) -> Option<gst::Element> {
    pipeline.iterate_elements().into_iter().flatten().find(|e| {
        e.factory().is_some_and(|f| {
            let n = f.name();
            n.starts_with("rtp") && n.ends_with("pay")
        })
    })
}

// ── Payload construction ────────────────────────────────────────────────────────

fn event_name(kind: &CaptureKind) -> &'static str {
    match kind {
        CaptureKind::PipelineDot => "diag.pipeline_dot",
        CaptureKind::EncoderProps => "diag.encoder_props",
        CaptureKind::BurstStats => "diag.burst_stats",
        CaptureKind::Other(_) => "diag.unknown",
    }
}

fn kind_name(kind: &CaptureKind) -> &'static str {
    match kind {
        CaptureKind::PipelineDot => "pipeline_dot",
        CaptureKind::EncoderProps => "encoder_props",
        CaptureKind::BurstStats => "burst_stats",
        CaptureKind::Other(_) => "unknown",
    }
}

/// A gzip+base64 text payload, truncated at a LINE boundary if the compressed form
/// would exceed the cap. Line-boundary truncation keeps a truncated dot graph
/// readable (and a truncated anything greppable) instead of ending mid-token.
fn finish_text(
    active: &Active,
    text: String,
    content_type: &'static str,
    error: Option<&'static str>,
) -> CaptureReport {
    let original_bytes = text.len();
    let (body, truncated) = fit_text(text, active.budget.max_bytes);
    let bytes = body.len();
    let gz = gzip(body.as_bytes());
    let data = base64::engine::general_purpose::STANDARD.encode(&gz);
    let duration_ms = active.started.elapsed().as_millis() as u64;
    let payload = json!({
        "capture_id": active.req.capture_id,
        "kind": kind_name(&active.req.kind),
        "encoding": "gzip+base64",
        "content_type": content_type,
        "data": data,
        "bytes": bytes,
        "compressed_bytes": gz.len(),
        "original_bytes": original_bytes,
        "truncated": truncated,
        "duration_ms": duration_ms,
        "error": error,
    });
    CaptureReport {
        event: event_name(&active.req.kind),
        payload,
        capture_id: active.req.capture_id.clone(),
        kind: kind_name(&active.req.kind),
        compressed_bytes: gz.len(),
        duration_ms,
        error,
    }
}

/// A JSON payload: inline when it is small, gzip+base64 when it is not.
///
/// JSON cannot be line-truncated without corrupting it, so oversize is handled
/// STRUCTURALLY by [`shrink_json`] before encoding — drop the raw sample arrays
/// first (they are the bulk and the least load-bearing), then drop sub-windows from
/// the end.
fn finish_json(active: &Active, value: Value, error: Option<&'static str>) -> CaptureReport {
    let original_bytes = serde_json::to_string(&value).map(|s| s.len()).unwrap_or(0);
    let (value, truncated) = shrink_json(value, active.budget.max_bytes);
    let text = serde_json::to_string(&value).unwrap_or_else(|_| "{}".to_string());
    let bytes = text.len();
    let duration_ms = active.started.elapsed().as_millis() as u64;

    let (encoding, body, compressed_bytes) =
        if bytes <= JSON_INLINE_MAX && bytes <= active.budget.max_bytes {
            ("json", json!({ "json": value }), bytes)
        } else {
            let gz = gzip(text.as_bytes());
            let n = gz.len();
            let data = base64::engine::general_purpose::STANDARD.encode(&gz);
            ("gzip+base64", json!({ "data": data }), n)
        };

    let mut payload = json!({
        "capture_id": active.req.capture_id,
        "kind": kind_name(&active.req.kind),
        "encoding": encoding,
        "content_type": "application/json",
        "bytes": bytes,
        "compressed_bytes": compressed_bytes,
        "original_bytes": original_bytes,
        "truncated": truncated,
        "duration_ms": duration_ms,
        "error": error,
    });
    if let (Some(obj), Some(extra)) = (payload.as_object_mut(), body.as_object()) {
        for (k, v) in extra {
            obj.insert(k.clone(), v.clone());
        }
    }
    CaptureReport {
        event: event_name(&active.req.kind),
        payload,
        capture_id: active.req.capture_id.clone(),
        kind: kind_name(&active.req.kind),
        compressed_bytes,
        duration_ms,
        error,
    }
}

fn gzip(bytes: &[u8]) -> Vec<u8> {
    let mut enc = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    // In-memory writer: neither write nor finish can fail for a Vec sink, but a
    // capture must never panic the runner thread, so both are handled.
    if enc.write_all(bytes).is_err() {
        return Vec::new();
    }
    enc.finish().unwrap_or_default()
}

/// Truncate `text` at a line boundary until its GZIPPED form fits `max_bytes`.
///
/// Halving rather than a linear walk: compression ratio is not a function we can
/// invert, so each step measures. Bounded by `log2(lines)` compressions of a body
/// that is already shrinking.
fn fit_text(text: String, max_bytes: usize) -> (String, bool) {
    if gzip(text.as_bytes()).len() <= max_bytes {
        return (text, false);
    }
    let lines: Vec<&str> = text.split_inclusive('\n').collect();
    let (mut lo, mut hi) = (0usize, lines.len());
    let mut best = String::new();
    while lo < hi {
        let mid = lo + (hi - lo).div_ceil(2);
        let candidate: String = lines[..mid].concat();
        if gzip(candidate.as_bytes()).len() <= max_bytes {
            best = candidate;
            lo = mid;
        } else {
            hi = mid - 1;
        }
    }
    (best, true)
}

/// Make a JSON result fit by removing content, worst-value-first.
fn shrink_json(mut value: Value, max_bytes: usize) -> (Value, bool) {
    let fits = |v: &Value| serde_json::to_string(v).map(|s| s.len()).unwrap_or(0) <= max_bytes;
    if fits(&value) {
        return (value, false);
    }
    // 1. Raw sample arrays: the bulk of a burst, and fully described by the
    //    percentiles that stay.
    if let Some(windows) = value.get_mut("windows").and_then(|w| w.as_array_mut()) {
        for w in windows.iter_mut() {
            for key in ["encode_ms", "dwell_ms"] {
                if let Some(s) = w.get_mut(key).and_then(|s| s.as_object_mut()) {
                    s.remove("samples");
                }
            }
        }
    }
    if fits(&value) {
        return (value, true);
    }
    // 2. Sub-windows from the end, the oldest being the more useful half.
    loop {
        if fits(&value) {
            break;
        }
        let popped = value
            .get_mut("windows")
            .and_then(|w| w.as_array_mut())
            .is_some_and(|w| w.len() > 1 && w.pop().is_some());
        if !popped {
            break;
        }
    }
    (value, true)
}

#[cfg(test)]
mod tests;
