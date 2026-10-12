//! #267: an encoder's GStreamer plugin is loaded before a session's compositor starts,
//! and a fault during any plugin load stays a SIGSEGV.
//!
//! In a binary of its own because whether a plugin is loaded is process-global, and the
//! lib's unit tests load plugins in whatever order they run. The fault itself (two
//! threads in one GPU driver) needs the hardware; these pin what the fix is made of.

use gstreamer as gst;
use gstreamer::prelude::*;
use quasar_node_agent::session::{self, pipeline::preload_encoder_plugins, SessionConfig};

/// In gst-plugins-base, so registered on every image the suite runs on; its plugin
/// carries the same name.
const ELEMENT: &str = "videotestsrc";

fn plugin_loaded(element: &str) -> bool {
    gst::ElementFactory::find(element)
        .and_then(|f| f.plugin())
        .is_some_and(|p| p.is_loaded())
}

#[test]
fn init_turns_segtrap_off_and_the_preload_loads_what_is_registered() {
    session::init_gstreamer(&SessionConfig::from_env(true, None)).unwrap();
    assert!(
        !gst::segtrap_is_enabled(),
        "segtrap on: a fault during a plugin load exits 255 blamed on that plugin"
    );

    // What makes the preload necessary: the scan registered the element without
    // running its plugin's init in this process.
    assert!(
        !plugin_loaded(ELEMENT),
        "the registry scan loaded {ELEMENT}'s plugin in-process (no gst-plugin-scanner \
         helper?), so this run cannot show the preload doing anything"
    );

    assert_eq!(
        preload_encoder_plugins(&["quasar-no-such-element", ELEMENT]),
        [ELEMENT],
        "an unregistered element is skipped, a registered one has its plugin loaded"
    );
    assert!(plugin_loaded(ELEMENT));
    assert!(preload_encoder_plugins(&[ELEMENT]).is_empty());
}

fn source(rel: &str) -> String {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("src")
        .join(rel);
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

/// `src` from `from` on, or a failure naming what moved.
fn after<'a>(src: &'a str, from: &str) -> &'a str {
    &src[src
        .find(from)
        .unwrap_or_else(|| panic!("`{from}` not found: update this test with the rename"))..]
}

fn offset(src: &str, needle: &str) -> usize {
    src.len() - after(src, needle).len()
}

/// The order the fix depends on, which no unit test can run: both sites sit in
/// functions that need a control plane or a compositor.
#[test]
fn the_preload_runs_before_a_session_can_start() {
    let agent = source("agent.rs");
    let probe = after(&agent, "\nfn probe_host_codecs(");
    let probe = &probe[..offset(probe, "\n}\n")];
    assert!(
        probe.contains("preload_encoder_plugins("),
        "the codec probe no longer preloads the encoders it reports"
    );
    // The probe's report goes into the first capacity report, and only the loop after
    // that report handles an assignment.
    let connect = after(&agent, "\nasync fn connect_and_run(");
    assert!(
        offset(connect, "probe_host_codecs(") < offset(connect, "mgr.handle_control("),
        "connect_and_run handles control messages before the codec probe has run"
    );

    let runner = source("session/runner.rs");
    let run = after(&runner, "\npub fn run_blocking(");
    assert!(
        offset(run, "preload_encoder_plugins(") < offset(run, "start_first_source("),
        "run_blocking starts the compositor source before its encoder's plugin is loaded"
    );
}
