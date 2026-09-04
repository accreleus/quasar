//! Per-session application container lifecycle: launch the assigned image as a
//! containerized Wayland client of the per-session compositor, and tear it down with
//! no orphans on every terminal transition (a leaked container is leaked GPU/VRAM the
//! control plane already released — P1-6 reservation-release depends on it).
//!
//! ## Runtime: Docker, shelled out to the CLI
//! `games-on-whales/wolf`'s published app images are Docker images; it fits invariant
//! #1 (in production the agent runs on the host and talks to the local daemon, in dev
//! it runs inside `quasar-agent-dev` and launches SIBLING containers via the mounted
//! `/var/run/docker.sock`, same code path); and the exact `docker run` line is then
//! logged and copy-pasteable. Podman is a drop-in (`QUASAR_CONTAINER_RUNTIME`); Docker
//! is the tested default. The end-state K8s model replaces this module with a CRI/pod
//! spec, threading the same inputs.
//!
//! ## App-container launch contract (what `run()` guarantees, and why)
//! Not independent knobs: the minimum set that lets a real Steam/Proton title run
//! under an unprivileged, isolated container. Each per-flag rationale is at its call
//! site in `run`; knob values are in `docs/configuration.md`.
//!   - `--cap-drop ALL` + a re-added user-switch subset (`CHOWN`, `DAC_OVERRIDE`,
//!     `FOWNER`, `SETGID`, `SETUID`, `SETPCAP`, plus `KILL` and `SYS_NICE`), because
//!     the image entrypoint is root-init-then-`setpriv`.
//!   - `--security-opt seccomp=unconfined` (`QUASAR_APP_SECCOMP`): Docker's default
//!     profile denies the userns creation Steam's pressure-vessel (bwrap) requires.
//!   - `--security-opt apparmor=…`, on AppArmor hosts only: `docker-default` denies mount
//!     inside that userns, which is the other half of the same gate. The scoped
//!     `quasar-app` profile (`deploy/apparmor/quasar-app`) is used when the host has it
//!     loaded, `unconfined` when it does not.
//!   - `--security-opt no-new-privileges` + `--pids-limit` (`QUASAR_APP_PIDS_LIMIT`,
//!     default 8192, a fork-bomb backstop that still fits Steam + a game).
//!   - `--shm-size` (`QUASAR_APP_SHM_SIZE`, default 1g): Chromium-embedding apps fail
//!     their GPU command buffers on Docker's 64 MB shm.
//!   - `--network` (`QUASAR_CONTAINER_NETWORK`, default `none`).
//!   - `--security-opt systempaths=unconfined` (per-app `systempaths_unconfined`,
//!     default off): desktop images need /proc unmasked or Flatpak's bwrap sandbox
//!     cannot mount a fresh /proc.
//!   - PUID/PGID forwarded as env (`QUASAR_APP_PUID`/`PGID`), never `--user`, which
//!     would bypass the image's root-init-then-drop entrypoint.
//!   - Wayland: only the session's own socket FILE is bind-mounted in, chmod'd 0666 so
//!     a non-root app UID can `connect()` it.
//!   - Audio: `PULSE_SERVER`/`PULSE_SINK`/`PULSE_SOURCE` injected by the caller
//!     (`host.rs`/`source.rs`); `PULSE_SOURCE` points voice chat at the session's
//!     remapped mic (`quasar_mic_src`), and catalog env wins for the device names. The
//!     sidecar socket grants anonymous auth, so no cookie is shared
//!     (`audio::pulse_run_args`).
//!   - GPU: `--gpus all` (NVIDIA) or `--device /dev/dri` (VA/DRI), plus one numeric
//!     `--group-add` per group owning a passed DRM node. The image registers the
//!     runtime-injected driver itself, never baked.

use std::collections::BTreeMap;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{anyhow, Context, Result};

use crate::messages::AppExitPolicy;

/// Upper bound on any single container-runtime CLI invocation (#149). A wedged docker
/// daemon otherwise blocks the session thread forever (`run` at launch, `rm -f` at
/// teardown). On timeout the child is killed and the call fails, so the caller's error
/// path surfaces it and the control plane can reap the reservation.
const RUNTIME_CMD_TIMEOUT: Duration = Duration::from_secs(30);

/// Image pulls legitimately take minutes (multi-GB layers) and run off the agent loop
/// on a spawned thread, so this bound only guards a wedged daemon.
const RUNTIME_PULL_TIMEOUT: Duration = Duration::from_secs(600);

/// Per-stream cap on captured child stdout/stderr, so a pathological runtime cannot
/// flood the agent's memory. Every consumer here reads a container id, a short
/// `--format` line, an `OOMKilled` bool or a one-line error, so it never truncates real
/// output. Reads run only after the child has exited (the `try_wait` → `Some` arm), so
/// the writer is gone and draining the two pipes sequentially cannot deadlock.
const MAX_CAPTURE_BYTES: u64 = 256 * 1024;

/// `Command::output()` with a deadline: spawn, poll `try_wait`, kill on timeout.
fn output_with_timeout(cmd: &mut Command, what: &str) -> Result<Output> {
    output_with_deadline(cmd, what, RUNTIME_CMD_TIMEOUT)
}

fn output_with_deadline(cmd: &mut Command, what: &str, timeout: Duration) -> Result<Output> {
    let mut child = cmd
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .stdin(Stdio::null())
        .spawn()
        .with_context(|| format!("failed to exec {what}"))?;
    let deadline = Instant::now() + timeout;
    loop {
        match child.try_wait() {
            Ok(Some(status)) => {
                // Exited ⇒ both write ends are closed, so a sequential bounded drain
                // sees EOF and cannot block.
                let stdout = capped_read(child.stdout.take());
                let stderr = capped_read(child.stderr.take());
                return Ok(Output {
                    status,
                    stdout,
                    stderr,
                });
            }
            Ok(None) => {
                if Instant::now() >= deadline {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Err(anyhow!(
                        "{what} timed out after {}s — container runtime unresponsive",
                        timeout.as_secs()
                    ));
                }
                std::thread::sleep(Duration::from_millis(50));
            }
            Err(e) => return Err(anyhow!("wait for {what}: {e}")),
        }
    }
}

/// Drain a child pipe into a `Vec`, capped at [`MAX_CAPTURE_BYTES`]. A read error or a
/// `None` stream yields what was collected so far.
fn capped_read(stream: Option<impl Read>) -> Vec<u8> {
    let mut buf = Vec::new();
    if let Some(s) = stream {
        let _ = s.take(MAX_CAPTURE_BYTES).read_to_end(&mut buf);
    }
    buf
}

/// Name prefix for per-session app containers, used to build a session's container
/// name and to sweep orphans on startup.
pub const SESSION_NAME_PREFIX: &str = "quasar-sess-";

/// Fixed `--cap-add` set re-granted after `--cap-drop ALL` in `run()`; per-capability
/// rationale is at that call site. A named const so a silent drop is caught by
/// `sys_nice_is_in_the_fixed_app_container_cap_add_set`, not only live.
const APP_CONTAINER_CAP_ADDS: [&str; 8] = [
    "CHOWN",
    "DAC_OVERRIDE",
    "FOWNER",
    "SETGID",
    "SETUID",
    "SETPCAP",
    "KILL",
    "SYS_NICE",
];

/// Container mount destination for the injected 32-bit NVIDIA driver libs (#375).
/// Must stay a quasar-private path: `/usr/nvidia/lib` collides with the GOW nvidia
/// driver-volume convention — upstream `30-nvidia.sh` cont-init sees `/usr/nvidia` and
/// copies `lib/gbm/nvidia-drm_gbm.so`, which a bare host-lib bind lacks, so every GOW
/// app container exited 1 at launch (black stream, 2026-07-19). quasar-images ships the
/// matching `ld.so.conf.d` entry for this path.
const NVIDIA_LIB32_MOUNT_DST: &str = "/opt/quasar/nvidia-lib32";

/// Upper bound on the #375 startup probe container (a small image pull + a glob).
const NVIDIA_LIB32_PROBE_TIMEOUT: Duration = Duration::from_secs(60);

/// Minimal images the #375 probe tries in order; both ship `sh`.
const NVIDIA_LIB32_PROBE_IMAGES: [&str; 2] = ["busybox", "alpine:3"];

/// POSIX `sh`: print the first 32-bit lib dir holding `libGLX_nvidia.so.*`, exit 0;
/// else exit 1. The search list is exactly the 32-bit dirs — `/usr/lib64` and
/// `/usr/lib/x86_64-linux-gnu` are never searched, and each glob is non-recursive so
/// `/host-usr/lib` cannot descend into a 64-bit multiarch subdir.
const NVIDIA_LIB32_PROBE_SCRIPT: &str = "for d in /host-usr/lib /host-usr/lib32 /host-usr/lib/i386-linux-gnu; do for f in \"$d\"/libGLX_nvidia.so.*; do [ -e \"$f\" ] && printf %s \"$d\" && exit 0; done; done; exit 1";

/// #384: the display mode handed to the app container — the session's streamed
/// `width`/`height`/`fps`. The compositor's `wl_output` advertises it, but an app that
/// does not read the Wayland output (notably a nested gamescope, which sizes its
/// virtual output from `-W/-H/-r`) has no other way to learn it: without the injection
/// every gamescope session rendered at the image's baked 1080p60 and the compositor
/// upscaled it, so the selected profile silently did nothing.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AppDisplayMode {
    pub width: i32,
    pub height: i32,
    pub fps: i32,
}

/// Where the container's display-mode env came from (see [`app_display_env`]). Recorded
/// verbatim in the `session.effective_media` trace so a diagnosis can tell a true 1440p
/// session from a 1080p render upscaled into a 1440p stream.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AppDisplaySource {
    /// The agent injected the session mode (the normal case).
    Agent,
    /// The catalog's `runtime_spec.env` pinned at least one key, so the agent deferred
    /// for that key.
    AppCatalog,
    /// `QUASAR_APP_DISPLAY_ENV` off: nothing injected, the container uses its image
    /// default.
    Disabled,
}

impl AppDisplaySource {
    pub fn as_str(self) -> &'static str {
        match self {
            AppDisplaySource::Agent => "agent",
            AppDisplaySource::AppCatalog => "app-catalog",
            AppDisplaySource::Disabled => "disabled",
        }
    }
}

/// The generic Quasar contract every image should read.
const APP_DISPLAY_VARS: [&str; 3] = [
    "QUASAR_STREAM_WIDTH",
    "QUASAR_STREAM_HEIGHT",
    "QUASAR_STREAM_FPS",
];

/// Compatibility shim for images that only read gamescope's own variables — today every
/// published quasar-images game image, which bakes `GAMESCOPE_WIDTH=1920
/// GAMESCOPE_HEIGHT=1080 GAMESCOPE_REFRESH=60` and passes them to `gamescope -W/-H/-r`.
/// A runtime `-e` overrides an image `ENV`, so this fixes them with no rebuild. Images
/// should migrate to `QUASAR_STREAM_*`; `QUASAR_APP_GAMESCOPE_ENV` drops the shim.
const APP_DISPLAY_GAMESCOPE_VARS: [&str; 3] =
    ["GAMESCOPE_WIDTH", "GAMESCOPE_HEIGHT", "GAMESCOPE_REFRESH"];

/// What to inject as the app-container display env, and how to describe it in the trace.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AppDisplayEnv {
    /// `(key, value)` pairs to emit as `-e key=value`. Keys the app catalog already set
    /// are absent: the spec-env loop emits those, and re-emitting would duplicate `-e`.
    pub vars: Vec<(String, String)>,
    pub source: AppDisplaySource,
    /// Whether the gamescope shim was emitted.
    pub gamescope_env: bool,
}

/// Resolve the display-mode env for an app container (#384). Pure over its inputs plus
/// the two knobs `QUASAR_APP_DISPLAY_ENV` / `QUASAR_APP_GAMESCOPE_ENV`, so
/// [`ContainerRuntime::run`] and the `session.effective_media` trace can never disagree
/// about what the container was told. Precedence: app-catalog `runtime_spec.env` wins
/// per key — a key the catalog set is skipped here (an app pinned to 720p keeps working,
/// no duplicate `-e KEY=`) and the source is reported as `app-catalog`.
pub fn app_display_env(mode: AppDisplayMode, spec_env: &BTreeMap<String, String>) -> AppDisplayEnv {
    app_display_env_with(
        mode,
        spec_env,
        !env_disabled("QUASAR_APP_DISPLAY_ENV"),
        !env_disabled("QUASAR_APP_GAMESCOPE_ENV"),
    )
}

/// [`app_display_env`]'s knob-free core, so unit tests never mutate process-global env
/// (which races under the parallel test harness).
fn app_display_env_with(
    mode: AppDisplayMode,
    spec_env: &BTreeMap<String, String>,
    enabled: bool,
    want_gamescope: bool,
) -> AppDisplayEnv {
    if !enabled {
        return AppDisplayEnv {
            vars: Vec::new(),
            source: AppDisplaySource::Disabled,
            gamescope_env: false,
        };
    }
    let values = [
        mode.width.to_string(),
        mode.height.to_string(),
        mode.fps.to_string(),
    ];

    let mut vars = Vec::with_capacity(6);
    let mut deferred = false;
    let mut gamescope_emitted = false;
    let mut push = |names: &[&str; 3], gamescope: bool, vars: &mut Vec<(String, String)>| {
        for (name, value) in names.iter().zip(values.iter()) {
            if spec_env.contains_key(*name) {
                deferred = true;
                continue;
            }
            vars.push(((*name).to_string(), value.clone()));
            if gamescope {
                gamescope_emitted = true;
            }
        }
    };
    push(&APP_DISPLAY_VARS, false, &mut vars);
    if want_gamescope {
        push(&APP_DISPLAY_GAMESCOPE_VARS, true, &mut vars);
    }

    AppDisplayEnv {
        vars,
        source: if deferred {
            AppDisplaySource::AppCatalog
        } else {
            AppDisplaySource::Agent
        },
        gamescope_env: gamescope_emitted,
    }
}

/// `VAR=0|false|no` ⇒ disabled. Unset or anything else ⇒ enabled (default-on).
fn env_disabled(var: &str) -> bool {
    matches!(
        std::env::var(var).ok().as_deref(),
        Some("0") | Some("false") | Some("no")
    )
}

/// The container runtime CLI (docker by default; podman is compatible).
#[derive(Debug, Clone)]
pub struct ContainerRuntime {
    bin: String,
    /// Request an NVIDIA GPU via `--gpus all` (AMD/Intel use `--device /dev/dri`).
    /// Knob: `QUASAR_GPU_NVIDIA`.
    nvidia: bool,
}

impl ContainerRuntime {
    /// Knobs: `QUASAR_CONTAINER_RUNTIME`, `QUASAR_GPU_NVIDIA`.
    pub fn from_env() -> Self {
        let bin =
            std::env::var("QUASAR_CONTAINER_RUNTIME").unwrap_or_else(|_| "docker".to_string());
        let nvidia = matches!(
            std::env::var("QUASAR_GPU_NVIDIA").ok().as_deref(),
            Some("1") | Some("true") | Some("TRUE")
        );
        ContainerRuntime { bin, nvidia }
    }

    /// The "NVIDIA in play" signal: gates the #375 startup probe for the host's 32-bit
    /// driver libs and the mount in [`ContainerRuntime::run`].
    pub fn is_nvidia(&self) -> bool {
        self.nvidia
    }

    /// The container-runtime binary. Exposed for the S1 driver-volume provisioner,
    /// which resolves the agent's own mounts to find the volume's host path.
    pub fn bin(&self) -> &str {
        &self.bin
    }

    /// Read one `ENV` value baked into an image. The S1 driver-volume wiring must
    /// APPEND to an image's own `LD_LIBRARY_PATH` rather than replace it (docker `-e`
    /// replaces). Best-effort: an inspect failure returns `None`.
    pub fn image_env(&self, image: &str, key: &str) -> Option<String> {
        let out = output_with_timeout(
            Command::new(&self.bin).args([
                "image",
                "inspect",
                "--format",
                "{{range .Config.Env}}{{println .}}{{end}}",
                image,
            ]),
            "image env inspect",
        )
        .ok()?;
        if !out.status.success() {
            return None;
        }
        let prefix = format!("{key}=");
        String::from_utf8_lossy(&out.stdout)
            .lines()
            .find_map(|l| l.strip_prefix(&prefix).map(str::to_string))
            .filter(|v| !v.is_empty())
    }

    /// S5: follow an app container's log stream into a bounded [`AppLogRing`] from
    /// launch. Returns the thread handle so the exit path can drain it before
    /// snapshotting ([`ContainerRuntime::await_log_drain`]).
    ///
    /// A follower, not a `docker logs` at exit: containers run `--rm`, so the daemon
    /// reaps the container and its logs the moment it exits, losing exactly the case
    /// that matters (#463: Steam prints "Steam needs to be online to update" and exits 0
    /// in under a second). `--tail 100`, not `--tail 0`, because the follower attaches a
    /// few ms after `docker run` returns and `--tail 0` skips precisely the lines a
    /// container that died in that window already wrote; the ring's cap makes the replay
    /// overlap free.
    ///
    /// Residual race, accepted: a container reaped before this thread's `docker logs`
    /// reaches the daemon leaves an empty tail. Closing it means dropping `--rm` and
    /// reaping ourselves, real orphan-leak risk for a sub-attach-latency window; the
    /// failure still reports its exit code.
    ///
    /// Two threads per generation, one per stream, since the daemon keeps stdout and
    /// stderr separate and nothing here merges them: interleaving BETWEEN streams is
    /// approximate, each stream's own order exact. Both end when the streams close.
    pub fn spawn_log_follower(
        &self,
        container_id: String,
        ring: AppLogRing,
    ) -> Option<std::thread::JoinHandle<()>> {
        let bin = self.bin.clone();
        let builder = std::thread::Builder::new().name("quasar-app-logs".to_string());
        let tail = APP_LOG_TAIL_LINES.to_string();
        let log_span = tracing::Span::current();
        let spawned = builder.spawn(move || {
            // C9: re-enter the session span so this thread's lines carry session=<id>.
            let _log_span = log_span.enter();
            let child = Command::new(&bin)
                .args(["logs", "-f", "--tail", &tail, &container_id])
                .stdout(Stdio::piped())
                .stderr(Stdio::piped())
                .stdin(Stdio::null())
                .spawn();
            let mut child = match child {
                Ok(c) => c,
                Err(e) => {
                    tracing::debug!("app log follower: failed to exec `{bin} logs`: {e}");
                    return;
                }
            };
            let stderr = child.stderr.take();
            let err_ring = ring.clone();
            let err_thread = stderr.and_then(|s| {
                std::thread::Builder::new()
                    .name("quasar-app-logs-err".to_string())
                    .spawn(move || drain_into_ring(s, &err_ring))
                    .ok()
            });
            if let Some(out) = child.stdout.take() {
                drain_into_ring(out, &ring);
            }
            if let Some(t) = err_thread {
                let _ = t.join();
            }
            // The child is `docker logs`, not the app: reap it so it cannot zombie.
            let _ = child.wait();
        });
        match spawned {
            Ok(h) => Some(h),
            Err(e) => {
                tracing::warn!(
                    token = "app-log-follower-spawn-failed",
                    "app log follower: could not spawn reader thread: {e} — an early app exit \
                     will be reported without its log tail"
                );
                None
            }
        }
    }

    /// Wait briefly for a log follower to finish draining, so a snapshot taken right
    /// after the container exited sees its LAST lines rather than whatever had arrived
    /// when the exit waiter fired.
    ///
    /// Bounded, never a plain join: `JoinHandle` has no timed join, so this polls
    /// `is_finished` against [`APP_LOG_DRAIN_BUDGET`] rather than blocking teardown
    /// behind an unresponsive runtime. Giving up drops the handle — the thread detaches
    /// and ends when the stream closes, and the ring is shared, so a late line is missed,
    /// never corrupt.
    pub fn await_log_drain(handle: std::thread::JoinHandle<()>, budget: Duration) {
        let deadline = Instant::now() + budget;
        while !handle.is_finished() {
            if Instant::now() >= deadline {
                tracing::debug!(
                    "app log follower still draining after {budget:?}; snapshotting the tail as-is"
                );
                return;
            }
            std::thread::sleep(Duration::from_millis(5));
        }
        let _ = handle.join();
    }

    /// #375: locate the host dir holding the 32-bit NVIDIA driver libs, for read-only
    /// injection into NVIDIA app containers. The agent cannot see the host filesystem,
    /// so this runs a short-lived probe container that bind-mounts host `/usr` read-only
    /// and globs `libGLX_nvidia.so.*` under `/usr/lib`, `/usr/lib32`,
    /// `/usr/lib/i386-linux-gnu`, excluding the 64-bit dirs. Returns the first hit as a
    /// host path.
    ///
    /// Fails open, never fatal: it tries `busybox` then `alpine:3`, moving on if a
    /// pull/run fails (no network on a locked-down host). Script exit 1 ("ran, found
    /// nothing") is definitive and returns `None` without trying further images. Any
    /// failure ⇒ `None`, and the operator can set `QUASAR_NV_LIB32_PATH`.
    pub fn probe_nvidia_lib32_path(&self) -> Option<String> {
        for image in NVIDIA_LIB32_PROBE_IMAGES {
            let out = output_with_deadline(
                Command::new(&self.bin).args([
                    "run",
                    "--rm",
                    "-v",
                    "/usr:/host-usr:ro",
                    image,
                    "sh",
                    "-c",
                    NVIDIA_LIB32_PROBE_SCRIPT,
                ]),
                "nvidia lib32 probe",
                NVIDIA_LIB32_PROBE_TIMEOUT,
            );
            match out {
                // exit 0: the script printed the container-path dir it found libs in.
                Ok(o) if o.status.success() => {
                    let container_dir = String::from_utf8_lossy(&o.stdout).trim().to_string();
                    // Container view back to host path: /host-usr/lib -> /usr/lib.
                    if let Some(rest) = container_dir.strip_prefix("/host-usr") {
                        let host_path = format!("/usr{rest}");
                        if host_path.len() > "/usr".len() {
                            return Some(host_path);
                        }
                    }
                    tracing::debug!(
                        token = "nvidia-lib32-probe-unexpected-dir",
                        "nvidia lib32 probe returned an unexpected dir {container_dir:?}; ignoring"
                    );
                    return None;
                }
                // exit 1: ran, found nothing. Definitive; another image won't differ.
                Ok(o) if o.status.code() == Some(1) => return None,
                // Any other non-zero (125 = couldn't run/pull): try the next image.
                Ok(o) => {
                    tracing::debug!(
                        "nvidia lib32 probe via {image}: exit {:?}: {}",
                        o.status.code(),
                        String::from_utf8_lossy(&o.stderr).trim()
                    );
                    continue;
                }
                Err(e) => {
                    tracing::debug!("nvidia lib32 probe via {image} failed to run: {e}");
                    continue;
                }
            }
        }
        None
    }

    /// Pull the image, on `session_assign` so `session_start` is fast (the
    /// reserve→prepare→go-live split). Idempotent; a present image is a no-op.
    ///
    /// Must inspect before pulling: a locally-built image has no registry to pull from,
    /// so a plain `docker pull` fails with "pull access denied" even though the image is
    /// present, turning the intended no-op into a spurious assign-time warning.
    pub fn pull(&self, image: &str) -> Result<()> {
        let present = output_with_timeout(
            Command::new(&self.bin).args(["image", "inspect", image]),
            "image inspect",
        )
        .map(|o| o.status.success())
        .unwrap_or(false);
        if present {
            tracing::debug!("image {image} already present locally — skipping pull");
            return Ok(());
        }
        tracing::info!("pulling image {image} via {}", self.bin);
        let out = output_with_deadline(
            Command::new(&self.bin).args(["pull", image]),
            "image pull",
            RUNTIME_PULL_TIMEOUT,
        )?;
        if !out.status.success() {
            return Err(anyhow!(
                "`{} pull {image}` failed: {}",
                self.bin,
                String::from_utf8_lossy(&out.stderr).trim()
            ));
        }
        Ok(())
    }

    /// Deterministic container name for a session, so a stale container from a
    /// crashed prior run is found and removed before we launch (no orphans, no
    /// name collision). Docker names allow `[a-zA-Z0-9_.-]`; a UUID qualifies.
    pub fn container_name(session_id: &str) -> String {
        format!("{SESSION_NAME_PREFIX}{session_id}")
    }

    /// Force-remove every container whose name contains any of `prefixes`. Called at
    /// agent startup to reap session/sidecar containers orphaned by a previous agent
    /// that died without a clean unwind: `docker run --rm` siblings survive a SIGKILL of
    /// the agent (P2-05). Fails open — logged, never fatal, since a clean boot must not
    /// hinge on the sweep. Returns the number removed.
    pub fn sweep_orphans(&self, prefixes: &[&str]) -> usize {
        let mut removed = 0;
        for prefix in prefixes {
            // `--filter name=` matches as a substring; our names all start with the
            // prefix and no other container carries it, so this selects exactly the
            // orphans.
            let out = output_with_timeout(
                Command::new(&self.bin).args(["ps", "-aq", "--filter", &format!("name={prefix}")]),
                "container ps",
            );
            let ids = match out {
                Ok(o) if o.status.success() => {
                    String::from_utf8_lossy(&o.stdout).trim().to_string()
                }
                Ok(o) => {
                    tracing::warn!(
                        token = "orphan-sweep-ps-failed",
                        "orphan sweep: `{} ps` for {prefix} failed: {}",
                        self.bin,
                        String::from_utf8_lossy(&o.stderr).trim()
                    );
                    continue;
                }
                Err(e) => {
                    tracing::warn!(
                        token = "orphan-sweep-ps-exec-failed",
                        "orphan sweep: failed to exec `{} ps` for {prefix}: {e}",
                        self.bin
                    );
                    continue;
                }
            };
            for id in ids.lines().filter(|l| !l.is_empty()) {
                tracing::info!("orphan sweep: removing stale container {id} (prefix {prefix})");
                self.force_remove(id);
                removed += 1;
            }
        }
        removed
    }

    /// Launch the app container as a detached Wayland client of the session
    /// compositor. Returns a handle whose `Drop` guarantees teardown.
    pub fn run(&self, spec: &ContainerSpec, params: &LaunchParams) -> Result<RunningContainer> {
        // A swap (P2-07) briefly runs the old and new app containers at once, so a
        // per-generation name override avoids a collision; the demo path passes None
        // and gets the stable per-session name.
        let name = params
            .container_name
            .clone()
            .unwrap_or_else(|| Self::container_name(params.session_id));

        // Validate the network BEFORE anything is spawned: an out-of-set value must
        // fail the launch, never reach `docker run`.
        let network = resolve_network(spec.network.as_deref())?;

        // Clear any stale same-named container first (idempotent prepare).
        self.force_remove(&name);

        let mut args: Vec<String> = vec![
            "run".into(),
            "-d".into(),   // detached — the agent supervises via lifecycle, not stdout
            "--rm".into(), // self-clean if it exits on its own (no orphan)
            "--name".into(),
            name.clone(),
            // Isolated by default: `none` unless the app declares a requirement
            // (§S2: Steam's first boot must download steamui.so) or the operator sets
            // a host-wide default.
            "--network".into(),
            network,
            // Tenant apps need no capabilities to connect to the session-owned Wayland
            // socket or use explicitly mapped devices. Privilege reduction stays on
            // even when an operator enables app networking; this bounds the launched
            // workload, not the admin.
            "--cap-drop".into(),
            "ALL".into(),
        ];
        // The quasar-images entrypoint is root-init-then-drop (useradd for PUID/PGID,
        // then `setpriv --reuid`), which a bare cap-drop ALL kills at init: useradd
        // exits 10 (mode-000 Fedora shadow files need CAP_DAC_OVERRIDE) and setpriv
        // needs SET[UG]ID. The re-added user-switch set is a strict subset of Docker's
        // defaults; the entrypoint still drops everything for the app itself.
        //
        // KILL: tini (container root) must SIGTERM the post-setpriv unprivileged
        // launcher on `docker stop`. Without CAP_KILL that cross-uid kill() is EPERM,
        // tini treats it as fatal, and pid-1 death SIGKILLs the namespace — every
        // "graceful" stop silently degraded to a hard kill (gate G1; also explains
        // Steam's persistent unclean-shutdown marker). Grants nothing new: container
        // root holds CAP_SETUID and could setuid and signal anyway.
        //
        // SYS_NICE: without it gamescope falls back to regular-priority compute, seen as
        // jitter under concurrent-session CPU contention. Reaches the app only when the
        // quasar-images entrypoint ambient-grants it across the setpriv uid-drop.
        for cap in APP_CONTAINER_CAP_ADDS {
            args.push("--cap-add".into());
            args.push((*cap).into());
        }
        args.extend([
            // Fork-bomb backstop, not a workload sizing knob. 512 strangled a full
            // Steam client: threads count against the pids cgroup, and Steam (~290
            // idle) plus a game's thread pool, overlay and Fossilize shader workers
            // blow past it — pthread_create returns EAGAIN and the game aborts (Redout
            // reached 516 pids the instant the cap was lifted). 8192 still bounds a
            // runaway; GOW/Wolf leave Steam uncapped entirely.
            "--pids-limit".into(),
            std::env::var("QUASAR_APP_PIDS_LIMIT").unwrap_or_else(|_| "8192".into()),
            // Steam's embedded Chromium fails its GPU command buffers and renders
            // black/flashing below ~1 GB of /dev/shm, well above Docker's 64 MB
            // default. shm is tmpfs, allocated on use, so a roomy default is free.
            "--shm-size".into(),
            std::env::var("QUASAR_APP_SHM_SIZE").unwrap_or_else(|_| "1g".into()),
        ]);

        // Both privilege opt-outs below ride the wire, so a catalog manifest chooses
        // them. `deny` makes this host ignore both and keep the hardened posture, for
        // an operator running a catalog they do not author. Knob:
        // `QUASAR_APP_PRIVILEGE_OPTOUT`.
        let privilege_optout = privilege_optout_allowed();
        let no_new_privileges = spec.no_new_privileges || !privilege_optout;
        let systempaths_unconfined = spec.systempaths_unconfined && privilege_optout;
        if !privilege_optout && (!spec.no_new_privileges || spec.systempaths_unconfined) {
            tracing::warn!(
                token = "app-privilege-optout-denied",
                "app spec asked to weaken container hardening (no_new_privileges={}, \
                 systempaths_unconfined={}); QUASAR_APP_PRIVILEGE_OPTOUT=deny — ignored",
                spec.no_new_privileges,
                spec.systempaths_unconfined
            );
        }

        // Default-on hardening with a per-app runtime_spec opt-out: upstream GOW
        // desktop images `sudo` in their startup scripts, which no-new-privileges turns
        // into container exit 1 and a bare black compositor.
        if no_new_privileges {
            args.push("--security-opt".into());
            args.push("no-new-privileges:true".into());
        }

        // Docker's default seccomp profile denies unprivileged user-namespace creation,
        // which Steam's pressure-vessel (bwrap) hard-requires: with it every Steam
        // launch dies at "bwrap: No permissions to create a new namespace" even though
        // the kernel allows userns. Game containers therefore default to
        // seccomp=unconfined, as GOW/Wolf do. Knob: `QUASAR_APP_SECCOMP`.
        match std::env::var("QUASAR_APP_SECCOMP").as_deref() {
            Ok("default") => {} // omit the flag ⇒ Docker's builtin profile
            Ok(profile) if !profile.is_empty() => {
                args.push("--security-opt".into());
                args.push(format!("seccomp={profile}"));
            }
            _ => {
                args.push("--security-opt".into());
                args.push("seccomp=unconfined".into());
            }
        }

        // seccomp is not the only gate on the user namespaces the images need: Ubuntu's
        // `docker-default` AppArmor profile denies mount inside one, so Steam's bootstrap
        // fails ("Steam now requires user namespaces to be enabled") while `unshare -U`
        // alone succeeds and `unshare -Urm` dies with "cannot change root filesystem
        // propagation: Permission denied". The scoped `quasar-app` profile
        // (deploy/apparmor/quasar-app) is docker-default with that one family allowed;
        // without it loaded the fallback is still unconfined. Only on AppArmor hosts — an
        // SELinux host (Fedora, `spc_t`) must get a byte-identical argv. Pairs with a HOST
        // setting on Ubuntu 24.04+: `kernel.apparmor_restrict_unprivileged_userns=0`.
        let apparmor = app_apparmor_selection();
        match &apparmor {
            AppArmorChoice::NoFlag => {}
            AppArmorChoice::Profile(p) => tracing::info!(
                token = "app-apparmor-profile",
                profile = %p,
                "host uses AppArmor: app container is confined by the {p} profile"
            ),
            AppArmorChoice::Unconfined => tracing::info!(
                token = "app-apparmor-unconfined",
                "host uses AppArmor: app container runs unconfined so its user namespaces can mount"
            ),
        }
        args.extend(apparmor.args());

        // Per-app opt-in (`"systempaths_unconfined": true`): unmask /proc and /sys.
        // Docker's default `systempaths=masked` blocks Flatpak's sandbox helper
        // (`bwrap`) from mounting a fresh /proc inside the app's mount namespace, so
        // `flatpak run` fails even with seccomp=unconfined (verified 2026-08-13). Must
        // stay per-app, never host-wide: it widens what the container sees.
        if systempaths_unconfined {
            args.push("--security-opt".into());
            args.push("systempaths=unconfined".into());
        }

        // Off by default: many game images write under their root filesystem, so
        // forcing read-only would be a compatibility break. Knob:
        // `QUASAR_APP_READ_ONLY`, for hosts that validated their catalog and route
        // writable state through explicit mounts/tmpfs.
        if matches!(
            std::env::var("QUASAR_APP_READ_ONLY").as_deref(),
            Ok("1" | "true" | "yes")
        ) {
            args.push("--read-only".into());
        }

        // ── udev records: gamepad discovery inside the container ─────────────
        // SDL/Steam enumerate input devices via libudev, not by scanning /dev/input, so
        // with no udevd records in the container the --device-passed virtual gamepad is
        // invisible to games. `virtual_input::export_udev_data` writes this session's
        // fake-udev records to a host-shared dir, mounted read-only where libudev reads.
        // Skipped when absent (test-src sessions).
        let udev_dir = super::virtual_input::udev_export_dir(params.runtime_dir, params.session_id);
        if udev_dir.is_dir() {
            args.push("--mount".into());
            args.push(format!(
                "type=bind,src={},dst=/run/udev/data,readonly",
                udev_dir.display()
            ));
        }

        // ── Wayland: hand only this session's socket to the container ───
        // The host runtime dir is 0700, agent-owned. Mounting the whole directory made
        // uid-0 app containers unable to traverse it after `--cap-drop ALL`, and
        // granting DAC caps would expose every other session's sockets and Pulse state.
        // A file bind gives a traversable, container-private parent holding one socket.
        args.extend(wayland_mount_args(params));

        // ── GPU passthrough ───────────────────────────────────────────────────
        if spec.gpu {
            if self.nvidia {
                args.push("--gpus".into());
                args.push("all".into());
                // #375: bind the host's 32-bit NVIDIA driver libs read-only so native
                // 32-bit titles resolve libGLX_nvidia.so.* — the container ships only
                // 64-bit libs and the toolkit/CDI spec never injects 32-bit. NVIDIA
                // only; empty path ⇒ no mount. Falls back to the driver volume's
                // 32-bit half, resolved live so a provision completed after startup
                // takes effect on the next launch with no agent restart.
                let lib32 = if params.nvidia_lib32_path.is_empty() {
                    crate::nvidia_volume::lib32_host_path(crate::nvidia_volume::current().as_ref())
                        .unwrap_or_default()
                } else {
                    params.nvidia_lib32_path.to_string()
                };
                args.extend(nvidia_lib32_mount_args(&lib32));

                // First-run S1: on a host whose NVIDIA userspace came from the
                // Quasar-provisioned driver volume, the CDI injection into this app
                // container is as CUDA-only as the agent's was, so the app needs the
                // same 64-bit GL/EGL/Vulkan set, vendor configs and loader path. Empty
                // on every host with its own driver.
                args.extend(nvidia_driver_volume_args(self, &spec.image));
            }
            // AMD/Intel (and NVIDIA's render node for Vulkan/EGL) want the DRI nodes.
            args.push("--device".into());
            args.push(DRI_DIR.into());

            // The nodes arrive 0660 root:render, and the app user (PUID, no supplementary
            // groups) is neither — so RADV fails `Could not open device
            // /dev/dri/renderD128: Permission denied`, Vulkan enumerates llvmpipe only,
            // and gamescope aborts with "physical device doesn't support
            // VK_EXT_physical_device_drm" (desktop images degrade silently to software
            // rendering instead). NVIDIA never hit it: its ICD opens the 0666
            // /dev/nvidia* nodes. Grants nothing the passed device did not already imply.
            let dri_nodes = dri_node_owners(Path::new(DRI_DIR));
            let group_add = dri_group_add_args(&dri_nodes);
            if !group_add.is_empty() {
                tracing::info!(
                    token = "app-dri-group-add",
                    nodes = %dri_nodes
                        .iter()
                        .map(|n| format!("{}:{:o}:{}", n.name, n.mode & 0o777, n.gid))
                        .collect::<Vec<_>>()
                        .join(","),
                    "app container joins DRM node groups: {}",
                    group_add.join(" ")
                );
            }
            args.extend(group_add);
        }

        // ── Virtual input device nodes (mouse/keyboard for evdev-native apps,
        //    gamepad always — Wayland has no pad protocol) ─────────────────────
        for node in &params.device_nodes {
            args.push("--device".into());
            args.push(node.to_string_lossy().into_owned());
        }

        // ── xdg document portal: /dev/fuse for its fuse mount ─────────────────
        // Desktop images run xdg-document-portal, which fuse-mounts itself and fails
        // with "fuse: device not found" without this. Only add it when the host has the
        // node: `docker create --device` with a missing source path fails the whole
        // launch. Host capability detection, no knob.
        args.extend(fuse_device_args(host_has_fuse_node()));

        // ── runtime_spec: env, mounts, then image + args ──────────────────────
        for (k, v) in &spec.env {
            args.push("-e".into());
            args.push(format!("{k}={v}"));
        }

        // #384: the session's display mode. The app catalog wins per key —
        // `app_display_env` skips anything the spec-env loop already emitted. Named
        // `app_display`, not `display`: a bare `display` ident inside `tracing::info!`
        // resolves to `tracing::field::display`.
        let app_display = app_display_env(params.display, &spec.env);
        for (k, v) in &app_display.vars {
            args.push("-e".into());
            args.push(format!("{k}={v}"));
        }
        tracing::info!(
            "app display mode: {}x{}@{} (source={}, gamescope_env={})",
            params.display.width,
            params.display.height,
            params.display.fps,
            app_display.source.as_str(),
            app_display.gamescope_env
        );

        // Forward PUID/PGID as ENV, never docker `--user`: `--user` bypasses the
        // quasar-images root-init-then-drop entrypoint and breaks images that need it.
        // Unset ⇒ inject nothing. A catalog PUID/PGID in `spec.env` was emitted above
        // and wins, so skip it here rather than duplicate `-e PUID=`.
        for (host_var, app_var) in [("QUASAR_APP_PUID", "PUID"), ("QUASAR_APP_PGID", "PGID")] {
            if spec.env.contains_key(app_var) {
                continue;
            }
            if let Ok(val) = std::env::var(host_var) {
                if !val.is_empty() {
                    args.push("-e".into());
                    args.push(format!("{app_var}={val}"));
                }
            }
        }
        for m in &spec.mounts {
            args.push("-v".into());
            args.push(m.clone());
        }
        args.push(spec.image.clone());
        args.extend(spec.args.iter().cloned());

        tracing::info!("launching container: {} {}", self.bin, args.join(" "));
        let out = output_with_timeout(Command::new(&self.bin).args(&args), "container run")?;
        if !out.status.success() {
            return Err(anyhow!(
                "`{} run` failed for image {}: {}",
                self.bin,
                spec.image,
                String::from_utf8_lossy(&out.stderr).trim()
            ));
        }
        let id = String::from_utf8_lossy(&out.stdout).trim().to_string();
        tracing::info!("container {name} started (id={})", short_id(&id));

        // Desktop launch-profile post-start fixup, NVIDIA hosts in practice (verified
        // 2026-08-14). The kernel treats a container's /proc as "fully visible" — a
        // precondition bwrap's fresh `--proc` mount checks — only when nothing under it
        // is bind-mounted over, and the nvidia container runtime overmounts exactly one
        // file, `/proc/driver/nvidia/params`, in every GPU container (`--gpus all` and
        // CDI both). That trips the check even with `systempaths=unconfined`, so bwrap
        // fails "Can't mount proc" until it is cleared. The privileged umount's elevated
        // caps apply to that one exec'd process, never to the app container, whose
        // posture is unchanged. Gated on `systempaths_unconfined` and best-effort: a
        // non-GPU or AMD host has nothing mounted there, so a non-zero `umount` is the
        // expected silent case, not a launch failure.
        if systempaths_unconfined {
            match output_with_timeout(
                Command::new(&self.bin).args([
                    "exec",
                    "--privileged",
                    "--user",
                    "root",
                    &name,
                    "umount",
                    "/proc/driver/nvidia/params",
                ]),
                "container nvidia params unmount",
            ) {
                Ok(out) if out.status.success() => {
                    tracing::info!(
                        "container {name}: cleared /proc/driver/nvidia/params overmount \
                         (nvidia GPU host, desktop launch profile)"
                    );
                }
                Ok(out) => {
                    tracing::debug!(
                        "container {name}: no nvidia params overmount to clear ({})",
                        String::from_utf8_lossy(&out.stderr).trim()
                    );
                }
                Err(e) => {
                    tracing::debug!(
                        "container {name}: no nvidia params overmount to clear (exec failed: {e})"
                    );
                }
            }
        }

        // When the app contract requests GPU access, verify the daemon actually realized
        // the device mapping: a parsed command line is not enough, runtime policy can
        // discard it. Fail the launch rather than run a GPU app on software rendering.
        if spec.gpu {
            let inspect = output_with_timeout(
                Command::new(&self.bin).args([
                    "inspect",
                    "--format",
                    "{{json .HostConfig.Devices}} {{json .HostConfig.DeviceRequests}}",
                    &name,
                ]),
                "container GPU inspect",
            )?;
            let realized = String::from_utf8_lossy(&inspect.stdout);
            let dri_realized = inspect.status.success() && realized.contains("/dev/dri");
            let nvidia_realized = !self.nvidia
                || (realized.contains("DeviceRequests")
                    || realized.contains("\"Driver\":\"nvidia\"")
                    || realized.contains("\"Capabilities\":[[\"gpu\"]]"));
            if !dri_realized || !nvidia_realized {
                self.force_remove(&name);
                return Err(anyhow!(
                    "app requested GPU access but container runtime did not realize the expected devices (dri={dri_realized}, nvidia={nvidia_realized})"
                ));
            }
            tracing::info!("container {name} GPU access verified from runtime inspect");
        }

        Ok(RunningContainer {
            runtime: self.clone(),
            name,
            container_id: id,
            removed: Arc::new(AtomicBool::new(false)),
        })
    }

    /// A `docker pull -- <image>` for a caller that wants to STREAM the pull's output
    /// (the image-management P2 progress scraper) rather than take `pull`'s
    /// wait-for-completion behaviour. Exists so the executable name resolves in one
    /// place: a caller building its own `Command` would re-read
    /// `QUASAR_CONTAINER_RUNTIME` and could drift. `--` ends option parsing so a ref can
    /// never be read as a flag.
    pub fn pull_command(&self, image: &str) -> Command {
        let mut cmd = Command::new(&self.bin);
        cmd.args(["pull", "--", image]);
        cmd
    }

    /// The image-management P4 template-build command, the analogue of
    /// [`Self::pull_command`] and resolving the runtime executable in the same single
    /// place. `DOCKER_BUILDKIT=0` forces the classic builder so progress is the
    /// line-oriented `Step N/M : ...` form the scraper parses. `--` ends option parsing
    /// so the context path can never be read as a flag; each build arg is one argv
    /// element (`K=V`), so a value cannot inject one either.
    pub fn build_command(
        &self,
        local_tag: &str,
        dockerfile_path: &str,
        context_dir: &str,
        build_args: &BTreeMap<String, String>,
    ) -> Command {
        let mut cmd = Command::new(&self.bin);
        cmd.env("DOCKER_BUILDKIT", "0");
        cmd.args(["build", "-f", dockerfile_path, "-t", local_tag]);
        for (k, v) in build_args {
            cmd.arg("--build-arg");
            cmd.arg(format!("{k}={v}"));
        }
        cmd.arg("--");
        cmd.arg(context_dir);
        cmd
    }

    /// Run an arbitrary `docker <args>` command and return stdout on success.
    /// Used by the PulseAudio sidecar and other agent-managed containers that
    /// don't follow the full app-container spec path.
    pub fn run_raw(&self, args: &[&str]) -> anyhow::Result<String> {
        let out = output_with_timeout(
            Command::new(&self.bin).args(args),
            &format!("`{} {}`", self.bin, args.join(" ")),
        )?;
        if !out.status.success() {
            return Err(anyhow!(
                "`{} {}` failed: {}",
                self.bin,
                args.join(" "),
                String::from_utf8_lossy(&out.stderr).trim()
            ));
        }
        Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
    }

    /// Classify a `docker image inspect` for the image-management reconciliation paths
    /// (`ImageManager::new`, `refresh_register_images`). A plain `.is_ok()` on `run_raw`
    /// conflates "the image is gone" with "the daemon hiccuped", which would demote
    /// every managed image to `absent` — and persist that — on one transient error.
    /// `Ok(true)` present, `Ok(false)` the daemon's definitive "no such image", `Err`
    /// anything else (leave the existing record untouched and warn).
    pub fn image_present(&self, registry_ref: &str) -> Result<bool, String> {
        match self.run_raw(&["image", "inspect", "--", registry_ref]) {
            Ok(_) => Ok(true),
            Err(e) => {
                let msg = e.to_string();
                if msg.to_lowercase().contains("no such image") {
                    Ok(false)
                } else {
                    Err(msg)
                }
            }
        }
    }

    /// Graceful teardown: `docker stop -t N` (SIGTERM then SIGKILL after N seconds),
    /// then the `rm -f` backstop. The TERM window lets app images shut down cleanly —
    /// Steam marks its install unclean when killed and re-verifies every executable
    /// checksum on the next launch (~9 s measured), so kill-only teardown taxes every
    /// subsequent session start. Containers run `--rm`, so a successful stop
    /// auto-removes and `force_remove` is the idempotent backstop. Knob:
    /// `QUASAR_APP_STOP_TIMEOUT_SECS`.
    pub fn graceful_remove(&self, name: &str) {
        let secs: u64 = std::env::var("QUASAR_APP_STOP_TIMEOUT_SECS")
            .ok()
            .and_then(|v| v.parse().ok())
            .unwrap_or(10);
        if secs > 0 {
            let res = output_with_deadline(
                Command::new(&self.bin).args(["stop", "-t", &secs.to_string(), name]),
                "container stop",
                Duration::from_secs(secs + 20),
            );
            match res {
                Ok(out) if out.status.success() => {
                    tracing::debug!("stopped container {name} gracefully");
                }
                Ok(out) => {
                    let err = String::from_utf8_lossy(&out.stderr);
                    if !err.contains("No such container") {
                        tracing::warn!(
                            token = "container-stop-failed",
                            "`{} stop {name}` reported: {}",
                            self.bin,
                            err.trim()
                        );
                    }
                }
                Err(e) => tracing::warn!(
                    token = "container-stop-exec-failed",
                    "failed to exec `{} stop {name}`: {e}",
                    self.bin
                ),
            }
        }
        self.force_remove(name);
    }

    /// `rm -f` a container by name. Idempotent and best-effort: a missing
    /// container is success (the orphan-free postcondition holds either way).
    pub fn force_remove(&self, name: &str) {
        let res = output_with_timeout(
            Command::new(&self.bin).args(["rm", "-f", name]),
            "container rm",
        );
        match res {
            Ok(out) if out.status.success() => {
                tracing::debug!("removed container {name}");
            }
            Ok(out) => {
                let err = String::from_utf8_lossy(&out.stderr);
                // "No such container" = already gone; "removal ... in progress" = the
                // --rm auto-removal racing us after a graceful stop. Both satisfy the
                // orphan-free postcondition.
                if !err.contains("No such container") && !err.contains("is already in progress") {
                    tracing::warn!(
                        token = "container-rm-failed",
                        "`{} rm -f {name}` reported: {}",
                        self.bin,
                        err.trim()
                    );
                }
            }
            Err(e) => tracing::warn!(
                token = "container-rm-exec-failed",
                "failed to exec `{} rm -f {name}`: {e}",
                self.bin
            ),
        }
    }

    /// Block until `id` exits, then classify it (app-liveness spec §3 D1=b).
    ///
    /// MUST be called from a dedicated thread, never the session poll loop: `docker
    /// wait` blocks for the container's entire remaining lifetime, so routing it through
    /// `output_with_timeout`'s 30 s cap would falsely kill every long-lived session.
    /// Hence `Command::output()` directly, with no deadline.
    ///
    /// `docker wait` gives the exit code but not whether the cgroup OOM killer caused
    /// it, so a follow-up `docker inspect` runs immediately after — before the `--rm`
    /// auto-removal races it away — to read `OOMKilled`. That call IS bounded, being a
    /// single-shot inspect; a miss falls back to the wait exit code alone.
    pub fn wait_for_exit(&self, id: &str) -> AppExitStatus {
        let out = match Command::new(&self.bin)
            .args(["wait", id])
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .stdin(Stdio::null())
            .output()
        {
            Ok(o) if o.status.success() => o,
            Ok(o) => {
                tracing::warn!(
                    token = "container-wait-failed",
                    "`{} wait {id}` failed: {}",
                    self.bin,
                    String::from_utf8_lossy(&o.stderr).trim()
                );
                return AppExitStatus::Unknown;
            }
            Err(e) => {
                tracing::warn!(
                    token = "container-wait-exec-failed",
                    "failed to exec `{} wait {id}`: {e}",
                    self.bin
                );
                return AppExitStatus::Unknown;
            }
        };
        let Ok(code) = String::from_utf8_lossy(&out.stdout).trim().parse::<i32>() else {
            tracing::warn!(
                token = "container-wait-nonnumeric",
                "`{} wait {id}` printed a non-numeric exit code: {:?}",
                self.bin,
                String::from_utf8_lossy(&out.stdout)
            );
            return AppExitStatus::Unknown;
        };
        if let Ok(inspect) = output_with_timeout(
            Command::new(&self.bin).args(["inspect", "--format", "{{.State.OOMKilled}}", id]),
            "container exit inspect",
        ) {
            if inspect.status.success() && String::from_utf8_lossy(&inspect.stdout).trim() == "true"
            {
                return AppExitStatus::OomKilled;
            }
        }
        AppExitStatus::Code(code)
    }
}

/// A terminal app-container exit, classified for the app-liveness policy (spec §2/§3).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AppExitStatus {
    /// Exited with this code (0 = clean exit).
    Code(i32),
    /// The cgroup OOM-killed the container (`docker inspect` `OOMKilled=true`).
    OomKilled,
    /// `docker wait`/`inspect` could not be read (daemon hiccup, or the `--rm`
    /// auto-removal raced the inspect away). The runner's `fail` policy ends the session
    /// as for any other exit: fail-closed on purpose, because a session stuck on a
    /// frozen frame with no way to tell whether the app is alive is worse than an
    /// occasional false-positive failure.
    Unknown,
}

fn short_id(id: &str) -> String {
    id.chars().take(12).collect()
}

/// The container spec mirrored from the assign's `app` object (`apps.runtime_spec`,
/// `agent-api.md`).
#[derive(Debug, Clone, Default)]
pub struct ContainerSpec {
    pub image: String,
    pub args: Vec<String>,
    pub env: BTreeMap<String, String>,
    pub mounts: Vec<String>,
    pub gpu: bool,
    /// `--security-opt no-new-privileges` (default on). Per-app opt-out for images whose
    /// startup legitimately re-escalates (GOW desktop images sudo in startup);
    /// `messages::AppSpec::no_new_privileges`. `derive(Default)` yields `false` here, but
    /// every real construction path sets it explicitly.
    pub no_new_privileges: bool,
    /// App-liveness policy for a steady-state exit (D2/D3): `fail` (default) ends the
    /// session, `keep` logs and continues. `messages::AppExitPolicy`.
    pub on_app_exit: AppExitPolicy,
    /// Per-app docker network mode (§S2). `None`/empty inherits the host default:
    /// `QUASAR_CONTAINER_NETWORK`, then `none`. See [`resolve_network`].
    pub network: Option<String>,
    /// `--security-opt systempaths=unconfined` (default off), the desktop-session launch
    /// profile knob for apps needing an unmasked `/proc`.
    pub systempaths_unconfined: bool,
}

/// The docker network modes an APP may ask for over the wire (`AppSpec.network`, which
/// the control plane resolved from `runtime_spec.network` or the runtime preset).
///
/// `host` must never be in this set (#464). `--network host` does not widen the
/// container's reach, it REMOVES the network namespace: the app shares this host's
/// stack, reaching everything on host loopback (control plane, Postgres, the docker
/// proxy, any admin-only port) and able to bind host ports. Everything in this set is
/// portable — it can originate in a catalog manifest authored on another machine — so a
/// wire-reachable `host` would let a manifest dissolve this host's isolation boundary.
/// `none` and `bridge` (all Steam's steamui.so download needs) are the real requirement.
const APP_CONTAINER_NETWORKS: [&str; 2] = ["none", "bridge"];

/// The modes an OPERATOR may select via `QUASAR_CONTAINER_NETWORK`: a superset of
/// [`APP_CONTAINER_NETWORKS`] by exactly `host`. The difference is provenance, not risk
/// appetite — this knob is set by whoever administers this machine and travels nowhere,
/// while no object that moves between machines may select it.
const HOST_CONTAINER_NETWORKS: [&str; 3] = ["none", "bridge", "host"];

/// Resolve the effective `--network` for an app container. Precedence (§S2):
///   1. `AppSpec.network` from the assign, checked against the APP set;
///   2. `QUASAR_CONTAINER_NETWORK`, checked against the HOST set (which permits `host`);
///   3. `none`, the hardened default.
///
/// An out-of-set value from EITHER source fails the launch. The host knob is validated
/// too: a typo in an operator's `.env` must not silently become a docker argument. This
/// is the agent's OWN boundary, not a delegation to the control plane's identical-looking
/// checks — the wire is untrusted, so a misconfigured or compromised control plane
/// sending `"host"` must fail loudly here.
/// Whether this host honours the wire's `no_new_privileges: false` and
/// `systempaths_unconfined: true`. Defaults to `allow`, because the shipped catalog
/// needs both (Steam re-escalates via sudo, #432; KDE needs an unmasked `/proc` for
/// bwrap) and refusing them by default would break the default library out of the
/// box. An unrecognised value warns and keeps the permissive default rather than
/// silently hardening a working host. Knob: `QUASAR_APP_PRIVILEGE_OPTOUT`.
fn privilege_optout_allowed() -> bool {
    privilege_optout_from(std::env::var("QUASAR_APP_PRIVILEGE_OPTOUT").ok().as_deref())
}

fn privilege_optout_from(value: Option<&str>) -> bool {
    match value.map(str::trim) {
        Some("deny") => false,
        None | Some("") | Some("allow") => true,
        Some(other) => {
            tracing::warn!(
                token = "privilege-optout-unrecognised",
                "QUASAR_APP_PRIVILEGE_OPTOUT={other:?} is not `allow` or `deny`; \
                 keeping the default (allow)"
            );
            true
        }
    }
}

fn resolve_network(spec_network: Option<&str>) -> Result<String> {
    let (value, source, allowed): (String, &str, &[&str]) =
        match spec_network.map(str::trim).filter(|s| !s.is_empty()) {
            Some(v) => (v.to_string(), "the app spec", &APP_CONTAINER_NETWORKS),
            None => match std::env::var("QUASAR_CONTAINER_NETWORK") {
                Ok(v) if !v.trim().is_empty() => (
                    v.trim().to_string(),
                    "QUASAR_CONTAINER_NETWORK",
                    &HOST_CONTAINER_NETWORKS,
                ),
                _ => return Ok("none".into()),
            },
        };
    if !allowed.contains(&value.as_str()) {
        // Name the host-only escape hatch when an app asked for `host`, so the failure
        // reads as policy rather than as a bug worth working around.
        let hint = if value == "host" {
            " — `host` removes the container's network isolation and is available \
             only via this host's QUASAR_CONTAINER_NETWORK, never from an app, \
             preset, or image manifest"
        } else {
            ""
        };
        anyhow::bail!(
            "container network {value:?} from {source} is not an allowed value \
             (expected one of {}){hint}",
            allowed.join(", ")
        );
    }
    Ok(value)
}

impl ContainerSpec {
    /// Build a spec from the environment for the demo/dev path (`node-agent session
    /// --image ...` / `QUASAR_APP_IMAGE`); the control-plane path builds from the
    /// assign's `AppSpec`. `None` when no image is configured (bare compositor).
    pub fn from_env() -> Option<ContainerSpec> {
        let image = std::env::var("QUASAR_APP_IMAGE")
            .ok()
            .filter(|s| !s.is_empty())?;
        let args = std::env::var("QUASAR_APP_ARGS")
            .ok()
            .map(|s| s.split_whitespace().map(String::from).collect())
            .unwrap_or_default();
        // GPU on by default for a real app (game streaming); opt out explicitly.
        let gpu = !matches!(
            std::env::var("QUASAR_APP_GPU").ok().as_deref(),
            Some("0") | Some("false")
        );
        // Knob: QUASAR_APP_EXIT_POLICY. D2/D3: absent or unrecognized ⇒ `fail`.
        let on_app_exit = match std::env::var("QUASAR_APP_EXIT_POLICY").as_deref() {
            Ok("keep") => AppExitPolicy::Keep,
            _ => AppExitPolicy::Fail,
        };
        Some(ContainerSpec {
            image,
            args,
            env: BTreeMap::new(),
            mounts: Vec::new(),
            gpu,
            no_new_privileges: true,
            on_app_exit,
            // No catalog row on this path: `None` keeps the
            // QUASAR_CONTAINER_NETWORK-else-none chain, and the profile knob stays off.
            network: None,
            systempaths_unconfined: false,
        })
    }
}

/// Per-launch wiring the agent computes at `session_start` time (not part of the
/// app spec): the Wayland socket, its runtime dir, and the input device nodes.
pub struct LaunchParams<'a> {
    pub session_id: &'a str,
    /// e.g. `wayland-1` — the socket basename the compositor reported.
    pub wayland_display: &'a str,
    /// Host dir holding the Wayland socket. Only the selected socket FILE is
    /// bind-mounted in; the directory stays private to the agent and other sessions.
    pub runtime_dir: &'a str,
    /// uinput evdev nodes to expose (`--device`).
    pub device_nodes: Vec<PathBuf>,
    /// Container name override for the P2-07 swap, where old+new containers coexist
    /// briefly. `None` ⇒ the stable per-session name.
    pub container_name: Option<String>,
    /// #375: resolved host dir of 32-bit NVIDIA driver libs, bind-mounted read-only at
    /// `/opt/quasar/nvidia-lib32`. Empty ⇒ no mount; NVIDIA + GPU only.
    pub nvidia_lib32_path: &'a str,
    /// #384: the session's streamed display mode, injected as env so an app that cannot
    /// read the Wayland output (nested gamescope) runs at the selected profile.
    pub display: AppDisplayMode,
}

const APP_WAYLAND_RUNTIME_DIR: &str = "/run/quasar-wayland";

/// #375: the read-only 32-bit NVIDIA driver-lib mount args, or empty when the resolved
/// path is. Called only from the NVIDIA branch of [`ContainerRuntime::run`], so it need
/// not re-check the NVIDIA/GPU gate.
fn nvidia_lib32_mount_args(path: &str) -> Vec<String> {
    if path.is_empty() {
        return Vec::new();
    }
    vec!["-v".into(), format!("{path}:{NVIDIA_LIB32_MOUNT_DST}:ro")]
}

/// Container mount destination for the S1 driver volume in an APP container. Same path
/// as in the agent so a log line means the same thing on both sides. Must never be
/// `/usr/nvidia` — see [`NVIDIA_LIB32_MOUNT_DST`].
const NVIDIA_DRIVER_VOLUME_DST: &str = "/opt/quasar/nvidia-driver";

/// The mount + discovery env for the S1 driver volume, or empty when nothing is
/// provisioned. Reads the image's own `LD_LIBRARY_PATH` because `-e` REPLACES it, and
/// overwriting an app image's loader path trades one breakage for another.
fn nvidia_driver_volume_args(runtime: &ContainerRuntime, image: &str) -> Vec<String> {
    let Some(info) = crate::nvidia_volume::current() else {
        return Vec::new();
    };
    let image_ld = runtime
        .image_env(image, "LD_LIBRARY_PATH")
        .unwrap_or_default();
    let args =
        crate::nvidia_volume::app_container_args(Some(&info), NVIDIA_DRIVER_VOLUME_DST, &image_ld);
    if !args.is_empty() {
        tracing::info!(
            target: "quasar.nvidia_volume",
            image,
            "app container receives the Quasar-provisioned NVIDIA driver volume (v{})",
            info.manifest.driver_version
        );
    }
    args
}

/// Does the HOST have a `/dev/fuse` node?
///
/// The agent usually runs in a container whose private `/dev` is Docker's minimal set,
/// where `/dev/fuse` is absent even when the host has the module loaded — probing the
/// agent's own always answered "no" and the device was never passed through. The compose
/// file binds the host's `/dev` read-only at `/host/dev` (same convention as
/// `/host/etc/os-release`), which is authoritative when present; a bare-metal agent has
/// no such mount and falls back to its own `/dev/fuse`, which IS the host's.
///
/// The fallback must stay reachable only when `/host/dev` is absent: with the mount
/// present, a missing `/host/dev/fuse` is a real "host has no fuse" answer.
fn host_has_fuse_node() -> bool {
    let host_dev = Path::new("/host/dev");
    if host_dev.is_dir() {
        return host_dev.join("fuse").exists();
    }
    Path::new("/dev/fuse").exists()
}

/// Does the HOST enforce AppArmor?
///
/// `/sys/module` is the host's module tree even inside a container (sysfs is not
/// namespaced), so this reads the host's answer with no `/host` mount. The securityfs
/// directory is the other host-wide signal but is NOT mounted in the agent container, so
/// it can only add a yes, never a no.
pub(crate) fn host_uses_apparmor_in(root: &Path) -> bool {
    if let Ok(enabled) =
        std::fs::read_to_string(root.join("sys/module/apparmor/parameters/enabled"))
    {
        return enabled.trim().eq_ignore_ascii_case("y");
    }
    root.join("sys/kernel/security/apparmor").is_dir()
}

/// The scoped app-container profile, shipped as `deploy/apparmor/quasar-app` and loaded on
/// the host by `deploy/enroll-host.sh` (or by hand). Its whole reason to exist is to
/// replace `apparmor=unconfined` here.
pub(crate) const APP_APPARMOR_PROFILE: &str = "quasar-app";

/// The one command that makes [`APP_APPARMOR_PROFILE`] available. Every message about the
/// profile being absent carries it, because the profile is useless until someone with root
/// ON THE HOST runs this — the agent must never load policy itself.
pub(crate) const APP_APPARMOR_LOAD_CMD: &str =
    "sudo apparmor_parser -r -W <compose dir>/apparmor/quasar-app \
     (deploy/apparmor/quasar-app in the repo; enrolled hosts have it next to their \
     docker-compose.yml), then relaunch the session";

/// Where the kernel lists loaded AppArmor profiles, most specific first.
///
/// securityfs is not mounted inside a container, so `deploy/docker-compose.yml` binds the
/// host's read-only at `/host/sys/kernel/security` (the `/host/dev` convention). The bare
/// path is the bare-metal agent's own. An agent whose compose predates that mount reads
/// neither and gets [`AppArmorProfileState::Unknown`].
const APPARMOR_PROFILES_RELS: [&str; 2] = [
    "host/sys/kernel/security/apparmor/profiles",
    "sys/kernel/security/apparmor/profiles",
];

/// Is [`APP_APPARMOR_PROFILE`] loaded, as far as the agent can tell?
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum AppArmorProfileState {
    Loaded,
    NotLoaded,
    /// The profile list does not read from in here. Distinct from `NotLoaded` on purpose:
    /// "we cannot see" must keep today's behaviour, never confine with a profile whose
    /// presence was never established.
    Unknown,
}

/// What confinement an app container launches under.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum AppArmorChoice {
    /// Not an AppArmor host: no `--security-opt apparmor=` at all. An SELinux host
    /// (Fedora, `spc_t`) must get a byte-identical argv.
    NoFlag,
    Profile(String),
    Unconfined,
}

impl AppArmorChoice {
    fn args(&self) -> Vec<String> {
        match self {
            AppArmorChoice::NoFlag => Vec::new(),
            AppArmorChoice::Profile(p) => vec!["--security-opt".into(), format!("apparmor={p}")],
            AppArmorChoice::Unconfined => {
                vec!["--security-opt".into(), "apparmor=unconfined".into()]
            }
        }
    }
}

/// The confinement decision, as a pure function of the three inputs that drive it.
///
/// `override_name` is `QUASAR_APP_APPARMOR_PROFILE`: a profile name, or `unconfined` to
/// force the pre-#76 behaviour when the profile turns out to break a title. It is ignored
/// on a non-AppArmor host — the flag must not appear there whatever it is set to.
pub(crate) fn app_apparmor_choice(
    host_enforces_apparmor: bool,
    state: AppArmorProfileState,
    override_name: Option<&str>,
) -> AppArmorChoice {
    if !host_enforces_apparmor {
        return AppArmorChoice::NoFlag;
    }
    match override_name {
        Some("unconfined") => AppArmorChoice::Unconfined,
        Some(name) => AppArmorChoice::Profile(name.to_string()),
        None => match state {
            AppArmorProfileState::Loaded => {
                AppArmorChoice::Profile(APP_APPARMOR_PROFILE.to_string())
            }
            AppArmorProfileState::NotLoaded | AppArmorProfileState::Unknown => {
                AppArmorChoice::Unconfined
            }
        },
    }
}

pub(crate) fn apparmor_profile_state(root: &Path, name: &str) -> AppArmorProfileState {
    for rel in APPARMOR_PROFILES_RELS {
        let Ok(body) = std::fs::read_to_string(root.join(rel)) else {
            continue;
        };
        return if profile_list_contains(&body, name) {
            AppArmorProfileState::Loaded
        } else {
            AppArmorProfileState::NotLoaded
        };
    }
    AppArmorProfileState::Unknown
}

/// One profile per line as `name (mode)`; a child profile is `parent//child (mode)`, which
/// must not answer for its parent.
fn profile_list_contains(body: &str, name: &str) -> bool {
    body.lines()
        .any(|line| line.split(" (").next().map(str::trim) == Some(name))
}

/// `QUASAR_APP_APPARMOR_PROFILE`, empty or whitespace treated as unset.
pub(crate) fn app_apparmor_override() -> Option<String> {
    let v = std::env::var("QUASAR_APP_APPARMOR_PROFILE").ok()?;
    let v = v.trim();
    (!v.is_empty()).then(|| v.to_string())
}

/// Read the live host and decide. Warns ONCE per process when an AppArmor host ends up
/// unconfined for want of the profile: it is a standing security posture, not a per-launch
/// event, and a line per session would train the operator to scroll past it.
fn app_apparmor_selection() -> AppArmorChoice {
    static WARNED: std::sync::Once = std::sync::Once::new();
    let host = host_uses_apparmor_in(Path::new("/"));
    let over = app_apparmor_override();
    let state = if host && over.is_none() {
        apparmor_profile_state(Path::new("/"), APP_APPARMOR_PROFILE)
    } else {
        AppArmorProfileState::Unknown
    };
    let choice = app_apparmor_choice(host, state, over.as_deref());
    if choice == AppArmorChoice::Unconfined && over.is_none() {
        WARNED.call_once(|| {
            let reason = match state {
                AppArmorProfileState::NotLoaded => {
                    format!("the {APP_APPARMOR_PROFILE} profile is not loaded on this host")
                }
                _ => "the agent cannot read the host's loaded-profile list \
                      (/sys/kernel/security is not mounted into this container — recreate \
                      it from a current deploy/docker-compose.yml)"
                    .to_string(),
            };
            tracing::warn!(
                token = "app-apparmor-profile-missing",
                "app containers run APPARMOR-UNCONFINED on this AppArmor host: {reason}. \
                 Load it: {APP_APPARMOR_LOAD_CMD}"
            );
        });
    }
    choice
}

/// The DRM node directory, in the agent and in every GPU app container alike: `--device`
/// reproduces each node with the HOST's mode and gid, so what the agent stats here is
/// what the app container gets.
const DRI_DIR: &str = "/dev/dri";

/// Ownership of one DRM node, as the `--group-add` decision needs it.
struct DrmNodeOwner {
    name: String,
    mode: u32,
    gid: u32,
}

/// Stat every DRM node in `dir`. A node whose metadata will not read is dropped with a
/// WARN and simply contributes no group: a launch must never fail on a stat.
fn dri_node_owners(dir: &Path) -> Vec<DrmNodeOwner> {
    use std::os::unix::fs::MetadataExt as _;
    let Ok(entries) = std::fs::read_dir(dir) else {
        return Vec::new();
    };
    let mut owners = Vec::new();
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        if !(name.starts_with("renderD") || name.starts_with("card")) {
            continue;
        }
        match std::fs::metadata(entry.path()) {
            Ok(md) => owners.push(DrmNodeOwner {
                name,
                mode: md.mode(),
                gid: md.gid(),
            }),
            Err(e) => tracing::warn!(
                token = "dri-node-stat-failed",
                node = %entry.path().display(),
                error = %e,
                "cannot read DRM node ownership; the app container gets no group for it"
            ),
        }
    }
    owners
}

/// One `--group-add <gid>` per distinct group owning a DRM node the app cannot already
/// open through the node's `other` bits. Sorted and deduped so the argv is stable.
///
/// Numeric only: `render`/`video` do not exist in the app images, and a name that does
/// not resolve fails the whole `docker run`.
fn dri_group_add_args(nodes: &[DrmNodeOwner]) -> Vec<String> {
    let mut gids: Vec<u32> = nodes
        .iter()
        .filter(|n| dri_group_granted(n.mode, n.gid))
        .map(|n| n.gid)
        .collect();
    gids.sort_unstable();
    gids.dedup();
    gids.iter()
        .flat_map(|gid| ["--group-add".to_string(), gid.to_string()])
        .collect()
}

/// Does the launcher hand the app container this node's owning group? Readiness
/// (`dri_node_app_access`) predicts app access with the same rule, so the check and the
/// launch cannot drift apart.
///
/// A world-rw node needs no group. gid 0 is never granted: root-group membership widens
/// far past the DRM node, so a 0660 root:root node stays unopenable and readiness says so.
pub(crate) fn dri_group_granted(mode: u32, gid: u32) -> bool {
    mode & 0o006 != 0o006 && mode & 0o060 != 0 && gid != 0
}

/// `--device /dev/fuse` args, gated on host fuse-node presence. Split out so the
/// decision is testable without touching the live host filesystem.
fn fuse_device_args(host_has_fuse: bool) -> Vec<String> {
    if !host_has_fuse {
        return Vec::new();
    }
    vec!["--device".into(), "/dev/fuse".into()]
}

fn wayland_mount_args(params: &LaunchParams<'_>) -> Vec<String> {
    let host_socket = PathBuf::from(params.runtime_dir).join(params.wayland_display);
    let container_socket = PathBuf::from(APP_WAYLAND_RUNTIME_DIR).join(params.wayland_display);
    // The compositor creates its socket 0755, but a Wayland connect() needs WRITE on
    // the socket file, so a non-root app container (PUID 99 etc.) gets EACCES and e.g.
    // gamescope dies with "Failed to connect to wayland socket"; root containers never
    // noticed. The parent dir stays agent-private (only this file is bind-mounted in),
    // so 0666 grants nothing beyond this session's own clients.
    use std::os::unix::fs::PermissionsExt as _;
    if let Err(e) = std::fs::set_permissions(&host_socket, std::fs::Permissions::from_mode(0o666)) {
        tracing::warn!(
            token = "wayland-socket-perms-failed",
            socket = %host_socket.display(),
            error = %e,
            "could not open Wayland socket permissions for non-root app clients"
        );
    }
    vec![
        "--mount".into(),
        format!(
            "type=bind,src={},dst={}",
            host_socket.display(),
            container_socket.display()
        ),
        "-e".into(),
        format!("XDG_RUNTIME_DIR={APP_WAYLAND_RUNTIME_DIR}"),
        "-e".into(),
        format!("WAYLAND_DISPLAY={}", params.wayland_display),
    ]
}

/// App-container log lines retained per session generation (S5). Enough for a
/// launcher's fatal preamble, small enough that the whole tail rides the
/// `session_state` message and sits in a `TEXT` column with no truncation policy.
pub const APP_LOG_TAIL_LINES: usize = 100;

/// Hard cap on the bytes retained for a single log RECORD: an app printing a megabyte
/// with no newline must not be able to make the failure report, or the agent's heap,
/// unbounded. The ring bounds how many records are kept; this bounds each one, and
/// [`drain_into_ring`] enforces it INCREMENTALLY so the excess is never accumulated.
const APP_LOG_MAX_LINE: usize = 2000;

/// Appended to a record that hit [`APP_LOG_MAX_LINE`], so a reader can tell a
/// clipped line from a short one.
const APP_LOG_TRUNCATION_MARKER: &str = " …[truncated]";

/// How long the exit path waits for the log follower to drain before snapshotting the
/// ring (S5). A budget, not a join: this runs on the session's own thread and teardown
/// must never hang on a wedged runtime. `docker logs` closes its stream promptly once
/// the container is gone, so the timeout only bites on an unresponsive daemon, where a
/// partial tail beats a stuck teardown.
pub const APP_LOG_DRAIN_BUDGET: Duration = Duration::from_millis(750);

/// A bounded, newest-wins ring of an app container's log lines (S5). Cloneable and
/// internally synchronised: the source generation holds one handle, the two follower
/// threads the others. A poisoned mutex is recovered from, never propagated — this is
/// diagnostic, and a lost log line must never fail a session.
#[derive(Clone, Default)]
pub struct AppLogRing {
    inner: Arc<std::sync::Mutex<std::collections::VecDeque<String>>>,
}

impl AppLogRing {
    pub fn new() -> Self {
        AppLogRing {
            inner: Arc::new(std::sync::Mutex::new(
                std::collections::VecDeque::with_capacity(APP_LOG_TAIL_LINES),
            )),
        }
    }

    /// Retain one line, evicting the oldest once the ring is full.
    ///
    /// The length guard must walk back to a CHAR BOUNDARY before truncating:
    /// `String::truncate` panics if the index lands inside a multi-byte character, and a
    /// game printing an accented title at exactly the cap would panic the follower
    /// thread. Reachable, not merely defensive — the reader bounds records bytewise, but
    /// `from_utf8_lossy` can EXPAND one past the cap (an invalid byte becomes a
    /// three-byte replacement char). Guarded by
    /// `a_multibyte_char_straddling_the_cap_does_not_panic`.
    fn push(&self, line: String) {
        let mut line = line;
        if line.len() > APP_LOG_MAX_LINE {
            let mut end = APP_LOG_MAX_LINE;
            while end > 0 && !line.is_char_boundary(end) {
                end -= 1;
            }
            line.truncate(end);
            if !line.ends_with(APP_LOG_TRUNCATION_MARKER) {
                line.push_str(APP_LOG_TRUNCATION_MARKER);
            }
        }
        let mut g = match self.inner.lock() {
            Ok(g) => g,
            Err(p) => p.into_inner(),
        };
        if g.len() == APP_LOG_TAIL_LINES {
            g.pop_front();
        }
        g.push_back(line);
    }

    /// The retained lines, oldest first. Clones the ring: failure path only.
    pub fn tail(&self) -> Vec<String> {
        let g = match self.inner.lock() {
            Ok(g) => g,
            Err(p) => p.into_inner(),
        };
        g.iter().cloned().collect()
    }
}

/// Append `chunk` to `record`, never letting it exceed [`APP_LOG_MAX_LINE`].
/// Sets `overflowed` once anything has been dropped.
fn append_bounded(record: &mut Vec<u8>, chunk: &[u8], overflowed: &mut bool) {
    let room = APP_LOG_MAX_LINE.saturating_sub(record.len());
    if chunk.len() <= room {
        record.extend_from_slice(chunk);
        return;
    }
    record.extend_from_slice(&chunk[..room]);
    *overflowed = true;
}

/// Read `stream` into `ring`, one newline-delimited record at a time, until EOF.
///
/// Must stay bounded BY CONSTRUCTION: `BufRead::split(b'\n')` buys the whole record into
/// a `Vec` before any cap can apply, so a container writing forever without a newline
/// grows an unbounded allocation inside the agent. This drives `fill_buf`/`consume`
/// directly and keeps at most [`APP_LOG_MAX_LINE`] bytes per record, discarding the rest
/// as it streams past; peak memory is the cap plus one `BufReader` buffer. Guarded by
/// `a_newline_less_flood_stays_bounded`.
///
/// Non-UTF8 bytes are lossily replaced rather than aborting the drain: a stray byte must
/// not cost the surrounding lines.
fn drain_into_ring<R: std::io::Read>(stream: R, ring: &AppLogRing) {
    use std::io::BufRead;
    let mut reader = std::io::BufReader::new(stream);
    let mut record: Vec<u8> = Vec::new();
    let mut overflowed = false;

    loop {
        // The `fill_buf` borrow must end before `consume`: decide inside, act outside.
        let (consumed, complete) = {
            let buf = match reader.fill_buf() {
                Ok(b) => b,
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                Err(_) => break,
            };
            if buf.is_empty() {
                break; // EOF
            }
            match buf.iter().position(|&b| b == b'\n') {
                Some(i) => {
                    append_bounded(&mut record, &buf[..i], &mut overflowed);
                    (i + 1, true)
                }
                None => {
                    let n = buf.len();
                    append_bounded(&mut record, buf, &mut overflowed);
                    (n, false)
                }
            }
        };
        reader.consume(consumed);
        if complete {
            push_record(ring, std::mem::take(&mut record), overflowed);
            overflowed = false;
        }
    }

    // A final record with no trailing newline: the shape a crashing app leaves behind,
    // and precisely the line worth keeping.
    if !record.is_empty() {
        push_record(ring, record, overflowed);
    }
}

fn push_record(ring: &AppLogRing, record: Vec<u8>, overflowed: bool) {
    let mut text = String::from_utf8_lossy(&record)
        .trim_end_matches('\r')
        .to_string();
    if overflowed {
        text.push_str(APP_LOG_TRUNCATION_MARKER);
    }
    ring.push(text);
}

/// A launched container whose `Drop` tears it down — so any early return / panic
/// / dropped session on the agent side cannot leak a container.
pub struct RunningContainer {
    runtime: ContainerRuntime,
    name: String,
    /// Lets the app-liveness waiter (`source.rs`) `docker wait` THIS container without
    /// racing a same-named replacement (app-liveness spec §3.1).
    container_id: String,
    /// Shared with the app-liveness waiter: set BEFORE the `docker stop`/`rm` call, so a
    /// thread blocked in `docker wait` can tell "we tore this down ourselves" (swap,
    /// session stop) from a genuine app exit. A deliberate stop must never be
    /// misclassified as an app failure (spec §3 G5 swap safety).
    removed: Arc<AtomicBool>,
}

impl RunningContainer {
    /// Tear the container down (idempotent) on the normal stop/failure path; `Drop` is
    /// the backstop. TERM first (`graceful_remove`) so the app can exit cleanly.
    pub fn stop(&mut self) {
        // `swap`, not load+store: the idempotency check is itself the one-shot gate.
        if !self.removed.swap(true, Ordering::SeqCst) {
            self.runtime.graceful_remove(&self.name);
        }
    }

    pub fn name(&self) -> &str {
        &self.name
    }

    /// The daemon-assigned container id (for `docker wait`).
    pub fn container_id(&self) -> &str {
        &self.container_id
    }

    /// A clone of the shared teardown marker — see the field doc comment.
    pub fn removed_flag(&self) -> Arc<AtomicBool> {
        self.removed.clone()
    }
}

impl Drop for RunningContainer {
    fn drop(&mut self) {
        self.stop();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn node(name: &str, mode: u32, gid: u32) -> DrmNodeOwner {
        DrmNodeOwner {
            name: name.to_string(),
            mode,
            gid,
        }
    }

    /// Ubuntu's `docker-default` denies mount inside a user namespace, which Steam's
    /// bootstrap needs; an SELinux host must see a byte-identical argv.
    #[test]
    fn apparmor_confines_with_the_profile_when_it_is_loaded_and_never_on_a_non_apparmor_host() {
        use AppArmorProfileState::*;
        let choose = |host, state| app_apparmor_choice(host, state, None);

        assert_eq!(
            choose(true, Loaded),
            AppArmorChoice::Profile("quasar-app".into())
        );
        assert_eq!(
            choose(true, Loaded).args(),
            vec!["--security-opt", "apparmor=quasar-app"]
        );

        // Absent, and "cannot tell", both keep the pre-#76 behaviour.
        assert_eq!(choose(true, NotLoaded), AppArmorChoice::Unconfined);
        assert_eq!(choose(true, Unknown), AppArmorChoice::Unconfined);
        assert_eq!(
            choose(true, NotLoaded).args(),
            vec!["--security-opt", "apparmor=unconfined"]
        );

        for state in [Loaded, NotLoaded, Unknown] {
            assert_eq!(choose(false, state), AppArmorChoice::NoFlag);
            assert!(choose(false, state).args().is_empty());
        }
    }

    /// The escape hatch, for a host where the profile turns out to break a title.
    #[test]
    fn the_apparmor_override_wins_over_detection_but_not_over_a_non_apparmor_host() {
        use AppArmorProfileState::*;
        assert_eq!(
            app_apparmor_choice(true, Loaded, Some("unconfined")),
            AppArmorChoice::Unconfined
        );
        assert_eq!(
            app_apparmor_choice(true, NotLoaded, Some("my-profile")),
            AppArmorChoice::Profile("my-profile".into())
        );
        assert_eq!(
            app_apparmor_choice(false, Loaded, Some("my-profile")),
            AppArmorChoice::NoFlag,
            "an SELinux host gets no apparmor flag whatever the override says"
        );
    }

    /// `Unknown` (unreadable list) must never collapse into `NotLoaded`: an agent that
    /// cannot see securityfs would otherwise report a confinement it never established.
    #[test]
    fn the_loaded_profile_list_is_parsed_by_name_and_absent_means_unknown() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        assert_eq!(
            apparmor_profile_state(root, "quasar-app"),
            AppArmorProfileState::Unknown
        );

        let rel = root.join("sys/kernel/security/apparmor");
        std::fs::create_dir_all(&rel).unwrap();
        // A child profile of another parent must not answer for `quasar-app`.
        std::fs::write(
            rel.join("profiles"),
            "docker-default (enforce)\nsomething//quasar-app (enforce)\n",
        )
        .unwrap();
        assert_eq!(
            apparmor_profile_state(root, "quasar-app"),
            AppArmorProfileState::NotLoaded
        );

        std::fs::write(
            rel.join("profiles"),
            "docker-default (enforce)\nquasar-app (enforce)\nquasar-app//bwrap (enforce)\n",
        )
        .unwrap();
        assert_eq!(
            apparmor_profile_state(root, "quasar-app"),
            AppArmorProfileState::Loaded
        );

        // The compose bind wins over the agent's own (empty) securityfs.
        let host = root.join("host/sys/kernel/security/apparmor");
        std::fs::create_dir_all(&host).unwrap();
        std::fs::write(host.join("profiles"), "docker-default (enforce)\n").unwrap();
        assert_eq!(
            apparmor_profile_state(root, "quasar-app"),
            AppArmorProfileState::NotLoaded
        );
    }

    /// The AMD/Intel launch defect: 0660 root:render nodes with no group-add make RADV
    /// fail to open renderD128, so Vulkan enumerates llvmpipe and gamescope exits 1.
    #[test]
    fn dri_group_add_covers_every_node_the_app_cannot_open_otherwise() {
        // hermes: renderD128 root:render(991), card0 root:video(44).
        assert_eq!(
            dri_group_add_args(&[node("renderD128", 0o660, 991), node("card0", 0o660, 44)]),
            vec!["--group-add", "44", "--group-add", "991"],
            "both owning gids, ascending"
        );

        // Deduped across nodes sharing a group, and ordered independently of readdir.
        assert_eq!(
            dri_group_add_args(&[
                node("renderD129", 0o660, 991),
                node("renderD128", 0o660, 991),
                node("card1", 0o660, 44),
            ]),
            vec!["--group-add", "44", "--group-add", "991"]
        );

        // Nothing to grant: world-rw needs no group, a groupless mode has none to give,
        // and gid 0 is never handed out.
        assert!(dri_group_add_args(&[
            node("renderD128", 0o666, 991),
            node("card0", 0o600, 44),
            node("renderD129", 0o660, 0),
        ])
        .is_empty());

        assert!(dri_group_add_args(&[]).is_empty());
    }

    /// Readiness predicts app access with this same predicate; a divergence is how the
    /// check false-passes a host whose sessions cannot start.
    #[test]
    fn a_granted_group_is_exactly_what_the_args_contain() {
        for (mode, gid) in [(0o660, 991), (0o666, 991), (0o600, 44), (0o660, 0)] {
            let granted = dri_group_granted(mode, gid);
            let args = dri_group_add_args(&[node("renderD128", mode, gid)]);
            assert_eq!(granted, !args.is_empty(), "mode {mode:o} gid {gid}");
        }
    }

    // `deny` is the only value that hardens; everything else, including a typo, keeps
    // the shipped catalog launching.
    #[test]
    fn privilege_optout_defaults_to_allow_and_only_deny_hardens() {
        assert!(privilege_optout_from(None));
        assert!(privilege_optout_from(Some("")));
        assert!(privilege_optout_from(Some("allow")));
        assert!(privilege_optout_from(Some("nonsense")));
        assert!(!privilege_optout_from(Some("deny")));
        assert!(!privilege_optout_from(Some("  deny  ")));
    }

    // ── S5: the app-log ring and its bounded reader ─────────────────────────
    // This runs inside the agent, fed by an untrusted container's stdout, so "bounded"
    // must mean bounded against a hostile app, not tidy for a well-behaved one.

    /// `BufRead::split(b'\n')` materialises a whole record before any cap can apply, so
    /// a container writing forever without a newline grows an unbounded `Vec` inside the
    /// agent. Feed that shape; the retained record must be capped.
    #[test]
    fn a_newline_less_flood_stays_bounded() {
        let flood = vec![b'x'; APP_LOG_MAX_LINE * 50];
        let ring = AppLogRing::new();
        drain_into_ring(std::io::Cursor::new(flood), &ring);

        let tail = ring.tail();
        assert_eq!(tail.len(), 1, "one unterminated record, got {}", tail.len());
        assert!(
            tail[0].len() <= APP_LOG_MAX_LINE + APP_LOG_TRUNCATION_MARKER.len(),
            "retained {} bytes for a {}-byte flood — the cap did not hold",
            tail[0].len(),
            APP_LOG_MAX_LINE * 50
        );
        assert!(
            tail[0].ends_with(APP_LOG_TRUNCATION_MARKER),
            "a clipped record must say it was clipped: {:?}",
            &tail[0][tail[0].len().saturating_sub(40)..]
        );
    }

    /// The bound must hold across MANY oversized records: per-record state is reset, so
    /// record N+1 does not inherit N's exhausted budget and silently drop every later
    /// line.
    #[test]
    fn each_record_gets_its_own_budget() {
        let mut input: Vec<u8> = Vec::new();
        for _ in 0..5 {
            input.extend(std::iter::repeat_n(b'y', APP_LOG_MAX_LINE * 3));
            input.push(b'\n');
        }
        input.extend_from_slice(b"short tail line\n");
        let ring = AppLogRing::new();
        drain_into_ring(std::io::Cursor::new(input), &ring);

        let tail = ring.tail();
        assert_eq!(tail.len(), 6);
        for (i, line) in tail.iter().take(5).enumerate() {
            assert!(
                line.len() <= APP_LOG_MAX_LINE + APP_LOG_TRUNCATION_MARKER.len(),
                "record {i} exceeded the cap at {} bytes",
                line.len()
            );
        }
        assert_eq!(
            tail[5], "short tail line",
            "a later short record must survive intact"
        );
    }

    /// `String::truncate` panics if the index lands inside a multi-byte char, which
    /// would silently lose the log by panicking the follower thread.
    #[test]
    fn a_multibyte_char_straddling_the_cap_does_not_panic() {
        // 'é' is two bytes and APP_LOG_MAX_LINE is even, so an odd-length ASCII prefix
        // puts a char boundary exactly one byte past the cap.
        let mut record = String::from("a");
        while record.len() < APP_LOG_MAX_LINE + 10 {
            record.push('é');
        }
        assert!(!record.is_char_boundary(APP_LOG_MAX_LINE), "test premise");

        let ring = AppLogRing::new();
        ring.push(record);
        let tail = ring.tail();
        assert_eq!(tail.len(), 1);
        assert!(tail[0].len() <= APP_LOG_MAX_LINE + APP_LOG_TRUNCATION_MARKER.len());
        assert!(tail[0].ends_with(APP_LOG_TRUNCATION_MARKER));
    }

    /// The byte reader must also survive a multi-byte char split across the cap, where
    /// `from_utf8_lossy` replaces the orphaned half.
    #[test]
    fn the_reader_survives_a_multibyte_char_split_by_the_cap() {
        let mut bytes = vec![b'a'];
        while bytes.len() < APP_LOG_MAX_LINE + 20 {
            bytes.extend_from_slice("é".as_bytes());
        }
        bytes.push(b'\n');
        let ring = AppLogRing::new();
        drain_into_ring(std::io::Cursor::new(bytes), &ring);
        assert_eq!(ring.tail().len(), 1);
    }

    /// A crashing app's last words are the point, so overflow evicts from the front.
    #[test]
    fn the_ring_keeps_the_newest_lines() {
        let ring = AppLogRing::new();
        let input: String = (0..APP_LOG_TAIL_LINES + 50)
            .map(|i| format!("line {i}\n"))
            .collect();
        drain_into_ring(std::io::Cursor::new(input.into_bytes()), &ring);

        let tail = ring.tail();
        assert_eq!(tail.len(), APP_LOG_TAIL_LINES);
        assert_eq!(tail[APP_LOG_TAIL_LINES - 1], "line 149");
        assert_eq!(tail[0], "line 50");
    }

    /// A final record with no trailing newline is what a crashing app leaves behind, and
    /// the single most valuable line in the capture.
    #[test]
    fn an_unterminated_final_record_is_retained() {
        let ring = AppLogRing::new();
        drain_into_ring(
            std::io::Cursor::new(b"first\nSteam needs to be online to update".to_vec()),
            &ring,
        );
        let tail = ring.tail();
        assert_eq!(tail, vec!["first", "Steam needs to be online to update"]);
    }

    /// A stray \r from a CRLF image must not ride into the stored line: it renders as a
    /// mangled overwrite in the admin UI.
    #[test]
    fn carriage_returns_are_trimmed() {
        let ring = AppLogRing::new();
        drain_into_ring(std::io::Cursor::new(b"crlf line\r\n".to_vec()), &ring);
        assert_eq!(ring.tail(), vec!["crlf line"]);
    }

    /// Generation isolation: `AppSource` outlives its container across a relaunch, so a
    /// shared ring would report the PREVIOUS container's dying words as the new app's
    /// failure. A fresh ring per launch leaves the retired one to the old follower.
    #[test]
    fn a_fresh_ring_does_not_inherit_the_previous_generations_lines() {
        let first = AppLogRing::new();
        drain_into_ring(std::io::Cursor::new(b"old app: fatal\n".to_vec()), &first);
        assert_eq!(first.tail(), vec!["old app: fatal"]);

        // What `launch` does for every generation.
        let second = AppLogRing::new();
        assert!(
            second.tail().is_empty(),
            "a new generation must start with an empty capture"
        );

        // The retired ring stays writable for its own detached follower without
        // touching the new generation's capture.
        drain_into_ring(std::io::Cursor::new(b"old app: more\n".to_vec()), &first);
        assert_eq!(first.tail().len(), 2);
        assert!(
            second.tail().is_empty(),
            "the retired follower leaked into the new ring"
        );
    }

    /// A follower that never finishes must never hang session teardown.
    #[test]
    fn await_log_drain_gives_up_within_its_budget() {
        let (tx, rx) = std::sync::mpsc::channel::<()>();
        let handle = std::thread::spawn(move || {
            // Parks until released: stands in for a wedged `docker logs`.
            let _ = rx.recv();
        });
        let started = Instant::now();
        ContainerRuntime::await_log_drain(handle, Duration::from_millis(80));
        let waited = started.elapsed();
        assert!(
            waited < Duration::from_millis(1000),
            "teardown blocked for {waited:?} on a wedged follower"
        );
        let _ = tx.send(());
    }

    /// …and it DOES wait for a follower about to finish, the reason it exists:
    /// `docker wait` routinely returns before the log stream has been read out.
    #[test]
    fn await_log_drain_waits_for_a_follower_that_finishes() {
        let ring = AppLogRing::new();
        let writer = ring.clone();
        let handle = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(40));
            drain_into_ring(std::io::Cursor::new(b"final words\n".to_vec()), &writer);
        });
        ContainerRuntime::await_log_drain(handle, Duration::from_secs(5));
        assert_eq!(
            ring.tail(),
            vec!["final words"],
            "the snapshot raced the follower instead of draining it"
        );
    }

    /// §S2 container-network resolution + its defensive validation.
    /// `QUASAR_CONTAINER_NETWORK` is process-global, so every case lives in ONE
    /// serialized test that saves and restores it (no `serial_test` dep here).
    #[test]
    fn container_network_precedence_and_validation() {
        const KEY: &str = "QUASAR_CONTAINER_NETWORK";
        let saved = std::env::var(KEY).ok();
        std::env::remove_var(KEY);

        // 1. Nothing stated anywhere ⇒ the hardened default.
        assert_eq!(resolve_network(None).unwrap(), "none");
        // An empty/whitespace app value is "unset", not a value.
        assert_eq!(resolve_network(Some("")).unwrap(), "none");
        assert_eq!(resolve_network(Some("  ")).unwrap(), "none");

        // 2. The app spec wins outright (the #463 Steam case: the app declares bridge
        //    on a host that never set the knob).
        assert_eq!(resolve_network(Some("bridge")).unwrap(), "bridge");
        assert_eq!(resolve_network(Some("none")).unwrap(), "none");

        // 3. The host knob applies only when the app states nothing…
        std::env::set_var(KEY, "bridge");
        assert_eq!(resolve_network(None).unwrap(), "bridge");
        //    …and an app that states one still overrides it, in BOTH directions:
        //    an app can also pin itself back to `none` on a bridged host.
        assert_eq!(resolve_network(Some("bridge")).unwrap(), "bridge");
        assert_eq!(resolve_network(Some("none")).unwrap(), "none");

        // 4. An out-of-set value fails the launch from EITHER source: the backstop that
        //    keeps `container:<id>` off the docker command line. (`"bridge "` is not
        //    here: whitespace is trimmed, so it is the legitimate value.)
        std::env::remove_var(KEY);
        for bad in [
            "container:quasar-control-plane",
            "my-net",
            "NONE",
            "host;rm",
        ] {
            let err = resolve_network(Some(bad))
                .expect_err(&format!("{bad:?} from the app spec must be rejected"));
            assert!(
                err.to_string().contains("not an allowed value"),
                "unexpected error for {bad:?}: {err}"
            );
        }
        std::env::set_var(KEY, "container:quasar-control-plane");
        let err = resolve_network(None).expect_err("a bad host knob must be rejected too");
        assert!(err.to_string().contains("QUASAR_CONTAINER_NETWORK"));

        // A blank host knob is "unset", not an invalid value.
        std::env::set_var(KEY, "");
        assert_eq!(resolve_network(None).unwrap(), "none");

        // 5. The asymmetry (#464): `host` removes the container's network namespace, so
        //    it is reachable ONLY from this host's operator knob, never from the wire,
        //    where the value may come from a portable manifest authored elsewhere.
        std::env::set_var(KEY, "host");
        assert_eq!(
            resolve_network(None).unwrap(),
            "host",
            "an operator must still be able to select host networking on their own machine"
        );
        // Even with the operator knob set to host, an app asking for host is refused: a
        // permissive host must not become a permissive wire.
        let err = resolve_network(Some("host"))
            .expect_err("`host` from the app spec must be rejected, always");
        assert!(
            err.to_string()
                .contains("removes the container's network isolation"),
            "the rejection must explain the policy and name the operator knob: {err}"
        );
        std::env::remove_var(KEY);
        let err = resolve_network(Some("host")).expect_err("`host` from the wire is never allowed");
        assert!(err.to_string().contains("QUASAR_CONTAINER_NETWORK"));
        // The app-facing message must not advertise host as an option.
        assert!(
            !err.to_string()
                .contains("expected one of none, bridge, host"),
            "the app-facing error must not list host as available: {err}"
        );

        match saved {
            Some(v) => std::env::set_var(KEY, v),
            None => std::env::remove_var(KEY),
        }
    }

    /// The wire field is ADDITIVE and OPTIONAL: an assign from a control plane that
    /// never heard of it must deserialize to `None` and launch byte-identically, and an
    /// unrecognised value must survive deserialization so `resolve_network` is the
    /// single place that rejects it — a parse failure would abort the session with an
    /// opaque serde error instead of an actionable one.
    #[test]
    fn app_spec_network_is_optional_and_additive() {
        let legacy: crate::messages::AppSpec =
            serde_json::from_str(r#"{"image":"a:1"}"#).expect("legacy spec must still parse");
        assert_eq!(legacy.network, None);

        let stated: crate::messages::AppSpec =
            serde_json::from_str(r#"{"image":"a:1","network":"bridge"}"#).unwrap();
        assert_eq!(stated.network.as_deref(), Some("bridge"));

        let nulled: crate::messages::AppSpec =
            serde_json::from_str(r#"{"image":"a:1","network":null}"#).unwrap();
        assert_eq!(nulled.network, None);

        let bogus: crate::messages::AppSpec =
            serde_json::from_str(r#"{"image":"a:1","network":"container:x"}"#)
                .expect("an unknown value parses; resolve_network is what rejects it");
        assert!(resolve_network(bogus.network.as_deref()).is_err());

        // `host` on the wire parses and is refused at resolve time, so the operator gets
        // a named policy error rather than an opaque serde failure.
        let hostile: crate::messages::AppSpec =
            serde_json::from_str(r#"{"image":"a:1","network":"host"}"#).unwrap();
        assert!(resolve_network(hostile.network.as_deref()).is_err());
    }

    /// Additive and optional, same contract as `network` above: an assign that never
    /// heard of it deserializes to `false` and launches byte-identically.
    #[test]
    fn app_spec_systempaths_unconfined_is_optional_and_additive() {
        let legacy: crate::messages::AppSpec =
            serde_json::from_str(r#"{"image":"a:1"}"#).expect("legacy spec must still parse");
        assert!(!legacy.systempaths_unconfined);

        let stated: crate::messages::AppSpec =
            serde_json::from_str(r#"{"image":"a:1","systempaths_unconfined":true}"#).unwrap();
        assert!(stated.systempaths_unconfined);

        let explicit_false: crate::messages::AppSpec =
            serde_json::from_str(r#"{"image":"a:1","systempaths_unconfined":false}"#).unwrap();
        assert!(!explicit_false.systempaths_unconfined);
    }

    const MODE_1440P120: AppDisplayMode = AppDisplayMode {
        width: 2560,
        height: 1440,
        fps: 120,
    };

    fn spec_env(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
            .collect()
    }

    fn injected(env: &AppDisplayEnv, key: &str) -> Option<String> {
        env.vars
            .iter()
            .find(|(k, _)| k == key)
            .map(|(_, v)| v.clone())
    }

    // #384: the session's display mode must reach the app container, or a nested
    // gamescope sizes itself from the image's baked 1080p60 and the selected profile
    // silently does nothing (Steam reported 1080p on a 1440p120 session).
    #[test]
    fn display_mode_is_injected_for_both_contracts() {
        let env = app_display_env_with(MODE_1440P120, &BTreeMap::new(), true, true);
        assert_eq!(
            injected(&env, "QUASAR_STREAM_WIDTH").as_deref(),
            Some("2560")
        );
        assert_eq!(
            injected(&env, "QUASAR_STREAM_HEIGHT").as_deref(),
            Some("1440")
        );
        assert_eq!(injected(&env, "QUASAR_STREAM_FPS").as_deref(), Some("120"));
        assert_eq!(injected(&env, "GAMESCOPE_WIDTH").as_deref(), Some("2560"));
        assert_eq!(injected(&env, "GAMESCOPE_HEIGHT").as_deref(), Some("1440"));
        assert_eq!(injected(&env, "GAMESCOPE_REFRESH").as_deref(), Some("120"));
        assert_eq!(env.source, AppDisplaySource::Agent);
        assert!(env.gamescope_env);
    }

    // The app catalog wins per key: a pinned app keeps its mode, and the launched
    // command carries no duplicate `-e KEY=` (the spec-env loop in `run` emits it).
    #[test]
    fn app_catalog_env_wins_per_key_without_duplicating() {
        let spec = spec_env(&[("GAMESCOPE_WIDTH", "1280"), ("GAMESCOPE_HEIGHT", "720")]);
        let env = app_display_env_with(MODE_1440P120, &spec, true, true);
        assert_eq!(injected(&env, "GAMESCOPE_WIDTH"), None);
        assert_eq!(injected(&env, "GAMESCOPE_HEIGHT"), None);
        // Un-pinned keys are still injected.
        assert_eq!(injected(&env, "GAMESCOPE_REFRESH").as_deref(), Some("120"));
        assert_eq!(
            injected(&env, "QUASAR_STREAM_WIDTH").as_deref(),
            Some("2560")
        );
        assert_eq!(env.source, AppDisplaySource::AppCatalog);
    }

    // QUASAR_APP_DISPLAY_ENV off is the full revert: nothing injected, and the trace
    // says so rather than claiming the session mode reached the app.
    #[test]
    fn display_env_knob_off_injects_nothing() {
        let env = app_display_env_with(MODE_1440P120, &BTreeMap::new(), false, true);
        assert!(env.vars.is_empty());
        assert_eq!(env.source, AppDisplaySource::Disabled);
        assert!(!env.gamescope_env);
    }

    // QUASAR_APP_GAMESCOPE_ENV off drops only the shim: an image reading the
    // QUASAR_STREAM_* contract still gets its mode.
    #[test]
    fn gamescope_shim_can_be_dropped_alone() {
        let env = app_display_env_with(MODE_1440P120, &BTreeMap::new(), true, false);
        assert_eq!(
            injected(&env, "QUASAR_STREAM_WIDTH").as_deref(),
            Some("2560")
        );
        assert_eq!(injected(&env, "GAMESCOPE_WIDTH"), None);
        assert_eq!(env.source, AppDisplaySource::Agent);
        assert!(!env.gamescope_env);
    }

    // P2-05: the name must be session-unique so concurrent sessions never collide and a
    // force-remove, which targets an exact name, can never touch another session's
    // container.
    #[test]
    fn container_name_is_session_unique() {
        let a = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
        let b = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
        assert_ne!(
            ContainerRuntime::container_name(a),
            ContainerRuntime::container_name(b)
        );
        assert!(ContainerRuntime::container_name(a).contains(a));
        assert!(ContainerRuntime::container_name(a).starts_with("quasar-sess-"));
    }

    #[test]
    fn wayland_mount_exposes_only_the_session_socket() {
        let params = LaunchParams {
            session_id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
            wayland_display: "wayland-7",
            runtime_dir: "/run/quasar-agent",
            device_nodes: Vec::new(),
            container_name: None,
            nvidia_lib32_path: "",
            display: AppDisplayMode {
                width: 1920,
                height: 1080,
                fps: 60,
            },
        };

        let args = wayland_mount_args(&params);
        assert_eq!(
            args,
            vec![
                "--mount",
                "type=bind,src=/run/quasar-agent/wayland-7,dst=/run/quasar-wayland/wayland-7",
                "-e",
                "XDG_RUNTIME_DIR=/run/quasar-wayland",
                "-e",
                "WAYLAND_DISPLAY=wayland-7",
            ]
        );
        assert!(!args
            .iter()
            .any(|arg| arg == "/run/quasar-agent:/run/quasar-agent"));
    }

    // #375: emitted only when a path is configured (the NVIDIA/GPU gate is upstream in
    // run(), so this helper only sees the path).
    #[test]
    fn nvidia_lib32_mount_appears_only_when_configured() {
        assert_eq!(
            nvidia_lib32_mount_args("/usr/lib"),
            vec![
                "-v".to_string(),
                "/usr/lib:/opt/quasar/nvidia-lib32:ro".to_string()
            ]
        );
        assert!(nvidia_lib32_mount_args("").is_empty());
    }

    /// First-run S1: on a host with its own driver nothing is provisioned, so the
    /// driver-volume wiring must contribute ZERO arguments. The populated case is
    /// covered in `nvidia_volume::tests`, which can build a `VolumeInfo` without docker.
    #[test]
    fn driver_volume_args_are_absent_on_a_host_with_its_own_driver() {
        assert!(
            crate::nvidia_volume::current().is_none(),
            "unit tests must never see a provisioned volume"
        );
        let rt = ContainerRuntime {
            bin: "docker".to_string(),
            nvidia: true,
        };
        assert!(nvidia_driver_volume_args(&rt, "quasar-steam:latest").is_empty());
    }

    /// The mount destination must never be GOW's `/usr/nvidia`: upstream cont-init
    /// treats that path as a full driver volume and exits 1 otherwise (#375).
    #[test]
    fn driver_volume_destination_avoids_the_gow_usr_nvidia_path() {
        assert_ne!(NVIDIA_DRIVER_VOLUME_DST, "/usr/nvidia");
        assert!(NVIDIA_DRIVER_VOLUME_DST.starts_with("/opt/quasar/"));
        assert_ne!(NVIDIA_DRIVER_VOLUME_DST, NVIDIA_LIB32_MOUNT_DST);
    }

    /// SYS_NICE must be in the fixed cap-add set: without it gamescope falls back to
    /// regular-priority compute under concurrent-session CPU contention. Asserts the
    /// whole set so an edit cannot silently drop one cap while touching another.
    #[test]
    fn sys_nice_is_in_the_fixed_app_container_cap_add_set() {
        assert!(APP_CONTAINER_CAP_ADDS.contains(&"SYS_NICE"));
        assert_eq!(
            APP_CONTAINER_CAP_ADDS,
            [
                "CHOWN",
                "DAC_OVERRIDE",
                "FOWNER",
                "SETGID",
                "SETUID",
                "SETPCAP",
                "KILL",
                "SYS_NICE",
            ]
        );
    }

    /// The xdg document portal's fuse mount needs `/dev/fuse`, but only on a host that
    /// has the node: `docker create --device` with a missing source path fails the whole
    /// launch.
    #[test]
    fn fuse_device_is_added_only_when_the_host_has_it() {
        assert_eq!(
            fuse_device_args(true),
            vec!["--device".to_string(), "/dev/fuse".to_string()]
        );
        assert!(fuse_device_args(false).is_empty());
    }
}
