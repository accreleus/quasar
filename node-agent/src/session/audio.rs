//! Per-session PulseAudio sidecar.
//!
//! Each session gets a dedicated PulseAudio process owning a Unix socket (Wolf's
//! pattern). The app container sends audio to it (`PULSE_SERVER=unix:…`); the host
//! pipeline captures via `pulsesrc`. Explicit stop and Drop request journalled
//! cleanup; unreachable engines leave a durable obligation for recovery.
//!
//! The socket directory is `{runtime_dir}/pulse-{session_id}`: deterministic, per-session,
//! and safe as a Docker bind-mount source because the same path applies on the host (where
//! the daemon resolves it) and inside the agent container. The socket is `{dir}/native`,
//! pinned by an explicit `module-native-protocol-unix socket=…` daemon arg, never via
//! `PULSE_RUNTIME_PATH` — that must stay private, because PulseAudio force-chmods its
//! runtime dir 0700 and locks out non-root app-container clients.
//!
//! Image: `QUASAR_PULSE_IMAGE`, otherwise the running agent image. It ships
//! the `pulseaudio` binary, so no extra pull is needed; the sidecar uses no GStreamer,
//! Wayland, or GPU facilities from it.

use std::io::Read;
use std::path::{Path, PathBuf};
use std::thread;
use std::time::Duration;

use anyhow::{anyhow, Context, Result};

use super::container::ContainerRuntime;

/// Name prefix for PulseAudio sidecars; CLI orphan cleanup must preserve these.
pub const PULSE_NAME_PREFIX: &str = "quasar-pulse-";

/// The session's single PulseAudio sink, baked into the daemon args
/// (`module-null-sink sink_name=…`) and injected into app containers as `PULSE_SINK`, so a
/// client enumerating sinks by name routes here instead of a phantom/`auto_null` sink.
/// Public so both app dispatch sites (`host.rs`, `source.rs`) inject the same literal the
/// daemon args use.
pub const QUASAR_SINK_NAME: &str = "quasar_output";

/// The monitor source of [`QUASAR_SINK_NAME`], recorded by the host-audio capture
/// `pulsesrc` (WebRTC encode branch and console local-audio leg). Pinned explicitly as
/// `pulsesrc device=…`, never relying on it being the daemon DEFAULT source: the sidecar
/// also loads a microphone feed sink and a remap-source, and a moved default would
/// silently point host capture at the client's own microphone. Kept in lockstep with
/// [`QUASAR_SINK_NAME`]; guarded by `device_name_constants_agree_with_baked_modules`.
pub const QUASAR_MONITOR_SOURCE_NAME: &str = "quasar_output.monitor";

/// The session's microphone FEED sink. The agent's decoded client-mic audio plays into it
/// (`pulsesink device=quasar_mic`), and its monitor is what [`QUASAR_MIC_SOURCE_NAME`]
/// re-presents as a real capture source. Loaded unconditionally so the devices exist for
/// the sidecar's whole life (a runtime `pactl load-module` does not survive a sidecar
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

/// A per-session PulseAudio sidecar container owning a Unix socket (`{dir}/native`) that
/// the app container connects to and `pulsesrc` captures from. Drop requests cleanup.
pub struct PulseSidecar {
    socket_dir: PathBuf,
    runtime: crate::runtime::RuntimeClient,
    operation: String,
    removed: bool,
    cleanup_attempted: bool,
}

/// `QUASAR_PULSE_IMAGE`, or the running agent's own image. Shared by the per-session
/// sidecar and the audio host probe (#259), so a probe proves the exact image a session
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
            entrypoint: vec!["pulseaudio".into()],
            command: pulse_command(&socket_dir.to_string_lossy()),
        };
        // Construct the cleanup backstop before submission: a lost create/start
        // response must not leave the caller without an explicit stop request.
        let mut sidecar = Self {
            socket_dir,
            runtime: api,
            operation,
            removed: false,
            cleanup_attempted: false,
        };
        if let Err(error) = sidecar.runtime.run_audio_sidecar(helper, request).wait() {
            sidecar.stop();
            return Err(anyhow!(error).context("pulseaudio sidecar start failed"));
        }
        if !wait_for_socket(&sidecar.socket_dir.join("native")) {
            tracing::warn!(token = "audio-pulse-socket-timeout",
                "pulseaudio socket '{}' did not become ready within {}s — falling back to silent audio",
                sidecar.socket_dir.join("native").display(), PULSE_WAIT_TOTAL.as_secs());
            sidecar.stop();
            return Ok(None);
        }
        tracing::info!(
            "PulseAudio sidecar ready: socket={}",
            sidecar.socket_dir.join("native").display()
        );
        Ok(Some(sidecar))
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
    pub fn stop(&mut self) {
        if self.removed {
            return;
        }
        self.cleanup_attempted = true;
        match self
            .runtime
            .abandon_audio_sidecar(self.operation.clone())
            .wait()
        {
            Ok(()) => self.removed = true,
            Err(error) => tracing::warn!(token = "audio-pulse-cleanup-pending", %error,
                operation = %self.operation,
                "audio cleanup could not be confirmed; preserve its socket directory and inspect runtime recovery state"),
        }
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

pub(crate) fn pulse_command(socket_dir: &str) -> Vec<String> {
    // Runtime profile owns Docker settings, HOME and private PULSE_RUNTIME_PATH.
    // Only the Pulse daemon command and device topology belong to this caller.
    vec![
        "--daemonize=no".into(),
        "--system=no".into(),
        "--disable-shm=true".into(),
        "--exit-idle-time=-1".into(),
        "--log-target=stderr".into(),
        // `-n` skips default.pa and loads an explicit module set, so the socket lands at
        // a pinned shared path instead of under the now-private runtime dir.
        //
        // `auth-anonymous=1` accepts every client on this socket without a cookie. The
        // socket is already private (a per-session dir bind-mounted only into that
        // session's own containers), so there is no untrusted peer to authenticate — the
        // Wolf/GOW model. Not a convenience: cookie auth across the pressure-vessel
        // (Proton/Steam) sandbox boundary is fundamentally broken, because
        // pressure-vessel re-homes XDG_RUNTIME_DIR and remaps the cookie, so the file a
        // Proton game reads diverges from the one the daemon wrote (three different
        // cookie hashes were observed for one logical file) and every Proton title is
        // silently denied. The anonymous grant removes that class of failure and lets the
        // agent carry no cookie machinery at all.
        "-n".into(),
        // The session's one sink. `device.class=sound` because some clients (Steam) hide
        // `abstract`-class devices (module-null-sink's default) from their output
        // pickers. Baked into the daemon args so it lives as long as the sidecar: a live
        // `pactl load-module` does not survive a restart. First sink loaded == default
        // sink, so a default-source capture lands on its monitor.
        format!(
            "--load=module-null-sink sink_name={QUASAR_SINK_NAME} \
             sink_properties=\"device.class='sound' device.description='Quasar Output'\""
        ),
        // Microphone capture (client → host). These two loads MUST stay AFTER the
        // quasar_output null-sink above: PulseAudio makes the FIRST loaded sink the
        // default, and the ordering is load-bearing for app clients that follow it. (The
        // capture pulsesrc also pins `device=quasar_output.monitor` explicitly.)
        //
        // `quasar_mic` is the feed sink the agent's decoded mic audio plays into
        // (`pulsesink device=quasar_mic`); `device.class='sound'` for the same Steam
        // reason as above. rate/channels are pinned to the Opus wire format (48 kHz
        // stereo) so decoded mic audio is not resampled through the daemon default 44100.
        format!(
            "--load=module-null-sink sink_name={QUASAR_MIC_SINK_NAME} \
             rate=48000 channels=2 \
             sink_properties=\"device.class='sound' device.description='Quasar Microphone Feed'\""
        ),
        // `quasar_mic_src` re-presents that sink's monitor as a first-class capture
        // source, because Steam and many games hide monitor-class sources in their
        // microphone pickers. The app container records from it (injected as
        // PULSE_SOURCE at both dispatch sites). Loaded unconditionally: it is silent
        // until a session negotiates a mic m-line, and a static argv stays testable.
        format!(
            "--load=module-remap-source master={QUASAR_MIC_SINK_NAME}.monitor \
             source_name={QUASAR_MIC_SOURCE_NAME} \
             source_properties=\"device.class='sound' device.description='Quasar Microphone'\""
        ),
        // The socket must load LAST: every consumer (the agent's `wait_for_socket` poll,
        // and the app container started moments later) treats "socket exists" as
        // "sidecar ready". Loading the protocol module after every device makes that
        // signal true — no client can connect while the three devices are still being
        // created, and the app container resolves PULSE_SINK/PULSE_SOURCE by name at
        // connect, so it is genuinely exposed to that ordering window.
        format!("--load=module-native-protocol-unix socket={socket_dir}/native auth-anonymous=1"),
    ]
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

/// The ALSA mixer control gating the HDA codec's digital converter (`AC_DIG1_ENABLE`). On
/// some HDA HDMI/DP outputs it defaults to OFF on every boot/module reload, and with no
/// `alsactl` restore a session opening `alsasink device=hw:0,3` plays audio that never
/// reaches the wire (hw_ptr advances, codec `Digital:` node stays blank).
const IEC958_SWITCH_NAME: &str = "IEC958 Playback Switch";

/// Best-effort: enable every `{IEC958_SWITCH_NAME}` boolean mixer control on the ALSA card
/// `alsa_device` (an `alsasink`-style string like `"hw:0,3"`) resolves to. Returns how
/// many were flipped on; 0 when the card has none, as most non-HDMI cards do.
///
/// Enables EVERY matching control rather than mapping the device's PCM index to a control
/// index: there is no stable index↔pcm-device mapping, and enabling an unconnected pin's
/// switch is harmless.
///
/// Never fails the caller — a pre-flight nicety for the local-audio pipeline, not a hard
/// dependency. Log any error and start the pipeline anyway.
pub fn enable_iec958_playback_switches(alsa_device: &str) -> Result<usize> {
    if alsa_device == "auto" {
        return enable_iec958_playback_switches_auto();
    }

    let card = card_spec_from_device(alsa_device)
        .ok_or_else(|| anyhow!("could not parse an ALSA card from device '{alsa_device}'"))?;

    enable_iec958_playback_switches_on_card(&card)
}

/// The ALSA `default` PCM does not expose its backing card through the device string, so
/// enumerate every card visible inside the agent container and enable the matching
/// switches on each. Console deployments expose only the intended `/dev/snd/controlC*`
/// devices; inaccessible cards are tolerated as long as one opened successfully.
fn enable_iec958_playback_switches_auto() -> Result<usize> {
    let mut enabled = 0usize;
    let mut opened = 0usize;
    let mut first_error = None;

    for card in alsa::card::Iter::new() {
        let card = match card {
            Ok(card) => card,
            Err(e) => {
                first_error.get_or_insert_with(|| anyhow!("enumerate ALSA cards: {e}"));
                continue;
            }
        };
        let spec = format!("hw:{}", card.get_index());
        match enable_iec958_playback_switches_on_card(&spec) {
            Ok(n) => {
                opened += 1;
                enabled += n;
            }
            Err(e) => {
                tracing::debug!(card = %spec, error = %format!("{e:#}"), "cannot inspect IEC958 controls on automatic ALSA card");
                first_error.get_or_insert(e);
            }
        }
    }

    match (opened, first_error) {
        (0, Some(error)) => Err(error),
        _ => Ok(enabled),
    }
}

fn enable_iec958_playback_switches_on_card(card: &str) -> Result<usize> {
    let hctl =
        alsa::hctl::HCtl::new(card, false).with_context(|| format!("open HCtl for '{card}'"))?;
    hctl.load()
        .with_context(|| format!("load HCtl for '{card}'"))?;

    let mut enabled = 0usize;
    for elem in hctl.elem_iter() {
        let id = match elem.get_id() {
            Ok(id) => id,
            Err(_) => continue,
        };
        if id.get_interface() != alsa::ctl::ElemIface::Mixer {
            continue;
        }
        let Ok(name) = id.get_name() else {
            continue;
        };
        if name != IEC958_SWITCH_NAME {
            continue;
        }

        let info = match elem.info() {
            Ok(info) => info,
            Err(e) => {
                tracing::debug!(
                    token = "audio-iec958-inspect-failed",card, control = IEC958_SWITCH_NAME, error = %e, "cannot inspect IEC958 playback switch");
                continue;
            }
        };
        if info.get_type() != alsa::ctl::ElemType::Boolean {
            continue;
        }
        let count = info.get_count();

        let mut value = match elem.read() {
            Ok(v) => v,
            Err(e) => {
                tracing::warn!(
                    token = "audio-iec958-read-failed",card, control = IEC958_SWITCH_NAME, error = %e, "cannot read IEC958 playback switch");
                continue;
            }
        };
        for idx in 0..count {
            value.set_boolean(idx, true);
        }
        match elem.write(&value) {
            Ok(_) => enabled += 1,
            Err(e) => {
                tracing::warn!(
                    token = "audio-iec958-enable-failed",card, control = IEC958_SWITCH_NAME, error = %e, "cannot enable IEC958 playback switch");
            }
        }
    }

    Ok(enabled)
}

/// Reduce `"hw:0,3"` / `"hw:CARD=NAME,DEV=X"` / `"plughw:1,0"` to the bare card spec
/// ALSA's `Ctl`/`HCtl` open calls expect (`"hw:0"` / `"hw:NAME"`). `None` for anything
/// that is not an ALSA hw device (`"auto"`), so the caller skips rather than misparses.
fn card_spec_from_device(device: &str) -> Option<String> {
    let rest = device
        .strip_prefix("plughw:")
        .or_else(|| device.strip_prefix("hw:"))?;
    let card_part = rest.split(',').next()?.trim();
    if card_part.is_empty() {
        return None;
    }
    let card = card_part.strip_prefix("CARD=").unwrap_or(card_part).trim();
    if card.is_empty() {
        return None;
    }
    Some(format!("hw:{card}"))
}

#[cfg(test)]
mod tests {
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

    #[test]
    fn card_spec_parses_common_forms() {
        assert_eq!(card_spec_from_device("hw:0,3").as_deref(), Some("hw:0"));
        assert_eq!(card_spec_from_device("hw:1").as_deref(), Some("hw:1"));
        assert_eq!(card_spec_from_device("plughw:0,3").as_deref(), Some("hw:0"));
        assert_eq!(
            card_spec_from_device("hw:CARD=NVidia,DEV=3").as_deref(),
            Some("hw:NVidia")
        );
        assert_eq!(card_spec_from_device("auto"), None);
        assert_eq!(card_spec_from_device(""), None);
        assert_eq!(card_spec_from_device("hw:"), None);
    }

    /// The device-name constants must stay in lockstep with the baked daemon argv: the
    /// capture pulsesrc pins `quasar_output.monitor` by literal, and the mic
    /// remap-source's master is `quasar_mic.monitor`.
    #[test]
    fn device_name_constants_agree_with_baked_modules() {
        assert_eq!(
            QUASAR_MONITOR_SOURCE_NAME,
            format!("{QUASAR_SINK_NAME}.monitor")
        );
        let args = pulse_command("/run/quasar-agent/pulse-test");
        assert!(args
            .iter()
            .any(|arg| arg.contains(&format!("master={QUASAR_MIC_SINK_NAME}.monitor"))));
        assert!(args
            .iter()
            .any(|arg| arg.contains(&format!("source_name={QUASAR_MIC_SOURCE_NAME}"))));
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

    // The daemon's runtime path must be a PRIVATE subdir (PulseAudio force-chmods it
    // 0700), while socket and sink are pinned at the shared socket dir via `-n --load=`
    // so non-root app containers (Steam at 99:100) can reach them.
    #[test]
    fn pulse_run_pins_shared_paths_outside_private_runtime_dir() {
        let dir = "/run/quasar-agent/pulse-test";
        let args = pulse_command(dir);
        assert!(args.iter().any(|arg| arg == "-n"));
        let native = args
            .iter()
            .find(|arg| arg.starts_with("--load=module-native-protocol-unix"))
            .expect("explicit native-protocol load");
        assert!(native.contains(&format!("socket={dir}/native")));
        // Anonymous grant on the private per-session socket: the Wolf/GOW model that lets
        // Proton/pressure-vessel clients, whose remapped cookie diverges from any daemon
        // cookie, authenticate at all. No cookie is pinned.
        assert!(native.contains("auth-anonymous=1"));
        assert!(!native.contains("auth-cookie"));
        assert!(!args.iter().any(|arg| arg.starts_with("PULSE_COOKIE=")));

        // The baked module set is exactly: the session OUTPUT null-sink, the microphone
        // FEED null-sink, and the remap-source over the latter's monitor, in that order.
        // First sink loaded = default SINK, so `quasar_output` must never be displaced by
        // `quasar_mic`. The default SOURCE does become `quasar_mic_src` (a remap-source
        // outranks monitors regardless of load order), which is intended: an app reading
        // the default source gets the microphone. Nothing on the capture side relies on
        // the default — both agent `pulsesrc`s pin `device=quasar_output.monitor`.
        let null_sinks: Vec<&String> = args
            .iter()
            .filter(|arg| arg.starts_with("--load=module-null-sink"))
            .collect();
        assert_eq!(null_sinks.len(), 2, "output sink + microphone feed sink");
        assert!(null_sinks[0].contains("sink_name=quasar_output"));
        assert!(null_sinks[0].contains("device.class='sound'"));
        assert!(null_sinks[1].contains("sink_name=quasar_mic"));
        assert!(null_sinks[1].contains("device.class='sound'"));
        assert!(null_sinks[1].contains("device.description='Quasar Microphone Feed'"));
        // Pinned to the Opus wire format so decoded mic audio is not resampled through
        // the daemon default 44100.
        assert!(null_sinks[1].contains("rate=48000"));
        assert!(null_sinks[1].contains("channels=2"));

        let remap = args
            .iter()
            .find(|arg| arg.starts_with("--load=module-remap-source"))
            .expect("baked-in microphone capture source");
        assert!(remap.contains("master=quasar_mic.monitor"));
        assert!(remap.contains("source_name=quasar_mic_src"));
        // Steam hides `abstract`-class devices from its pickers, so the mic source must
        // present as a real sound device or voice chat cannot select it.
        assert!(remap.contains("device.class='sound'"));

        // The remap-source must be loaded AFTER its master sink exists.
        let mic_sink_at = args
            .iter()
            .position(|arg| arg.contains("sink_name=quasar_mic"))
            .unwrap();
        let output_sink_at = args
            .iter()
            .position(|arg| arg.contains("sink_name=quasar_output"))
            .unwrap();
        let remap_at = args
            .iter()
            .position(|arg| arg.starts_with("--load=module-remap-source"))
            .unwrap();
        assert!(
            output_sink_at < mic_sink_at,
            "quasar_output must load first so it stays the default sink"
        );
        assert!(mic_sink_at < remap_at, "remap master must exist first");
    }
}
