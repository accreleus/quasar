use quasar_node_agent::{agent, config, memstat, session};

use session::SessionConfig;

/// What this invocation should do.
enum Mode {
    /// The production role: connect to the control plane (register / capacity / heartbeat).
    Agent,
    /// Dev path: one session behind a direct WebSocket signaling server. `--image` (or
    /// `QUASAR_APP_IMAGE`) launches that container; otherwise `--test-src` streams
    /// videotestsrc.
    Session {
        addr: String,
        use_test_src: bool,
        stun: Option<String>,
        image: Option<String>,
    },
    /// Reach ICE-connected against a running `session` server without a browser.
    SessionAnswerer { url: String },
    /// Synthetic input events into a live compositor; no browser or GPU needed.
    InjectSelfTest,
    /// Create the uinput devices, print their evdev nodes, emit events. Needs /dev/uinput.
    VirtualInputSelfTest,
    /// EGL dispatcher self-test. Spawned as a CHILD by the readiness probe
    /// (`nvidia_volume::probe_egl_runtime`) so a segfault in a broken vendor stack cannot
    /// take the agent down.
    EglSelfTest { vendor_lib: Option<String> },
    /// #500: one throwaway-home sweep, then exit. Same knobs, guards and code path as the
    /// daily timer; `make homes-gc` execs it in the running agent container.
    HomesGc { dry_run: bool },
    /// Builds the encoder branch through the same code a session uses. A hand-typed
    /// `gst-launch` probe shares no code with production and negotiated `profile=main-444`,
    /// which read as a driver regression.
    ProbeEncoder {
        codec: String,
        width: i32,
        height: i32,
        fps: i32,
        seconds: u64,
        json: bool,
    },
}

/// Parse `--size 1920x1080@60`. Any part omitted keeps the default.
fn parse_size(raw: Option<&str>) -> (i32, i32, i32) {
    let (mut w, mut h, mut fps) = (1920, 1080, 60);
    let Some(raw) = raw else { return (w, h, fps) };
    let (dims, rate) = match raw.split_once('@') {
        Some((d, r)) => (d, Some(r)),
        None => (raw, None),
    };
    if let Some((a, b)) = dims.split_once('x') {
        if let (Ok(a), Ok(b)) = (a.parse(), b.parse()) {
            w = a;
            h = b;
        }
    }
    if let Some(Ok(r)) = rate.map(str::parse) {
        fps = r;
    }
    (w, h, fps)
}

fn parse_args() -> Mode {
    let args: Vec<String> = std::env::args().skip(1).collect();

    match args.first().map(String::as_str) {
        Some("session") => {
            let addr = arg_value(&args, "--addr").unwrap_or_else(|| "0.0.0.0:8443".to_string());
            let stun = arg_value(&args, "--stun");
            let use_test_src = args.iter().any(|a| a == "--test-src");
            let image = arg_value(&args, "--image");
            Mode::Session {
                addr,
                use_test_src,
                stun,
                image,
            }
        }
        Some("session-answerer") => {
            let url = args
                .get(1)
                .filter(|s| !s.starts_with("--"))
                .cloned()
                .unwrap_or_else(|| "ws://127.0.0.1:8443".to_string());
            Mode::SessionAnswerer { url }
        }
        Some(quasar_node_agent::nvidia_volume::EGL_SELFTEST_ARG) => Mode::EglSelfTest {
            vendor_lib: args.get(1).filter(|s| !s.starts_with("--")).cloned(),
        },
        Some("homes-gc") => Mode::HomesGc {
            dry_run: args.iter().any(|a| a == "--dry-run"),
        },
        Some("probe-encoder") => {
            let (width, height, fps) = parse_size(arg_value(&args, "--size").as_deref());
            Mode::ProbeEncoder {
                codec: arg_value(&args, "--codec").unwrap_or_else(|| "h264".to_string()),
                width,
                height,
                fps,
                seconds: arg_value(&args, "--seconds")
                    .and_then(|s| s.parse().ok())
                    .unwrap_or(2),
                json: args.iter().any(|a| a == "--json"),
            }
        }
        Some("inject-selftest") => Mode::InjectSelfTest,
        Some("vinput-selftest") => Mode::VirtualInputSelfTest,
        _ => Mode::Agent,
    }
}

/// Fetch the value following `flag` (e.g. `--addr 0.0.0.0:8443`).
fn arg_value(args: &[String], flag: &str) -> Option<String> {
    let pos = args.iter().position(|a| a == flag)?;
    args.get(pos + 1).filter(|s| !s.starts_with("--")).cloned()
}

#[tokio::main]
async fn main() {
    // Must run before the subscriber and any other setup: its stdout contract is
    // `KEY=value` lines only, and it must observe the loader in a virgin process.
    if let Mode::EglSelfTest { vendor_lib } = parse_args() {
        std::process::exit(quasar_node_agent::nvidia_volume::egl_selftest_main(
            vendor_lib.as_deref(),
        ));
    }

    // Logs go to stderr: stdout belongs to machine-readable subcommands
    // (`probe-encoder --json`). Format knob: QUASAR_LOG_FORMAT.
    quasar_node_agent::logging::init_subscriber();

    install_sigusr1_fallback();

    match parse_args() {
        Mode::Agent => spawn_agent().await,
        Mode::Session {
            addr,
            use_test_src,
            stun,
            image,
        } => run_session(addr, use_test_src, stun, image).await,
        Mode::SessionAnswerer { url } => run_session_answerer(url).await,
        Mode::HomesGc { dry_run } => run_homes_gc(dry_run),
        Mode::ProbeEncoder {
            codec,
            width,
            height,
            fps,
            seconds,
            json,
        } => run_probe_encoder(&codec, width, height, fps, seconds, json),
        Mode::InjectSelfTest => run_inject_selftest(),
        Mode::VirtualInputSelfTest => run_vinput_selftest(),
        // Handled above, before the subscriber is installed.
        Mode::EglSelfTest { .. } => unreachable!(),
    }
}

/// SIGUSR1 must never be fatal (#429). The soak harness signals USR1 for the leaks tracer's
/// checkpoint dump, but the tracer installs its handler only when it loads; with no handler
/// the default disposition terminates the process, indistinguishable from a crash. Install
/// this before `gst::init()` so the tracer's raw `signal()` replaces ours when it does load.
fn install_sigusr1_fallback() {
    use tokio::signal::unix::{signal, SignalKind};
    match signal(SignalKind::user_defined1()) {
        Ok(mut sig) => {
            tokio::spawn(async move {
                while sig.recv().await.is_some() {
                    tracing::info!(
                        "SIGUSR1 received — no GStreamer leaks-tracer handler is active, ignoring \
                         (a leaks checkpoint was requested but the tracer did not load)"
                    );
                }
            });
        }
        Err(e) => tracing::warn!(
            token = "sigusr1-handler-install-failed",
            "could not install SIGUSR1 fallback handler: {e}"
        ),
    }
}

/// Fail-fast on env combinations compose silently gets wrong, rather than letting sessions
/// fail at runtime.
fn check_env_sanity() {
    let encoder = std::env::var("QUASAR_ENCODER").unwrap_or_default();
    let is_va = encoder.eq_ignore_ascii_case("va") || encoder.eq_ignore_ascii_case("vaapi");

    if is_va {
        let mesa_override = std::env::var("MESA_LOADER_DRIVER_OVERRIDE").unwrap_or_default();
        if mesa_override.contains("softpipe") {
            // softpipe forces a software Mesa path with no VA encode entrypoint, hiding
            // the VA encoder even with /dev/dri present.
            tracing::error!(
                token = "boot-va-mesa-override-conflict",
                "QUASAR_ENCODER=va conflicts with MESA_LOADER_DRIVER_OVERRIDE={} — \
                 softpipe hides the VA encoder. Unset MESA_LOADER_DRIVER_OVERRIDE \
                 (and LIBGL_ALWAYS_SOFTWARE) in deploy/.env, then restart.",
                mesa_override
            );
            std::process::exit(1);
        }
        if !std::path::Path::new("/dev/dri").exists() {
            tracing::error!(
                token = "boot-va-no-dri",
                "QUASAR_ENCODER=va requires /dev/dri but the device is not present. \
                 Either mount /dev/dri in docker-compose.yml (uncomment the devices: \
                 entry) or switch to QUASAR_ENCODER=openh264 for software encode."
            );
            std::process::exit(1);
        }
    }
}

/// #532: the control loop must run as a SPAWNED task, never as the `#[tokio::main]` future.
/// Tokio emits `runtime.spawn` spans only for spawned tasks, so on the main future the
/// busiest future in the process is absent from tokio-console, not merely slow. Panic
/// behaviour is unchanged: a panic still aborts, re-raised from the `JoinError`.
async fn spawn_agent() {
    join_agent(tokio::spawn(run_agent())).await
}

/// Split out so the spawn contract is testable without the real agent, which never returns.
async fn join_agent(handle: tokio::task::JoinHandle<()>) {
    match handle.await {
        Ok(()) => {}
        Err(e) if e.is_panic() => std::panic::resume_unwind(e.into_panic()),
        Err(e) => {
            tracing::error!(
                token = "agent-task-cancelled",
                "agent control task did not complete: {e}"
            );
            std::process::exit(1);
        }
    }
}

async fn run_agent() {
    check_env_sanity();

    let cfg = match config::Config::from_env() {
        Ok(c) => c,
        Err(e) => {
            eprintln!("config error: {e}");
            std::process::exit(1);
        }
    };

    tracing::info!(
        "quasar node-agent {} starting (node_name={})",
        env!("CARGO_PKG_VERSION"),
        cfg.node_name
    );
    // #419: records the allocator A/B arm plus baseline RSS, so a soak artifact is
    // self-describing.
    memstat::log_startup();

    agent::run(cfg).await;
}

async fn run_session(
    addr: String,
    use_test_src: bool,
    stun: Option<String>,
    image: Option<String>,
) {
    // `--image` is sugar for QUASAR_APP_IMAGE, which `SessionConfig::from_env` reads.
    if let Some(img) = image {
        std::env::set_var("QUASAR_APP_IMAGE", img);
    }
    let cfg = SessionConfig::from_env(use_test_src, stun);
    // GST_REGISTRY for the VA encoder must be set before `gst::init()`.
    if let Err(e) = session::init_gstreamer(&cfg) {
        eprintln!("gstreamer init failed: {e:#}");
        std::process::exit(1);
    }
    tracing::info!(
        "quasar node-agent {} — P1-5 session (direct signaling)",
        env!("CARGO_PKG_VERSION")
    );
    if let Err(e) = session::server::serve_direct(&addr, cfg).await {
        tracing::error!(
            token = "session-server-exited",
            "session server exited: {e:#}"
        );
        std::process::exit(1);
    }
}

async fn run_session_answerer(url: String) {
    let cfg = SessionConfig::from_env(false, None);
    if let Err(e) = session::init_gstreamer(&cfg) {
        eprintln!("gstreamer init failed: {e:#}");
        std::process::exit(1);
    }
    tracing::info!("quasar node-agent — loopback answerer");
    if let Err(e) = session::server::run_answerer(&url).await {
        tracing::error!(token = "answerer-exited", "answerer exited: {e:#}");
        std::process::exit(1);
    }
}

/// `quasar-node-agent homes-gc [--dry-run]`. Non-zero exit means the sweep could not run at
/// all, so the DX verb can tell that apart from "ran, deleted nothing".
fn run_homes_gc(dry_run: bool) {
    match session::homes_gc::run_once(dry_run) {
        Some(rep) => {
            tracing::info!(
                "homes-gc: done — deleted {} home(s), {:.1} MiB",
                rep.deleted,
                rep.bytes as f64 / (1024.0 * 1024.0)
            );
        }
        None => {
            tracing::error!(
                token = "homes-gc-not-configured",
                "homes-gc: nothing to do — QUASAR_HOME_ROOT is unset (no local-driver \
                 homes on this host) or QUASAR_HOMES_GC is off"
            );
            std::process::exit(1);
        }
    }
}

/// `quasar-node-agent probe-encoder --codec h264|h265|av1 [--size WxH@FPS] [--seconds N]
/// [--json]`. Exits non-zero when the encoder branch did not negotiate, so a harness can
/// gate on it.
fn run_probe_encoder(codec: &str, width: i32, height: i32, fps: i32, seconds: u64, json: bool) {
    let codec = match session::Codec::parse(codec) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("{e:#}");
            std::process::exit(2);
        }
    };
    // Must init GStreamer exactly as a session does (GST_REGISTRY for VA included), or the
    // probe answers a different question than the one the host will face.
    let cfg = SessionConfig::from_env(true, None);
    if let Err(e) = session::init_gstreamer(&cfg) {
        eprintln!("gstreamer init failed: {e:#}");
        std::process::exit(1);
    }
    let report = session::probe_encoder::run(codec, width, height, fps, seconds);
    if json {
        println!(
            "{}",
            serde_json::to_string_pretty(&report)
                .unwrap_or_else(|e| format!("{{\"error\":\"{e}\"}}"))
        );
    } else {
        println!("codec               = {}", report.codec);
        println!("configured_encoder  = {}", report.configured_encoder);
        println!("effective_encoder   = {}", report.effective_encoder);
        println!(
            "encoder_factory     = {}",
            report.encoder_factory.as_deref().unwrap_or("-")
        );
        println!(
            "profile             = {}",
            report.profile.as_deref().unwrap_or("-")
        );
        println!(
            "level               = {}",
            report.level.as_deref().unwrap_or("-")
        );
        println!(
            "negotiated_sink_caps= {}",
            report.negotiated_sink_caps.as_deref().unwrap_or("-")
        );
        println!(
            "negotiated_src_caps = {}",
            report.negotiated_src_caps.as_deref().unwrap_or("-")
        );
        println!("ok                  = {}", report.ok);
        if let Some(err) = &report.error {
            println!("error               = {err}");
        }
    }
    if !report.ok {
        std::process::exit(1);
    }
}

fn run_inject_selftest() {
    let cfg = SessionConfig::from_env(false, None);
    if let Err(e) = session::init_gstreamer(&cfg) {
        eprintln!("gstreamer init failed: {e:#}");
        std::process::exit(1);
    }
    tracing::info!("quasar node-agent — input inject self-test");
    if let Err(e) = session::input::run_inject_selftest() {
        tracing::error!(
            token = "inject-selftest-failed",
            "inject self-test failed: {e:#}"
        );
        std::process::exit(1);
    }
}

/// Proves the uinput path end-to-end (device creation, fake-udev node, event write) with no
/// compositor, GPU or browser. Needs `/dev/uinput`.
fn run_vinput_selftest() {
    tracing::info!("quasar node-agent — virtual input self-test");
    let devices = match session::virtual_input::VirtualDevices::create("selftest") {
        Ok(d) => d,
        Err(e) => {
            tracing::error!(
                token = "vinput-selftest-failed",
                "virtual device creation failed: {e:#}"
            );
            std::process::exit(1);
        }
    };
    tracing::info!(
        "created: keyboard={}, mouse={}, gamepad={}",
        devices.keyboard_path.display(),
        devices.mouse_path.display(),
        devices.gamepad_path.display()
    );

    let report = |name: &str, result: anyhow::Result<()>| match result {
        Ok(()) => tracing::info!("  ✅ {name}"),
        Err(e) => {
            tracing::error!(token = "selftest-check-failed", "  ❌ {name}: {e:#}");
            std::process::exit(1);
        }
    };
    report(
        "key A down+up",
        devices.key(30, true).and_then(|_| devices.key(30, false)),
    );
    report(
        "mouse move + left click",
        devices
            .mouse_move_rel(10.0, -5.0)
            .and_then(|_| devices.mouse_button(0x110, true))
            .and_then(|_| devices.mouse_button(0x110, false)),
    );
    report("scroll", devices.scroll(0.0, 120.0));
    report(
        "gamepad A + left stick",
        devices.gamepad(&[1.0], &[0.5, -0.5]),
    );
    tracing::info!("✅ virtual input self-test PASS");
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::Arc;

    /// Fails to COMPILE the day `agent::run`'s future stops being `Send + 'static` — the
    /// day the spawn would silently have to be reverted. The future is never polled.
    #[test]
    fn the_agent_control_loop_is_spawnable() {
        fn assert_spawnable<F: std::future::Future<Output = ()> + Send + 'static>(_f: F) {}
        assert_spawnable(run_agent());
    }

    /// Spawning must not change when `main` returns: the process lives exactly as long as
    /// the agent does.
    #[tokio::test]
    async fn join_agent_awaits_the_spawned_task_to_completion() {
        let done = Arc::new(AtomicBool::new(false));
        let flag = done.clone();
        join_agent(tokio::spawn(async move {
            tokio::time::sleep(std::time::Duration::from_millis(50)).await;
            flag.store(true, Ordering::SeqCst);
        }))
        .await;
        assert!(
            done.load(Ordering::SeqCst),
            "join_agent returned before the spawned task finished"
        );
    }

    /// A panic in the control loop must still abort, not become a shrugged-off `JoinError`.
    #[tokio::test]
    #[should_panic(expected = "agent exploded")]
    async fn join_agent_repropagates_a_panic() {
        join_agent(tokio::spawn(async { panic!("agent exploded") })).await;
    }
}
