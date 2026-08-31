//! `session::capture` unit tests.
//!
//! The positive cases prove the seam produces the shapes the wire spec promises.
//! The NEGATIVE cases are the point of the file: they are the executable form of
//! the safety envelope, and each one names the leak it forbids.

use super::*;
use crate::session::metrics::SessionMetrics;

// ── helpers ─────────────────────────────────────────────────────────────────────

fn gst_ready() -> bool {
    gst::init().is_ok()
}

fn metrics() -> SessionMetrics {
    SessionMetrics::new("off", 60)
}

fn req(kind: CaptureKind) -> CaptureRequest {
    CaptureRequest {
        capture_id: "cap-1".to_string(),
        kind,
        budget: CaptureBudget::default(),
        params: CaptureParams::default(),
        slot: CaptureSlot::new(),
    }
}

/// A tiny, GPU-free pipeline shaped like the real one: a source, an "encoder"
/// stand-in (`identity` — a real one needs a GPU/plugins the verify container
/// lacks, and the seam only reads properties + caps, which `identity` has too),
/// and a sink — plus one element carrying a path-valued PROPERTY for the
/// `NON_DEFAULT_PARAMS` negative test to leak.
///
/// The path lives on `filesink.location`, a genuine string property, not an
/// element NAME: names appear in a dot graph as node identifiers under every
/// detail flag, so a name-based fixture couldn't distinguish "flags leaked" from
/// "graphviz labelled a node".
fn test_pipeline(marker: &str) -> Option<(gst::Pipeline, gst::Element)> {
    if !gst_ready() {
        return None;
    }
    let pipeline = gst::Pipeline::new();
    let src = gst::ElementFactory::make("fakesrc").build().ok()?;
    let enc = gst::ElementFactory::make("identity")
        .name("quasar-video-encoder")
        .build()
        .ok()?;
    let sink = gst::ElementFactory::make("fakesink").build().ok()?;
    pipeline.add_many([&src, &enc, &sink]).ok()?;
    gst::Element::link_many([&src, &enc, &sink]).ok()?;
    // Unlinked and never started — present purely so the graph HAS a path-valued
    // property to leak. `filesink` in NULL touches no filesystem.
    if let Ok(leaky) = gst::ElementFactory::make("filesink")
        .property("location", secret_path(marker))
        .build()
    {
        let _ = pipeline.add(&leaky);
    }
    Some((pipeline, enc))
}

fn secret_path(marker: &str) -> String {
    format!("/var/run/secrets/{marker}")
}

fn ctx<'a>(
    pipeline: &'a gst::Pipeline,
    encoder: Option<&'a gst::Element>,
    m: &'a SessionMetrics,
) -> CaptureCtx<'a> {
    CaptureCtx {
        encode_pipe: pipeline,
        encoder,
        metrics: m,
        stage: json!({ "external_resize_supported": false }),
    }
}

/// Poll until the capture completes, with a bounded number of ticks so a bug is a
/// failing test rather than a hung suite.
fn drive(capture: &mut Capture, c: &CaptureCtx<'_>, ticks: usize) -> Option<CaptureReport> {
    for _ in 0..ticks {
        if let Some(report) = capture.poll(c) {
            return Some(report);
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    None
}

fn payload_text(report: &CaptureReport) -> String {
    let data = report.payload["data"].as_str().expect("gzip+base64 data");
    let gz = base64::engine::general_purpose::STANDARD
        .decode(data)
        .expect("valid base64");
    let mut out = String::new();
    use std::io::Read as _;
    flate2::read::GzDecoder::new(&gz[..])
        .read_to_string(&mut out)
        .expect("valid gzip");
    out
}

// ── admission / single-flight ───────────────────────────────────────────────────

#[test]
fn admit_refuses_an_unknown_kind_without_taking_the_slot() {
    let slot = CaptureSlot::new();
    let kind = CaptureKind::Other("bitstream_dump".to_string());
    assert_eq!(
        admit(&kind, true, &slot),
        Err(CaptureRefusal::UnknownKind),
        "a kind this build does not implement must be unknown_kind, never run"
    );
    assert!(
        !slot.is_busy(),
        "a refused capture must not consume the session's single-flight slot"
    );
}

#[test]
fn admit_refuses_a_session_with_no_encode_pipeline_as_unsupported() {
    let slot = CaptureSlot::new();
    assert_eq!(
        admit(&CaptureKind::PipelineDot, false, &slot),
        Err(CaptureRefusal::Unsupported)
    );
    assert!(!slot.is_busy());
}

#[test]
fn admit_is_single_flight_and_the_second_request_is_busy() {
    let slot = CaptureSlot::new();
    assert_eq!(admit(&CaptureKind::PipelineDot, true, &slot), Ok(()));
    assert!(slot.is_busy());
    assert_eq!(
        admit(&CaptureKind::EncoderProps, true, &slot),
        Err(CaptureRefusal::Busy),
        "a capture in flight must be refused, never queued"
    );
    slot.release();
    assert_eq!(admit(&CaptureKind::EncoderProps, true, &slot), Ok(()));
}

#[test]
fn a_completed_capture_returns_the_slot() {
    let Some((pipeline, enc)) = test_pipeline("slot") else {
        return;
    };
    let m = metrics();
    let slot = CaptureSlot::new();
    assert_eq!(admit(&CaptureKind::PipelineDot, true, &slot), Ok(()));
    let mut r = req(CaptureKind::PipelineDot);
    r.slot = slot.clone();
    let mut capture = Capture::new();
    capture.arm(r, &m).unwrap();
    assert!(slot.is_busy());
    assert!(drive(&mut capture, &ctx(&pipeline, Some(&enc), &m), 5).is_some());
    assert!(
        !slot.is_busy(),
        "the slot must be free the moment the capture completes"
    );
}

#[test]
fn dropping_an_armed_capture_returns_the_slot() {
    let m = metrics();
    let slot = CaptureSlot::new();
    assert_eq!(admit(&CaptureKind::BurstStats, true, &slot), Ok(()));
    {
        let mut r = req(CaptureKind::BurstStats);
        r.slot = slot.clone();
        let mut capture = Capture::new();
        capture.arm(r, &m).unwrap();
        assert!(slot.is_busy());
    }
    assert!(
        !slot.is_busy(),
        "a runner that exits mid-capture must not leave the session permanently busy"
    );
}

// ── pipeline_dot ────────────────────────────────────────────────────────────────

#[test]
fn pipeline_dot_produces_a_graphviz_payload() {
    let Some((pipeline, enc)) = test_pipeline("dot") else {
        return;
    };
    let m = metrics();
    let mut capture = Capture::new();
    capture.arm(req(CaptureKind::PipelineDot), &m).unwrap();
    let report = drive(&mut capture, &ctx(&pipeline, Some(&enc), &m), 5).expect("one-shot");

    assert_eq!(report.event, "diag.pipeline_dot");
    assert_eq!(report.payload["kind"], "pipeline_dot");
    assert_eq!(report.payload["encoding"], "gzip+base64");
    assert_eq!(report.payload["content_type"], "text/vnd.graphviz");
    assert_eq!(report.payload["capture_id"], "cap-1");
    assert_eq!(report.payload["truncated"], false);
    let text = payload_text(&report);
    assert!(
        text.contains("digraph"),
        "not a graphviz document: {text:.200}"
    );
}

/// SAFETY: the dot dump must never render element property VALUES.
///
/// `NON_DEFAULT_PARAMS` / `FULL_PARAMS` print them, and string properties are where
/// socket/device paths and URIs live. With `CAPS_DETAILS | STATES` the path appears
/// only as a node identifier, never a rendered parameter row.
#[test]
fn dot_details_never_include_non_default_params() {
    let Some((pipeline, enc)) = test_pipeline("nodump") else {
        return;
    };
    let m = metrics();
    let mut capture = Capture::new();
    capture.arm(req(CaptureKind::PipelineDot), &m).unwrap();
    let report = drive(&mut capture, &ctx(&pipeline, Some(&enc), &m), 5).expect("one-shot");
    let text = payload_text(&report);

    let secret = secret_path("nodump");
    // Prove the fixture has something to leak, or the negative below passes for
    // the wrong reason.
    let leaky = pipeline
        .debug_to_dot_data(
            gst::DebugGraphDetails::CAPS_DETAILS
                | gst::DebugGraphDetails::STATES
                | gst::DebugGraphDetails::NON_DEFAULT_PARAMS,
        )
        .to_string();
    assert!(
        leaky.contains(&secret),
        "fixture is not exercising the rule — NON_DEFAULT_PARAMS did not render the path"
    );
    assert!(
        !text.contains(&secret),
        "a path-valued property reached the capture payload — the detail flags leaked"
    );
}

/// The compressed payload must respect `budget.max_bytes`, and truncation must
/// happen at a LINE boundary so the survivor is still a readable document.
#[test]
fn text_truncation_respects_the_cap_and_cuts_at_a_line_boundary() {
    let body: String = (0..20_000)
        .map(|i| format!("line-{i}-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"))
        .collect();
    let cap = 4_096;
    let (fitted, truncated) = fit_text(body.clone(), cap);

    assert!(truncated, "a body far over the cap must report truncated");
    assert!(
        gzip(fitted.as_bytes()).len() <= cap,
        "the COMPRESSED payload must be at or under the cap"
    );
    assert!(!fitted.is_empty(), "truncation must keep what it can");
    assert!(
        fitted.ends_with('\n'),
        "truncation must land on a line boundary, not mid-token"
    );
    assert!(
        body.starts_with(&fitted),
        "truncation must be a prefix of the original, never a reflow"
    );
}

#[test]
fn text_under_the_cap_is_not_marked_truncated() {
    let (fitted, truncated) = fit_text("digraph { a -> b }\n".to_string(), 262_144);
    assert!(!truncated);
    assert_eq!(fitted, "digraph { a -> b }\n");
}

// ── encoder_props ───────────────────────────────────────────────────────────────

#[test]
fn encoder_props_reports_caps_and_the_allow_listed_properties_only() {
    let Some((pipeline, enc)) = test_pipeline("props") else {
        return;
    };
    let m = metrics();
    let mut capture = Capture::new();
    capture.arm(req(CaptureKind::EncoderProps), &m).unwrap();
    let report = drive(&mut capture, &ctx(&pipeline, Some(&enc), &m), 5).expect("one-shot");

    assert_eq!(report.event, "diag.encoder_props");
    assert_eq!(report.payload["encoding"], "json");
    assert_eq!(report.payload["content_type"], "application/json");
    let body = &report.payload["json"];
    assert_eq!(body["encoder_factory"], "identity");
    assert!(body["caps"].is_object());
    assert!(body["scale_stage"].is_object());

    let props = body["properties"].as_object().expect("properties object");
    for name in props.keys() {
        assert!(
            ENCODER_PROP_ALLOW.contains(&name.as_str()),
            "{name} is not on the allow-list but reached the payload"
        );
    }
}

/// SAFETY: the allow-list is a *filter*, not documentation. A property that is not
/// on it is never read, whatever it is called and whatever it holds.
#[test]
fn a_property_off_the_allow_list_is_never_read() {
    let Some((_pipeline, enc)) = test_pipeline("deny") else {
        return;
    };
    // `identity` really does have these, and none of them are on the list.
    for banned in ["name", "dump", "silent", "last-message", "parent"] {
        assert!(
            read_allowed_property(&enc, banned).is_none(),
            "{banned} is off the allow-list and must not be readable through this seam"
        );
    }
}

/// SAFETY: a long string value is elided rather than emitted. Long strings are
/// where paths, URIs and blobs hide; the useful encoder knobs are short.
#[test]
fn a_long_string_property_value_is_elided() {
    let short = elide_long("constrained-baseline".to_string());
    assert_eq!(short, Value::String("constrained-baseline".to_string()));

    let long = "/tmp/".to_string() + &"x".repeat(MAX_PROP_STRING);
    let elided = elide_long(long.clone());
    let rendered = elided.as_str().expect("a string");
    assert!(
        !rendered.contains("xxxx"),
        "the long value itself must not survive: {rendered}"
    );
    assert!(
        rendered.contains(&long.len().to_string()),
        "the LENGTH is kept"
    );
}

#[test]
fn an_absent_property_is_not_an_error() {
    let Some((_pipeline, enc)) = test_pipeline("absent") else {
        return;
    };
    // `find_property` first is load-bearing: reading an absent property on a gst
    // element panics, and a panic on the runner thread kills the session.
    assert!(read_allowed_property(&enc, "bitrate").is_none());
}

#[test]
fn encoder_props_without_an_encoder_reports_an_error_not_a_panic() {
    let Some((pipeline, _enc)) = test_pipeline("noenc") else {
        return;
    };
    let m = metrics();
    let mut capture = Capture::new();
    capture.arm(req(CaptureKind::EncoderProps), &m).unwrap();
    let report = drive(&mut capture, &ctx(&pipeline, None, &m), 5).expect("one-shot");
    assert_eq!(report.error, Some("no_encoder"));
}

// ── burst_stats ─────────────────────────────────────────────────────────────────

#[test]
fn burst_plan_clamps_windows_and_window_ms() {
    let huge = BurstPlan::resolve(
        CaptureParams {
            windows: Some(10_000),
            window_ms: Some(5),
        },
        60_000,
    );
    assert_eq!(huge.windows, BurstPlan::MAX_WINDOWS);
    assert_eq!(huge.window_ms, BurstPlan::MIN_WINDOW_MS);

    let slow = BurstPlan::resolve(
        CaptureParams {
            windows: Some(1),
            window_ms: Some(999_999),
        },
        60_000,
    );
    assert_eq!(slow.window_ms, BurstPlan::MAX_WINDOW_MS);
    assert_eq!(slow.windows, 1);
}

#[test]
fn burst_plan_never_exceeds_the_wall_clock_budget() {
    let plan = BurstPlan::resolve(
        CaptureParams {
            windows: Some(40),
            window_ms: Some(1_000),
        },
        2_500,
    );
    assert!(
        plan.total_ms() <= 2_500,
        "windows x window_ms ({}) overran the budget",
        plan.total_ms()
    );
    assert_eq!(plan.windows, 2);
}

#[test]
fn burst_plan_always_yields_at_least_one_window() {
    let plan = BurstPlan::resolve(
        CaptureParams {
            windows: Some(20),
            window_ms: Some(1_000),
        },
        // Below one window: the plan still returns one, and the deadline truncates.
        100,
    );
    assert_eq!(plan.windows, 1);
}

#[test]
fn burst_plan_defaults_match_the_wire_spec() {
    let plan = BurstPlan::resolve(CaptureParams::default(), 10_000);
    assert_eq!(plan.windows, 20);
    assert_eq!(plan.window_ms, 250);
}

#[test]
fn burst_stats_collects_its_windows_and_reports_no_error() {
    let Some((pipeline, enc)) = test_pipeline("burst") else {
        return;
    };
    let m = metrics();
    let mut r = req(CaptureKind::BurstStats);
    r.params = CaptureParams {
        windows: Some(2),
        window_ms: Some(100),
    };
    r.budget = CaptureBudget {
        max_bytes: 262_144,
        max_ms: 5_000,
    };
    let mut capture = Capture::new();
    capture.arm(r, &m).unwrap();
    let report = drive(&mut capture, &ctx(&pipeline, Some(&enc), &m), 100).expect("burst finishes");

    assert_eq!(report.event, "diag.burst_stats");
    assert_eq!(report.error, None);
    let body = &report.payload["json"];
    assert_eq!(body["window_ms"], 100);
    assert_eq!(body["windows"].as_array().expect("windows").len(), 2);
}

/// A burst that cannot finish inside `budget.max_ms` reports what it has, flagged
/// `error: "deadline"` — never nothing, and never an overrun.
#[test]
fn burst_stats_hits_the_deadline_and_reports_what_it_has() {
    let Some((pipeline, enc)) = test_pipeline("deadline") else {
        return;
    };
    let m = metrics();
    let mut r = req(CaptureKind::BurstStats);
    // 40 windows of 1000 ms cannot fit a 300 ms budget; the plan clamps to what the
    // budget affords, and the deadline is what actually stops it.
    r.params = CaptureParams {
        windows: Some(40),
        window_ms: Some(1_000),
    };
    r.budget = CaptureBudget {
        max_bytes: 262_144,
        max_ms: 300,
    };
    let mut capture = Capture::new();
    capture.arm(r, &m).unwrap();
    let started = Instant::now();
    let report = drive(&mut capture, &ctx(&pipeline, Some(&enc), &m), 200).expect("deadline fires");

    assert_eq!(report.error, Some("deadline"));
    assert!(
        started.elapsed() < Duration::from_secs(3),
        "the deadline must stop the capture, not the window plan"
    );
    assert!(
        !capture.is_active(),
        "a deadlined capture must not stay armed"
    );
}

#[test]
fn a_burst_series_reports_only_the_samples_since_the_previous_snapshot() {
    let all = [1.0, 2.0, 3.0, 4.0, 5.0];
    let v = series(3, &all);
    assert_eq!(v["n"], 2);
    assert_eq!(v["samples"], json!([4.0, 5.0]));

    // A heartbeat drain landing mid-burst shortens the vector; that must degrade to
    // "report what is here", never index off the end.
    let short = series(9, &all);
    assert_eq!(short["n"], 5);
}

#[test]
fn a_burst_series_caps_the_raw_samples() {
    let many: Vec<f64> = (0..1_000).map(|i| i as f64).collect();
    let v = series(0, &many);
    assert_eq!(v["n"], 1_000, "the COUNT is the true count");
    assert_eq!(
        v["samples"].as_array().expect("samples").len(),
        MAX_BURST_SAMPLES,
        "raw samples are capped even though the summary covers everything"
    );
}

#[test]
fn snapshot_burst_does_not_disturb_the_heartbeat_window() {
    let m = metrics();
    let t0 = Instant::now();
    m.record_encode_in(t0);
    m.record_encode_out(t0 + Duration::from_millis(2), 1_000);

    let snap = m.snapshot_burst(Instant::now());
    assert_eq!(snap.encode_ms.len(), 1, "the snapshot sees the sample");

    // The heartbeat drain must still see it — the capture read nothing away.
    let window = m.drain_window(Instant::now());
    assert_eq!(window.frames_encoded, 1);
    assert!(
        window.encode_ms.is_some(),
        "snapshot_burst stole the heartbeat window's encode samples"
    );
}

// ── payload shaping ─────────────────────────────────────────────────────────────

#[test]
fn an_oversize_json_result_drops_raw_samples_before_windows() {
    let window = |i: usize| {
        json!({
            "t_ms": i * 100,
            "encode_ms": { "n": 200, "p50": 2.0, "samples": (0..200).collect::<Vec<i32>>() },
            "dwell_ms": { "n": 200, "p50": 1.0, "samples": (0..200).collect::<Vec<i32>>() },
        })
    };
    let value = json!({
        "window_ms": 100,
        "windows": (0..40).map(window).collect::<Vec<_>>(),
    });
    let full = serde_json::to_string(&value).unwrap().len();

    let (shrunk, truncated) = shrink_json(value, full / 4);
    assert!(truncated);
    assert!(serde_json::to_string(&shrunk).unwrap().len() <= full / 4);
    let windows = shrunk["windows"].as_array().unwrap();
    assert!(!windows.is_empty(), "at least one window must survive");
    assert!(
        windows[0]["encode_ms"].get("samples").is_none(),
        "raw samples are dropped before whole windows are"
    );
    assert_eq!(
        windows[0]["encode_ms"]["p50"], 2.0,
        "the percentiles that describe the dropped samples must survive"
    );
}

#[test]
fn a_json_result_that_fits_is_left_alone() {
    let value = json!({ "windows": [] });
    let (out, truncated) = shrink_json(value.clone(), 262_144);
    assert!(!truncated);
    assert_eq!(out, value);
}

#[test]
fn the_budget_is_clamped_to_something_runnable() {
    let clamped = clamp_budget(CaptureBudget {
        max_bytes: 1,
        max_ms: 0,
    });
    assert!(clamped.max_bytes >= MIN_BUDGET_BYTES);
    assert!(clamped.max_ms >= MIN_BUDGET_MS);

    let clamped = clamp_budget(CaptureBudget {
        max_bytes: usize::MAX,
        max_ms: u64::MAX,
    });
    assert_eq!(clamped.max_ms, MAX_BUDGET_MS);
}

// ── the envelope, as a grep ─────────────────────────────────────────────────────

/// SAFETY: no new pad probes, and no blocking waits, in the capture seam.
///
/// A grep over this module's own source, not a mock — the ban is on the
/// *technique*. #270 removed the host-side probe because it could crash a
/// stream; a future `add_probe` here should fail a test, not a review.
#[test]
fn the_capture_seam_adds_no_pad_probes_and_never_blocks() {
    let src = include_str!("../capture.rs");
    for banned in ["add_probe", "PadProbe", "thread::sleep", "blocking_recv"] {
        assert!(
            !src.contains(banned),
            "`{banned}` appeared in session/capture.rs — this seam runs on the runner \
             tick and must never probe a pad or block"
        );
    }
}

/// SAFETY: the seam never reads the environment wholesale, and never names a
/// secret. One allow-listed variable (`WOLF_VULKAN_RING`) is read by name.
#[test]
fn the_capture_seam_reads_no_secrets_and_no_bulk_environment() {
    let src = include_str!("../capture.rs");
    assert!(
        !src.contains("env::vars"),
        "the environment must never be read wholesale"
    );
    for secret in ["node_secret", "enrollment_token", "QUASAR_DEV_AGENT_AUTH"] {
        assert!(
            !src.contains(secret),
            "`{secret}` must never be reachable from a capture"
        );
    }
    assert_eq!(
        src.matches("std::env::var(").count(),
        1,
        "exactly one named environment read (WOLF_VULKAN_RING) is allowed here"
    );
}
