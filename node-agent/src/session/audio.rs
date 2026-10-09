//! Per-session audio sidecar: PipeWire speaking the PulseAudio protocol (#392).
//!
//! Each session gets a dedicated daemon owning a Unix socket (Wolf's pattern). The app
//! container sends audio to it (`PULSE_SERVER=unix:…`); the host pipeline captures via
//! `pulsesrc`. Explicit stop and Drop request journalled cleanup; unreachable engines leave
//! a durable obligation for recovery.
//!
//! The socket directory is `{runtime_dir}/pulse-{session_id}`: deterministic, per-session,
//! and safe as a Docker bind-mount source because the same path applies on the host (where
//! the daemon resolves it) and inside the agent container. The socket is `{dir}/native`:
//! the baked `deploy/audio/quasar-session-pulse.conf` binds `unix:../native` relative to
//! the private `PULSE_RUNTIME_PATH={dir}/.runtime` the runtime profile sets.
//!
//! Image: `QUASAR_PULSE_IMAGE`, otherwise the running agent image. It ships PipeWire,
//! WirePlumber and the session configs (`deploy/audio/`), so no extra pull is needed; the
//! sidecar uses no GStreamer, Wayland, or GPU facilities from it.

use std::io::Read;
use std::path::{Path, PathBuf};
use std::thread;
use std::time::Duration;

use anyhow::{anyhow, Context, Result};

use super::container::ContainerRuntime;

/// Name prefix for audio sidecars; CLI orphan cleanup must preserve these.
pub const PULSE_NAME_PREFIX: &str = "quasar-pulse-";

/// The session's output sink, baked into the daemon config
/// (`module-null-sink sink_name=…`) and injected into app containers as `PULSE_SINK`, so a
/// client enumerating sinks by name routes here instead of a phantom/`auto_null` sink.
/// Public so both app dispatch sites (`host.rs`, `source.rs`) inject the same literal the
/// daemon config uses.
pub const QUASAR_SINK_NAME: &str = "quasar_output";

/// The monitor source of [`QUASAR_SINK_NAME`], recorded by the host-audio capture
/// `pulsesrc` (the WebRTC encode branch). Pinned explicitly as
/// `pulsesrc device=…`, never relying on it being the daemon DEFAULT source: the sidecar
/// also loads a microphone feed sink and a remap-source, and a moved default would
/// silently point host capture at the client's own microphone. Kept in lockstep with
/// [`QUASAR_SINK_NAME`]; guarded by `device_name_constants_agree_with_the_baked_config`.
pub const QUASAR_MONITOR_SOURCE_NAME: &str = "quasar_output.monitor";

/// The session's microphone FEED sink. The agent's decoded client-mic audio plays into it
/// (`pulsesink device=quasar_mic`), and its monitor is what [`QUASAR_MIC_SOURCE_NAME`]
/// re-presents as a real capture source. Baked into the daemon config so the devices exist
/// for the sidecar's whole life (a runtime `pactl load-module` does not survive a sidecar
/// restart); silent unless the session negotiated a microphone m-line.
pub const QUASAR_MIC_SINK_NAME: &str = "quasar_mic";

/// The session's microphone CAPTURE source: a `module-remap-source` over
/// `{QUASAR_MIC_SINK_NAME}.monitor`, injected into app containers as `PULSE_SOURCE`. The
/// remap exists because Steam and many games hide monitor-class sources in their device
/// pickers, while a remapped source is a first-class capture device to them.
pub const QUASAR_MIC_SOURCE_NAME: &str = "quasar_mic_src";

/// Deterministic, per-session sidecar container name. Distinct session ids must yield
/// distinct names, so two concurrent sidecars never collide and a force-remove of one
/// cannot touch another.
pub fn pulse_container_name(session_id: &str) -> String {
    format!("{PULSE_NAME_PREFIX}{session_id}")
}

/// Per-session socket directory under `runtime_dir`: its own `pulse-{id}` subdirectory,
/// so two sidecars never share a socket path.
pub fn pulse_socket_dir(runtime_dir: &str, session_id: &str) -> PathBuf {
    PathBuf::from(runtime_dir).join(format!("pulse-{session_id}"))
}

/// How long to wait for the socket file to appear before giving up.
pub(crate) const PULSE_WAIT_TOTAL: Duration = Duration::from_secs(2);
const PULSE_POLL_INTERVAL: Duration = Duration::from_millis(100);

/// A per-session audio sidecar container owning a Unix socket (`{dir}/native`) that
/// the app container connects to and `pulsesrc` captures from. Drop requests cleanup.
pub struct PulseSidecar {
    socket_dir: PathBuf,
    runtime: crate::runtime::RuntimeClient,
    operation: String,
    removed: bool,
    cleanup_attempted: bool,
    /// The session end's shared retry allowance (`teardown::RetryBudget`). A
    /// sidecar starts with its own so a failed start can still release it; the
    /// session replaces it with the shared one via [`Self::adopt_budget`], so
    /// the sidecar's retries and the app container's draw down one pool.
    budget: super::teardown::RetryBudget,
}

/// `QUASAR_PULSE_IMAGE`, or the running agent's own image. Shared by the per-session
/// sidecar and the audio host probe, so a probe proves the exact image a session
/// would use.
pub(crate) fn sidecar_image(runtime: &ContainerRuntime) -> Result<String> {
    match std::env::var("QUASAR_PULSE_IMAGE").ok().filter(|v| !v.is_empty()) {
        Some(image) => Ok(image),
        None => runtime.own_image().context(
            "cannot select audio sidecar image; set QUASAR_PULSE_IMAGE when running outside a container",
        ),
    }
}

impl PulseSidecar {
    /// Start the fixed audio profile and wait up to two seconds for a live Unix
    /// socket. A readiness timeout preserves the intentional silent fallback.
    /// The runtime owns directory creation and cleanup; unknown outcomes retain
    /// their original operation instead of granting another launch authority.
    pub fn start(
        session_id: &str,
        runtime: &ContainerRuntime,
        runtime_dir: &str,
    ) -> Result<Option<Self>> {
        if super::udev_export::malformed_session_id(session_id) {
            return Err(anyhow!("refusing malformed session id {session_id:?}"));
        }
        let socket_dir = pulse_socket_dir(runtime_dir, session_id);
        let image = sidecar_image(runtime)?;
        let api = crate::runtime::configured()?.clone();
        if let Err(error) = api.recover_audio_sidecars().wait() {
            tracing::warn!(token = "audio-pulse-recovery-pending", %error,
                "prior audio cleanup remains pending; a conflicting launch will be refused");
        }
        let mut entropy = [0u8; 24];
        std::fs::File::open("/dev/urandom")?.read_exact(&mut entropy)?;
        let operation = format!(
            "audio-{}",
            entropy
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect::<String>()
        );
        let helper = crate::runtime::DiagnosticHelper {
            operation: operation.clone(),
            name: pulse_container_name(session_id),
            image,
        };
        let request = crate::runtime::AudioRun {
            socket_dir: socket_dir.clone(),
            entrypoint: vec!["pipewire".into()],
            command: pulse_command(),
        };
        // Construct the cleanup backstop before submission: a lost create/start
        // response must not leave the caller without an explicit stop request.
        let mut sidecar = Self {
            socket_dir,
            runtime: api,
            operation,
            removed: false,
            cleanup_attempted: false,
            budget: super::teardown::RetryBudget::new(super::teardown::STOP_RETRY_BUDGET),
        };
        let id = match sidecar.runtime.run_audio_sidecar(helper, request).wait() {
            Ok(id) => id,
            Err(error) => {
                sidecar.stop();
                return Err(anyhow!(error).context("audio sidecar start failed"));
            }
        };
        if !wait_for_socket(&sidecar.socket_dir.join("native")) {
            // #411: say why. The daemon's own words, once it is stopped, are the reason.
            let why = match sidecar.runtime.stop_audio_sidecar(id.clone()).wait() {
                Ok(()) => match sidecar.runtime.observe_audio_sidecar(id).wait() {
                    Ok(result) => format!(
                        "exit {:?}; output: {}",
                        result.exit_code,
                        output_tail(&result.stdout, &result.stderr)
                    ),
                    Err(error) => format!("its output could not be read ({:?})", error.kind),
                },
                Err(error) => format!(
                    "it could not be stopped to read its output ({:?})",
                    error.kind
                ),
            };
            tracing::warn!(token = "audio-pulse-socket-timeout",
                "audio sidecar socket '{}' did not become ready within {}s — falling back to silent audio; the sidecar: {why}",
                sidecar.socket_dir.join("native").display(), PULSE_WAIT_TOTAL.as_secs());
            sidecar.stop();
            return Ok(None);
        }
        tracing::info!(
            "audio sidecar ready: socket={}",
            sidecar.socket_dir.join("native").display()
        );
        Ok(Some(sidecar))
    }

    /// Join this sidecar's retries to the session end's shared allowance, so
    /// releasing the app container and releasing the sidecar cannot each spend a
    /// full [`super::teardown::STOP_RETRY_BUDGET`].
    pub fn adopt_budget(&mut self, budget: super::teardown::RetryBudget) {
        self.budget = budget;
    }

    /// The `unix:…` URI pulsesrc and PULSE_SERVER clients use to connect.
    pub fn server_uri(&self) -> String {
        format!("unix:{}", self.socket_dir.join("native").display())
    }

    /// The socket directory to bind-mount into the app container (`-v dir:dir`).
    pub fn socket_dir(&self) -> &Path {
        &self.socket_dir
    }

    /// Tear the sidecar container down (idempotent). `Drop` is the backstop.
    ///
    /// A busy or cancelled client never wrote a stop into the journal, so it
    /// does not latch: routine audio recovery ignores a sidecar still in
    /// `Running`, and latching here would leave `quasar-pulse-<sid>` up for
    /// the life of the agent. An unconfirmed stop did write durable intent
    /// and must not be repeated on the way down.
    pub fn stop(&mut self) {
        if self.removed || self.cleanup_attempted {
            return;
        }
        // Cloned out first: the attempt closure borrows `self` mutably.
        let budget = self.budget.clone();
        let report = super::teardown::retry_with_budget(
            || match self
                .runtime
                .abandon_audio_sidecar(self.operation.clone())
                .wait()
            {
                Ok(()) => {
                    self.removed = true;
                    self.cleanup_attempted = true;
                    super::teardown::StopAttempt::Confirmed
                }
                Err(error) if !super::teardown::pulse_stop_latches(error.kind) => {
                    super::teardown::StopAttempt::Retryable
                }
                Err(error) => {
                    self.cleanup_attempted = true;
                    tracing::warn!(token = "audio-pulse-cleanup-pending", %error,
                        operation = %self.operation,
                        "audio cleanup could not be confirmed; preserve its socket directory and inspect runtime recovery state");
                    super::teardown::StopAttempt::Unconfirmed
                }
            },
            &budget,
        );
        if matches!(report, super::teardown::StopAttempt::Retryable) {
            tracing::warn!(
                token = "audio-pulse-cleanup-busy",
                operation = %self.operation,
                "audio cleanup could not start; the runtime client stayed busy"
            );
        }
    }
}

impl super::teardown::Sidecar for PulseSidecar {
    fn server_uri(&self) -> String {
        PulseSidecar::server_uri(self)
    }

    fn socket_dir(&self) -> PathBuf {
        PulseSidecar::socket_dir(self).to_path_buf()
    }

    fn adopt_budget(&mut self, budget: super::teardown::RetryBudget) {
        PulseSidecar::adopt_budget(self, budget)
    }

    fn stop(&mut self) {
        PulseSidecar::stop(self)
    }
}

impl Drop for PulseSidecar {
    fn drop(&mut self) {
        // An explicit failed stop already left durable intent. Do not double
        // the caller's deadline by immediately repeating it while unwinding.
        if !self.cleanup_attempted {
            self.stop();
        }
    }
}

/// The daemon's argv. Its devices, socket and auth live in the baked config
/// (`deploy/audio/`); the runtime profile owns the environment that places the socket.
pub(crate) fn pulse_command() -> Vec<String> {
    vec!["-c".into(), "/etc/pipewire/quasar-session.conf".into()]
}

/// Probe without blocking on a saturated listener backlog.
fn socket_accepts_connection(path: &Path) -> bool {
    use std::os::{
        fd::{AsRawFd, FromRawFd, OwnedFd},
        unix::ffi::OsStrExt,
    };
    let bytes = path.as_os_str().as_bytes();
    // SAFETY: zero is a valid initial representation of sockaddr_un.
    let mut address: libc::sockaddr_un = unsafe { std::mem::zeroed() };
    if bytes.is_empty() || bytes.len() >= address.sun_path.len() || bytes.contains(&0) {
        return false;
    }
    address.sun_family = libc::AF_UNIX as libc::sa_family_t;
    for (target, source) in address.sun_path.iter_mut().zip(bytes) {
        *target = *source as libc::c_char;
    }
    // SAFETY: socket has no pointer arguments. OwnedFd closes a successful descriptor.
    let fd = unsafe {
        libc::socket(
            libc::AF_UNIX,
            libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
            0,
        )
    };
    if fd < 0 {
        return false;
    }
    // SAFETY: this newly created descriptor has exactly one owner.
    let socket = unsafe { OwnedFd::from_raw_fd(fd) };
    // SAFETY: address is initialized and remains live for the stated size.
    unsafe {
        libc::connect(
            socket.as_raw_fd(),
            (&address as *const libc::sockaddr_un).cast(),
            std::mem::size_of_val(&address) as libc::socklen_t,
        ) == 0
    }
}

/// Poll for a connectable socket at 100 ms intervals up to `PULSE_WAIT_TOTAL`.
pub(crate) fn wait_for_socket(path: &Path) -> bool {
    let steps = (PULSE_WAIT_TOTAL.as_millis() / PULSE_POLL_INTERVAL.as_millis()) as u32;
    for _ in 0..steps {
        if socket_accepts_connection(path) {
            return true;
        }
        thread::sleep(PULSE_POLL_INTERVAL);
    }
    socket_accepts_connection(path)
}

/// The last lines a daemon wrote, bounded for one log line.
fn output_tail(stdout: &str, stderr: &str) -> String {
    const MAX: usize = 600;
    let joined = format!("{stdout}{stderr}");
    let text = joined.trim();
    if text.is_empty() {
        return "(none)".into();
    }
    let start = text
        .char_indices()
        .map(|(i, _)| i)
        .find(|&i| text.len() - i <= MAX)
        .unwrap_or(0);
    text[start..].replace('\n', " | ")
}

#[cfg(test)]
mod tests {
    use super::super::teardown::RetryBudget;
    use crate::runtime::{RuntimeClient, RuntimeConfig};
    use sha2::{Digest, Sha256};
    use std::os::unix::net::UnixListener;
    use std::time::Duration;

    #[test]
    fn socket_readiness_is_bounded_when_the_listener_backlog_is_full() {
        use std::os::fd::AsRawFd;
        use std::os::unix::net::{UnixListener, UnixStream};
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("native");
        let listener = UnixListener::bind(&path).unwrap();
        // SAFETY: the listener owns a valid listening descriptor.
        assert_eq!(unsafe { libc::listen(listener.as_raw_fd(), 0) }, 0);
        let connection = UnixStream::connect(&path).unwrap();
        let (send, receive) = std::sync::mpsc::channel();
        let worker = std::thread::spawn(move || {
            let _ = send.send(super::wait_for_socket(&path));
        });
        let result = receive.recv_timeout(std::time::Duration::from_secs(3));
        // Release a blocked pre-fix probe before reporting the failure.
        drop(connection);
        drop(listener);
        worker.join().unwrap();
        assert!(!result.expect("readiness exceeded its bound"));
    }

    use super::*;

    /// A config the image installs under `/etc/pipewire/`.
    fn baked_config(name: &str) -> String {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../deploy/audio")
            .join(name);
        std::fs::read_to_string(&path)
            .unwrap_or_else(|e| panic!("cannot read {}: {e}", path.display()))
    }

    /// The device-name constants must stay in lockstep with the baked daemon config: the
    /// capture pulsesrc pins `quasar_output.monitor` by literal, and the mic
    /// remap-source's master is `quasar_mic.monitor`.
    #[test]
    fn device_name_constants_agree_with_the_baked_config() {
        assert_eq!(
            QUASAR_MONITOR_SOURCE_NAME,
            format!("{QUASAR_SINK_NAME}.monitor")
        );
        let config = baked_config("quasar-session-pulse.conf");
        for needle in [
            format!("module-null-sink sink_name={QUASAR_SINK_NAME} "),
            format!("module-null-sink sink_name={QUASAR_MIC_SINK_NAME} "),
            format!(
                "module-remap-source master={QUASAR_MIC_SINK_NAME}.monitor \
                 source_name={QUASAR_MIC_SOURCE_NAME} "
            ),
        ] {
            assert!(config.contains(&needle), "baked config lacks `{needle}`");
        }
    }

    // The sidecar name and socket dir must be session-unique so two concurrent sessions
    // never share a container name or a socket path.
    #[test]
    fn pulse_names_are_session_unique() {
        let a = "11111111-1111-1111-1111-111111111111";
        let b = "22222222-2222-2222-2222-222222222222";

        assert_ne!(pulse_container_name(a), pulse_container_name(b));
        assert!(pulse_container_name(a).contains(a));
        assert!(pulse_container_name(a).starts_with(PULSE_NAME_PREFIX));

        let rt = "/run/user/1000";
        assert_ne!(pulse_socket_dir(rt, a), pulse_socket_dir(rt, b));
        assert!(pulse_socket_dir(rt, a).starts_with(rt));
        assert!(pulse_socket_dir(rt, a)
            .to_string_lossy()
            .contains(&format!("pulse-{a}")));
    }

    #[test]
    fn start_refuses_a_malformed_session_id() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        let runtime = ContainerRuntime::from_env();
        for sid in ["", "../escape", "a/b", ".."] {
            let err = PulseSidecar::start(sid, &runtime, runtime_dir)
                .err()
                .unwrap_or_else(|| panic!("sid {sid:?} should be refused"));
            assert!(err.to_string().contains("malformed session id"), "{err:#}");
        }
        assert!(std::fs::read_dir(runtime_dir).unwrap().next().is_none());
    }

    /// A runtime client bounded to ONE in-flight call, talking to a socket that
    /// accepts and never answers, with a journal root of our own. No Docker, no
    /// process env: the two things the release path needs to be driven through
    /// are a refused admission and a journal that already says `Completed`.
    struct Bench {
        _dir: tempfile::TempDir,
        _listener: UnixListener,
        client: RuntimeClient,
        state: std::path::PathBuf,
    }

    impl Bench {
        fn new() -> Self {
            let dir = tempfile::tempdir().unwrap();
            let socket = dir.path().join("engine.sock");
            let listener = UnixListener::bind(&socket).unwrap();
            let state = dir.path().join("state");
            std::fs::create_dir_all(&state).unwrap();
            let mut config = RuntimeConfig::unix(&socket);
            config.deadline = Duration::from_secs(2);
            config.max_in_flight = 1;
            config.image_state_path = Some(state.clone());
            let client = RuntimeClient::new(config).unwrap();
            Bench {
                _dir: dir,
                _listener: listener,
                client,
                state,
            }
        }

        /// A helper journal whose audio operation is already proven terminal, so
        /// `abandon_audio_sidecar` answers `Ok` without opening Docker.
        fn completed_audio_journal(&self, operation: &str) {
            let key = Sha256::digest(operation.as_bytes())
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect::<String>();
            let helpers = self.state.join("helpers");
            std::fs::create_dir_all(&helpers).unwrap();
            let intent = serde_json::json!({
                "operation": operation,
                "name": pulse_container_name("wiring"),
                "image": "quasar-node-agent:test",
                "owner": "wiring-owner",
                "socket": "/run/quasar-agent/engine.sock",
                "id": null,
                "profile": "Audio",
                "phase": "Completed",
            });
            std::fs::write(helpers.join(key), serde_json::to_vec(&intent).unwrap()).unwrap();
        }

        fn sidecar(&self, operation: &str) -> PulseSidecar {
            PulseSidecar {
                socket_dir: self.state.join("pulse-wiring"),
                runtime: self.client.clone(),
                operation: operation.to_string(),
                removed: false,
                cleanup_attempted: false,
                budget: RetryBudget::spent(),
            }
        }
    }

    // #314 as it actually happened: the idle reap raced a busy runtime client.
    // The refused call wrote nothing, so it must leave the sidecar stoppable —
    // the pre-fix code latched `cleanup_attempted` before ever calling, and
    // routine audio recovery will not remove a sidecar whose journal still says
    // `Running`, so `quasar-pulse-<sid>` survived until the agent restarted.
    #[test]
    fn a_busy_client_does_not_latch_the_sidecar_and_a_later_stop_removes_it() {
        let bench = Bench::new();
        let operation = "audio-wiring-busy-then-confirmed";
        bench.completed_audio_journal(operation);
        let mut sidecar = bench.sidecar(operation);

        // `submit_owned` takes the admission permit on the CALLING thread, so the
        // slot is held the moment this returns — no sleep, no race.
        let hold = bench.client.discover();
        sidecar.stop();
        assert!(
            !sidecar.cleanup_attempted,
            "a refused call left no durable intent, so it must not disarm the retry"
        );
        assert!(!sidecar.removed, "and it certainly did not remove anything");

        hold.cancel();
        let _ = hold.wait();
        // The real budget: cancellation releases admission a moment after the
        // caller's wait returns, and the retry loop is what rides that out.
        sidecar.adopt_budget(RetryBudget::new(Duration::from_secs(5)));
        sidecar.stop();
        assert!(sidecar.removed, "the sidecar container is released");
        assert!(sidecar.cleanup_attempted);
    }

    #[test]
    fn an_unconfirmed_pulse_stop_latches_so_the_way_down_does_not_repeat_it() {
        // No journal for this operation: the acquire fails before Docker is
        // opened, which is an unconfirmed stop, which DID record durable intent.
        let bench = Bench::new();
        let mut sidecar = bench.sidecar("audio-wiring-unconfirmed");
        sidecar.stop();
        assert!(!sidecar.removed);
        assert!(
            sidecar.cleanup_attempted,
            "an unconfirmed stop must not be spun on the way down"
        );
    }

    // Non-root app containers (Steam at 99:100, Proton behind pressure-vessel) must reach
    // the socket at the shared `{dir}/native` without a cookie, while the daemon's runtime
    // dir stays private (`PULSE_RUNTIME_PATH={dir}/.runtime`, the runtime profile's half).
    #[test]
    fn the_baked_config_pins_the_shared_socket_the_devices_and_the_wire_format() {
        assert_eq!(pulse_command(), ["-c", "/etc/pipewire/quasar-session.conf"]);
        let config = baked_config("quasar-session-pulse.conf");
        assert!(config.contains(
            r#"server.address = [ { address = "unix:../native" client.access = "unrestricted" } ]"#
        ));

        let line = |needle: &str| {
            let at = config
                .find(needle)
                .unwrap_or_else(|| panic!("baked config lacks `{needle}`"));
            (at, config[at..].lines().next().unwrap())
        };
        let (output_at, output) = line("sink_name=quasar_output ");
        let (mic_at, mic) = line("sink_name=quasar_mic ");
        let (remap_at, remap) = line("module-remap-source ");
        assert!(mic_at < remap_at, "remap master must exist first");
        assert!(output_at < mic_at);
        for sink in [output, mic] {
            // The capture pulsesrc holds the monitor from session start, so the sink cannot
            // switch rate once a game connects (#351).
            assert!(sink.contains("rate=48000 channels=2"), "{sink}");
            // Steam hides `abstract`-class devices from its pickers.
            assert!(sink.contains("device.class='sound'"), "{sink}");
        }
        assert!(remap.contains("device.class='sound'"), "{remap}");

        // quasar_output is the default sink by priority, not by discovery order. The default
        // source is quasar_mic_src, the only non-monitor source, which is intended: an app
        // reading the default source gets the microphone, and both agent pulsesrcs pin
        // `device=quasar_output.monitor`.
        let priority = |line: &str| -> u32 {
            let value = line.split("priority.session=").nth(1).expect(line);
            value[..value.find(|c: char| !c.is_ascii_digit()).unwrap()]
                .parse()
                .unwrap()
        };
        assert!(priority(output) > priority(mic));
    }

    // Stock PipeWire's VM rule, copied from /usr/share/pipewire/pipewire{,-pulse}.conf: small
    // quanta crackle under VM timer jitter, and the session daemon has no RT priority.
    #[test]
    fn both_configs_keep_the_stock_vm_quantum_floor() {
        let daemon = baked_config("quasar-session.conf");
        let pulse = baked_config("quasar-session-pulse.conf");
        for (config, section, floor) in [
            (
                &daemon,
                "context.properties.rules",
                "default.clock.min-quantum = 1024",
            ),
            (
                &pulse,
                "pulse.properties.rules",
                "pulse.min.quantum = 1024/48000",
            ),
        ] {
            let rules = &config[config.find(section).expect(section)..];
            assert!(
                rules.contains("matches = [ { cpu.vm.name = !null } ]"),
                "{section}"
            );
            assert!(rules.contains(floor), "{section} lacks `{floor}`");
        }
    }
}

#[cfg(test)]
mod output_tail_tests {
    #[test]
    fn a_daemons_last_words_are_bounded_and_on_one_line() {
        assert_eq!(super::output_tail("", "  "), "(none)");
        assert_eq!(
            super::output_tail("a\n", "E: bind failed\n"),
            "a | E: bind failed"
        );
        let long = "x".repeat(2000);
        assert!(super::output_tail(&long, "").len() <= 600);
    }
}
