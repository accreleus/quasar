//! Per-session application container lifecycle: launch the assigned image as a
//! containerized Wayland client of the per-session compositor, and tear it down with
//! no orphans on every terminal transition (a leaked container is leaked GPU/VRAM the
//! control plane already released — P1-6 reservation-release depends on it).
//!
//! ## Runtime: the Docker Engine API over a Unix socket
//! `games-on-whales/wolf`'s published app images are Docker images, and this module
//! launches them as SIBLING containers of the agent through the engine's HTTP API on
//! the mounted `/var/run/docker.sock` (`crate::runtime`) — the same code path whether
//! the agent runs on the host or inside `quasar-agent-dev`, which fits invariant #1.
//! Since #239 the agent runs no engine executable at all: there is no CLI in the
//! image and no API-to-CLI fallback. The argument vector built in `run()` below is
//! still written in `docker run` spelling because that is the readable, reviewable
//! form of the launch contract; `application_request_from_args` translates it into an
//! owned `ApplicationRequest`, and an argument with no representation there is a
//! launch error rather than an unchecked flag. The end-state K8s model replaces this
//! module with a CRI/pod spec, threading the same inputs.
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

use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, Context, Result};

use crate::messages::AppExitPolicy;
use crate::nvidia_volume::VolumeInfo;
use crate::runtime::{ApplicationId, ApplicationMount, ApplicationRequest};

/// A launch whose response or retirement is uncertain. The operation is the only
/// identity allowed to reconcile it. Writable bind sources are retained as well as the
/// generated name: a later generation can have a new name while still targeting the
/// same managed home.
#[derive(Debug, Clone, PartialEq, Eq)]
struct PendingApplication {
    operation: String,
    writable_sources: BTreeSet<String>,
}

/// Failed launch/stop paths retain their exact durable operation until RuntimeClient
/// proves retirement. This blocks a new writer with either the same container name or
/// a matching normalized writable host source.
fn pending_application_operations() -> &'static Mutex<HashMap<String, PendingApplication>> {
    static PENDING: OnceLock<Mutex<HashMap<String, PendingApplication>>> = OnceLock::new();
    PENDING.get_or_init(|| Mutex::new(HashMap::new()))
}

fn lexical_mount_source(source: &str) -> Option<String> {
    let path = Path::new(source);
    if !path.is_absolute()
        || path
            .components()
            .any(|part| matches!(part, std::path::Component::ParentDir))
    {
        return None;
    }
    let mut normalized = PathBuf::from("/");
    for part in path.components() {
        if let std::path::Component::Normal(part) = part {
            normalized.push(part);
        }
    }
    Some(normalized.to_string_lossy().into_owned())
}

fn writable_application_sources(request: &ApplicationRequest) -> BTreeSet<String> {
    let mut sources = BTreeSet::new();
    for mount in &request.typed_mounts {
        match mount {
            ApplicationMount::Bind {
                source,
                read_only: false,
                ..
            } => {
                if let Some(source) = lexical_mount_source(source) {
                    sources.insert(source);
                }
            }
            ApplicationMount::Volume {
                source,
                read_only: false,
                ..
            } => {
                if !source.is_empty() {
                    sources.insert(format!("volume:{source}"));
                }
            }
            _ => {}
        }
    }
    for mount in &request.mounts {
        let mut fields = mount.split(':');
        let Some(source) = fields.next() else {
            continue;
        };
        let _target = fields.next();
        let options = fields.next().unwrap_or_default();
        let read_only = options
            .split(',')
            .any(|option| matches!(option, "ro" | "readonly"));
        if read_only {
            continue;
        }
        if let Some(source) = lexical_mount_source(source) {
            sources.insert(source);
        } else if !source.is_empty() && !source.contains('/') {
            // Legacy `-v name:/target` names a writable volume, not a relative bind.
            sources.insert(format!("volume:{source}"));
        }
    }
    sources
}

fn pending_matches(
    request: &ApplicationRequest,
    name: &str,
    pending_name: &str,
    pending: &PendingApplication,
) -> bool {
    pending_name == name
        || !pending
            .writable_sources
            .is_disjoint(&writable_application_sources(request))
}

fn retain_pending_application(name: String, request: &ApplicationRequest, operation: String) {
    retain_pending_application_sources(name, writable_application_sources(request), operation);
}

fn retain_pending_application_sources(
    name: String,
    writable_sources: BTreeSet<String>,
    operation: String,
) {
    if let Ok(mut pending) = pending_application_operations().lock() {
        pending.insert(
            name,
            PendingApplication {
                operation,
                writable_sources,
            },
        );
    }
}

fn clear_pending_application(name: &str, operation: &str) {
    clear_pending_application_in(pending_application_operations(), name, operation);
}

fn clear_pending_application_in(
    map: &Mutex<HashMap<String, PendingApplication>>,
    name: &str,
    operation: &str,
) {
    if let Ok(mut pending) = map.lock() {
        if pending
            .get(name)
            .is_some_and(|entry| entry.operation == operation)
        {
            pending.remove(name);
        }
    }
}

/// Return the unresolved application operation retaining a writable source. Paths are
/// compared in the same lexical form used by launch-time exclusion; a path that cannot
/// take that form can never be a pending key. `Err` means the pending state itself
/// could not be read — never "no writer".
pub(crate) fn pending_writer_for_source(source: &Path) -> Result<Option<String>> {
    let Some(source) = lexical_mount_source(&source.to_string_lossy()) else {
        return Ok(None);
    };
    Ok(pending_application_operations()
        .lock()
        .map_err(|_| anyhow!("application pending-operation lock poisoned"))?
        .values()
        .find(|pending| pending.writable_sources.contains(&source))
        .map(|pending| pending.operation.clone()))
}

/// How many times [`prove_source_teardown`] retries the exact pending operation before
/// reporting it unresolved. Each attempt is bounded by the runtime client's own deadline.
const TEARDOWN_PROOF_ATTEMPTS: usize = 3;

/// Prove that no unresolved application writer still holds `source` (a warm-up scratch
/// home or a managed home). While one does, that entry's exact durable operation — never
/// a name, never an unrelated entry — is retried through `retire`, a bounded number of
/// times, and the source is re-checked. `Err` names the operation that remains
/// unresolved or says the pending state could not be read; it makes no claim about
/// whether the workload is alive, only that its teardown is unproven. Unreadable state
/// is unproven, never proof.
pub(crate) fn prove_source_teardown<F>(source: &Path, mut retire: F) -> Result<(), String>
where
    F: FnMut(&str) -> Result<()>,
{
    let unreadable = |error: anyhow::Error| format!("application teardown unproven: {error}");
    for attempt in 0..TEARDOWN_PROOF_ATTEMPTS {
        let Some(operation) = pending_writer_for_source(source).map_err(unreadable)? else {
            return Ok(());
        };
        let names = pending_application_operations()
            .lock()
            .map_err(|_| unreadable(anyhow!("application pending-operation lock poisoned")))?
            .iter()
            .filter(|(_, pending)| pending.operation == operation)
            .map(|(name, _)| name.clone())
            .collect::<Vec<_>>();
        match retire(&operation) {
            Ok(()) => {
                for name in &names {
                    clear_pending_application(name, &operation);
                }
            }
            Err(error) => tracing::warn!(
                token = "application-teardown-proof-retry",
                operation = %operation,
                attempt = attempt + 1,
                "application teardown remains unproven: {error}"
            ),
        }
    }
    match pending_writer_for_source(source).map_err(unreadable)? {
        None => Ok(()),
        Some(operation) => Err(format!(
            "application teardown remains unproven (operation {operation})"
        )),
    }
}

/// Snapshot explicit caller cleanup obligations without holding the mutex across Docker.
/// A running container enters this set only after its caller requested abandonment or stop.
fn pending_application_operation_snapshot(
    map: &Mutex<HashMap<String, PendingApplication>>,
) -> Result<Vec<(String, String)>> {
    Ok(map
        .lock()
        .map_err(|_| anyhow!("application pending-operation lock poisoned"))?
        .iter()
        .map(|(name, pending)| (name.clone(), pending.operation.clone()))
        .collect())
}

/// Retry each explicit caller obligation independently. A failed operation stays in the
/// map for its exact identity, while a healthy later operation still gets its chance.
pub(crate) fn recover_pending_application_operations<F>(retire: F) -> Result<()>
where
    F: FnMut(&str) -> Result<()>,
{
    recover_pending_application_operations_in(pending_application_operations(), retire)
}

/// The sweep over `map`. It retires every entry, so a test must pass a private map: on
/// the process-global one it would retire other tests' entries (#285).
fn recover_pending_application_operations_in<F>(
    map: &Mutex<HashMap<String, PendingApplication>>,
    mut retire: F,
) -> Result<()>
where
    F: FnMut(&str) -> Result<()>,
{
    for (name, operation) in pending_application_operation_snapshot(map)? {
        match retire(&operation) {
            Ok(()) => clear_pending_application_in(map, &name, &operation),
            Err(error) => tracing::warn!(
                token = "application-pending-operation-retry",
                operation = %operation,
                "explicit application cleanup remains pending: {error}"
            ),
        }
    }
    Ok(())
}

fn retire_matching_pending_applications<F>(
    name: &str,
    request: &ApplicationRequest,
    mut retire: F,
) -> Result<()>
where
    F: FnMut(&str) -> Result<()>,
{
    let matches = pending_application_operations()
        .lock()
        .map_err(|_| anyhow!("application pending-operation lock poisoned"))?
        .iter()
        .filter(|(pending_name, pending)| pending_matches(request, name, pending_name, pending))
        .map(|(pending_name, pending)| (pending_name.clone(), pending.operation.clone()))
        .collect::<Vec<_>>();
    for (pending_name, operation) in matches {
        retire(&operation).with_context(|| {
            format!("previous application operation {operation} remains unresolved")
        })?;
        // A concurrent uncertain launch can replace this name while the old operation
        // is being retired outside the lock. Remove only the exact operation we proved.
        clear_pending_application(&pending_name, &operation);
    }
    Ok(())
}

/// Translate the agent's already-validated launch policy into the Quasar runtime
/// request. This is intentionally strict: a new internal Docker flag must be
/// represented here before an application can launch through the API.
fn application_request_from_args(args: &[String], operation: String) -> Result<ApplicationRequest> {
    let mut request = ApplicationRequest {
        operation,
        ..Default::default()
    };
    let mut i = 0;
    while i < args.len() {
        let arg = &args[i];
        if !request.image.is_empty() {
            request.command.push(arg.clone());
            i += 1;
            continue;
        }
        let next = || {
            args.get(i + 1)
                .ok_or_else(|| anyhow!("missing value for {arg}"))
        };
        match arg.as_str() {
            "run" | "-d" | "--rm" => {}
            "--name" => {
                request.name = next()?.clone();
                i += 1;
            }
            "--label" => {
                let _ = next()?;
                i += 1;
            }
            "--network" => {
                request.network = next()?.clone();
                i += 1;
            }
            "--cap-drop" => {
                if next()? != "ALL" {
                    anyhow::bail!("unsupported cap-drop");
                }
                request.security.cap_drop_all = true;
                i += 1;
            }
            "--cap-add" => {
                request.security.cap_add.push(next()?.clone());
                i += 1;
            }
            "--pids-limit" => {
                request.security.pids_limit = next()?.parse()?;
                i += 1;
            }
            "--shm-size" => {
                request.security.shm_size = parse_size(next()?)?;
                i += 1;
            }
            "--pull=never" => request.pull_never = true,
            "--security-opt" => {
                let mut option = next()?.clone();
                i += 1;
                if let Some(path) = option
                    .strip_prefix("seccomp=")
                    .filter(|v| *v != "unconfined")
                {
                    option = format!(
                        "seccomp={}",
                        std::fs::read_to_string(path)
                            .with_context(|| format!("read seccomp profile {path}"))?
                    );
                }
                if option == "no-new-privileges:true" {
                    request.security.no_new_privileges = true;
                } else if option == "systempaths=unconfined" {
                    request.security.systempaths_unconfined = true;
                } else {
                    request.security.security_opt.push(option);
                }
            }
            "--read-only" => request.security.read_only_rootfs = true,
            "--device" => {
                request.devices.push(next()?.clone());
                i += 1;
            }
            "--group-add" => {
                request.group_add.push(next()?.clone());
                i += 1;
            }
            "--gpus" => {
                if next()? != "all" {
                    anyhow::bail!("unsupported GPU request");
                }
                request.nvidia_gpu = true;
                request.gpu = true;
                i += 1;
            }
            "-e" => {
                request.environment.push(next()?.clone());
                i += 1;
            }
            "-v" => {
                request.mounts.push(next()?.clone());
                i += 1;
            }
            "--mount" => {
                let raw = next()?;
                i += 1;
                let parts = raw.split(',').collect::<Vec<_>>();
                let get = |key| parts.iter().find_map(|part| part.strip_prefix(key));
                let src = get("src=")
                    .or_else(|| get("source="))
                    .ok_or_else(|| anyhow!("mount source missing"))?;
                let dst = get("dst=")
                    .or_else(|| get("target="))
                    .ok_or_else(|| anyhow!("mount target missing"))?;
                let read_only = parts.contains(&"readonly") || parts.contains(&"ro");
                match get("type=") {
                    Some("bind") => {
                        if parts.iter().any(|part| {
                            !matches!(*part, "type=bind" | "readonly" | "ro")
                                && !part.starts_with("src=")
                                && !part.starts_with("source=")
                                && !part.starts_with("dst=")
                                && !part.starts_with("target=")
                                && !part.starts_with("consistency=")
                        }) {
                            anyhow::bail!("unsupported bind mount option {raw}");
                        }
                        request.typed_mounts.push(ApplicationMount::Bind {
                            source: src.to_owned(),
                            target: dst.to_owned(),
                            read_only,
                            consistency: get("consistency=").map(str::to_owned),
                        })
                    }
                    Some("volume") => {
                        if parts.iter().any(|part| {
                            !matches!(
                                *part,
                                "type=volume" | "readonly" | "ro" | "volume-nocopy" | "nocopy"
                            ) && !part.starts_with("src=")
                                && !part.starts_with("source=")
                                && !part.starts_with("dst=")
                                && !part.starts_with("target=")
                        }) {
                            anyhow::bail!("unsupported volume mount option {raw}");
                        }
                        request.typed_mounts.push(ApplicationMount::Volume {
                            source: src.to_owned(),
                            target: dst.to_owned(),
                            read_only,
                            no_copy: parts.contains(&"volume-nocopy") || parts.contains(&"nocopy"),
                        })
                    }
                    _ => anyhow::bail!("unsupported mount type {raw}"),
                }
            }
            value if value.starts_with('-') => {
                anyhow::bail!("unrepresented application runtime argument {value}")
            }
            image if request.image.is_empty() => request.image = image.to_owned(),
            value => request.command.push(value.to_owned()),
        }
        i += 1;
    }
    if !request.is_valid() {
        anyhow::bail!("invalid application runtime request");
    }
    Ok(request)
}

fn parse_size(value: &str) -> Result<i64> {
    let (number, factor) = match value.as_bytes().last().copied() {
        Some(b'g' | b'G') => (&value[..value.len() - 1], 1024_i64.pow(3)),
        Some(b'm' | b'M') => (&value[..value.len() - 1], 1024_i64.pow(2)),
        Some(b'k' | b'K') => (&value[..value.len() - 1], 1024),
        _ => (value, 1),
    };
    number
        .parse::<i64>()?
        .checked_mul(factor)
        .ok_or_else(|| anyhow!("size overflow"))
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

/// The probe's caller-level budget, covering image availability plus the bounded
/// shell check. Each owned lifecycle operation inside it remains under the
/// runtime's own configured deadline.
const NVIDIA_LIB32_PROBE_TIMEOUT: Duration = Duration::from_secs(60);

/// Minimal images the #375 probe tries in order; both ship `sh`.
const NVIDIA_LIB32_PROBE_IMAGES: [&str; 2] = ["busybox", "alpine:3"];

/// POSIX `sh`: print the first 32-bit lib dir holding `libGLX_nvidia.so.*`, exit 0;
/// else exit 1. The search list is exactly the 32-bit dirs — `/usr/lib64` and
/// `/usr/lib/x86_64-linux-gnu` are never searched, and each lookup is non-recursive so
/// `/host-usr/lib` cannot descend into a 64-bit multiarch subdir.
const NVIDIA_LIB32_PROBE_SCRIPT: &str = r#"
root=${2:-/host-usr}
for d in "$root/lib" "$root/lib32" "$root/lib/i386-linux-gnu"; do
    f="$d/libGLX_nvidia.so.$1"
    [ -f "$f" ] || continue
    # ELF magic plus EI_CLASS=1: never mistake a 64-bit library for lib32.
    header=$(od -An -tx1 -N5 "$f" | tr -d ' \n')
    [ "$header" = 7f454c4601 ] && printf %s "$d" && exit 0
done
exit 1
"#;

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

/// This host's app-container launch POLICY, plus the thin shims the launch path
/// reads image facts through. It runs no engine CLI: every operation below is
/// the Docker Engine API over the configured Unix socket (#239). The one piece
/// of per-host state it carries is the GPU vendor decision.
#[derive(Debug, Clone)]
pub struct ContainerRuntime {
    /// Request an NVIDIA GPU via `--gpus all` (AMD/Intel use `--device /dev/dri`).
    /// Knob: `QUASAR_GPU_NVIDIA`.
    nvidia: bool,
}

impl ContainerRuntime {
    /// An explicit policy, for callers that already know the vendor answer —
    /// notably tests, which must not depend on the host's real GPU.
    pub fn new(nvidia: bool) -> Self {
        Self { nvidia }
    }

    /// Knob: `QUASAR_GPU_NVIDIA`; unset means detect the host's GPU vendor.
    pub fn from_env() -> Self {
        let nvidia = match std::env::var("QUASAR_GPU_NVIDIA").ok().as_deref() {
            Some("0" | "false" | "FALSE") => false,
            Some("1" | "true" | "TRUE") => true,
            _ => matches!(
                crate::gpu_vendor::detect(),
                Some((crate::gpu_vendor::GpuVendor::Nvidia, _))
            ),
        };
        ContainerRuntime { nvidia }
    }

    /// The "NVIDIA in play" signal: gates the #375 startup probe for the host's 32-bit
    /// driver libs and the mount in [`ContainerRuntime::run`].
    pub fn is_nvidia(&self) -> bool {
        self.nvidia
    }

    /// This host's GPU access as a host probe sees it.
    pub fn app_gpu_access_live(&self) -> AppGpuAccess {
        self.app_gpu_access_with(crate::nvidia_volume::current())
    }

    /// A launch passes the volume its gate validated, never a second read: a provision
    /// completing in between would mount a volume no check had seen.
    fn app_gpu_access_with(&self, volume: Option<VolumeInfo>) -> AppGpuAccess {
        let nodes = dri_node_owners(Path::new(DRI_DIR));
        let access = app_gpu_access(self.nvidia, volume, &nodes);
        // The nodes arrive 0660 root:render, and the app user (PUID, no supplementary
        // groups) is neither — so RADV fails `Could not open device
        // /dev/dri/renderD128: Permission denied`, Vulkan enumerates llvmpipe only, and
        // gamescope aborts with "physical device doesn't support
        // VK_EXT_physical_device_drm" (desktop images degrade silently to software
        // rendering instead). NVIDIA never hit it: its ICD opens the 0666 /dev/nvidia*
        // nodes. Grants nothing the passed device did not already imply.
        let group_add = access.group_add_args();
        if !group_add.is_empty() {
            tracing::info!(
                token = "app-dri-group-add",
                nodes = %nodes
                    .iter()
                    .map(|n| format!("{}:{:o}:{}", n.name, n.mode & 0o777, n.gid))
                    .collect::<Vec<_>>()
                    .join(","),
                "app container joins DRM node groups: {}",
                group_add.join(" ")
            );
        }
        access
    }

    /// The exact locally running image, rather than a guessed development tag.
    pub fn own_image(&self) -> Result<String> {
        let budget = crate::runtime::configured()
            .map(|client| client.deadline())
            .unwrap_or(crate::runtime::ENGINE_INSPECTION_BUDGET);
        self.own_image_within(budget)
    }

    /// [`Self::own_image`] under an explicit budget. The readiness refresh's sibling EGL
    /// probe calls it first, before its own result cache, so on a hung daemon it was worth
    /// a full client deadline on the report path (#274).
    pub fn own_image_within(&self, budget: std::time::Duration) -> Result<String> {
        let id = crate::nvidia_volume::self_container_id()
            .context("cannot determine the agent container identity")?;
        let image = crate::runtime::configured()?
            .inspect_container_within(id, budget)
            .wait()?
            .ok_or_else(|| anyhow::anyhow!("agent container disappeared during image inspection"))?
            .image_id;
        Ok(image.trim().to_owned())
    }

    /// Read one `ENV` value baked into an image. The S1 driver-volume wiring must
    /// APPEND to an image's own `LD_LIBRARY_PATH` rather than replace it (docker `-e`
    /// replaces). Best-effort: an inspect failure returns `None`.
    pub fn image_env(&self, image: &str, key: &str) -> Option<String> {
        self.image_env_checked(image, key).ok().flatten()
    }

    /// One coherent read of an image's baked facts (id, environment, working directory)
    /// through the owned runtime API. `Ok(None)` is a missing image; `Err` is an
    /// inspection failure the caller must not mistake for absence.
    pub fn image_metadata(&self, image: &str) -> Result<Option<crate::runtime::ImageMetadata>> {
        crate::runtime::configured()?
            .inspect_image_metadata(image)
            .wait()
            .map_err(|error| anyhow!(error))
    }

    fn image_env_checked(&self, image: &str, key: &str) -> Result<Option<String>> {
        let metadata = crate::runtime::configured()?
            .inspect_image_metadata(image).wait()?
            .ok_or_else(|| anyhow::anyhow!("cannot inspect app image {image} before configuring its NVIDIA loader environment"))?;
        let prefix = format!("{key}=");
        Ok(metadata
            .baked_env
            .iter()
            .find_map(|l| l.strip_prefix(&prefix).map(str::to_string))
            .filter(|v| !v.is_empty()))
    }

    /// #375: locate the host dir holding the 32-bit NVIDIA driver libs, for read-only
    /// injection into NVIDIA app containers. The agent cannot see the host filesystem,
    /// so this runs a short-lived probe container that bind-mounts host `/usr` read-only
    /// and checks `libGLX_nvidia.so.<loaded driver version>` under `/usr/lib`,
    /// `/usr/lib32`, and `/usr/lib/i386-linux-gnu`. Only an ELF32 library matching
    /// the loaded kernel driver qualifies. Returns the first hit as a host path.
    ///
    /// Uses the running agent's exact local image; legacy/bare-metal agents whose
    /// image cannot be identified try `busybox` then `alpine:3`. Script exit 1
    /// ("ran, found nothing") is definitive. Failure returns `None`, allowing the
    /// driver-volume provisioner to supply the libraries; an explicit
    /// `QUASAR_NV_LIB32_PATH` remains available.
    pub fn probe_nvidia_lib32_path(&self) -> Option<String> {
        let version = crate::nvidia_volume::kernel_driver_version(Path::new("/"))?;
        let api = match crate::runtime::configured() {
            Ok(api) => api,
            Err(error) => {
                tracing::debug!(
                    token = "nvidia-lib32-probe-runtime-unavailable",
                    "nvidia lib32 probe cannot start owned diagnostic: {error}"
                );
                return None;
            }
        };
        if let Err(error) = api.recover_diagnostics().wait() {
            tracing::debug!(
                token = "nvidia-lib32-probe-recovery-pending",
                "nvidia lib32 probe has pending owned cleanup: {error}"
            );
            return None;
        }
        let images = self
            .own_image()
            .map(|image| vec![image])
            .unwrap_or_else(|_| {
                NVIDIA_LIB32_PROBE_IMAGES
                    .iter()
                    .map(|image| (*image).to_owned())
                    .collect()
            });
        for image in &images {
            if let Err(error) = api
                .ensure_image(image, NVIDIA_LIB32_PROBE_TIMEOUT)
                .wait(|_| {})
            {
                tracing::debug!(token = "nvidia-lib32-probe-image-unavailable", "nvidia lib32 probe image {image} is unavailable through the owned runtime: {error}");
                match error.kind {
                    crate::runtime::ErrorKind::Missing
                    | crate::runtime::ErrorKind::RegistryDenied => continue,
                    _ => return None,
                }
            }
            let nonce = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_nanos();
            let helper = crate::runtime::DiagnosticHelper {
                operation: format!("nvidia-lib32-{nonce}"),
                name: format!("quasar-lib32-probe-{}-{nonce}", std::process::id()),
                image: image.clone(),
            };
            let run = crate::runtime::DiagnosticRun {
                // Both the agent image and busybox/alpine compatibility
                // images provide `timeout`; keep an in-container deadline so
                // a lost observer cannot leave the `/usr` scan running.
                entrypoint: vec!["timeout".into()],
                command: vec![
                    "20s".into(),
                    "sh".into(),
                    "-c".into(),
                    NVIDIA_LIB32_PROBE_SCRIPT.into(),
                    "quasar-lib32-probe".into(),
                    version.clone(),
                ],
                bind: crate::runtime::ReadOnlyHostBind {
                    source: "/usr".into(),
                    target: "/host-usr".into(),
                },
            };
            let id = match api.run_diagnostic(helper, run).wait() {
                Ok(id) => id,
                Err(error) => {
                    tracing::debug!(token = "nvidia-lib32-probe-indeterminate", "nvidia lib32 probe via {image} has uncertain creation/start outcome: {error}");
                    return None;
                }
            };
            let out = match api.observe_diagnostic(id.clone()).wait() {
                Ok(result) => result,
                Err(error) => {
                    // This is an explicit termination of the identified helper,
                    // followed by its tracked cleanup.  Never try a fresh name
                    // while the original operation remains uncertain.
                    let _ = api.stop_diagnostic(id.clone()).wait();
                    let _ = api.cleanup_diagnostic(id).wait();
                    tracing::debug!(
                        token = "nvidia-lib32-probe-indeterminate",
                        "nvidia lib32 probe via {image} has uncertain observation outcome: {error}"
                    );
                    return None;
                }
            };
            if let Err(error) = api.cleanup_diagnostic(id).wait() {
                tracing::debug!(
                    token = "nvidia-lib32-probe-cleanup-pending",
                    "nvidia lib32 probe via {image} has cleanup pending: {error}"
                );
                return None;
            }
            match out {
                // exit 0: the script printed the container-path dir it found libs in.
                o if o.exit_code == Some(0) => {
                    let container_dir = o.stdout.trim().to_string();
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
                o if o.exit_code == Some(1) => return None,
                // A confirmed non-zero result may be image-specific; the
                // helper's final logs and cleanup were retained before trying
                // the compatibility fallback. Uncertain lifecycles returned
                // above and never reach this branch.
                o => {
                    tracing::debug!(
                        "nvidia lib32 probe via {image}: exit {:?}: {}",
                        o.exit_code,
                        o.stderr.trim()
                    );
                    continue;
                }
            }
        }
        None
    }

    /// Deterministic container name for a session, so a stale container from a
    /// crashed prior run is found and removed before we launch (no orphans, no
    /// name collision). Docker names allow `[a-zA-Z0-9_.-]`; a UUID qualifies.
    pub fn container_name(session_id: &str) -> String {
        format!("{SESSION_NAME_PREFIX}{session_id}")
    }

    /// Launch the app container as a detached Wayland client of the session
    /// compositor. Returns a handle whose `Drop` guarantees teardown.
    pub fn run(&self, spec: &ContainerSpec, params: &LaunchParams) -> Result<RunningContainer> {
        if let Some(error) = crate::readiness::sibling_mount_error() {
            anyhow::bail!("Cannot launch an app with unverified host mounts: {error}");
        }
        // A swap (P2-07) briefly runs the old and new app containers at once, so a
        // per-generation name override avoids a collision; the demo path passes None
        // and gets the stable per-session name.
        let name = params
            .container_name
            .clone()
            .unwrap_or_else(|| Self::container_name(params.session_id));

        // Validate the network BEFORE anything is spawned: an out-of-set value must
        // fail the launch, never reach the engine.
        let network = resolve_network(spec.network.as_deref())?;

        anyhow::ensure!(
            name.starts_with(SESSION_NAME_PREFIX),
            "app container name must start with {SESSION_NAME_PREFIX}"
        );
        let owner = crate::container_ownership::token().map_err(anyhow::Error::msg)?;
        // Every application is recovered by its durable operation journal; the
        // boot-only legacy sweep must never delete one of these by name.

        let mut args: Vec<String> = vec![
            "run".into(),
            "-d".into(),   // detached — the agent supervises via lifecycle, not stdout
            "--rm".into(), // self-clean if it exits on its own (no orphan)
            "--name".into(),
            name.clone(),
            "--label".into(),
            format!("{}={owner}", crate::container_ownership::LABEL),
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

        if spec.require_local_image {
            args.push("--pull=never".into());
        }

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
            // #375: bind the host's 32-bit NVIDIA driver libs read-only so native
            // 32-bit titles resolve libGLX_nvidia.so.* — the container ships only
            // 64-bit libs and the toolkit/CDI spec never injects 32-bit. NVIDIA
            // only; empty path ⇒ no mount. Falls back to the driver volume's
            // 32-bit half, resolved live so a provision completed after startup
            // takes effect on the next launch with no agent restart.
            let mut lib32 = String::new();
            // First-run S1: on a host whose NVIDIA userspace came from the
            // Quasar-provisioned driver volume, the CDI injection into this app
            // container is as CUDA-only as the agent's was, so the app needs the
            // same 64-bit GL/EGL/Vulkan set, vendor configs and loader path. Empty
            // on every host with its own driver.
            let mut image_ld = String::new();
            let mut gated_volume = None;
            if self.nvidia {
                lib32 = if params.nvidia_lib32_path.is_empty() {
                    crate::nvidia_volume::lib32_host_path(crate::nvidia_volume::current().as_ref())
                        .unwrap_or_default()
                } else {
                    params.nvidia_lib32_path.to_string()
                };
                if let Some((volume, ld)) = nvidia_driver_volume_gate(self, &spec.image)? {
                    gated_volume = Some(volume);
                    image_ld = ld;
                }
            }
            args.extend(
                self.app_gpu_access_with(gated_volume)
                    .session_args(&image_ld, &nvidia_lib32_mount_args(&lib32)),
            );
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

        // A lifecycle operation names one launch attempt, not a generation. A
        // failed/retired attempt cannot authorize a later rollback launch, but
        // retries inside RuntimeClient retain this exact identity.
        let operation = format!(
            "application-{name}-{}",
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_nanos()
        );
        let mut request = application_request_from_args(&args, operation.clone())?;
        request.unmount_nvidia_params = systempaths_unconfined;
        let runtime = crate::runtime::configured().map_err(|error| anyhow!(error))?;
        // Do not mint this attempt until every older writer for this name or
        // normalized managed-home source has been explicitly retired. RuntimeClient
        // reconciles only the recorded stable operation.
        retire_matching_pending_applications(&name, &request, |operation| {
            runtime
                .abandon_application(operation.to_owned())
                .wait()
                .map_err(|error| anyhow!(error))
        })?;
        if request.pull_never {
            anyhow::ensure!(
                runtime.image_present(&request.image).wait()?,
                "required local application image is missing"
            );
        } else {
            runtime
                .ensure_image(request.image.clone(), Duration::from_secs(300))
                .wait(|_| {})?;
        }
        let application = match runtime.start_application(request.clone()).wait() {
            Ok(application) => application,
            Err(error) => {
                // Start may have created or started the exact operation before
                // its reply was lost. Persist retirement by operation rather
                // than removing by name, which could hit somebody else's container.
                let abandon = runtime.abandon_application(operation.clone()).wait().err();
                if abandon.is_some() {
                    retain_pending_application(name.clone(), &request, operation);
                }
                return Err(anyhow!(
                    "application launch failed: {error}; durable cleanup {}",
                    abandon
                        .map(|pending| pending.to_string())
                        .unwrap_or_else(|| "completed".into())
                ));
            }
        };
        let id = application.as_str().to_owned();
        tracing::info!(
            "container {name} started through runtime API (id={})",
            short_id(&id)
        );

        Ok(RunningContainer {
            name,
            container_id: id,
            application,
            removed: Arc::new(AtomicBool::new(false)),
            cleanup_proven: false,
            writable_sources: writable_application_sources(&request),
        })
    }

    /// Inspect through the Quasar API interface for image-management reconciliation
    /// (`ImageManager::new`, `refresh_register_images`). Conflating "the image is gone"
    /// with "the daemon hiccuped" would demote every managed image to `absent` — and
    /// persist that — on one transient error.
    /// `Ok(true)` present, `Ok(false)` the daemon's definitive "no such image", `Err`
    /// anything else (leave the existing record untouched and warn).
    pub fn image_present(&self, registry_ref: &str) -> Result<bool, String> {
        crate::runtime::configured()
            .and_then(|runtime| runtime.image_present(registry_ref).wait())
            .map_err(|error| error.to_string())
    }
}

/// A terminal app-container exit, classified for the app-liveness policy (spec §2/§3).
/// Built from the owned runtime's [`crate::runtime::ApplicationResult`], which is the
/// only evidence allowed to end a generation: the observer in `source.rs` retries every
/// transport error rather than publishing one as an exit.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AppExitStatus {
    /// Exited with this code (0 = clean exit).
    Code(i32),
    /// The cgroup OOM-killed the container (`ApplicationResult::oom_killed`).
    OomKilled,
    /// The daemon proved the application terminal but reported no usable exit code.
    /// The runner's `fail` policy ends the session as for any other exit: fail-closed
    /// on purpose, because a session stuck on a frozen frame with no way to tell
    /// whether the app is alive is worse than an occasional false-positive failure.
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
    /// Internal launch policy after verified API preparation: the control plane's
    /// `image_ensure` step has already verified the image locally, so the launch must
    /// not pull. Every launch — gen 0, a swap's replacement, a rollback relaunch and a
    /// warm-up — goes through the same owned runtime request; swap and warm-up simply
    /// leave this `false` because nothing verified the image for them.
    pub require_local_image: bool,
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
    resolve_network_with(
        spec_network,
        std::env::var("QUASAR_CONTAINER_NETWORK").ok().as_deref(),
    )
}

/// Pure core of [`resolve_network`]: `host_knob` is the `QUASAR_CONTAINER_NETWORK` value
/// as read from env, `None` for unset.
fn resolve_network_with(spec_network: Option<&str>, host_knob: Option<&str>) -> Result<String> {
    let (value, source, allowed): (String, &str, &[&str]) =
        match spec_network.map(str::trim).filter(|s| !s.is_empty()) {
            Some(v) => (v.to_string(), "the app spec", &APP_CONTAINER_NETWORKS),
            None => match host_knob {
                Some(v) if !v.trim().is_empty() => (
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
            require_local_image: false,
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
const NVIDIA_DRIVER_VOLUME_DST: &str = crate::nvidia_volume::VOLUME_MOUNT;

/// Launch policy for the S1 driver volume: everything that may refuse a launch, plus the
/// app image's own `LD_LIBRARY_PATH` (docker `-e` REPLACES it, and overwriting an image's
/// loader path trades one breakage for another). `None` when nothing is provisioned.
///
/// Must run before the access facts are gathered: `retry_mount_resolution` is what can
/// still resolve the mount a launch is about to be given.
fn nvidia_driver_volume_gate(
    runtime: &ContainerRuntime,
    image: &str,
) -> Result<Option<(VolumeInfo, String)>> {
    crate::nvidia_volume::validate_host_path_for_launch().map_err(anyhow::Error::msg)?;
    crate::nvidia_volume::retry_mount_resolution();
    let Some(info) = crate::nvidia_volume::current() else {
        return Ok(None);
    };
    anyhow::ensure!(info.host.is_some() || info.name.is_some(),
        "NVIDIA driver is provisioned but its app-container mount is unresolved; check Docker socket and identity inspection or set QUASAR_NVIDIA_DRIVER_HOST_PATH to the existing host directory");
    if let crate::nvidia_volume::EglRuntime::Broken { detail, .. } =
        crate::nvidia_volume::probe_sibling_egl()
    {
        anyhow::bail!("NVIDIA driver failed the sibling-container EGL test: {detail}");
    }
    tracing::info!(
        target: "quasar.nvidia_volume",
        image,
        "app container receives the Quasar-provisioned NVIDIA driver volume (v{})",
        info.manifest.driver_version
    );
    let image_ld = runtime
        .image_env_checked(image, "LD_LIBRARY_PATH")?
        .unwrap_or_default();
    Ok(Some((info, image_ld)))
}

/// What a GPU-enabled application container is given for GPU access (#259). Gathered
/// once and realized two ways — a session's docker argv, and a host probe's closed
/// profile — so a probe cannot be given more or less than the launch it stands for.
pub struct AppGpuAccess {
    nvidia: bool,
    /// Never set on a non-NVIDIA host, whatever the provisioner published.
    driver_volume: Option<VolumeInfo>,
    /// Non-zero, ascending and distinct, as the GPU probe profile requires.
    dri_groups: Vec<u32>,
}

/// Decide from observed facts; the live gathering is [`ContainerRuntime::app_gpu_access_live`].
fn app_gpu_access(
    nvidia: bool,
    volume: Option<VolumeInfo>,
    nodes: &[DrmNodeOwner],
) -> AppGpuAccess {
    AppGpuAccess {
        nvidia,
        driver_volume: volume.filter(|_| nvidia),
        dri_groups: granted_dri_gids(nodes),
    }
}

impl AppGpuAccess {
    /// Numeric only: `render`/`video` do not exist in the app images, and a name that
    /// does not resolve fails the whole `docker run`.
    fn group_add_args(&self) -> Vec<String> {
        self.dri_groups
            .iter()
            .flat_map(|gid| ["--group-add".to_string(), gid.to_string()])
            .collect()
    }

    /// `docker run` arguments. Order is load-bearing: `nvidia_lib32_mount` must precede
    /// the driver volume or the two swap places in the realized `Mounts`.
    pub fn session_args(
        &self,
        image_ld_library_path: &str,
        nvidia_lib32_mount: &[String],
    ) -> Vec<String> {
        let mut args = Vec::new();
        if self.nvidia {
            args.push("--gpus".into());
            args.push("all".into());
            args.extend(nvidia_lib32_mount.iter().cloned());
            args.extend(crate::nvidia_volume::app_container_args(
                self.driver_volume.as_ref(),
                NVIDIA_DRIVER_VOLUME_DST,
                image_ld_library_path,
            ));
        }
        // AMD/Intel, and NVIDIA's render node for Vulkan/EGL, all want the DRM nodes.
        args.push("--device".into());
        args.push(DRI_DIR.into());
        args.extend(self.group_add_args());
        args
    }

    /// Whether the probe container mounts the Quasar driver volume, so its EGL test can
    /// name the vendor library inside it.
    pub fn carries_driver_volume(&self) -> bool {
        self.driver_volume
            .as_ref()
            .and_then(nvidia_driver_access)
            .is_some()
    }

    /// The same access as the closed GPU probe profile (#258), field for field with
    /// [`Self::session_args`]: `nvidia` drives the device request there and here, and the
    /// driver volume is mounted in both only when the host provisioned one.
    ///
    /// #280: these two are independent. An NVIDIA host taking its driver userspace from
    /// the container toolkit provisions no volume, and the device request is the only
    /// thing that gives the container an EGL stack — a probe without it fails on a host
    /// where a real session succeeds.
    pub fn probe_run(
        &self,
        entrypoint: Vec<String>,
        command: Vec<String>,
    ) -> crate::runtime::GpuProbeRun {
        crate::runtime::GpuProbeRun {
            entrypoint,
            command,
            devices: vec![DRI_DIR.into()],
            groups: self.dri_groups.clone(),
            nvidia_device_request: self.nvidia,
            nvidia: self.driver_volume.as_ref().and_then(nvidia_driver_access),
        }
    }
}

/// The NVIDIA half of GPU access: the all-GPUs device request, the read-only driver
/// mount and the loader environment. `None` when the provisioned volume has neither a
/// resolved host path nor a volume name, which is what a session refuses to launch on.
///
/// The probe runs the agent's own image, so it appends no image loader path.
pub(crate) fn nvidia_driver_access(
    info: &VolumeInfo,
) -> Option<crate::runtime::NvidiaDriverAccess> {
    let driver_mount = match (&info.name, &info.host) {
        (Some(name), _) => crate::runtime::NvidiaDriverMount::NamedVolume {
            name: name.clone(),
            target: NVIDIA_DRIVER_VOLUME_DST.into(),
        },
        (None, Some(source)) => {
            crate::runtime::NvidiaDriverMount::ReadOnlyBind(crate::runtime::ReadOnlyHostBind {
                source: source.clone(),
                target: NVIDIA_DRIVER_VOLUME_DST.into(),
            })
        }
        (None, None) => return None,
    };
    Some(crate::runtime::NvidiaDriverAccess {
        driver_mount,
        image_ld_library_path: String::new(),
        has_gbm_backend: info
            .local
            .join(crate::nvidia_volume::layout::GBM_DIR)
            .join("nvidia-drm_gbm.so")
            .is_file(),
    })
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
/// on the host runs this — the agent must never load policy itself.
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
///
/// The `/host` prefix is also what makes the read succeed at all in a container: the agent
/// itself runs under `docker-default`, whose `deny /sys/kernel/security/** rwklx` covers the
/// bare path but not the bind's. Do not "simplify" this to the direct path.
const APPARMOR_PROFILES_RELS: [&str; 2] = [
    "host/sys/kernel/security/apparmor/profiles",
    "sys/kernel/security/apparmor/profiles",
];

/// Is [`APP_APPARMOR_PROFILE`] loaded, as far as the agent can tell?
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum AppArmorProfileState {
    /// Loaded and enforcing (`enforce` or the stricter `kill`).
    Loaded,
    /// Loaded, but in a mode that only logs what it would have denied. Confining with it
    /// is harmless and still the right flag, but nothing is actually being enforced, so it
    /// must not read as "confined" anywhere an operator looks.
    Complain,
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
        // Complain picks the profile too: it is the flag the operator asked for, and a
        // profile that logs instead of denying can only be an improvement over no profile
        // at all. Readiness is where the mode is called out.
        None => match state {
            AppArmorProfileState::Loaded | AppArmorProfileState::Complain => {
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
        return profile_list_state(&body, name);
    }
    AppArmorProfileState::Unknown
}

/// One profile per line as `name (mode)`; a child profile is `parent//child (mode)`, which
/// must not answer for its parent.
///
/// The mode is not decoration: a profile loaded in `complain` matches by name while
/// enforcing nothing, so matching on the name alone would report a confinement that does
/// not exist. `enforce` and the stricter `kill` are the enforcing modes; every other mode
/// AppArmor can report (`complain`, `prompt`, `unconfined`) only logs.
fn profile_list_state(body: &str, name: &str) -> AppArmorProfileState {
    for line in body.lines() {
        let (found, rest) = match line.split_once(" (") {
            Some((n, rest)) => (n.trim(), rest),
            None => continue,
        };
        if found != name {
            continue;
        }
        let mode = rest.trim_end().trim_end_matches(')');
        return match mode {
            "enforce" | "kill" => AppArmorProfileState::Loaded,
            _ => AppArmorProfileState::Complain,
        };
    }
    AppArmorProfileState::NotLoaded
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
                "app containers run apparmor-unconfined on this AppArmor host: {reason}. \
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

/// Every distinct group owning a DRM node the app cannot already open through the node's
/// `other` bits. Ascending and deduped, so one set of nodes has one fingerprint.
fn granted_dri_gids(nodes: &[DrmNodeOwner]) -> Vec<u32> {
    let mut gids: Vec<u32> = nodes
        .iter()
        .filter(|n| dri_group_granted(n.mode, n.gid))
        .map(|n| n.gid)
        .collect();
    gids.sort_unstable();
    gids.dedup();
    gids
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

/// Hard cap on the bytes retained for a single log line: an app printing a megabyte
/// must not be able to make the failure report, or the agent's heap, unbounded. The
/// ring bounds how many lines are kept; this bounds each one.
const APP_LOG_MAX_LINE: usize = 2000;

/// Appended to a record that hit [`APP_LOG_MAX_LINE`], so a reader can tell a
/// clipped line from a short one.
const APP_LOG_TRUNCATION_MARKER: &str = " …[truncated]";

/// A bounded, newest-wins ring of an app container's log lines (S5). One ring per
/// generation: the source holds the reading handle and its observer thread the writing
/// one, and lines arrive already split, as the `stdout`/`stderr` strings of a runtime
/// `ApplicationResult` or `ApplicationLogTail`. Cloneable and internally synchronised.
/// A poisoned mutex is recovered from, never propagated — this is diagnostic, and a
/// lost log line must never fail a session.
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
    pub(crate) fn push(&self, line: String) {
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

/// A launched container whose `Drop` tears it down — so any early return / panic
/// / dropped session on the agent side cannot leak a container.
pub struct RunningContainer {
    name: String,
    /// Lets the app-liveness observer bind exact RuntimeClient evidence to THIS container
    /// without racing a same-named replacement (app-liveness spec §3.1).
    container_id: String,
    application: ApplicationId,
    /// Shared with the app-liveness observer: set before the API stop/cleanup request, so
    /// an observer can tell "we tore this down ourselves" (swap,
    /// session stop) from a genuine app exit. A deliberate stop must never be
    /// misclassified as an app failure (spec §3 G5 swap safety).
    removed: Arc<AtomicBool>,
    cleanup_proven: bool,
    /// Normalized writable sources retained so an uncertain stop survives this
    /// handle being dropped and blocks a later generation's shared-home launch.
    writable_sources: BTreeSet<String>,
}

impl RunningContainer {
    /// Tear the container down (idempotent) on the normal stop/failure path; `Drop` is
    /// the backstop. Intentional stop is deliberately separate from removal proof: a
    /// lost stop/remove reply retains the same handle and requires another exact-ID
    /// reconciliation before any replacement may use its managed home.
    pub fn stop(&mut self) -> Result<()> {
        let seconds: u64 = std::env::var("QUASAR_APP_STOP_TIMEOUT_SECS")
            .ok()
            .and_then(|v| v.parse().ok())
            .unwrap_or(10);
        self.stop_with(|application| {
            let api = crate::runtime::configured().map_err(anyhow::Error::from)?;
            api.stop_application(application.clone(), Duration::from_secs(seconds))
                .wait()
                .map_err(anyhow::Error::from)?;
            api.cleanup_application(application.clone())
                .wait()
                .map_err(anyhow::Error::from)
        })
    }

    fn stop_with<F>(&mut self, mut retire: F) -> Result<()>
    where
        F: FnMut(&ApplicationId) -> Result<()>,
    {
        if self.cleanup_proven {
            return Ok(());
        }
        // Intent is visible to the observer before Docker sees a mutation, but it is
        // never removal proof. Any error keeps `cleanup_proven=false`, so the caller
        // must retry this exact durable application identity.
        self.removed.store(true, Ordering::SeqCst);
        if let Err(error) = retire(&self.application) {
            retain_pending_application_sources(
                self.name.clone(),
                self.writable_sources.clone(),
                self.application.operation.clone(),
            );
            tracing::warn!(
                token = "application-stop-pending",
                "runtime application stop remains journalled: {error}"
            );
            return Err(error);
        }
        self.cleanup_proven = true;
        clear_pending_application(&self.name, &self.application.operation);
        Ok(())
    }

    pub fn name(&self) -> &str {
        &self.name
    }

    /// The daemon-assigned container id retained for exact ownership diagnostics.
    pub fn container_id(&self) -> &str {
        &self.container_id
    }

    pub fn application_id(&self) -> ApplicationId {
        self.application.clone()
    }

    /// A clone of the shared teardown marker — see the field doc comment.
    pub fn removed_flag(&self) -> Arc<AtomicBool> {
        self.removed.clone()
    }
}

impl Drop for RunningContainer {
    fn drop(&mut self) {
        if let Err(error) = self.stop() {
            tracing::warn!(
                token = "application-drop-cleanup-pending",
                "runtime application cleanup remains durable: {error}"
            );
        }
    }
}

#[cfg(test)]
mod tests {
    // `sh` here is the #375 probe SCRIPT under test, not an engine binary.
    use super::*;
    use std::process::Command;

    #[test]
    fn runtime_request_preserves_bind_options_and_embeds_seccomp_profile_content() {
        let dir = tempfile::tempdir().unwrap();
        let profile = dir.path().join("seccomp.json");
        std::fs::write(&profile, "{\"defaultAction\":\"SCMP_ACT_ERRNO\"}").unwrap();
        let args = vec![
            "run".into(),
            "-d".into(),
            "--name".into(),
            "quasar-sess-s1".into(),
            "--network".into(),
            "none".into(),
            "--security-opt".into(),
            format!("seccomp={}", profile.display()),
            "-v".into(),
            "/home/a:/home/a:Z,nocopy,cached".into(),
            "image:test".into(),
            "--arg".into(),
        ];
        let request =
            application_request_from_args(&args, "application-quasar-sess-s1".into()).unwrap();
        assert_eq!(request.mounts, vec!["/home/a:/home/a:Z,nocopy,cached"]);
        assert_eq!(
            request.security.security_opt,
            vec!["seccomp={\"defaultAction\":\"SCMP_ACT_ERRNO\"}"]
        );
        assert_eq!(request.command, vec!["--arg"]);
    }

    #[test]
    fn runtime_request_keeps_typed_mounts_volumes_and_catalog_security_optout() {
        let args = vec![
            "run".into(),
            "-d".into(),
            "--name".into(),
            "quasar-sess-s1".into(),
            "--security-opt".into(),
            "seccomp=unconfined".into(),
            "--mount".into(),
            "type=bind,src=/missing-on-host,dst=/run/wayland-0,readonly".into(),
            "--mount".into(),
            "type=volume,src=quasar-driver,dst=/opt/quasar/nvidia-driver,readonly,volume-nocopy"
                .into(),
            "image:test".into(),
        ];
        let request =
            application_request_from_args(&args, "application-quasar-sess-s1".into()).unwrap();
        assert!(!request.security.no_new_privileges);
        assert!(!request.pull_never);
        assert_eq!(request.command, Vec::<String>::new());
        assert_eq!(
            request.typed_mounts,
            vec![
                ApplicationMount::Bind {
                    source: "/missing-on-host".into(),
                    target: "/run/wayland-0".into(),
                    read_only: true,
                    consistency: None
                },
                ApplicationMount::Volume {
                    source: "quasar-driver".into(),
                    target: "/opt/quasar/nvidia-driver".into(),
                    read_only: true,
                    no_copy: true
                },
            ]
        );
    }

    #[test]
    fn runtime_request_rejects_unrepresented_typed_mount_options() {
        let args = vec![
            "run".into(),
            "-d".into(),
            "--name".into(),
            "quasar-sess-s1".into(),
            "--mount".into(),
            "type=bind,src=/host,dst=/guest,bind-nonrecursive".into(),
            "image:test".into(),
        ];
        assert!(application_request_from_args(&args, "application-quasar-sess-s1".into()).is_err());
    }

    #[test]
    fn runtime_request_maps_systempaths_to_api_path_lists_not_security_opt() {
        let args = vec![
            "run".into(),
            "-d".into(),
            "--name".into(),
            "quasar-sess-s1".into(),
            "--security-opt".into(),
            "seccomp=unconfined".into(),
            "--security-opt".into(),
            "systempaths=unconfined".into(),
            "image:test".into(),
        ];
        let request =
            application_request_from_args(&args, "application-quasar-sess-s1".into()).unwrap();
        assert!(request.security.systempaths_unconfined);
        assert_eq!(request.security.security_opt, vec!["seccomp=unconfined"]);
    }

    fn node(name: &str, mode: u32, gid: u32) -> DrmNodeOwner {
        DrmNodeOwner {
            name: name.to_string(),
            mode,
            gid,
        }
    }

    #[test]
    fn lib32_probe_requires_matching_version_and_elf_class() {
        let root = tempfile::tempdir().unwrap();
        let lib = root.path().join("lib");
        std::fs::create_dir(&lib).unwrap();
        let probe = || {
            Command::new("sh")
                .args([
                    "-c",
                    NVIDIA_LIB32_PROBE_SCRIPT,
                    "test",
                    "610.57.04",
                    root.path().to_str().unwrap(),
                ])
                .output()
                .unwrap()
        };
        std::fs::write(lib.join("libGLX_nvidia.so.595.1"), b"\x7fELF\x01").unwrap();
        assert!(
            !probe().status.success(),
            "a stale driver must not be injected"
        );
        std::fs::write(lib.join("libGLX_nvidia.so.610.57.04"), b"\x7fELF\x02").unwrap();
        assert!(
            !probe().status.success(),
            "a 64-bit library must not satisfy lib32"
        );
        std::fs::write(lib.join("libGLX_nvidia.so.610.57.04"), b"\x7fELF\x01").unwrap();
        let out = probe();
        assert!(out.status.success());
        assert_eq!(
            String::from_utf8(out.stdout).unwrap(),
            lib.to_string_lossy()
        );
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

        // Complain enforces nothing, but the flag is still the one to pass: readiness is
        // what tells the operator the mode.
        assert_eq!(
            choose(true, Complain),
            AppArmorChoice::Profile("quasar-app".into())
        );

        for state in [Loaded, Complain, NotLoaded, Unknown] {
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

        // A complain-mode profile matches by name while enforcing nothing.
        std::fs::write(rel.join("profiles"), "quasar-app (complain)\n").unwrap();
        assert_eq!(
            apparmor_profile_state(root, "quasar-app"),
            AppArmorProfileState::Complain
        );
        // kill is stricter than enforce, not weaker.
        std::fs::write(rel.join("profiles"), "quasar-app (kill)\n").unwrap();
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
        let group_add = |nodes: &[DrmNodeOwner]| {
            flag_values(
                &app_gpu_access(false, None, nodes).session_args("", &[]),
                "--group-add",
            )
        };
        // hermes: renderD128 root:render(991), card0 root:video(44).
        assert_eq!(
            group_add(&[node("renderD128", 0o660, 991), node("card0", 0o660, 44)]),
            vec!["44", "991"],
            "both owning gids, ascending"
        );

        // Deduped across nodes sharing a group, and ordered independently of readdir.
        assert_eq!(
            group_add(&[
                node("renderD129", 0o660, 991),
                node("renderD128", 0o660, 991),
                node("card1", 0o660, 44),
            ]),
            vec!["44", "991"]
        );

        // Nothing to grant: world-rw needs no group, a groupless mode has none to give,
        // and gid 0 is never handed out.
        assert!(group_add(&[
            node("renderD128", 0o666, 991),
            node("card0", 0o600, 44),
            node("renderD129", 0o660, 0),
        ])
        .is_empty());

        assert!(group_add(&[]).is_empty());
    }

    /// Readiness predicts app access with this same predicate; a divergence is how the
    /// check false-passes a host whose sessions cannot start.
    #[test]
    fn a_granted_group_is_exactly_what_the_args_contain() {
        for (mode, gid) in [(0o660, 991), (0o666, 991), (0o600, 44), (0o660, 0)] {
            let granted = dri_group_granted(mode, gid);
            let gids = granted_dri_gids(&[node("renderD128", mode, gid)]);
            assert_eq!(granted, !gids.is_empty(), "mode {mode:o} gid {gid}");
        }
    }

    fn volume(local: &Path, name: Option<&str>, host: Option<&str>) -> VolumeInfo {
        VolumeInfo {
            local: local.to_path_buf(),
            host: host.map(PathBuf::from),
            name: name.map(str::to_string),
            manifest: crate::nvidia_volume::Manifest {
                driver_version: "610.57.04".into(),
                sha256: "f".repeat(64),
                url: "https://example.invalid/driver.run".into(),
                provisioned_at_unix: 0,
                agent_version: "test".into(),
                lib64_count: 1,
                lib32_count: 0,
                layout_version: crate::nvidia_volume::CURRENT_LAYOUT_VERSION,
            },
        }
    }

    /// Values of `--flag value` pairs in a docker argv.
    fn flag_values(args: &[String], flag: &str) -> Vec<String> {
        args.windows(2)
            .filter(|w| w[0] == flag)
            .map(|w| w[1].clone())
            .collect()
    }

    fn mount_field(mount: &str, key: &str) -> Option<String> {
        mount
            .split(',')
            .find_map(|f| f.strip_prefix(&format!("{key}=")))
            .map(str::to_string)
    }

    /// One `AppGpuAccess`, two realizations: a probe must be given what a session's
    /// application container is given. Editing one realization alone fails here.
    #[test]
    fn probe_and_session_gpu_access_cannot_diverge() {
        let with_gbm = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(with_gbm.path().join(crate::nvidia_volume::layout::GBM_DIR))
            .unwrap();
        std::fs::write(
            with_gbm
                .path()
                .join(crate::nvidia_volume::layout::GBM_DIR)
                .join("nvidia-drm_gbm.so"),
            b"x",
        )
        .unwrap();
        let no_gbm = tempfile::tempdir().unwrap();
        let amd_nodes = || vec![node("renderD128", 0o660, 991), node("card0", 0o660, 44)];
        let cases: Vec<(&str, bool, Option<VolumeInfo>, Vec<DrmNodeOwner>)> = vec![
            ("amd", false, None, amd_nodes()),
            (
                "root-owned node is never granted",
                false,
                None,
                vec![node("renderD128", 0o660, 0)],
            ),
            ("no nodes", false, None, Vec::new()),
            (
                "nvidia named volume",
                true,
                Some(volume(with_gbm.path(), Some("quasar-nvidia-driver"), None)),
                amd_nodes(),
            ),
            (
                "nvidia host bind",
                true,
                Some(volume(no_gbm.path(), None, Some("/srv/quasar/driver"))),
                amd_nodes(),
            ),
            ("nvidia without a volume", true, None, amd_nodes()),
        ];
        for (label, nvidia, volume, nodes) in cases {
            let access = app_gpu_access(nvidia, volume, &nodes);
            let session = access.session_args("/image/lib", &[]);
            let probe = access.probe_run(
                vec!["/usr/bin/timeout".into()],
                vec!["20s".into(), "/usr/local/bin/quasar-node-agent".into()],
            );

            assert_eq!(
                flag_values(&session, "--group-add"),
                probe
                    .groups
                    .iter()
                    .map(u32::to_string)
                    .collect::<Vec<String>>(),
                "{label}: groups"
            );
            assert!(
                probe.groups.first().is_none_or(|g| *g != 0)
                    && probe.groups.windows(2).all(|p| p[0] < p[1]),
                "{label}: the probe profile requires non-zero, ascending, distinct gids"
            );
            assert_eq!(
                flag_values(&session, "--device").contains(&DRI_DIR.to_string()),
                probe.devices == vec![DRI_DIR.to_string()],
                "{label}: DRM nodes"
            );

            let gpus_all = flag_values(&session, "--gpus") == ["all"];
            let mount = flag_values(&session, "--mount").into_iter().next();
            // The two halves of NVIDIA access are asserted apart: a host whose driver
            // userspace comes from the container toolkit gets the device request with no
            // driver volume, and conflating them is exactly how #280 shipped.
            assert_eq!(
                gpus_all, probe.nvidia_device_request,
                "{label}: NVIDIA device request"
            );
            assert_eq!(
                mount.is_some(),
                probe.nvidia.is_some(),
                "{label}: Quasar driver volume"
            );
            let Some(nvidia_access) = probe.nvidia else {
                continue;
            };
            let mount = mount.unwrap();
            let (source, target) = match &nvidia_access.driver_mount {
                crate::runtime::NvidiaDriverMount::NamedVolume { name, target } => {
                    (name.clone(), target.clone())
                }
                crate::runtime::NvidiaDriverMount::ReadOnlyBind(bind) => {
                    (bind.source.display().to_string(), bind.target.clone())
                }
            };
            assert_eq!(mount_field(&mount, "src"), Some(source), "{label}: source");
            assert_eq!(mount_field(&mount, "dst"), Some(target.clone()), "{label}");
            let ld = flag_values(&session, "-e")
                .into_iter()
                .find_map(|e| e.strip_prefix("LD_LIBRARY_PATH=").map(str::to_string))
                .expect("the driver volume prepends its lib64 to the loader path");
            assert_eq!(
                ld.split(':').next(),
                Some(format!("{target}/lib64").as_str()),
                "{label}: loader path"
            );
            assert_eq!(
                flag_values(&session, "-e")
                    .iter()
                    .any(|e| e.starts_with("GBM_BACKENDS_PATH=")),
                nvidia_access.has_gbm_backend,
                "{label}: gbm backend"
            );
        }
    }

    /// #280: an NVIDIA host that takes its driver userspace from the container toolkit
    /// provisions no Quasar driver volume. The application container still gets the
    /// `nvidia` device request — that is where its EGL stack comes from — so the probe
    /// standing for it must get one too, or it opens no GPU and blocks every launch.
    #[test]
    fn an_nvidia_host_without_a_driver_volume_still_gives_the_probe_the_device_request() {
        let access = app_gpu_access(true, None, &[node("renderD128", 0o660, 991)]);
        let probe = access.probe_run(vec!["/usr/bin/timeout".into()], vec!["20s".into()]);
        assert!(
            probe.nvidia_device_request,
            "the toolkit path must still request the GPU"
        );
        assert!(
            probe.nvidia.is_none(),
            "there is no Quasar driver volume to mount on this host"
        );
        assert!(flag_values(&access.session_args("/image/lib", &[]), "--gpus") == ["all"]);
    }

    /// The driver-volume host keeps both halves: the device request and the volume.
    #[test]
    fn an_nvidia_host_with_a_driver_volume_gives_the_probe_both_halves() {
        let dir = tempfile::tempdir().unwrap();
        let access = app_gpu_access(
            true,
            Some(volume(dir.path(), Some("quasar-nvidia-driver"), None)),
            &[node("renderD128", 0o660, 991)],
        );
        let probe = access.probe_run(vec!["/usr/bin/timeout".into()], vec!["20s".into()]);
        assert!(probe.nvidia_device_request);
        assert!(probe.nvidia.is_some());
    }

    /// A non-NVIDIA host asks for no device request at all, volume or not.
    #[test]
    fn a_non_nvidia_host_gives_the_probe_no_device_request() {
        let dir = tempfile::tempdir().unwrap();
        for volume_info in [None, Some(volume(dir.path(), Some("stale"), None))] {
            let access = app_gpu_access(false, volume_info, &[node("renderD128", 0o660, 991)]);
            let probe = access.probe_run(vec!["/usr/bin/timeout".into()], vec!["20s".into()]);
            assert!(!probe.nvidia_device_request);
            assert!(probe.nvidia.is_none());
        }
    }

    /// Drift guard. Every field of `AppGpuAccess` is one injection the application path
    /// applies; the exhaustive destructuring below stops compiling the moment a field is
    /// added, so a new injection cannot land without saying how the probe mirrors it.
    #[test]
    fn every_app_gpu_injection_is_mirrored_into_the_probe_profile() {
        let dir = tempfile::tempdir().unwrap();
        let access = app_gpu_access(
            true,
            Some(volume(dir.path(), Some("quasar-nvidia-driver"), None)),
            &[node("renderD128", 0o660, 991), node("card0", 0o660, 44)],
        );
        let AppGpuAccess {
            nvidia,
            driver_volume,
            dri_groups,
        } = &access;
        let probe = access.probe_run(vec!["/usr/bin/timeout".into()], vec!["20s".into()]);
        assert_eq!(*nvidia, probe.nvidia_device_request);
        assert_eq!(driver_volume.is_some(), probe.nvidia.is_some());
        assert_eq!(*dri_groups, probe.groups);
        assert_eq!(probe.devices, vec![DRI_DIR.to_string()]);
    }

    /// The realized create body depends on argv order: the 32-bit bind must precede the
    /// driver volume, or the two swap places in `Mounts`.
    #[test]
    fn session_gpu_args_keep_their_launch_order() {
        let dir = tempfile::tempdir().unwrap();
        let access = app_gpu_access(
            true,
            Some(volume(dir.path(), Some("quasar-nvidia-driver"), None)),
            &[node("renderD128", 0o660, 991)],
        );
        let args = access.session_args("/image/lib", &nvidia_lib32_mount_args("/usr/lib32"));
        assert_eq!(
            args,
            vec![
                "--gpus".to_string(),
                "all".into(),
                "-v".into(),
                format!("/usr/lib32:{NVIDIA_LIB32_MOUNT_DST}:ro"),
                "--mount".into(),
                format!("type=volume,src=quasar-nvidia-driver,dst={NVIDIA_DRIVER_VOLUME_DST},readonly"),
                "-e".into(),
                format!("LD_LIBRARY_PATH={NVIDIA_DRIVER_VOLUME_DST}/lib64:/image/lib"),
                "-e".into(),
                format!("__EGL_VENDOR_LIBRARY_DIRS={NVIDIA_DRIVER_VOLUME_DST}/glvnd/egl_vendor.d:/etc/glvnd/egl_vendor.d:/usr/share/glvnd/egl_vendor.d"),
                "-e".into(),
                format!("__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS={NVIDIA_DRIVER_VOLUME_DST}/egl_external_platform.d:/usr/share/egl/egl_external_platform.d"),
                "-e".into(),
                format!("VK_ADD_DRIVER_FILES={NVIDIA_DRIVER_VOLUME_DST}/vulkan/icd.d/nvidia_icd.json"),
                "--device".into(),
                DRI_DIR.into(),
                "--group-add".into(),
                "991".into(),
            ]
        );
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

    /// An app log line arrives through `ApplicationResult`, so it can be arbitrarily
    /// long; the ring, not a reader, is what bounds it.
    #[test]
    fn an_oversized_line_is_clipped_and_says_so() {
        let ring = AppLogRing::new();
        ring.push("x".repeat(APP_LOG_MAX_LINE * 50));
        let tail = ring.tail();
        assert_eq!(tail.len(), 1);
        assert!(
            tail[0].len() <= APP_LOG_MAX_LINE + APP_LOG_TRUNCATION_MARKER.len(),
            "retained {} bytes — the cap did not hold",
            tail[0].len()
        );
        assert!(tail[0].ends_with(APP_LOG_TRUNCATION_MARKER));
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

    /// A crashing app's last words are the point, so overflow evicts from the front.
    #[test]
    fn the_ring_keeps_the_newest_lines() {
        let ring = AppLogRing::new();
        for i in 0..APP_LOG_TAIL_LINES + 50 {
            ring.push(format!("line {i}"));
        }

        let tail = ring.tail();
        assert_eq!(tail.len(), APP_LOG_TAIL_LINES);
        assert_eq!(tail[APP_LOG_TAIL_LINES - 1], "line 149");
        assert_eq!(tail[0], "line 50");
    }

    /// Generation isolation: `AppSource` outlives its container across a relaunch, so a
    /// shared ring would report the PREVIOUS container's dying words as the new app's
    /// failure. A fresh ring per launch leaves the retired one to the old follower.
    #[test]
    fn a_fresh_ring_does_not_inherit_the_previous_generations_lines() {
        let first = AppLogRing::new();
        first.push("old app: fatal".into());
        assert_eq!(first.tail(), vec!["old app: fatal"]);

        // What `launch` does for every generation.
        let second = AppLogRing::new();
        assert!(
            second.tail().is_empty(),
            "a new generation must start with an empty capture"
        );

        // The retired ring stays writable for its own retired observer without
        // touching the new generation's capture.
        first.push("old app: more".into());
        assert_eq!(first.tail().len(), 2);
        assert!(
            second.tail().is_empty(),
            "the retired follower leaked into the new ring"
        );
    }

    /// §S2 container-network resolution + its defensive validation.
    #[test]
    fn container_network_precedence_and_validation() {
        // 1. Nothing stated anywhere ⇒ the hardened default.
        assert_eq!(resolve_network_with(None, None).unwrap(), "none");
        // An empty/whitespace app value is "unset", not a value.
        assert_eq!(resolve_network_with(Some(""), None).unwrap(), "none");
        assert_eq!(resolve_network_with(Some("  "), None).unwrap(), "none");

        // 2. The app spec wins outright (the #463 Steam case: the app declares bridge
        //    on a host that never set the knob).
        assert_eq!(
            resolve_network_with(Some("bridge"), None).unwrap(),
            "bridge"
        );
        assert_eq!(resolve_network_with(Some("none"), None).unwrap(), "none");

        // 3. The host knob applies only when the app states nothing…
        assert_eq!(
            resolve_network_with(None, Some("bridge")).unwrap(),
            "bridge"
        );
        //    …and an app that states one still overrides it, in BOTH directions:
        //    an app can also pin itself back to `none` on a bridged host.
        assert_eq!(
            resolve_network_with(Some("bridge"), Some("bridge")).unwrap(),
            "bridge"
        );
        assert_eq!(
            resolve_network_with(Some("none"), Some("bridge")).unwrap(),
            "none"
        );

        // 4. An out-of-set value fails the launch from EITHER source: the backstop that
        //    keeps `container:<id>` off the docker command line. (`"bridge "` is not
        //    here: whitespace is trimmed, so it is the legitimate value.)
        for bad in [
            "container:quasar-control-plane",
            "my-net",
            "NONE",
            "host;rm",
        ] {
            let err = resolve_network_with(Some(bad), None)
                .expect_err(&format!("{bad:?} from the app spec must be rejected"));
            assert!(
                err.to_string().contains("not an allowed value"),
                "unexpected error for {bad:?}: {err}"
            );
        }
        let err = resolve_network_with(None, Some("container:quasar-control-plane"))
            .expect_err("a bad host knob must be rejected too");
        assert!(err.to_string().contains("QUASAR_CONTAINER_NETWORK"));

        // A blank host knob is "unset", not an invalid value.
        assert_eq!(resolve_network_with(None, Some("")).unwrap(), "none");

        // 5. The asymmetry (#464): `host` removes the container's network namespace, so
        //    it is reachable ONLY from this host's operator knob, never from the wire,
        //    where the value may come from a portable manifest authored elsewhere.
        assert_eq!(
            resolve_network_with(None, Some("host")).unwrap(),
            "host",
            "an operator must still be able to select host networking on their own machine"
        );
        // Even with the operator knob set to host, an app asking for host is refused: a
        // permissive host must not become a permissive wire.
        let err = resolve_network_with(Some("host"), Some("host"))
            .expect_err("`host` from the app spec must be rejected, always");
        assert!(
            err.to_string()
                .contains("removes the container's network isolation"),
            "the rejection must explain the policy and name the operator knob: {err}"
        );
        let err = resolve_network_with(Some("host"), None)
            .expect_err("`host` from the wire is never allowed");
        assert!(err.to_string().contains("QUASAR_CONTAINER_NETWORK"));
        // The app-facing message must not advertise host as an option.
        assert!(
            !err.to_string()
                .contains("expected one of none, bridge, host"),
            "the app-facing error must not list host as available: {err}"
        );
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
    fn intentional_stop_retries_exact_identity_until_cleanup_is_proven() {
        let mut container = RunningContainer {
            name: "quasar-sess-stop-retry".into(),
            container_id: "container-id".into(),
            application: ApplicationId {
                id: "container-id".into(),
                operation: "operation-stop-retry".into(),
            },
            removed: Arc::new(AtomicBool::new(false)),
            cleanup_proven: false,
            writable_sources: BTreeSet::from(["/managed/home-stop".into()]),
        };
        clear_pending_application("quasar-sess-stop-retry", "operation-stop-retry");
        let calls = std::cell::RefCell::new(Vec::new());
        for reason in ["lost remove reply", "still unavailable"] {
            assert!(container
                .stop_with(|application| {
                    calls.borrow_mut().push(application.operation.clone());
                    anyhow::bail!("{reason}")
                })
                .is_err());
            assert!(
                !container.cleanup_proven,
                "an uncertain stop cannot authorize a replacement"
            );
            assert!(
                container.removed.load(Ordering::SeqCst),
                "intentional stop must silence the observer"
            );
        }
        container
            .stop_with(|application| {
                calls.borrow_mut().push(application.operation.clone());
                Ok(())
            })
            .unwrap();
        assert!(container.cleanup_proven);
        container
            .stop_with(|_| panic!("proven cleanup must be idempotent"))
            .unwrap();
        assert_eq!(
            &*calls.borrow(),
            &[
                "operation-stop-retry",
                "operation-stop-retry",
                "operation-stop-retry"
            ]
        );
    }

    #[test]
    fn periodic_pending_recovery_retries_only_explicit_operations_and_continues_after_failure() {
        let stuck = PendingApplication {
            operation: "application-pending-stuck".into(),
            writable_sources: BTreeSet::from(["/managed/pending-stuck".into()]),
        };
        let healthy = PendingApplication {
            operation: "application-pending-healthy".into(),
            writable_sources: BTreeSet::from(["/managed/pending-healthy".into()]),
        };
        let map = Mutex::new(HashMap::new());
        let pending = &map;
        pending
            .lock()
            .unwrap()
            .insert("quasar-sess-pending-stuck".into(), stuck.clone());
        pending
            .lock()
            .unwrap()
            .insert("quasar-sess-pending-healthy".into(), healthy.clone());
        // This represents an active app absent from the caller's explicit-stop snapshot;
        // it must not be offered to abandonment at all.
        let calls = std::cell::RefCell::new(Vec::new());
        recover_pending_application_operations_in(pending, |operation| {
            calls.borrow_mut().push(operation.to_owned());
            if operation == stuck.operation {
                anyhow::bail!("daemon busy")
            }
            Ok(())
        })
        .unwrap();
        assert_eq!(calls.borrow().len(), 2);
        assert!(calls
            .borrow()
            .contains(&"application-pending-stuck".to_string()));
        assert!(calls
            .borrow()
            .contains(&"application-pending-healthy".to_string()));
        assert_eq!(
            pending.lock().unwrap().get("quasar-sess-pending-stuck"),
            Some(&stuck)
        );
        assert!(!pending
            .lock()
            .unwrap()
            .contains_key("quasar-sess-pending-healthy"));
        assert!(!pending
            .lock()
            .unwrap()
            .contains_key("quasar-sess-active-unrequested"));
    }

    #[test]
    fn uncertain_launch_blocks_same_home_until_its_exact_operation_retires() {
        let old = ApplicationRequest {
            operation: "application-old-stable-operation-home-a".into(),
            name: "quasar-sess-old-home-a".into(),
            mounts: vec!["/managed/home-a:/home/quasar:rw".into()],
            ..Default::default()
        };
        clear_pending_application(&old.name, &old.operation);
        retain_pending_application(old.name.clone(), &old, old.operation.clone());
        let replacement = ApplicationRequest {
            operation: "application-new-attempt-home-a".into(),
            name: "quasar-sess-new-home-a".into(),
            mounts: vec!["/managed/home-a/.:/home/quasar:rw".into()],
            ..Default::default()
        };
        let calls = std::cell::RefCell::new(Vec::new());
        assert!(retire_matching_pending_applications(
            &replacement.name,
            &replacement,
            |operation| {
                calls.borrow_mut().push(operation.to_owned());
                anyhow::bail!("lost cleanup reply")
            }
        )
        .is_err());
        assert_eq!(
            &*calls.borrow(),
            &["application-old-stable-operation-home-a"]
        );
        assert!(pending_application_operations()
            .lock()
            .unwrap()
            .contains_key(&old.name));
        assert!(retire_matching_pending_applications(
            &replacement.name,
            &replacement,
            |_operation| anyhow::bail!("still pending")
        )
        .is_err());
        retire_matching_pending_applications(&replacement.name, &replacement, |operation| {
            assert_eq!(operation, "application-old-stable-operation-home-a");
            Ok(())
        })
        .unwrap();
        assert!(!pending_application_operations()
            .lock()
            .unwrap()
            .contains_key(&old.name));
    }

    #[test]
    fn retiring_an_old_operation_never_erases_a_concurrent_newer_operation() {
        let old = ApplicationRequest {
            operation: "application-old-race".into(),
            name: "quasar-sess-race".into(),
            mounts: vec!["/managed/race:/home/quasar:rw".into()],
            ..Default::default()
        };
        clear_pending_application(&old.name, &old.operation);
        retain_pending_application(old.name.clone(), &old, old.operation.clone());
        let newer = PendingApplication {
            operation: "application-new-race".into(),
            writable_sources: BTreeSet::from(["/managed/race".into()]),
        };
        retire_matching_pending_applications(&old.name, &old, |_| {
            pending_application_operations()
                .lock()
                .unwrap()
                .insert(old.name.clone(), newer.clone());
            Ok(())
        })
        .unwrap();
        assert_eq!(
            pending_application_operations()
                .lock()
                .unwrap()
                .get(&old.name),
            Some(&newer)
        );
        clear_pending_application(&old.name, &newer.operation);
    }

    #[test]
    fn uncertain_launch_does_not_block_an_unrelated_writable_home() {
        let old = ApplicationRequest {
            operation: "application-old-home-unrelated".into(),
            name: "quasar-sess-old-unrelated".into(),
            mounts: vec!["/managed/home-unrelated-a:/home/quasar:rw".into()],
            ..Default::default()
        };
        clear_pending_application(&old.name, &old.operation);
        retain_pending_application(old.name.clone(), &old, old.operation.clone());
        let unrelated = ApplicationRequest {
            name: "quasar-sess-other-unrelated".into(),
            mounts: vec!["/managed/home-unrelated-b:/home/quasar:rw".into()],
            ..Default::default()
        };
        let calls = std::cell::Cell::new(0);
        retire_matching_pending_applications(&unrelated.name, &unrelated, |_| {
            calls.set(calls.get() + 1);
            Ok(())
        })
        .unwrap();
        assert_eq!(calls.get(), 0);
        clear_pending_application(&old.name, &old.operation);
    }

    #[test]
    fn writable_legacy_and_typed_volumes_share_a_pending_writer_key() {
        let old = ApplicationRequest {
            operation: "application-volume-writer".into(),
            name: "quasar-sess-volume-writer".into(),
            mounts: vec!["managed-home:/home/quasar".into()],
            ..Default::default()
        };
        let typed = ApplicationRequest {
            name: "quasar-sess-volume-next".into(),
            typed_mounts: vec![ApplicationMount::Volume {
                source: "managed-home".into(),
                target: "/home/quasar".into(),
                read_only: false,
                no_copy: false,
            }],
            ..Default::default()
        };
        clear_pending_application(&old.name, &old.operation);
        retain_pending_application(old.name.clone(), &old, old.operation.clone());
        assert!(
            retire_matching_pending_applications(&typed.name, &typed, |operation| {
                assert_eq!(operation, "application-volume-writer");
                anyhow::bail!("still pending")
            })
            .is_err()
        );
        clear_pending_application(&old.name, &old.operation);
    }

    #[test]
    fn pending_writer_lookup_normalizes_sources_and_clears_the_exact_operation() {
        let name = "quasar-rh01-pending-writer";
        let operation = "rh01-pending-writer-operation";
        clear_pending_application(name, operation);
        retain_pending_application_sources(
            name.into(),
            BTreeSet::from(["/managed/rh01-home".into()]),
            operation.into(),
        );

        assert_eq!(
            pending_writer_for_source(Path::new("/managed/rh01-home/")).unwrap(),
            Some(operation.into())
        );
        assert_eq!(
            pending_writer_for_source(Path::new("/managed/rh01-other")).unwrap(),
            None
        );

        clear_pending_application(name, operation);
        assert_eq!(
            pending_writer_for_source(Path::new("/managed/rh01-home")).unwrap(),
            None
        );
    }

    #[test]
    fn proving_a_source_teardown_retries_the_exact_pending_operation_once() {
        let name = "quasar-rh01-prove-teardown";
        let operation = "rh01-prove-teardown-operation";
        let source = Path::new("/staging/rh01-prove/scratch-home");
        clear_pending_application(name, operation);
        // An unrelated pending writer must be neither retried nor cleared by this proof.
        let unrelated = (
            "quasar-rh01-prove-unrelated",
            "rh01-prove-unrelated-operation",
        );
        retain_pending_application_sources(
            unrelated.0.into(),
            BTreeSet::from(["/managed/rh01-prove-unrelated".into()]),
            unrelated.1.into(),
        );
        assert_eq!(
            prove_source_teardown(source, |_| panic!("nothing pending")),
            Ok(())
        );

        // Retirement that still fails: the exact operation is retried, remains pending,
        // and the caller learns which operation is unresolved.
        retain_pending_application_sources(
            name.into(),
            BTreeSet::from([source.to_string_lossy().into_owned()]),
            operation.into(),
        );
        let mut retired = Vec::new();
        let unresolved = prove_source_teardown(source, |op| {
            retired.push(op.to_owned());
            Err(anyhow!("reply lost"))
        })
        .unwrap_err();
        assert!(unresolved.contains(operation), "{unresolved}");
        assert!(!retired.is_empty());
        assert!(retired.iter().all(|op| op == operation), "{retired:?}");
        assert_eq!(
            pending_writer_for_source(source).unwrap(),
            Some(operation.into())
        );

        // Retirement that succeeds on the retry: proven, and the entry is gone.
        assert_eq!(prove_source_teardown(source, |_| Ok(())), Ok(()));
        assert_eq!(pending_writer_for_source(source).unwrap(), None);
        assert_eq!(
            pending_writer_for_source(Path::new("/managed/rh01-prove-unrelated")).unwrap(),
            Some(unrelated.1.into()),
            "unrelated pending writers are untouched"
        );
        clear_pending_application(unrelated.0, unrelated.1);
    }

    #[test]
    fn readonly_volume_aliases_do_not_block_a_writable_launch() {
        for mount in [
            "managed-readonly:/home/quasar:ro",
            "managed-readonly:/home/quasar:readonly",
        ] {
            let request = ApplicationRequest {
                mounts: vec![mount.into()],
                ..Default::default()
            };
            assert!(writable_application_sources(&request).is_empty(), "{mount}");
        }
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
        let rt = ContainerRuntime::new(true);
        assert!(nvidia_driver_volume_gate(&rt, "quasar-steam:latest")
            .unwrap()
            .is_none());
        assert!(rt
            .app_gpu_access_live()
            .session_args("", &[])
            .iter()
            .all(|a| !a.starts_with("type=")));
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
