//! Wolf-style NVIDIA driver-volume auto-provisioner (first-run-experience spec S1).
//!
//! A CUDA-only NVIDIA install (nvidia-smi/CUDA/NVENC fine, no EGL/GL userspace, no
//! 32-bit GL) panics the session compositor on display creation and kills 32-bit Steam
//! before its first frame. This downloads the `.run` installer matching the LOADED
//! kernel module, extracts the userspace half of it into a named Docker volume, and
//! points the agent's and the app containers' loader/EGL/Vulkan discovery there.
//!
//! Constraints:
//! - Never a host installer: nothing is written outside the volume, and the installer
//!   runs only with `--extract-only`.
//! - Never a precedence override: a host whose graphics driver is CDI-injected has no
//!   gap, so the module no-ops.
//! - Never a gate: every failure degrades to the readiness card's manual remediation;
//!   nothing blocks registration or a session launch.
//!
//! The extract runs as a CHILD PROCESS of the agent, not a helper container: the agent
//! image already ships [`REQUIRED_TOOLS`] and already mounts the volume, so this needs
//! no second image to be pullable on a locked-down host and no docker-socket hop, and
//! the child's stdout/stderr is relayed into the agent's tracing stream.

use std::collections::BTreeMap;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, OnceLock, RwLock};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, bail, Context, Result};

use crate::artifact;

/// Tracing target for every line this module emits.
const T: &str = "quasar.nvidia_volume";

/// This provisioner's name in the shared [`artifact`] machinery: its download, lock and
/// preflight lines land on `quasar.artifact` carrying `artifact="nvidia-driver"`.
const ARTIFACT: &str = "nvidia-driver";

/// Where compose mounts the volume inside the agent. The app-container wiring uses the
/// same path, so a path in a log line means the same thing everywhere.
pub const VOLUME_MOUNT: &str = "/opt/quasar/nvidia-driver";

/// Pinned download host; every redirect is re-validated against it. The `.run` is an
/// executable payload with no signature to check, so the origin is the only control.
pub const DOWNLOAD_HOST: &str = "download.nvidia.com";

/// Checked before anything is downloaded, so a 350 MB fetch is not lost to `xz: not found`.
const REQUIRED_TOOLS: &[&str] = &["sh", "tar", "xz", "ldconfig"];

/// Bounded so a black-holed connection cannot hold the lock forever.
const DOWNLOAD_TIMEOUT: Duration = Duration::from_secs(30 * 60);

/// The `.run` (~350 MB) + extracted tree (~1.5 GB) + copied libraries all land inside
/// the volume, i.e. the docker data root — on unraid a size-capped `docker.img` shared
/// with postgres and the control plane. Filling it wedges the stack, so short space is
/// a refusal, not a warning.
const REQUIRED_FREE_BYTES: u64 = 3 * 1024 * 1024 * 1024;

/// Backoff for FAILED provisions: `BASE * 2^(failures-1)`, capped. Every process start
/// re-attempts while the gap persists, so without this an agent crash-looping for an
/// unrelated reason is a repeated 350 MB download against `download.nvidia.com`.
const PROVISION_BACKOFF_BASE: Duration = Duration::from_secs(5 * 60);
const PROVISION_BACKOFF_MAX: Duration = Duration::from_secs(6 * 60 * 60);

// ── layout ───────────────────────────────────────────────────────────────────

/// Sub-paths inside the volume. One place: the agent's env injection and the
/// app-container wiring must agree byte-for-byte.
pub mod layout {
    pub const MANIFEST: &str = "manifest.json";
    pub const LOCK: &str = ".provision.lock";
    pub const LIB64: &str = "lib64";
    pub const LIB32: &str = "lib32";
    pub const EGL_VENDOR_DIR: &str = "glvnd/egl_vendor.d";
    pub const EGL_VENDOR_JSON: &str = "glvnd/egl_vendor.d/10_nvidia.json";
    pub const EGL_EXTERNAL_DIR: &str = "egl_external_platform.d";
    pub const VULKAN_ICD_DIR: &str = "vulkan/icd.d";
    pub const VULKAN_ICD_JSON: &str = "vulkan/icd.d/nvidia_icd.json";
    pub const GBM_DIR: &str = "gbm";
    pub const LD_CONF: &str = "ld.so.conf.d/quasar-nvidia.conf";
    pub const SCRATCH: &str = "scratch";
    /// TOFU digest pins, `{"<driver version>": "<sha256>"}`. Not the manifest: that is
    /// rewritten per provision and describes only the current version, so it could
    /// never catch "the same version now hashes differently".
    pub const DIGESTS: &str = "driver-digests.json";
    pub const ATTEMPTS: &str = ".provision-attempts.json";
}

// ── manifest ─────────────────────────────────────────────────────────────────

/// What is in the volume, written LAST: presence of `manifest.json` is the readiness
/// signal for every consumer, so a half-populated volume is never mistaken for a good one.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct Manifest {
    /// The kernel module version this userspace matches. The two MUST agree exactly —
    /// a mismatch is "Failed to initialize NVML: Driver/library version mismatch".
    pub driver_version: String,
    /// sha256 of the fetched `.run`; also the TOFU pin's subject.
    pub sha256: String,
    pub url: String,
    pub provisioned_at_unix: u64,
    pub agent_version: String,
    pub lib64_count: usize,
    pub lib32_count: usize,
    /// Which population rules built this volume. Bump when a defect makes an
    /// already-written volume wrong rather than merely old, so the agent re-provisions
    /// by itself. Absent (`0`) means a pre-versioning build whose volume bricks EGL.
    #[serde(default)]
    pub layout_version: u32,
}

/// Population-rule generation. `0` copied every `*.so*` including the glvnd dispatch
/// layer, which shadowed the image's libglvnd and stripped `EGL_EXT_device_enumeration`
/// (see [`VENDOR_NEUTRAL_LIB_BASES`]); `1` is vendor libraries only; `2` copies the
/// installer's own `nvidia_icd.json` instead of synthesizing one.
pub const CURRENT_LAYOUT_VERSION: u32 = 2;

/// What the volume currently holds relative to the loaded kernel module.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum VolumeState {
    /// No manifest: never provisioned, or an attempt aborted before the manifest write.
    Empty,
    /// Manifest matches the loaded kernel module: usable as-is.
    Current(Manifest),
    /// The host's driver was upgraded under the volume. Must be re-provisioned; a
    /// userspace/module mismatch is worse than no volume.
    Stale {
        have: String,
        want: String,
        manifest: Manifest,
    },
    /// Right driver version, wrong population rules. Must be re-provisioned: an
    /// older-generation volume is harmful, not merely incomplete.
    ObsoleteLayout { have: u32, want: u32 },
    /// Manifest present but unparseable: a half-written volume, treated like `Empty`.
    Unreadable(String),
}

impl VolumeState {
    /// The manifest to trust, or `None` when the volume must not be consumed.
    pub fn usable(&self) -> Option<&Manifest> {
        match self {
            VolumeState::Current(m) => Some(m),
            _ => None,
        }
    }

    pub fn needs_provision(&self) -> bool {
        !matches!(self, VolumeState::Current(_))
    }
}

/// Read the volume's manifest and compare it against the loaded kernel module.
pub fn volume_state(volume: &Path, kernel_version: &str) -> VolumeState {
    let path = volume.join(layout::MANIFEST);
    let body = match std::fs::read_to_string(&path) {
        Ok(b) => b,
        Err(_) => return VolumeState::Empty,
    };
    let manifest: Manifest = match serde_json::from_str(&body) {
        Ok(m) => m,
        Err(e) => return VolumeState::Unreadable(e.to_string()),
    };
    if manifest.driver_version != kernel_version {
        return VolumeState::Stale {
            have: manifest.driver_version.clone(),
            want: kernel_version.to_string(),
            manifest,
        };
    }
    if manifest.layout_version != CURRENT_LAYOUT_VERSION {
        return VolumeState::ObsoleteLayout {
            have: manifest.layout_version,
            want: CURRENT_LAYOUT_VERSION,
        };
    }
    VolumeState::Current(manifest)
}

// ── driver version ───────────────────────────────────────────────────────────

/// The loaded kernel module's version, from `/sys/module/nvidia/version`. Not
/// `nvidia-smi`: sysfs needs no binary and no CUDA init, and it reports the kernel
/// module, which is the thing the userspace must match.
pub fn kernel_driver_version(root: &Path) -> Option<String> {
    let raw = std::fs::read_to_string(root.join("sys/module/nvidia/version")).ok()?;
    parse_driver_version(&raw)
}

/// Validate + normalise a driver version. It is interpolated into a URL and into
/// filesystem paths, so anything that is not digits-and-dots is rejected, not sanitised.
pub fn parse_driver_version(raw: &str) -> Option<String> {
    let v = raw.trim();
    if v.is_empty() || v.len() > 32 {
        return None;
    }
    if !v.chars().all(|c| c.is_ascii_digit() || c == '.') {
        return None;
    }
    // At least `NNN.NN`; reject a bare number or a leading/trailing dot.
    if !v.contains('.') || v.starts_with('.') || v.ends_with('.') || v.contains("..") {
        return None;
    }
    Some(v.to_string())
}

/// The pinned-host download URL for a version.
pub fn run_url(version: &str) -> String {
    format!(
        "https://{DOWNLOAD_HOST}/XFree86/Linux-x86_64/{version}/NVIDIA-Linux-x86_64-{version}.run"
    )
}

// ── process-global handle ────────────────────────────────────────────────────

/// Everything a consumer needs to wire the volume up.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VolumeInfo {
    /// Path inside the AGENT container.
    pub local: PathBuf,
    /// The volume's HOST path, from the agent's own mounts. `None` ⇒ app-container
    /// injection is skipped; the agent still uses the volume itself.
    pub host: Option<PathBuf>,
    /// Docker volume name, for logging.
    pub name: Option<String>,
    pub manifest: Manifest,
}

/// Live provisioning status, surfaced on the readiness card.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub enum Status {
    /// Not needed or not attempted (host driver fine, non-NVIDIA, or opted out).
    #[default]
    Idle,
    Provisioning {
        phase: String,
        percent: Option<u64>,
    },
    Provisioned(Manifest),
    /// Terminal failure. The readiness card shows this error alongside the manual
    /// remediation text.
    Failed(String),
}

static STATUS: RwLock<Option<Status>> = RwLock::new(None);
static CURRENT: RwLock<Option<VolumeInfo>> = RwLock::new(None);

pub fn status() -> Status {
    STATUS
        .read()
        .ok()
        .and_then(|g| g.clone())
        .unwrap_or_default()
}

fn set_status(s: Status) {
    if let Ok(mut g) = STATUS.write() {
        *g = Some(s);
    }
}

/// The provisioned volume, or `None` when there is nothing to consume. Process-global
/// rather than threaded through `SessionConfig`: the value is per-process by
/// construction, and a threaded copy would be a stale snapshot in every in-flight
/// assignment. Read by the agent's env injection and by `session::container`.
pub fn current() -> Option<VolumeInfo> {
    CURRENT.read().ok().and_then(|g| g.clone())
}

fn set_current(info: Option<VolumeInfo>) {
    if let Ok(mut g) = CURRENT.write() {
        *g = info;
    }
}

/// Retry a transient Docker inspection failure without restarting or re-downloading.
pub fn retry_mount_resolution(docker: &str) {
    let Some(info) = current() else {
        return;
    };
    if info.host.is_some() || info.name.is_some() {
        return;
    }
    let (host, name) = locate_host_path(docker);
    if host.is_none() && name.is_none() {
        return;
    }
    if let Ok(mut state) = CURRENT.write() {
        if let Some(current) = state.as_mut() {
            if current.manifest.driver_version == info.manifest.driver_version {
                current.host = host;
                current.name = name;
            }
        }
    }
}

// ── opt-out ──────────────────────────────────────────────────────────────────

/// `QUASAR_NVIDIA_DRIVER_VOLUME`. Defaults on; `0` is the opt-out for a host that must
/// not fetch ~350 MB from the public internet unprompted, or that mirrors drivers.
pub fn enabled() -> bool {
    !matches!(
        std::env::var("QUASAR_NVIDIA_DRIVER_VOLUME").as_deref(),
        Ok("0") | Ok("false") | Ok("no")
    )
}

// ── locating the volume ──────────────────────────────────────────────────────

/// Resolve the volume's HOST path and name from the agent's OWN container mounts.
///
/// Not `docker volume inspect <name>`: compose names volumes `<project>_<key>` and the
/// project name varies per worktree, so the agent cannot know it a priori. A miss is
/// also the exact signal that the compose overlay was never applied.
pub fn locate_host_path(docker: &str) -> (Option<PathBuf>, Option<String>) {
    let Some(id) = self_container_id() else {
        tracing::debug!(target: T, "could not determine own container id; driver mount resolution is unavailable");
        return (None, None);
    };
    let Some(out) = crate::readiness::run_with_timeout(
        docker,
        &["inspect", "--format", "{{json .Mounts}}", &id],
    ) else {
        return (None, None);
    };
    let Ok(mounts) = serde_json::from_str::<Vec<serde_json::Value>>(&out) else {
        return (None, None);
    };
    for mount in mounts {
        if mount["Destination"].as_str() != Some(VOLUME_MOUNT) {
            continue;
        }
        let host = mount["Source"]
            .as_str()
            .filter(|src| src.starts_with('/'))
            .map(PathBuf::from);
        let name = if mount["Type"].as_str() == Some("volume") {
            mount["Name"]
                .as_str()
                .filter(|name| !name.is_empty())
                .map(str::to_owned)
        } else {
            None
        };
        return (host, name);
    }
    (None, None)
}

/// Our own container id, from `/proc/self/mountinfo` with `$HOSTNAME` as fallback. Both
/// best-effort; a miss costs only app-container injection, never the agent's own use.
pub fn self_container_id() -> Option<String> {
    if let Ok(body) = std::fs::read_to_string("/proc/self/mountinfo") {
        if let Some(id) = parse_container_id_from_mountinfo(&body) {
            return Some(id);
        }
    }
    std::env::var("HOSTNAME")
        .ok()
        .filter(|h| hostname_is_container_id(h))
}

/// Whether `$HOSTNAME` may stand in for the container id. A compose stack that
/// sets `hostname:` makes it a DNS name (`quasar-dev.local`), which docker
/// answers "No such object" for — so the shape is checked, never assumed.
pub fn hostname_is_container_id(hostname: &str) -> bool {
    hostname.len() >= 12 && hostname.chars().all(|c| c.is_ascii_hexdigit())
}

/// Pull the 64-hex container id out of a mountinfo body.
pub fn parse_container_id_from_mountinfo(body: &str) -> Option<String> {
    // Overlay lowerdir digests are also 64 hex characters. Only Docker's
    // per-container identity-file mounts identify THIS container.
    for line in body.lines() {
        let fields: Vec<_> = line.split_whitespace().collect();
        if fields.len() < 6
            || !matches!(
                fields[4],
                "/etc/hosts" | "/etc/hostname" | "/etc/resolv.conf"
            )
        {
            continue;
        }
        let parts: Vec<_> = fields[3].split('/').collect();
        for pair in parts.windows(2) {
            if pair[0] == "containers"
                && pair[1].len() == 64
                && pair[1].chars().all(|c| c.is_ascii_hexdigit())
            {
                return Some(pair[1].to_owned());
            }
        }
    }
    None
}

// ── entry point ──────────────────────────────────────────────────────────────

/// The outcome the caller acts on.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Outcome {
    /// Opted out, non-NVIDIA, host driver present, or the volume is not mounted.
    NotNeeded(&'static str),
    AlreadyCurrent(Manifest),
    /// `restart_required` when the gap included the agent's own EGL/GL stack, which
    /// only re-initialises in a fresh process.
    Provisioned {
        manifest: Manifest,
        restart_required: bool,
    },
    /// The readiness card falls back to the manual remediation text.
    Failed(String),
}

/// Which gaps the readiness probe found: the provisioning trigger, and what decides
/// whether a self-restart follows.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Gap {
    /// EGL vendor json and/or `libnvidia-eglcore` missing: the compositor cannot start.
    /// Fixing it needs a fresh process — the loader latches `LD_LIBRARY_PATH` at exec.
    pub egl: bool,
    /// 32-bit GL missing: native Steam cannot render. Wired per SESSION at container
    /// launch, so no restart.
    pub lib32: bool,
}

impl Gap {
    pub fn any(&self) -> bool {
        self.egl || self.lib32
    }
}

/// Decide whether to provision, with no I/O beyond the manifest the caller read.
pub fn decide(
    nvidia_present: bool,
    gap: Gap,
    opted_out: bool,
    volume_mounted: bool,
    state: &VolumeState,
) -> Result<(), &'static str> {
    if !nvidia_present {
        return Err("no NVIDIA GPU on this host");
    }
    if opted_out {
        return Err("QUASAR_NVIDIA_DRIVER_VOLUME=0 (auto-provisioning disabled by the operator)");
    }
    if !volume_mounted {
        return Err(
            "driver volume is not mounted into the agent (apply deploy/docker-compose.nvidia.yml)",
        );
    }
    // A host with a working graphics driver never provisions, even with an empty
    // volume: CDI injection always wins (spec S1 precedence).
    if !gap.any() && !state.needs_provision() {
        return Err("host graphics driver is present and the volume is current");
    }
    if !gap.any() {
        return Err("host graphics driver is present (CDI injection wins)");
    }
    Ok(())
}

/// Adopt an already-provisioned volume at process start: one file read, one `docker
/// inspect` on a hit, never a download.
///
/// MUST run before `gst::init` — the fresh process has to set its EGL/Vulkan discovery
/// env before anything touches EGL. Also the steady-state path on every later boot.
pub fn adopt_current(docker: &str) -> Option<Manifest> {
    let volume = PathBuf::from(VOLUME_MOUNT);
    if !volume.is_dir() {
        return None;
    }
    let kernel_version = kernel_driver_version(Path::new("/"))?;
    match volume_state(&volume, &kernel_version) {
        VolumeState::Current(m) => {
            tracing::info!(
                target: T,
                version = %m.driver_version,
                sha256 = %m.sha256,
                lib64 = m.lib64_count,
                lib32 = m.lib32_count,
                "adopting the Quasar-provisioned NVIDIA driver volume for this process"
            );
            publish(&volume, m.clone(), docker);
            set_status(Status::Provisioned(m.clone()));
            Some(m)
        }
        VolumeState::Stale { have, want, .. } => {
            tracing::warn!(
                target: T, token = "drvvol-version-mismatch",
                have = %have, want = %want,
                "the driver volume was built for a DIFFERENT driver version than the loaded kernel \
                 module — NOT adopting it (a userspace/module mismatch fails worse than a missing \
                 userspace); it will be re-provisioned if the readiness probe reports a gap"
            );
            None
        }
        VolumeState::ObsoleteLayout { have, want } => {
            tracing::warn!(
                target: T, token = "drvvol-generation-not-adopted",
                have, want,
                "NOT adopting a driver volume built by population-rule generation {have} (current \
                 {want}) — it carries the installer's glvnd dispatch libraries, which break EGL \
                 when they shadow the image's. It will be re-provisioned."
            );
            None
        }
        _ => None,
    }
}

/// Run the provisioner. Blocking — call it on a dedicated thread.
pub fn provision_blocking(nvidia_present: bool, gap: Gap, docker: &str) -> Outcome {
    let volume = PathBuf::from(VOLUME_MOUNT);
    let volume_mounted = volume.is_dir();
    if nvidia_present && gap.any() && enabled() {
        let (host, name) = locate_host_path(docker);
        if host.is_none() && name.is_none() {
            let error = "Cannot verify a persistent NVIDIA driver mount through Docker. Check the agent's Docker socket, container identity and /opt/quasar/nvidia-driver mount; provisioning will retry automatically.".to_string();
            set_status(Status::Failed(error.clone()));
            return Outcome::Failed(error);
        }
    }
    let kernel_version = match kernel_driver_version(Path::new("/")) {
        Some(v) => v,
        None => {
            // No loaded module ⇒ nothing to match. Never a failure: the readiness
            // card's manual remediation covers it.
            tracing::debug!(target: T, "no /sys/module/nvidia/version — not an NVIDIA host, or the module is not loaded");
            return Outcome::NotNeeded("NVIDIA kernel module version unavailable");
        }
    };
    let state = volume_state(&volume, &kernel_version);

    if let Err(reason) = decide(nvidia_present, gap, !enabled(), volume_mounted, &state) {
        tracing::debug!(target: T, "driver-volume provisioning not needed: {reason}");
        // Not provisioning still has to publish a CURRENT volume, so the agent and app
        // containers consume what a previous run created.
        publish_if_current(&volume, &state, docker);
        return Outcome::NotNeeded(reason);
    }

    if let VolumeState::Current(m) = &state {
        tracing::info!(
            target: T,
            version = %m.driver_version,
            "driver volume already provisioned for the loaded kernel module — reusing it"
        );
        publish_if_current(&volume, &state, docker);
        return Outcome::AlreadyCurrent(m.clone());
    }

    match &state {
        VolumeState::Stale { have, want, .. } => tracing::warn!(
            target: T, token = "drvvol-stale",
            have = %have, want = %want,
            "driver volume is STALE (host driver was upgraded under it) — re-provisioning; \
             a userspace/kernel-module version mismatch is worse than no volume at all"
        ),
        VolumeState::Unreadable(e) => tracing::warn!(
            target: T, token = "drvvol-manifest-unreadable",
            error = %e,
            "driver volume manifest is unreadable (half-written by an interrupted run) — re-provisioning"
        ),
        VolumeState::ObsoleteLayout { have, want } => tracing::warn!(
            target: T, token = "drvvol-generation-reprovision",
            have, want,
            "driver volume was built by population-rule generation {have}, current is {want} — \
             re-provisioning. Generation 0 copied the installer's vendor-neutral glvnd dispatch \
             libraries into the volume, which shadowed the image's libglvnd and stripped \
             EGL_EXT_device_enumeration from the client extension string (the compositor then \
             panicked at 'Failed to enumerate EGLDevices')."
        ),
        _ => {}
    }

    tracing::info!(
        target: T,
        version = %kernel_version,
        egl_gap = gap.egl, lib32_gap = gap.lib32, volume = %volume.display(),
        "NVIDIA graphics userspace is missing on this host — auto-provisioning the driver volume \
         (Wolf-style: download + extract-only, NOTHING is installed on the host)"
    );

    match run_provision(&volume, &kernel_version, gap) {
        Ok(manifest) => {
            tracing::info!(
                target: T,
                version = %manifest.driver_version,
                sha256 = %manifest.sha256,
                lib64 = manifest.lib64_count,
                lib32 = manifest.lib32_count,
                "driver volume provisioned successfully"
            );
            set_status(Status::Provisioned(manifest.clone()));
            publish(&volume, manifest.clone(), docker);
            Outcome::Provisioned {
                restart_required: gap.egl,
                manifest,
            }
        }
        Err(e) => {
            let msg = format!("{e:#}");
            note_failure(&volume, &msg);
            tracing::error!(
                target: T, token = "drvvol-provision-failed",
                error = %msg,
                "driver-volume provisioning FAILED — falling back to the manual remediation shown \
                 on the host readiness card"
            );
            set_status(Status::Failed(msg.clone()));
            Outcome::Failed(msg)
        }
    }
}

fn publish_if_current(volume: &Path, state: &VolumeState, docker: &str) {
    if let Some(m) = state.usable() {
        publish(volume, m.clone(), docker);
        set_status(Status::Provisioned(m.clone()));
    }
}

fn publish(volume: &Path, manifest: Manifest, docker: &str) {
    let (host, name) = locate_host_path(docker);
    match &host {
        Some(p) => tracing::info!(
            target: T,
            volume = name.as_deref().unwrap_or("<unnamed>"),
            host_path = %p.display(),
            "driver volume resolved for app-container injection"
        ),
        None => tracing::warn!(
            target: T, token = "drvvol-host-path-unresolved",
            "the driver volume's HOST path could not be resolved from this container's mounts — \
             the agent will still use the volume, but app containers will NOT receive the driver \
             libraries. Check Docker socket access and the agent's container identity/mount inspection; \
             app launches are blocked until the driver mount can be resolved."
        ),
    }
    set_current(Some(VolumeInfo {
        local: volume.to_path_buf(),
        host,
        name,
        manifest,
    }));
}

// ── installer integrity ──────────────────────────────────────────────────────

/// Reviewed sha256 per driver version, compiled into the agent. The same discipline as
/// [`crate::cuda_runtime::NVRTC_SHA256`], with the one difference NVIDIA forces: there is
/// no published digest or signature for a `.run`, so each entry is a digest a human
/// computed from a payload they vouched for and committed here.
///
/// Adding one: fetch the `.run` from [`DOWNLOAD_HOST`], `sha256sum` it, cross-check the
/// value against an independent record of the same file, then add the pair here.
/// `docs/third-party-pins.md` §"NVIDIA driver installer digests" carries the procedure.
pub const REVIEWED_DRIVER_DIGESTS: &[(&str, &str)] = &[];

/// The reviewed digest for a driver version, if one has been staged into this agent.
pub fn reviewed_digest(version: &str) -> Option<&'static str> {
    REVIEWED_DRIVER_DIGESTS
        .iter()
        .find(|(v, _)| *v == version)
        .map(|(_, d)| *d)
}

/// `QUASAR_NVIDIA_DRIVER_TRUST_ON_FIRST_USE`. ON by default (operator decision,
/// 2026-08-28): NVIDIA publishes no digest to review against, so shipping this off would
/// refuse first provision on exactly the CUDA-only hosts the driver volume exists to
/// rescue. The first fetch of a version therefore pins whatever the network returned —
/// unverified, logged at WARN — and every later fetch must match it. Set to `0` to refuse
/// instead, which is the right posture once REVIEWED_DRIVER_DIGESTS covers your drivers.
pub fn trust_on_first_use() -> bool {
    !matches!(
        std::env::var("QUASAR_NVIDIA_DRIVER_TRUST_ON_FIRST_USE").as_deref(),
        Ok("0") | Ok("false") | Ok("no")
    )
}

/// `QUASAR_NVIDIA_DRIVER_RUN`, the air-gapped hatch: an operator-staged `.run` to use
/// instead of downloading. A SOURCE, never a destination.
pub const STAGED_RUN_VAR: &str = "QUASAR_NVIDIA_DRIVER_RUN";

pub fn staged_run() -> Option<PathBuf> {
    let v = std::env::var(STAGED_RUN_VAR).ok()?;
    let v = v.trim();
    (!v.is_empty()).then(|| PathBuf::from(v))
}

/// Per-host digest pins: driver version → the sha256 this host accepted for it. A
/// second line of defence behind [`REVIEWED_DRIVER_DIGESTS`], and the only one for a
/// version staged by the operator or accepted under trust-on-first-use. The pins live in
/// the volume, so deleting the volume also resets them.
pub type DigestPins = BTreeMap<String, String>;

/// Read the per-host pins. A malformed file is an ERROR, never an empty map: degrading a
/// corrupt pin file to "no pins" is how an integrity check silently stops checking, and
/// the payload it guards is a shell script run with the agent's privileges.
pub fn read_digest_pins(volume: &Path) -> Result<DigestPins> {
    let path = volume.join(layout::DIGESTS);
    let body = match std::fs::read_to_string(&path) {
        Ok(b) => b,
        // Absent is the honest "nothing pinned here yet", unlike unparseable.
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(DigestPins::new()),
        Err(e) => return Err(anyhow!("cannot read driver digest pins {}: {e}; refusing to provision without the existing trust record", path.display())),
    };
    serde_json::from_str(&body).map_err(|e| {
        anyhow!(
            "REFUSING to provision: the driver digest-pin file {} is unreadable ({e}), so this \
             host cannot tell whether the installer it is about to execute is the one it \
             accepted before. Nothing has been downloaded or executed. Inspect the file; if it \
             is genuinely corrupt, delete it (this host then re-pins from the next payload it \
             accepts) or delete the whole driver volume.",
            path.display()
        )
    })
}

/// Why an installer payload was accepted. Recorded so a log line says which control
/// actually held.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Trust {
    /// Matched [`REVIEWED_DRIVER_DIGESTS`].
    Reviewed,
    /// Matched the pin this host recorded earlier.
    HostPinned,
    /// No pin of either kind; the operator opted in via
    /// `QUASAR_NVIDIA_DRIVER_TRUST_ON_FIRST_USE`.
    TrustOnFirstUse,
    /// The operator staged the file themselves ([`STAGED_RUN_VAR`]) and no reviewed
    /// digest exists for the version, so the file is theirs to vouch for.
    OperatorStaged,
}

/// Decide whether a payload may be executed. Pure, so the whole verdict table is
/// unit-testable; the caller must run it BEFORE `sh` ever sees the file.
///
/// Fail-closed: an unpinned version with no operator opt-in is a refusal, not a new pin.
pub fn check_installer_digest(
    reviewed: Option<&str>,
    host_pins: &DigestPins,
    version: &str,
    sha256: &str,
    tofu_allowed: bool,
    operator_staged: bool,
) -> Result<Trust> {
    if let Some(pinned) = reviewed {
        if pinned == sha256 {
            return Ok(Trust::Reviewed);
        }
        bail!(
            "REFUSING to execute the NVIDIA installer for driver {version}: its sha256 does not \
             match the reviewed digest compiled into this agent.\n  reviewed: {pinned}\n  \
             got:      {sha256}\nA published .run for a released driver version is immutable, so \
             this is a corrupted, substituted or mis-staged payload — and the payload is a shell \
             script that would run with this agent's privileges. Nothing has been executed."
        );
    }
    if let Some(pinned) = host_pins.get(version) {
        if pinned == sha256 {
            return Ok(Trust::HostPinned);
        }
        bail!(
            "REFUSING to execute the NVIDIA installer for driver {version}: its sha256 has CHANGED \
             since this host first accepted that exact version.\n  pinned (first seen): {pinned}\n \
             seen now:            {sha256}\nA published .run for a released driver version is \
             immutable, so this is either a corrupted download or a substituted payload — and the \
             payload is a shell script that would run with this agent's privileges. Nothing has \
             been executed. If you are certain the new file is genuine, remove {} from the driver \
             volume to re-pin, or delete the volume entirely.",
            layout::DIGESTS
        );
    }
    if operator_staged {
        return Ok(Trust::OperatorStaged);
    }
    if tofu_allowed {
        return Ok(Trust::TrustOnFirstUse);
    }
    bail!(
        "REFUSING to execute the NVIDIA installer for driver {version}: this agent carries no \
         reviewed sha256 for that version, and this host has no pin for it either, so the only \
         thing vouching for the payload would be the payload itself. It is a shell script that \
         would run with this agent's privileges (host networking, NET_ADMIN, devices, the docker \
         socket). Nothing has been downloaded past the hash, and nothing has been executed.\n\
         Downloaded sha256: {sha256}\nPick one:\n  1. Stage the installer yourself: fetch \
         NVIDIA-Linux-x86_64-{version}.run, verify it against NVIDIA's own record, put it \
         somewhere the agent container can read, and set {STAGED_RUN_VAR}=<path>.\n  2. Add the \
         reviewed digest to REVIEWED_DRIVER_DIGESTS in node-agent/src/nvidia_volume.rs and ship a \
         new agent (see docs/third-party-pins.md).\n  3. Accept the fetched payload on this host \
         only: QUASAR_NVIDIA_DRIVER_TRUST_ON_FIRST_USE=1 (this host then refuses any later change \
         for the same version).\n  4. Skip auto-provisioning entirely and install the driver's \
         graphics userspace from your distribution's packages: QUASAR_NVIDIA_DRIVER_VOLUME=0."
    )
}

fn log_trust(trust: Trust, version: &str, sha256: &str) {
    match trust {
        Trust::Reviewed => tracing::info!(
            target: T, version = %version, sha256 = %sha256,
            "installer matches the reviewed digest compiled into this agent"
        ),
        Trust::HostPinned => tracing::info!(
            target: T, version = %version, sha256 = %sha256,
            "installer matches the digest this host pinned for this driver version"
        ),
        Trust::OperatorStaged => tracing::warn!(
            target: T, token = "drvvol-operator-staged-installer",
            version = %version, sha256 = %sha256,
            "executing an operator-staged installer ({STAGED_RUN_VAR}) with no reviewed digest \
             for driver {version} — its integrity is the operator's to vouch for. This host now \
             pins {sha256} and will refuse any later change for the same version."
        ),
        Trust::TrustOnFirstUse => tracing::warn!(
            target: T, token = "drvvol-trust-on-first-use",
            version = %version, sha256 = %sha256,
            "no reviewed digest for driver {version}; executing the downloaded installer because \
             QUASAR_NVIDIA_DRIVER_TRUST_ON_FIRST_USE is set. TLS and the pinned origin are the \
             only controls on this payload. This host now pins {sha256} and will refuse any later \
             change for the same version."
        ),
    }
}

/// Copy an operator-staged `.run` into scratch and hash it. Copied rather than executed
/// in place so the staged file cannot be swapped between the hash and the `sh`, and so
/// the extract writes only inside the volume.
fn stage_installer(src: &Path, scratch: &Path, version: &str) -> Result<String> {
    if !src.is_file() {
        bail!(
            "{STAGED_RUN_VAR}={} is not a readable file — nothing staged to provision driver \
             {version} from",
            src.display()
        );
    }
    let dest = scratch.join(format!("NVIDIA-Linux-x86_64-{version}.run"));
    std::fs::copy(src, &dest)
        .with_context(|| format!("copy {} into {}", src.display(), scratch.display()))?;
    let sha = sha256_file(&dest)?;
    tracing::info!(
        target: T, staged = %src.display(), sha256 = %sha,
        "using the operator-staged NVIDIA installer ({STAGED_RUN_VAR}) — no download"
    );
    Ok(sha)
}

fn sha256_file(path: &Path) -> Result<String> {
    use sha2::{Digest, Sha256};
    let mut f = std::fs::File::open(path).with_context(|| format!("open {}", path.display()))?;
    let mut hasher = Sha256::new();
    let mut buf = vec![0u8; 256 * 1024];
    loop {
        let n = f
            .read(&mut buf)
            .with_context(|| format!("read {}", path.display()))?;
        if n == 0 {
            break;
        }
        hasher.update(&buf[..n]);
    }
    Ok(artifact::hex(&hasher.finalize()))
}

fn write_digest_pin(volume: &Path, version: &str, sha256: &str) -> Result<()> {
    let mut pins = read_digest_pins(volume)?;
    if pins
        .insert(version.to_string(), sha256.to_string())
        .is_none()
    {
        tracing::info!(
            target: T,
            version = %version, sha256 = %sha256,
            "pinning this sha256 for driver {version} on this host — a later provision of the \
             SAME version with a different digest will be refused"
        );
    }
    let path = volume.join(layout::DIGESTS);
    std::fs::write(&path, serde_json::to_vec_pretty(&pins)?)
        .with_context(|| format!("write {}", path.display()))
}

// ── failure backoff (#477) ───────────────────────────────────────────────────

/// Persisted attempt bookkeeping, keyed on the driver version. Written BEFORE the
/// download, so a process that dies mid-provision still counts: a crash loop that never
/// reaches the failure path would otherwise be unlimited.
pub type Attempts = artifact::Attempts;

fn now_unix() -> u64 {
    artifact::now_unix()
}

fn attempts_path(volume: &Path) -> PathBuf {
    volume.join(layout::ATTEMPTS)
}

pub fn read_attempts(volume: &Path) -> Attempts {
    artifact::read_attempts(&attempts_path(volume))
}

/// How long before re-attempting, or `None` to go now. A driver-version change resets
/// the counter: the previous version's failures say nothing about the new one.
pub fn backoff_remaining(a: &Attempts, version: &str, now_unix: u64) -> Option<Duration> {
    artifact::backoff_remaining(
        a,
        version,
        now_unix,
        PROVISION_BACKOFF_BASE,
        PROVISION_BACKOFF_MAX,
    )
}

fn note_attempt(volume: &Path, version: &str) {
    artifact::note_attempt(&attempts_path(volume), version)
}

fn note_failure(volume: &Path, error: &str) {
    artifact::note_failure(&attempts_path(volume), error)
}

fn clear_attempts(volume: &Path) {
    artifact::clear_attempts(&attempts_path(volume))
}

// ── disk preflight (#477) ────────────────────────────────────────────────────

/// Bytes available to an unprivileged writer under `path`.
pub fn free_bytes(path: &Path) -> Option<u64> {
    artifact::free_bytes(path)
}

fn check_free_space(volume: &Path) -> Result<()> {
    let Some(free) = free_bytes(volume) else {
        // Fail open: a host whose statvfs fails is not thereby out of space.
        tracing::warn!(
            target: T, token = "drvvol-statvfs-failed",
            path = %volume.display(),
            "could not statvfs the driver volume — proceeding without a free-space preflight"
        );
        return Ok(());
    };
    free_space_verdict(free)
}

/// The pure half of the preflight, against [`REQUIRED_FREE_BYTES`].
pub fn free_space_verdict(free: u64) -> Result<()> {
    artifact::free_space_verdict(
        free,
        REQUIRED_FREE_BYTES,
        ARTIFACT,
        "The download (~350 MiB) and the extracted tree (~1.5 GiB) are written INSIDE the volume, \
         i.e. into the docker data root — on unraid that is the size-capped docker.img shared \
         with postgres and the control plane, so filling it would take the whole stack down \
         rather than just this provision. Free space (or move the docker data root / use \
         QUASAR_STORAGE_PROVIDER host paths) and restart the agent.",
    )
}

// ── the actual work ──────────────────────────────────────────────────────────

/// Deletes the scratch tree on every exit path, not just the happy one: a mid-provision
/// failure otherwise leaves ~1.8 GiB on the filesystem whose exhaustion likely caused it.
struct ScratchGuard(PathBuf);

impl Drop for ScratchGuard {
    fn drop(&mut self) {
        if self.0.exists() {
            let _ = std::fs::remove_dir_all(&self.0);
            tracing::info!(target: T, path = %self.0.display(), "scratch directory removed");
        }
    }
}

fn run_provision(volume: &Path, version: &str, _gap: Gap) -> Result<Manifest> {
    check_tools()?;
    let _lock = acquire_lock(volume)?;

    // Rate-limit repeated failures before spending anything.
    let attempts = read_attempts(volume);
    if let Some(wait) = backoff_remaining(&attempts, version, now_unix()) {
        bail!(
            "driver-volume provisioning has failed {} time(s) for driver {version} and is backing \
             off — not re-attempting for another {} min. Last error: {}. (Clearing the backoff is \
             deliberate: delete {} inside the driver volume.)",
            attempts.attempts,
            wait.as_secs().div_ceil(60),
            if attempts.last_error.is_empty() {
                "<the agent died mid-provision>"
            } else {
                attempts.last_error.as_str()
            },
            layout::ATTEMPTS
        );
    }
    check_free_space(volume)?;
    note_attempt(volume, version);

    let scratch = volume.join(layout::SCRATCH);
    let _ = std::fs::remove_dir_all(&scratch);
    std::fs::create_dir_all(&scratch).with_context(|| format!("create {}", scratch.display()))?;
    let _scratch_guard = ScratchGuard(scratch.clone());

    // Read before spending a 350 MB fetch: a corrupt pin file is a refusal, and it must
    // be one before the download, not after.
    let host_pins = read_digest_pins(volume)?;

    let staged = staged_run();
    let (run_path, url, sha256) = match &staged {
        Some(p) => {
            let sha = stage_installer(p, &scratch, version)?;
            (
                scratch.join(format!("NVIDIA-Linux-x86_64-{version}.run")),
                p.display().to_string(),
                sha,
            )
        }
        None => {
            let url = run_url(version);
            let run_path = scratch.join(format!("NVIDIA-Linux-x86_64-{version}.run"));
            phase("download", None);
            let sha = fetch_run_installer(&url, &run_path)?;
            (run_path, url, sha)
        }
    };

    // Before `sh <file>`: the point of the check is that an unvouched-for payload is
    // never executed.
    let trust = check_installer_digest(
        reviewed_digest(version),
        &host_pins,
        version,
        &sha256,
        trust_on_first_use(),
        staged.is_some(),
    )?;
    log_trust(trust, version, &sha256);
    write_digest_pin(volume, version, &sha256)?;

    phase("extract", None);
    let extracted = extract_run(&scratch, &run_path)?;

    phase("populate", None);
    let population = classify_extract_tree(&extracted)?;
    let counts = populate_volume(volume, &extracted, &population, version)?;

    phase("index", None);
    run_ldconfig(volume);

    let manifest = Manifest {
        driver_version: version.to_string(),
        sha256,
        url,
        provisioned_at_unix: SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_secs())
            .unwrap_or(0),
        agent_version: env!("CARGO_PKG_VERSION").to_string(),
        lib64_count: counts.0,
        lib32_count: counts.1,
        layout_version: CURRENT_LAYOUT_VERSION,
    };
    // LAST: its presence is what every consumer reads as "this volume is usable".
    let manifest_path = volume.join(layout::MANIFEST);
    std::fs::write(&manifest_path, serde_json::to_vec_pretty(&manifest)?)
        .with_context(|| format!("write {}", manifest_path.display()))?;
    tracing::info!(target: T, path = %manifest_path.display(), "manifest written");

    clear_attempts(volume);
    Ok(manifest)
}

fn phase(name: &str, percent: Option<u64>) {
    set_status(Status::Provisioning {
        phase: name.to_string(),
        percent,
    });
}

fn check_tools() -> Result<()> {
    let missing: Vec<&str> = REQUIRED_TOOLS
        .iter()
        .copied()
        .filter(|t| which(t).is_none())
        .collect();
    if missing.is_empty() {
        return Ok(());
    }
    bail!(
        "the agent image is missing the tool(s) needed to extract the NVIDIA installer: {}. \
         Rebuild the node image (deploy/build-images.sh) — these are expected to be present.",
        missing.join(", ")
    )
}

fn which(tool: &str) -> Option<PathBuf> {
    artifact::which(tool)
}

// ── lock ─────────────────────────────────────────────────────────────────────

/// Cross-process guard: two agents sharing the volume must never both write it.
fn acquire_lock(volume: &Path) -> Result<artifact::Lock> {
    artifact::Lock::acquire(&volume.join(layout::LOCK), ARTIFACT)
}

// ── download ─────────────────────────────────────────────────────────────────

/// Fetch the `.run` to `dest`, returning its sha256. HTTPS only, host pinned to
/// [`DOWNLOAD_HOST`], every redirect hop re-validated. A 404 means a vendor-repo-only
/// or OEM build that was never published to the XFree86 tree, and is reported as such
/// so the caller degrades to manual remediation rather than looking like a network fault.
fn fetch_run_installer(url: &str, dest: &Path) -> Result<String> {
    let version = version_from_url(url).unwrap_or_else(|| "?".into());
    artifact::fetch(&artifact::Download {
        url,
        dest,
        host: DOWNLOAD_HOST,
        what: ARTIFACT,
        timeout: DOWNLOAD_TIMEOUT,
        // A `.run` is ~300-400 MB; anything under 10 MB is an error page.
        min_bytes: 10 * 1024 * 1024,
        // makeself archives are shell scripts. Without this, a 200 that is really an
        // error page would be handed to `sh`.
        magic: Some(b"#!"),
        not_found: &format!(
            "NVIDIA does not publish a .run installer for driver {version} at {DOWNLOAD_HOST}. \
             This is normal for vendor-repo-only / OEM driver builds — the userspace must come \
             from the distribution's own packages instead."
        ),
        on_progress: Some(&|pct| phase("download", pct)),
    })
}

/// HTTPS + host pinned to [`DOWNLOAD_HOST`], nothing else.
pub fn validate_url(url: &str) -> Result<()> {
    artifact::validate_url(url, DOWNLOAD_HOST)
}

/// A connection closed mid-body still starts with `#!` and still clears the size floor,
/// so without this it reaches `sh` and fails as "installer --extract-only failed" —
/// makeself's CRC reporting our bug as somebody else's.
pub fn verify_complete(written: u64, total: Option<u64>) -> Result<()> {
    artifact::verify_complete(written, total)
}

/// Resolve a `Location` header against the URL it came from. Absolute URLs pass through
/// (the caller's [`validate_url`] pins them); `/path` joins onto the current origin;
/// anything else is refused rather than guessed at.
pub fn join_redirect(current: &str, location: &str) -> Result<String> {
    artifact::join_redirect(current, location)
}

fn version_from_url(url: &str) -> Option<String> {
    url.rsplit('/')
        .next()?
        .strip_prefix("NVIDIA-Linux-x86_64-")?
        .strip_suffix(".run")
        .map(str::to_string)
}

// ── extract ──────────────────────────────────────────────────────────────────

/// `sh <file> --extract-only` in `scratch`. NEVER `--install`, never `--silent`:
/// nothing may touch the host. The child's stdout and stderr are relayed into the agent
/// log line by line.
fn extract_run(scratch: &Path, run_path: &Path) -> Result<PathBuf> {
    use std::process::{Command, Stdio};

    tracing::info!(
        target: T,
        installer = %run_path.display(),
        "extracting installer (--extract-only; the installer is NEVER run with --install)"
    );
    let mut child = Command::new("sh")
        .arg(run_path)
        .arg("--extract-only")
        .current_dir(scratch)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .context("spawn the installer's extract-only pass")?;

    let out = child.stdout.take();
    let err = child.stderr.take();
    let t_out = out.map(|s| std::thread::spawn(move || relay(s, "out")));
    let t_err = err.map(|s| std::thread::spawn(move || relay(s, "err")));
    let status = child.wait().context("wait for the extract pass")?;
    if let Some(t) = t_out {
        let _ = t.join();
    }
    if let Some(t) = t_err {
        let _ = t.join();
    }
    if !status.success() {
        bail!("installer --extract-only failed: exit {:?}", status.code());
    }

    let dir = find_extract_dir(scratch)?;
    tracing::info!(target: T, dir = %dir.display(), "extraction complete");
    Ok(dir)
}

fn relay<R: Read + Send + 'static>(stream: R, which: &'static str) {
    let reader = std::io::BufReader::new(stream);
    use std::io::BufRead as _;
    for line in reader.lines().map_while(Result::ok) {
        if line.trim().is_empty() {
            continue;
        }
        tracing::info!(target: T, "[installer {which}] {line}");
    }
}

/// The `.run` extracts into one `NVIDIA-Linux-x86_64-<ver>` directory beside itself.
/// Found by shape, so an upstream naming change does not silently break provisioning.
fn find_extract_dir(scratch: &Path) -> Result<PathBuf> {
    let mut candidates: Vec<PathBuf> = std::fs::read_dir(scratch)
        .with_context(|| format!("read {}", scratch.display()))?
        .filter_map(|e| e.ok())
        .map(|e| e.path())
        .filter(|p| p.is_dir())
        .collect();
    candidates.sort();
    candidates
        .into_iter()
        .find(|p| {
            p.file_name()
                .and_then(|n| n.to_str())
                .is_some_and(|n| n.starts_with("NVIDIA-Linux"))
        })
        .ok_or_else(|| anyhow!("no NVIDIA-Linux-* directory found after extraction"))
}

// ── layout mapping ───────────────────────────────────────────────────────────

/// What we take out of an extracted `.run` tree, as a pure function of the tree.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Population {
    /// 64-bit shared objects (extract root, non-recursive).
    pub lib64: Vec<String>,
    /// 32-bit shared objects (the `32/` compat tree).
    pub lib32: Vec<String>,
    /// glvnd EGL external-platform configs for the Wayland/GBM platform the compositor
    /// uses (`10_nvidia_wayland.json`, `15_nvidia_gbm.json`).
    pub external_platform: Vec<String>,
    /// The soname the EGL vendor json must point at.
    pub egl_vendor_lib: Option<String>,
    /// The installer's OWN `nvidia_icd.json`, copied and path-rewritten — never
    /// synthesized, since only it states `api_version`/`file_format_version`
    /// correctly. `None` ⇒ no Vulkan wiring, for an installer that ships no ICD.
    pub vulkan_icd_json: Option<String>,
    /// `libnvidia-allocator.so.*`, which must be published as `gbm/nvidia-drm_gbm.so`.
    pub gbm_backend: Option<String>,
}

/// Vendor-NEUTRAL library base names the installer also ships, which must NEVER enter
/// the volume.
///
/// The volume sits first on `LD_LIBRARY_PATH`, so shipping the installer's own glvnd
/// dispatch layer shadows the image's libglvnd. Two of its libraries declare SONAME
/// `libEGL.so.1` (the dispatcher and NVIDIA's legacy pre-glvnd `libEGL.so.<ver>`), and
/// `ldconfig -n` picks the legacy one — the client extension string then loses
/// `EGL_EXT_device_enumeration` / `EGL_EXT_platform_wayland` / `EGL_KHR_platform_gbm`
/// and gst-wayland-display panics with "Failed to enumerate EGLDevices".
///
/// So the volume carries vendor implementations only and the dispatch layer stays the
/// image's. Matching is on the base name up to the first `.so`, so `libEGL_nvidia` is
/// kept while `libEGL` is dropped.
const VENDOR_NEUTRAL_LIB_BASES: &[&str] = &[
    "libEGL",
    "libGL",
    "libGLX",
    "libGLdispatch",
    "libOpenGL",
    "libGLESv1_CM",
    "libGLESv2",
    "libOpenCL",
];

/// The base name up to the first `.so`.
fn so_base(name: &str) -> &str {
    name.split(".so").next().unwrap_or(name)
}

/// Whether a library must be left to the image — see [`VENDOR_NEUTRAL_LIB_BASES`].
pub fn is_vendor_neutral_dispatch(name: &str) -> bool {
    VENDOR_NEUTRAL_LIB_BASES.contains(&so_base(name))
}

/// Every vendor `*.so*` in a directory, non-recursively, sorted. The dispatch layer is
/// filtered here rather than at copy time, so the manifest counts describe what is
/// actually in the volume.
fn shared_objects(dir: &Path) -> Vec<String> {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return Vec::new();
    };
    let mut names: Vec<String> = entries
        .filter_map(|e| e.ok())
        .filter(|e| e.file_type().map(|t| t.is_file()).unwrap_or(false))
        .filter_map(|e| e.file_name().to_str().map(str::to_string))
        .filter(|n| n.contains(".so"))
        .filter(|n| {
            if is_vendor_neutral_dispatch(n) {
                tracing::debug!(
                    target: T,
                    lib = %n,
                    "skipping a vendor-neutral dispatch library — the container image supplies it, \
                     and shadowing it via LD_LIBRARY_PATH breaks EGL"
                );
                return false;
            }
            true
        })
        .collect();
    names.sort();
    names
}

/// Map an extracted tree onto the volume layout.
pub fn classify_extract_tree(extracted: &Path) -> Result<Population> {
    let lib64 = shared_objects(extracted);
    if lib64.is_empty() {
        bail!(
            "extracted tree {} contains no shared objects — the installer layout is not what \
             this provisioner understands",
            extracted.display()
        );
    }
    let lib32 = shared_objects(&extracted.join("32"));

    let pick = |prefix: &str| -> Option<String> {
        lib64
            .iter()
            .find(|n| n.starts_with(prefix) && !n.ends_with(".so"))
            .or_else(|| lib64.iter().find(|n| n.starts_with(prefix)))
            .cloned()
    };

    let mut external_platform: Vec<String> = std::fs::read_dir(extracted)
        .map(|rd| {
            rd.filter_map(|e| e.ok())
                .filter_map(|e| e.file_name().to_str().map(str::to_string))
                .filter(|n| n.ends_with(".json") && (n.contains("_wayland") || n.contains("_gbm")))
                .collect()
        })
        .unwrap_or_default();
    external_platform.sort();

    let vulkan_icd_json = extracted
        .join("nvidia_icd.json")
        .is_file()
        .then(|| "nvidia_icd.json".to_string());

    Ok(Population {
        egl_vendor_lib: pick("libEGL_nvidia.so"),
        vulkan_icd_json,
        gbm_backend: pick("libnvidia-allocator.so"),
        external_platform,
        lib64,
        lib32,
    })
}

/// Copy the classified set into the volume. Returns `(lib64_count,
/// lib32_count)`.
fn populate_volume(
    volume: &Path,
    extracted: &Path,
    pop: &Population,
    version: &str,
) -> Result<(usize, usize)> {
    // WIPE SCOPE: clear lib64/ and lib32/ BY NAME so a re-provision cannot leave a
    // previous version's libraries for ldconfig to index alongside the new ones. Never
    // widen this to the volume — `cuda/` (cuda_runtime.rs) must survive it.
    for d in [layout::LIB64, layout::LIB32] {
        let p = volume.join(d);
        if p.exists() {
            std::fs::remove_dir_all(&p).with_context(|| format!("clear {}", p.display()))?;
        }
    }
    for d in [
        layout::LIB64,
        layout::LIB32,
        layout::EGL_VENDOR_DIR,
        layout::EGL_EXTERNAL_DIR,
        layout::VULKAN_ICD_DIR,
        layout::GBM_DIR,
        "ld.so.conf.d",
    ] {
        std::fs::create_dir_all(volume.join(d))
            .with_context(|| format!("create {}", volume.join(d).display()))?;
    }

    for name in &pop.lib64 {
        copy_file(
            &extracted.join(name),
            &volume.join(layout::LIB64).join(name),
        )?;
    }
    for name in &pop.lib32 {
        copy_file(
            &extracted.join("32").join(name),
            &volume.join(layout::LIB32).join(name),
        )?;
    }
    tracing::info!(
        target: T,
        lib64 = pop.lib64.len(),
        lib32 = pop.lib32.len(),
        "driver libraries placed"
    );
    if pop.lib32.is_empty() {
        tracing::warn!(
            target: T, token = "drvvol-no-lib32",
            "the installer carried no 32-bit compat tree — 32-bit apps (the native Steam client) \
             will still be unable to render"
        );
    }

    // glvnd EGL vendor config, written rather than copied so `library_path` is absolute
    // inside the volume: glvnd dlopens that string directly, and a bare soname would
    // depend on the very loader path this is fixing.
    let egl_lib = pop
        .egl_vendor_lib
        .as_ref()
        .ok_or_else(|| anyhow!("extracted tree has no libEGL_nvidia.so.* — cannot wire EGL"))?;
    let egl_abs = volume.join(layout::LIB64).join(egl_lib);
    write_json(
        &volume.join(layout::EGL_VENDOR_JSON),
        &serde_json::json!({
            "file_format_version": "1.0.0",
            "ICD": { "library_path": egl_abs.to_string_lossy() }
        }),
    )?;

    // Vulkan ICD: the installer's OWN json with only `library_path` made absolute.
    // Never synthesized — a guessed api_version/file_format_version was wrong for this
    // driver.
    //
    // KNOWN LIMITATION: on the plain loader path this driver's ICD refuses
    // `vk_icdGetInstanceProcAddr("vkCreateInstance")` unless `libEGL.so.1` resolves to
    // NVIDIA's legacy pre-glvnd libEGL — the one library that must stay off the search
    // path for the compositor. The compositor wins. It does NOT affect
    // QUASAR_ENCODER=vulkan, which goes through the agent's own VK_ADD_DRIVER_FILES
    // wiring (session::vulkan_share); the loader logs and skips an unusable ICD.
    match &pop.vulkan_icd_json {
        Some(name) => {
            let src = extracted.join(name);
            let dst = volume.join(layout::VULKAN_ICD_JSON);
            match rewrite_icd_library_path(&src, &volume.join(layout::LIB64)) {
                Ok(body) => {
                    if let Some(p) = dst.parent() {
                        std::fs::create_dir_all(p).ok();
                    }
                    std::fs::write(&dst, body)
                        .with_context(|| format!("write {}", dst.display()))?;
                    tracing::info!(target: T, path = %dst.display(), "Vulkan ICD placed (installer's own, path rewritten)");
                }
                Err(e) => tracing::warn!(
                    target: T, token = "drvvol-icd-rewrite-failed",
                    error = %format!("{e:#}"),
                    "could not rewrite the installer's nvidia_icd.json — skipping Vulkan wiring"
                ),
            }
        }
        None => tracing::info!(
            target: T,
            "the installer ships no nvidia_icd.json — no Vulkan wiring for this driver"
        ),
    }

    // EGL external platforms (Wayland, GBM): copied and rewritten so their library
    // paths point into the volume too.
    for name in &pop.external_platform {
        let src = extracted.join(name);
        let dst = volume.join(layout::EGL_EXTERNAL_DIR).join(name);
        match rewrite_icd_library_path(&src, &volume.join(layout::LIB64)) {
            Ok(body) => {
                std::fs::write(&dst, body).with_context(|| format!("write {}", dst.display()))?;
                tracing::info!(target: T, file = %name, "EGL external-platform config placed");
            }
            Err(e) => {
                tracing::warn!(target: T, token = "drvvol-egl-config-skipped", file = %name, error = %format!("{e:#}"), "skipping an EGL external-platform config")
            }
        }
    }

    // GBM backend: Mesa's gbm loader looks for `<backend>_gbm.so` by name.
    if let Some(alloc) = &pop.gbm_backend {
        let dst = volume.join(layout::GBM_DIR).join("nvidia-drm_gbm.so");
        let _ = std::fs::remove_file(&dst);
        copy_file(&volume.join(layout::LIB64).join(alloc), &dst)?;
        tracing::info!(target: T, from = %alloc, "GBM backend published as nvidia-drm_gbm.so");
    }

    // For consumers that prefer ldconfig over LD_LIBRARY_PATH.
    let conf = volume.join(layout::LD_CONF);
    std::fs::write(
        &conf,
        format!(
            "# Quasar-provisioned NVIDIA userspace for driver {version}\n{}\n{}\n",
            volume.join(layout::LIB64).display(),
            volume.join(layout::LIB32).display()
        ),
    )
    .with_context(|| format!("write {}", conf.display()))?;

    Ok((pop.lib64.len(), pop.lib32.len()))
}

fn copy_file(src: &Path, dst: &Path) -> Result<()> {
    std::fs::copy(src, dst)
        .with_context(|| format!("copy {} -> {}", src.display(), dst.display()))?;
    Ok(())
}

fn write_json(path: &Path, v: &serde_json::Value) -> Result<()> {
    if let Some(p) = path.parent() {
        std::fs::create_dir_all(p).ok();
    }
    std::fs::write(path, serde_json::to_vec_pretty(v)?)
        .with_context(|| format!("write {}", path.display()))?;
    tracing::info!(target: T, path = %path.display(), "wrote discovery config");
    Ok(())
}

/// Rewrite an `ICD.library_path` to an absolute path inside the volume, preserving
/// every other key. Used for both the EGL external-platform configs and the Vulkan ICD.
pub fn rewrite_icd_library_path_body(body: &str, lib_dir: &Path) -> Result<String> {
    let mut v: serde_json::Value = serde_json::from_str(body).context("parse config")?;
    let slot = v
        .get_mut("ICD")
        .and_then(|i| i.get_mut("library_path"))
        .ok_or_else(|| anyhow!("no ICD.library_path"))?;
    let name = slot
        .as_str()
        .ok_or_else(|| anyhow!("library_path is not a string"))?;
    // Only a bare soname is rewritten; an already-absolute path is left alone.
    if !name.starts_with('/') {
        let abs = lib_dir.join(name);
        *slot = serde_json::Value::String(abs.to_string_lossy().into_owned());
    }
    Ok(serde_json::to_string_pretty(&v)?)
}

fn rewrite_icd_library_path(src: &Path, lib_dir: &Path) -> Result<String> {
    let body = std::fs::read_to_string(src).with_context(|| format!("read {}", src.display()))?;
    rewrite_icd_library_path_body(&body, lib_dir)
}

/// `ldconfig -n <dir>` creates the SONAME symlinks every DT_NEEDED in the driver set
/// depends on; doing it by hand would mean parsing ELF SONAMEs. Best-effort — a partial
/// link set still beats nothing, and the readiness re-probe is the real verdict.
fn run_ldconfig(volume: &Path) {
    for dir in [layout::LIB64, layout::LIB32] {
        let p = volume.join(dir);
        if !p.is_dir() {
            continue;
        }
        match std::process::Command::new("ldconfig")
            .arg("-n")
            .arg(&p)
            .output()
        {
            Ok(o) => {
                let stderr = String::from_utf8_lossy(&o.stderr);
                for line in stderr.lines().filter(|l| !l.trim().is_empty()) {
                    tracing::info!(target: T, "[ldconfig {dir}] {line}");
                }
                if o.status.success() {
                    tracing::info!(target: T, dir = %p.display(), "soname symlinks created");
                } else {
                    tracing::warn!(target: T, token = "drvvol-ldconfig-failed", dir = %p.display(), code = ?o.status.code(), "ldconfig -n reported a failure");
                }
            }
            Err(e) => {
                tracing::warn!(target: T, token = "drvvol-ldconfig-exec-failed", dir = %p.display(), error = %e, "could not run ldconfig -n")
            }
        }
    }
}

// ── consumption: this process ────────────────────────────────────────────────

/// Environment the AGENT PROCESS sets so its own EGL / Vulkan / GBM discovery finds the
/// volume. Every variable here is read at use time, so setting it in-process before
/// `gst::init` works.
///
/// `LD_LIBRARY_PATH` is deliberately absent: glibc latches it at `execve`, so it can
/// only come from compose (`docker-compose.nvidia.yml`), where it is unconditional and
/// safe because a nonexistent directory on it is inert.
///
/// Each variable's shape is load-bearing:
/// - `__EGL_VENDOR_LIBRARY_DIRS` REPLACES glvnd's vendor search, so the system dirs are
///   appended. Alone and pointing at a missing directory it returns an EMPTY client
///   extension string — EGL is dead — which is why it is set here, only when a manifest
///   exists, and never in compose. The `*_FILENAMES` form would hide Mesa entirely.
/// - `VK_ADD_DRIVER_FILES` ADDS to the loader's driver list;
///   `VK_ICD_FILENAMES`/`VK_DRIVER_FILES` replace it and would hide radeon/intel/lavapipe.
///   An older loader ignores it (degradation, not breakage).
/// - `__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS` likewise replaces; system dir appended.
/// - `GBM_BACKENDS_PATH` replaces Mesa's backend dir, so it is emitted only when the
///   volume actually carries a backend.
pub fn process_env(volume: &Path) -> Vec<(String, String)> {
    let mut out = Vec::new();
    let egl_dir = volume.join(layout::EGL_VENDOR_DIR);
    if egl_dir.is_dir() {
        out.push((
            "__EGL_VENDOR_LIBRARY_DIRS".to_string(),
            join_paths(&[
                egl_dir,
                PathBuf::from("/etc/glvnd/egl_vendor.d"),
                PathBuf::from("/usr/share/glvnd/egl_vendor.d"),
            ]),
        ));
    }
    let ext_dir = volume.join(layout::EGL_EXTERNAL_DIR);
    if ext_dir.is_dir() {
        out.push((
            "__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS".to_string(),
            join_paths(&[
                ext_dir,
                PathBuf::from("/etc/egl/egl_external_platform.d"),
                PathBuf::from("/usr/share/egl/egl_external_platform.d"),
            ]),
        ));
    }
    let icd = volume.join(layout::VULKAN_ICD_JSON);
    if icd.is_file() {
        out.push((
            "VK_ADD_DRIVER_FILES".to_string(),
            icd.to_string_lossy().into_owned(),
        ));
    }
    let gbm = volume.join(layout::GBM_DIR);
    if gbm.join("nvidia-drm_gbm.so").is_file() {
        out.push((
            "GBM_BACKENDS_PATH".to_string(),
            gbm.to_string_lossy().into_owned(),
        ));
    }
    out
}

fn join_paths(paths: &[PathBuf]) -> String {
    paths
        .iter()
        .map(|p| p.to_string_lossy().into_owned())
        .collect::<Vec<_>>()
        .join(":")
}

static ENV_APPLIED: OnceLock<()> = OnceLock::new();

/// Apply [`process_env`] once, BEFORE `gst::init`. No-op when nothing is provisioned.
/// An operator-set value always wins.
pub fn apply_process_env() {
    if ENV_APPLIED.set(()).is_err() {
        return;
    }
    let Some(info) = current() else {
        return;
    };
    for (k, v) in process_env(&info.local) {
        if std::env::var_os(&k).is_some() {
            tracing::info!(target: T, key = %k, "leaving an operator-set {k} untouched");
            continue;
        }
        tracing::info!(target: T, key = %k, value = %v, "driver-volume discovery: {k}={v}");
        std::env::set_var(&k, &v);
    }
    let ld = std::env::var("LD_LIBRARY_PATH").unwrap_or_default();
    let want = info.local.join(layout::LIB64);
    if !ld.split(':').any(|p| Path::new(p) == want) {
        tracing::warn!(
            target: T, token = "drvvol-ld-library-path-missing",
            lib64 = %want.display(),
            ld_library_path = %ld,
            "LD_LIBRARY_PATH does not contain the driver volume's lib64 — the dynamic loader \
             latches it at exec time and cannot be fixed from here. Apply \
             deploy/docker-compose.nvidia.yml (it sets LD_LIBRARY_PATH unconditionally; the \
             directory is inert when the volume is empty) and recreate the node-agent container."
        );
    }
}

// ── EGL runtime verification ─────────────────────────────────────────────────

/// The argv[1] that puts the agent binary into EGL-self-test mode.
pub const EGL_SELFTEST_ARG: &str = "egl-selftest";

/// The client extension the compositor's first EGL call needs; without it
/// gst-wayland-display panics with "Failed to enumerate EGLDevices".
const REQUIRED_EGL_CLIENT_EXT: &str = "EGL_EXT_device_enumeration";

/// Does the EGL stack this process would actually load work? File presence is not the
/// question: all three NVIDIA readiness checks can be green off present files while the
/// wrong dispatcher is being loaded and no session can start.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub enum EglRuntime {
    /// Not probed (non-NVIDIA host, or a test injecting its own view).
    #[default]
    Unknown,
    Ok {
        /// The `libEGL.so.1` actually resolved — the most useful fact when this breaks.
        loaded: String,
    },
    /// The probe RAN and EGL genuinely failed. A verdict about the host, allowed to
    /// turn a file-presence pass red.
    Broken {
        detail: String,
        loaded: Option<String>,
    },
    /// The probe could not run or could not be believed (spawn failure, killed child,
    /// no verdict line, [`EGL_SELFTEST_TIMEOUT`]). Never a failure: it must not veto a
    /// check and must not contribute to a provisioning gap.
    Indeterminate { detail: String },
}

impl EglRuntime {
    pub fn is_broken(&self) -> bool {
        matches!(self, EglRuntime::Broken { .. })
    }

    /// No usable verdict. Callers must treat it as no information, never as a failure.
    pub fn is_indeterminate(&self) -> bool {
        matches!(self, EglRuntime::Indeterminate { .. })
    }
}

/// Parse the self-test child's output.
pub fn parse_egl_selftest(stdout: &str) -> EglRuntime {
    let field = |k: &str| {
        stdout
            .lines()
            .find_map(|l| l.strip_prefix(k).map(|v| v.trim().to_string()))
    };
    let loaded = field("LOADED=").filter(|s| !s.is_empty());
    if let Some(err) = field("DISPATCH_ERROR=").filter(|s| !s.is_empty()) {
        return EglRuntime::Broken {
            detail: format!("libEGL.so.1 could not be loaded at all: {err}"),
            loaded,
        };
    }
    if let Some(err) = field("VENDOR_ERROR=").filter(|s| !s.is_empty()) {
        return EglRuntime::Broken {
            detail: format!(
                "the NVIDIA EGL vendor library does not load — glvnd will skip it and the \
                 compositor will find no NVIDIA device: {err}"
            ),
            loaded,
        };
    }
    let Some(exts) = field("EXTENSIONS=") else {
        // No verdict line: the child crashed or was cut short, so the probe has said
        // nothing about this host's EGL stack.
        return EglRuntime::Indeterminate {
            detail: format!(
                "the EGL self-test produced no result (it crashed or was killed); loaded={}",
                loaded.as_deref().unwrap_or("<unresolved>")
            ),
        };
    };
    if exts.is_empty() {
        return EglRuntime::Broken {
            detail: "the EGL client-extension string is EMPTY — no vendor-neutral EGL dispatcher \
                     is loading. This is what an EGL vendor-config path pointing only at missing \
                     files, or a shadowed libglvnd, looks like."
                .to_string(),
            loaded,
        };
    }
    if !exts.contains(REQUIRED_EGL_CLIENT_EXT) {
        return EglRuntime::Broken {
            detail: format!(
                "the loaded EGL library does not advertise {REQUIRED_EGL_CLIENT_EXT}, so the \
                 session compositor panics on its first call (\"Failed to enumerate EGLDevices\"). \
                 This is the signature of NVIDIA's legacy pre-glvnd libEGL shadowing the image's \
                 libglvnd dispatcher. Advertised: {exts}"
            ),
            loaded,
        };
    }
    EglRuntime::Ok {
        loaded: loaded.unwrap_or_else(|| "<unknown>".into()),
    }
}

static SIBLING_EGL: Mutex<Option<(String, Instant, EglRuntime)>> = Mutex::new(None);

/// Validate the driver through Docker's sibling-container namespace using the
/// same mount/environment arguments as an app. Cached for one minute per image
/// and driver digest; failures are retried without re-downloading the driver.
pub fn probe_sibling_egl() -> EglRuntime {
    let Some(info) = current() else {
        return EglRuntime::Unknown;
    };
    if info.host.is_none() && info.name.is_none() {
        return EglRuntime::Unknown;
    }
    let runtime = crate::session::container::ContainerRuntime::from_env();
    let image = match runtime.own_image() {
        Ok(image) => image,
        Err(error) => {
            return EglRuntime::Indeterminate {
                detail: format!("cannot identify the sibling probe image: {error}"),
            }
        }
    };
    let key = format!(
        "{image}:{}:{:?}:{:?}",
        info.manifest.sha256, info.name, info.host
    );
    let Ok(mut cached) = SIBLING_EGL.lock() else {
        return EglRuntime::Indeterminate {
            detail: "sibling EGL probe cache is unavailable".into(),
        };
    };
    if let Some((old, at, result)) = &*cached {
        if old == &key && at.elapsed() < Duration::from_secs(60) {
            return result.clone();
        }
    }
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    let name = format!("quasar-driver-probe-{}-{nonce}", std::process::id());
    let mut args: Vec<String> = [
        "run",
        "--rm",
        "--name",
        &name,
        "--network",
        "none",
        "--read-only",
        "--cap-drop",
        "ALL",
        "--security-opt",
        "no-new-privileges",
        "--gpus",
        "all",
        "--entrypoint",
        "/usr/bin/timeout",
    ]
    .iter()
    .map(|s| (*s).to_owned())
    .collect();
    args.extend(app_container_args(Some(&info), VOLUME_MOUNT, ""));
    args.extend([
        image,
        "20s".into(),
        "/usr/local/bin/quasar-node-agent".into(),
        EGL_SELFTEST_ARG.into(),
        format!("{VOLUME_MOUNT}/lib64/libEGL_nvidia.so.0"),
    ]);
    let output = runtime.run_raw(&args.iter().map(String::as_str).collect::<Vec<_>>());
    // Also clean up after a daemon/client timeout. The in-container timeout
    // bounds the probe even if the agent itself is killed during this call.
    runtime.force_remove(&name);
    let result = match output {
        Ok(body) => parse_egl_selftest(&body),
        Err(error) => EglRuntime::Indeterminate {
            detail: format!("sibling EGL test could not complete: {error}"),
        },
    };
    *cached = Some((key, Instant::now(), result.clone()));
    result
}

/// Run the self-test as a CHILD of this binary (`/proc/self/exe egl-selftest`).
///
/// Out-of-process: it dlopens a driver stack already suspected of being broken, and a
/// segfault in a vendor library would otherwise take the agent down on every capacity
/// report. The child inherits this environment, so it tests what the compositor gets.
///
/// Every infrastructure failure of the probe — spawn failure, killed child, wait error,
/// and realistically [`EGL_SELFTEST_TIMEOUT`] — must be [`EglRuntime::Indeterminate`],
/// never `Broken`. Conflating them let one slow dlopen drag a healthy host into a
/// 350 MB download and a session-killing agent restart.
pub fn probe_egl_runtime(vendor_lib: Option<&Path>) -> EglRuntime {
    use std::process::{Command, Stdio};
    let exe = match std::env::current_exe() {
        Ok(p) => p,
        Err(e) => {
            return EglRuntime::Indeterminate {
                detail: format!("cannot locate the agent binary to run the EGL self-test: {e}"),
            }
        }
    };
    let mut cmd = Command::new(exe);
    cmd.arg(EGL_SELFTEST_ARG);
    if let Some(v) = vendor_lib {
        cmd.arg(v);
    }
    let child = cmd
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn();
    let mut child = match child {
        Ok(c) => c,
        Err(e) => {
            return EglRuntime::Indeterminate {
                detail: format!("could not spawn the EGL self-test: {e}"),
            }
        }
    };
    let deadline = Instant::now() + EGL_SELFTEST_TIMEOUT;
    loop {
        match child.try_wait() {
            Ok(Some(_)) => break,
            Ok(None) if Instant::now() < deadline => {
                std::thread::sleep(Duration::from_millis(20));
            }
            Ok(None) => {
                let _ = child.kill();
                let _ = child.wait();
                return EglRuntime::Indeterminate {
                    detail: format!(
                        "the EGL self-test did not finish within {EGL_SELFTEST_TIMEOUT:?}. That \
                         is a timeout, not a verdict: a first dlopen of the NVIDIA stack on a \
                         loaded box with persistence mode off is not guaranteed to beat it."
                    ),
                };
            }
            Err(e) => {
                return EglRuntime::Indeterminate {
                    detail: format!("EGL self-test wait failed: {e}"),
                }
            }
        }
    }
    let mut out = String::new();
    if let Some(mut s) = child.stdout.take() {
        let _ = s.read_to_string(&mut out);
    }
    let verdict = parse_egl_selftest(&out);
    match &verdict {
        EglRuntime::Ok { loaded } => {
            tracing::info!(target: T, loaded = %loaded, "EGL self-test: dispatcher OK, device enumeration available")
        }
        EglRuntime::Broken { detail, loaded } => tracing::warn!(
            target: T, token = "egl-selftest-failed",
            loaded = loaded.as_deref().unwrap_or("<unresolved>"),
            "EGL self-test FAILED: {detail}"
        ),
        EglRuntime::Indeterminate { detail } => tracing::info!(
            target: T, token = "egl-selftest-inconclusive",
            "EGL self-test INCONCLUSIVE (treated as no information — it will not fail a check or \
             trigger provisioning): {detail}"
        ),
        EglRuntime::Unknown => {}
    }
    verdict
}

const EGL_SELFTEST_TIMEOUT: Duration = Duration::from_secs(20);

/// CHILD side of the self-test: loads `libEGL.so.1` the way the compositor will and
/// reports the resolved file, the client extension string, and optionally whether the
/// NVIDIA vendor library dlopens. Prints `KEY=value` and always exits 0 — the parent
/// reads the text, so a non-zero exit would only lose information.
pub fn egl_selftest_main(vendor_lib: Option<&str>) -> i32 {
    // SAFETY: all four calls are plain libdl/EGL C entry points with the
    // documented signatures; every pointer handed in is either NULL or a
    // NUL-terminated CString that outlives the call, and every pointer read
    // back is null-checked before use.
    unsafe {
        let name = std::ffi::CString::new("libEGL.so.1").unwrap();
        let handle = libc::dlopen(name.as_ptr(), libc::RTLD_NOW | libc::RTLD_LOCAL);
        if handle.is_null() {
            println!("DISPATCH_ERROR={}", dl_error());
            return 0;
        }
        let sym = std::ffi::CString::new("eglQueryString").unwrap();
        let f = libc::dlsym(handle, sym.as_ptr());
        if f.is_null() {
            println!("DISPATCH_ERROR=libEGL.so.1 exports no eglQueryString");
            return 0;
        }
        // With two libraries claiming SONAME libEGL.so.1, the file the loader actually
        // resolved is the whole diagnosis.
        let mut info: libc::Dl_info = std::mem::zeroed();
        if libc::dladdr(f, &mut info) != 0 && !info.dli_fname.is_null() {
            println!(
                "LOADED={}",
                std::ffi::CStr::from_ptr(info.dli_fname).to_string_lossy()
            );
        }
        // EGL_EXTENSIONS on EGL_NO_DISPLAY is the client extension string, the one
        // carrying EGL_EXT_device_enumeration. No display, so this works headless.
        let query: extern "C" fn(*mut libc::c_void, i32) -> *const libc::c_char =
            std::mem::transmute(f);
        let exts = query(std::ptr::null_mut(), 0x3055 /* EGL_EXTENSIONS */);
        if exts.is_null() {
            println!("EXTENSIONS=");
        } else {
            println!(
                "EXTENSIONS={}",
                std::ffi::CStr::from_ptr(exts).to_string_lossy()
            );
        }
        if let Some(v) = vendor_lib {
            if let Ok(c) = std::ffi::CString::new(v) {
                let h = libc::dlopen(c.as_ptr(), libc::RTLD_NOW | libc::RTLD_LOCAL);
                if h.is_null() {
                    println!("VENDOR_ERROR={}", dl_error());
                } else {
                    println!("VENDOR=ok");
                    libc::dlclose(h);
                }
            }
        }
        libc::dlclose(handle);
    }
    0
}

/// SAFETY: `dlerror` returns either NULL or a pointer to a NUL-terminated
/// string owned by libdl and valid until the next libdl call.
fn dl_error() -> String {
    unsafe {
        let e = libc::dlerror();
        if e.is_null() {
            "unknown dynamic-linker error".to_string()
        } else {
            std::ffi::CStr::from_ptr(e).to_string_lossy().into_owned()
        }
    }
}

/// The vendor library the self-test should try, preferring the volume's copy.
pub fn vendor_lib_for_selftest() -> Option<PathBuf> {
    let info = current()?;
    let p = info.local.join(layout::EGL_VENDOR_JSON);
    let body = std::fs::read_to_string(p).ok()?;
    let v: serde_json::Value = serde_json::from_str(&body).ok()?;
    let path = v.get("ICD")?.get("library_path")?.as_str()?;
    Some(PathBuf::from(path))
}

// ── consumption: app containers ──────────────────────────────────────────────

/// Extra `docker run` arguments giving an APP container the provisioned driver
/// userspace. Empty when nothing is provisioned or the host path is unresolved, so a
/// host with its own driver is byte-for-byte unchanged.
///
/// `image_ld_library_path` is the image's own value: `-e` REPLACES rather than appends,
/// and dropping it would trade an old breakage for a new one.
///
/// `GBM_BACKENDS_PATH` is as load-bearing here as in [`process_env`]. Without it
/// `gbm_create_device` falls back to Mesa, which has no driver for `nvidia-drm`:
/// Xwayland refuses glamor on llvmpipe, advertises no DRI3, and Steam's CEF GPU process
/// crash-loops (a flickering Big Picture logo that never reaches sign-in). The
/// accompanying `card1: Permission denied` is a symptom of Mesa being on the path at
/// all — do NOT paper over it with `--group-add`, a device chmod, or wider caps.
pub fn app_container_args(
    info: Option<&VolumeInfo>,
    mount_dst: &str,
    image_ld_library_path: &str,
) -> Vec<String> {
    let Some(info) = info else {
        return Vec::new();
    };
    let source = if let Some(name) = &info.name {
        format!("type=volume,src={name},dst={mount_dst},readonly")
    } else if let Some(host) = &info.host {
        format!("type=bind,src={},dst={mount_dst},readonly", host.display())
    } else {
        return Vec::new();
    };
    let dst = Path::new(mount_dst);
    let mut ld = vec![dst.join(layout::LIB64).to_string_lossy().into_owned()];
    for p in image_ld_library_path.split(':').filter(|p| !p.is_empty()) {
        ld.push(p.to_string());
    }
    let mut args = vec![
        "--mount".into(),
        source,
        "-e".into(),
        format!("LD_LIBRARY_PATH={}", ld.join(":")),
        "-e".into(),
        format!(
            "__EGL_VENDOR_LIBRARY_DIRS={}:/etc/glvnd/egl_vendor.d:/usr/share/glvnd/egl_vendor.d",
            dst.join(layout::EGL_VENDOR_DIR).display()
        ),
        "-e".into(),
        format!(
            "__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS={}:/usr/share/egl/egl_external_platform.d",
            dst.join(layout::EGL_EXTERNAL_DIR).display()
        ),
        "-e".into(),
        format!(
            "VK_ADD_DRIVER_FILES={}",
            dst.join(layout::VULKAN_ICD_JSON).display()
        ),
    ];

    // GBM_BACKENDS_PATH REPLACES Mesa's backend directory, so it is emitted only when
    // the volume carries a backend. The existence check runs against `info.local` (the
    // volume as the AGENT sees it); the value emitted is the APP container's path.
    if info
        .local
        .join(layout::GBM_DIR)
        .join("nvidia-drm_gbm.so")
        .is_file()
    {
        args.push("-e".into());
        args.push(format!(
            "GBM_BACKENDS_PATH={}",
            dst.join(layout::GBM_DIR).display()
        ));
    }

    args
}

/// The host directory holding the volume's 32-bit libraries, as a plain path so the
/// existing lib32 mount mechanism (`nvidia_lib32_path`) is reused untouched. `None`
/// when nothing is provisioned, the host path is unresolved, or there is no 32-bit tree.
pub fn lib32_host_path(info: Option<&VolumeInfo>) -> Option<String> {
    let info = info?;
    let host = info.host.as_ref()?;
    if info.manifest.lib32_count == 0 {
        return None;
    }
    Some(host.join(layout::LIB32).to_string_lossy().into_owned())
}

// ── restart ──────────────────────────────────────────────────────────────────

/// Guarded self-restart after a successful EGL provision: `exit(0)` and let the
/// container restart policy bring the process back. Needed because the loader latches
/// `LD_LIBRARY_PATH` at exec and `gst::init` is a process-wide `Once`.
///
/// Only for a gap that included EGL. A lib32-only gap is wired per session at container
/// launch, so restarting would kill live sessions for nothing.
pub fn restart_for_egl(grace: Duration) {
    tracing::warn!(
        target: T, token = "drvvol-agent-restart-scheduled",
        grace_s = grace.as_secs(),
        "driver volume provisioned — RESTARTING the node agent so the compositor's EGL stack \
         re-initialises against it (the dynamic loader latches its search path at process start, \
         so this cannot be applied in place). The container restart policy brings the agent \
         straight back; the host readiness card will then show these checks passing."
    );
    static ONCE: Mutex<bool> = Mutex::new(false);
    if let Ok(mut done) = ONCE.lock() {
        if *done {
            return;
        }
        *done = true;
    }
    std::thread::spawn(move || {
        std::thread::sleep(grace);
        // #66: symmetric with the cudart path — never exit while any provision is still
        // writing into a shared volume, including the CUDA userspace fetch.
        if !crate::artifact::wait_for_quiescence(crate::agent::PROVISION_QUIESCENCE_WAIT) {
            tracing::warn!(
                target: T, token = "drvvol-agent-restart-deferred",
                in_flight = crate::artifact::provisioning_in_flight(),
                "another provision is still in flight — NOT restarting; the EGL stack will \
                 re-initialise on the next agent start instead"
            );
            return;
        }
        tracing::warn!(target: T, token = "drvvol-agent-restart-now", "restarting node agent now");
        std::process::exit(0);
    });
}

/// A compact operator-facing summary of the volume, for readiness summaries.
pub fn describe(manifest: &Manifest) -> String {
    format!(
        "provisioned by Quasar (driver volume, v{})",
        manifest.driver_version
    )
}

/// Extra key/values for the diagnostic bundle / logs.
pub fn debug_map() -> BTreeMap<String, String> {
    let mut m = BTreeMap::new();
    m.insert("enabled".into(), enabled().to_string());
    match status() {
        Status::Idle => m.insert("status".into(), "idle".into()),
        Status::Provisioning { phase, percent } => m.insert(
            "status".into(),
            format!(
                "provisioning:{phase}{}",
                percent.map(|p| format!(":{p}%")).unwrap_or_default()
            ),
        ),
        Status::Provisioned(mf) => m.insert(
            "status".into(),
            format!("provisioned:{}", mf.driver_version),
        ),
        Status::Failed(e) => m.insert("status".into(), format!("failed:{e}")),
    };
    m
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    struct Tmp(PathBuf);
    impl Tmp {
        fn new(tag: &str) -> Tmp {
            let d = std::env::temp_dir().join(format!(
                "quasar-nvvol-{tag}-{}-{}",
                std::process::id(),
                SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .unwrap()
                    .as_nanos()
            ));
            fs::create_dir_all(&d).unwrap();
            Tmp(d)
        }
        fn file(&self, rel: &str, body: &str) -> &Self {
            let p = self.0.join(rel);
            fs::create_dir_all(p.parent().unwrap()).unwrap();
            fs::write(p, body).unwrap();
            self
        }
    }
    impl Drop for Tmp {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    // ── version parsing ──────────────────────────────────────────────────────

    #[test]
    fn driver_version_parses_sysfs_and_rejects_anything_url_unsafe() {
        assert_eq!(
            parse_driver_version("610.57.04\n").as_deref(),
            Some("610.57.04")
        );
        assert_eq!(parse_driver_version(" 575.64 ").as_deref(), Some("575.64"));
        // The value is interpolated into a URL and a path; anything that could escape
        // either is refused outright.
        for bad in [
            "",
            "610",
            "610.57.04/../../etc",
            "610.57.04 && rm -rf /",
            "../570.86",
            "610..57",
            ".610.57",
            "610.57.",
            "v610.57.04",
            "610.57.04\nX",
        ] {
            assert!(parse_driver_version(bad).is_none(), "must reject {bad:?}");
        }
    }

    #[test]
    fn kernel_version_is_read_from_sysfs_root() {
        let t = Tmp::new("sysfs");
        t.file("sys/module/nvidia/version", "610.57.04\n");
        assert_eq!(kernel_driver_version(&t.0).as_deref(), Some("610.57.04"));
        let empty = Tmp::new("nosysfs");
        assert_eq!(kernel_driver_version(&empty.0), None);
    }

    // ── URL construction + origin pinning ────────────────────────────────────

    #[test]
    fn url_is_the_pinned_nvidia_xfree86_path() {
        assert_eq!(
            run_url("610.57.04"),
            "https://download.nvidia.com/XFree86/Linux-x86_64/610.57.04/NVIDIA-Linux-x86_64-610.57.04.run"
        );
        assert!(validate_url(&run_url("610.57.04")).is_ok());
    }

    #[test]
    fn only_https_download_nvidia_com_is_accepted() {
        for bad in [
            "http://download.nvidia.com/x.run",
            "https://evil.example/x.run",
            "https://download.nvidia.com.evil.example/x.run",
            "https://evil.example@download.nvidia.com/x.run",
            "https://download.nvidia.com@evil.example/x.run",
            "ftp://download.nvidia.com/x.run",
        ] {
            assert!(validate_url(bad).is_err(), "must refuse {bad}");
        }
        assert!(validate_url("https://download.nvidia.com/a/b.run").is_ok());
        assert!(validate_url("https://download.nvidia.com:443/a/b.run").is_ok());
    }

    // ── manifest / staleness ────────────────────────────────────────────────

    fn manifest(v: &str) -> Manifest {
        Manifest {
            driver_version: v.to_string(),
            sha256: "abc".into(),
            url: run_url(v),
            provisioned_at_unix: 1,
            agent_version: "test".into(),
            lib64_count: 40,
            lib32_count: 12,
            layout_version: CURRENT_LAYOUT_VERSION,
        }
    }

    #[test]
    fn manifest_absent_present_stale_and_corrupt_are_distinguished() {
        let t = Tmp::new("manifest");
        assert_eq!(volume_state(&t.0, "610.57.04"), VolumeState::Empty);

        fs::write(
            t.0.join(layout::MANIFEST),
            serde_json::to_vec(&manifest("610.57.04")).unwrap(),
        )
        .unwrap();
        let st = volume_state(&t.0, "610.57.04");
        assert!(matches!(st, VolumeState::Current(_)));
        assert!(!st.needs_provision());
        assert!(st.usable().is_some());

        // Driver upgraded under the volume: the userspace must be rebuilt, not consumed.
        let st = volume_state(&t.0, "611.10");
        match &st {
            VolumeState::Stale { have, want, .. } => {
                assert_eq!(have, "610.57.04");
                assert_eq!(want, "611.10");
            }
            other => panic!("expected Stale, got {other:?}"),
        }
        assert!(st.needs_provision());
        assert!(
            st.usable().is_none(),
            "a stale volume must never be consumed"
        );

        fs::write(t.0.join(layout::MANIFEST), b"{not json").unwrap();
        let st = volume_state(&t.0, "610.57.04");
        assert!(matches!(st, VolumeState::Unreadable(_)));
        assert!(st.needs_provision());
        assert!(st.usable().is_none());
    }

    // ── the decision table ──────────────────────────────────────────────────

    #[test]
    fn provisioning_decision_table() {
        let gap = Gap {
            egl: true,
            lib32: true,
        };
        let none = Gap::default();
        let empty = VolumeState::Empty;
        let current = VolumeState::Current(manifest("610.57.04"));

        // The one case that provisions.
        assert!(decide(true, gap, false, true, &empty).is_ok());
        // A host with its own driver never provisions, even with an empty volume.
        assert!(decide(true, none, false, true, &empty).is_err());
        assert!(decide(true, none, false, true, &current).is_err());
        // Non-NVIDIA, opted out, volume not mounted.
        assert!(decide(false, gap, false, true, &empty).is_err());
        assert!(decide(true, gap, true, true, &empty).is_err());
        assert!(decide(true, gap, false, false, &empty).is_err());
        // A lib32-only gap is enough to provision.
        assert!(decide(
            true,
            Gap {
                egl: false,
                lib32: true
            },
            false,
            true,
            &empty
        )
        .is_ok());
    }

    #[test]
    fn restart_is_required_only_for_an_egl_gap() {
        assert!(
            Gap {
                egl: true,
                lib32: false
            }
            .egl
        );
        assert!(
            !Gap {
                egl: false,
                lib32: true
            }
            .egl
        );
        assert!(!Gap::default().any());
    }

    // ── extraction layout mapping ───────────────────────────────────────────

    /// A fake `--extract-only` tree in the shape the real installer produces.
    fn fake_extract(tag: &str) -> Tmp {
        let t = Tmp::new(tag);
        for f in [
            "libEGL_nvidia.so.610.57.04",
            "libGLX_nvidia.so.610.57.04",
            "libnvidia-eglcore.so.610.57.04",
            "libnvidia-glcore.so.610.57.04",
            "libnvidia-glsi.so.610.57.04",
            "libnvidia-tls.so.610.57.04",
            "libnvidia-allocator.so.610.57.04",
            "libnvidia-egl-wayland.so.1.1.13",
            "libnvidia-gpucomp.so.610.57.04",
            "libcuda.so.610.57.04",
        ] {
            t.file(f, "\x7fELF");
        }
        // The glvnd dispatch layer + OpenCL ICD loader the .run also ships; these must
        // never reach the volume. `libEGL.so.610.57.04` is NVIDIA's legacy pre-glvnd
        // EGL, same SONAME as the dispatcher and the one ldconfig -n picked.
        for f in [
            "libEGL.so.1.1.0",
            "libEGL.so.610.57.04",
            "libGLdispatch.so.0",
            "libGL.so.1.7.0",
            "libGLX.so.0",
            "libOpenGL.so.0",
            "libGLESv1_CM.so.1.2.0",
            "libGLESv2.so.2.1.0",
            "libOpenCL.so.1.0.0",
        ] {
            t.file(f, "\x7fELF");
        }
        for f in [
            "32/libEGL_nvidia.so.610.57.04",
            "32/libGLX_nvidia.so.610.57.04",
            "32/libnvidia-glcore.so.610.57.04",
            "32/libEGL.so.610.57.04",
            "32/libGLdispatch.so.0",
        ] {
            t.file(f, "\x7fELF");
        }
        t.file(
            "10_nvidia_wayland.json",
            r#"{"file_format_version":"1.0.0","ICD":{"library_path":"libnvidia-egl-wayland.so.1"}}"#,
        );
        t.file(
            "15_nvidia_gbm.json",
            r#"{"file_format_version":"1.0.0","ICD":{"library_path":"libnvidia-egl-gbm.so.1"}}"#,
        );
        // Verbatim shape of the real installer's json.
        t.file(
            "nvidia_icd.json",
            r#"{"file_format_version":"1.0.1","ICD":{"library_path":"libGLX_nvidia.so.0","api_version":"1.4.341"}}"#,
        );
        // Noise that must NOT be swept into the library set.
        t.file("nvidia-installer", "#!/bin/sh");
        t.file("README.txt", "hi");
        t.file("kernel/nvidia.ko.stub", "not a userspace lib");
        t
    }

    #[test]
    fn extract_tree_maps_onto_the_volume_layout() {
        let t = fake_extract("classify");
        let pop = classify_extract_tree(&t.0).unwrap();

        assert_eq!(pop.lib64.len(), 10, "{:?}", pop.lib64);
        assert_eq!(pop.lib32.len(), 3, "{:?}", pop.lib32);
        // Non-libraries and the kernel subtree are excluded.
        assert!(!pop.lib64.iter().any(|n| n == "nvidia-installer"));
        assert!(!pop.lib64.iter().any(|n| n.contains("nvidia.ko")));
        assert_eq!(
            pop.egl_vendor_lib.as_deref(),
            Some("libEGL_nvidia.so.610.57.04")
        );
        assert_eq!(pop.vulkan_icd_json.as_deref(), Some("nvidia_icd.json"));
        assert_eq!(
            pop.gbm_backend.as_deref(),
            Some("libnvidia-allocator.so.610.57.04")
        );
        assert_eq!(
            pop.external_platform,
            vec!["10_nvidia_wayland.json", "15_nvidia_gbm.json"]
        );
    }

    /// The volume must carry vendor libraries only: the installer's glvnd dispatch
    /// layer on `LD_LIBRARY_PATH` shadows the image's libglvnd, and NVIDIA's legacy
    /// `libEGL.so.<ver>` then wins `libEGL.so.1` and panics the compositor. See
    /// [`VENDOR_NEUTRAL_LIB_BASES`].
    #[test]
    fn vendor_neutral_dispatch_libraries_never_enter_the_volume() {
        let t = fake_extract("dispatch");
        let pop = classify_extract_tree(&t.0).unwrap();

        for banned in [
            "libEGL.so.1.1.0",
            "libEGL.so.610.57.04", // the legacy pre-glvnd EGL, the actual culprit
            "libGLdispatch.so.0",
            "libGL.so.1.7.0",
            "libGLX.so.0",
            "libOpenGL.so.0",
            "libGLESv1_CM.so.1.2.0",
            "libGLESv2.so.2.1.0",
            "libOpenCL.so.1.0.0",
        ] {
            assert!(
                !pop.lib64.contains(&banned.to_string()),
                "{banned} is a vendor-NEUTRAL dispatch/loader library and must stay the image's: \
                 {:?}",
                pop.lib64
            );
            assert!(
                !pop.lib32.contains(&banned.to_string()),
                "{banned} must be excluded from lib32 too: {:?}",
                pop.lib32
            );
        }

        // …while every vendor library is kept. The prefix trap: matching on
        // `starts_with("libEGL")` would silently drop the vendor's EGL implementation.
        for kept in [
            "libEGL_nvidia.so.610.57.04",
            "libGLX_nvidia.so.610.57.04",
            "libnvidia-eglcore.so.610.57.04",
            "libnvidia-glcore.so.610.57.04",
            "libnvidia-glsi.so.610.57.04",
            "libnvidia-tls.so.610.57.04",
            "libnvidia-gpucomp.so.610.57.04",
            "libnvidia-egl-wayland.so.1.1.13",
            "libcuda.so.610.57.04",
        ] {
            assert!(
                pop.lib64.contains(&kept.to_string()),
                "{kept} is a vendor library and MUST be in the volume: {:?}",
                pop.lib64
            );
        }
        assert!(pop
            .lib32
            .contains(&"libEGL_nvidia.so.610.57.04".to_string()));
        // The vendor json must still point at the vendor's EGL, not at nothing.
        assert_eq!(
            pop.egl_vendor_lib.as_deref(),
            Some("libEGL_nvidia.so.610.57.04")
        );
    }

    #[test]
    fn so_base_matching_distinguishes_vendor_from_dispatch() {
        assert!(is_vendor_neutral_dispatch("libEGL.so.1"));
        assert!(is_vendor_neutral_dispatch("libEGL.so.610.57.04"));
        assert!(is_vendor_neutral_dispatch("libGLdispatch.so.0"));
        assert!(!is_vendor_neutral_dispatch("libEGL_nvidia.so.0"));
        assert!(!is_vendor_neutral_dispatch("libGLX_nvidia.so.610.57.04"));
        assert!(!is_vendor_neutral_dispatch("libGLESv2_nvidia.so.2"));
        assert!(!is_vendor_neutral_dispatch(
            "libnvidia-eglcore.so.610.57.04"
        ));
        assert!(!is_vendor_neutral_dispatch(
            "libglxserver_nvidia.so.610.57.04"
        ));
    }

    /// A generation-0 volume (no `layout_version`) is harmful, not merely old: it must
    /// be re-provisioned automatically, with no `docker volume rm` by hand.
    #[test]
    fn a_pre_fix_volume_is_rejected_and_re_provisioned() {
        let t = Tmp::new("layout");
        // Right driver version, no `layout_version` key at all.
        let old = serde_json::json!({
            "driver_version": "610.57.04",
            "sha256": "abc",
            "url": run_url("610.57.04"),
            "provisioned_at_unix": 1,
            "agent_version": "0.1.0",
            "lib64_count": 53,
            "lib32_count": 35,
        });
        fs::write(
            t.0.join(layout::MANIFEST),
            serde_json::to_vec(&old).unwrap(),
        )
        .unwrap();

        let st = volume_state(&t.0, "610.57.04");
        match &st {
            VolumeState::ObsoleteLayout { have, want } => {
                assert_eq!(*have, 0);
                assert_eq!(*want, CURRENT_LAYOUT_VERSION);
            }
            other => panic!("a pre-fix volume must be rejected, got {other:?}"),
        }
        assert!(st.needs_provision());
        assert!(
            st.usable().is_none(),
            "a pre-fix volume must never be adopted — it breaks EGL"
        );
        // …and with a gap the decision table says provision.
        assert!(decide(
            true,
            Gap {
                egl: true,
                lib32: true
            },
            false,
            true,
            &st
        )
        .is_ok());
    }

    // ── EGL runtime self-test verdicts ──────────────────────────────────────

    /// The self-test's whole job is telling these four states apart.
    #[test]
    fn egl_selftest_verdicts() {
        let good = parse_egl_selftest(
            "LOADED=/usr/lib64/libEGL.so.1.1.0\n\
             EXTENSIONS=EGL_EXT_device_base EGL_EXT_device_enumeration EGL_EXT_platform_wayland\n\
             VENDOR=ok\n",
        );
        assert_eq!(
            good,
            EglRuntime::Ok {
                loaded: "/usr/lib64/libEGL.so.1.1.0".into()
            }
        );
        assert!(!good.is_broken());

        // The string a generation-0 volume on LD_LIBRARY_PATH produces: plausible, with
        // no device enumeration.
        let shadowed = parse_egl_selftest(
            "LOADED=/opt/quasar/nvidia-driver/lib64/libEGL.so.610.57.04\n\
             EXTENSIONS=EGL_KHR_client_get_all_proc_addresses EGL_EXT_client_extensions \
             EGL_EXT_platform_base EGL_EXT_device_base EGL_KHR_debug EGL_KHR_display_reference \
             EGL_KHR_platform_x11 EGL_EXT_platform_x11 EGL_EXT_platform_device \
             EGL_MESA_platform_surfaceless EGL_EXT_explicit_device\n",
        );
        match &shadowed {
            EglRuntime::Broken { detail, loaded } => {
                assert!(detail.contains("EGL_EXT_device_enumeration"), "{detail}");
                assert_eq!(
                    loaded.as_deref(),
                    Some("/opt/quasar/nvidia-driver/lib64/libEGL.so.610.57.04")
                );
            }
            other => panic!("a shadowed dispatcher must be Broken, got {other:?}"),
        }

        // An empty string means the vendor config points only at missing files.
        let empty = parse_egl_selftest("LOADED=/usr/lib64/libEGL.so.1.1.0\nEXTENSIONS=\n");
        assert!(matches!(&empty, EglRuntime::Broken { detail, .. } if detail.contains("EMPTY")));

        // Vendor library present but unloadable: glvnd silently skips it, so it must be
        // caught here.
        let vend = parse_egl_selftest(
            "LOADED=/usr/lib64/libEGL.so.1.1.0\n\
             EXTENSIONS=EGL_EXT_device_enumeration\n\
             VENDOR_ERROR=libnvidia-gpucomp.so.610.57.04: cannot open shared object file\n",
        );
        assert!(
            matches!(&vend, EglRuntime::Broken { detail, .. } if detail.contains("gpucomp")),
            "{vend:?}"
        );

        // A crashed child produces nothing: not a pass, and not `Broken` either.
        assert!(!matches!(parse_egl_selftest(""), EglRuntime::Ok { .. }));
    }

    /// An infrastructure failure of the self-test (crashed child, no verdict line, and
    /// in production the 20s timeout) must be `Indeterminate`, never `Broken`. `Broken`
    /// is what the veto acts on: it becomes a provisioning gap, a 350 MB download and a
    /// session-killing restart on a healthy host.
    #[test]
    fn an_infrastructure_failure_of_the_self_test_is_indeterminate_not_broken() {
        for (label, stdout) in [
            ("no output at all (killed before it printed)", ""),
            (
                "resolved the library then died before querying",
                "LOADED=/usr/lib64/libEGL.so.1.1.0\n",
            ),
            ("truncated/garbage output", "something unexpected\n"),
        ] {
            let v = parse_egl_selftest(stdout);
            assert!(
                v.is_indeterminate(),
                "{label}: must be Indeterminate, got {v:?}"
            );
            assert!(
                !v.is_broken(),
                "{label}: a probe that could not answer must NEVER read as a broken driver — \
                 that is the chain that provisions on a healthy host (#475)"
            );
        }

        // And a real verdict is still a real verdict.
        assert!(parse_egl_selftest("EXTENSIONS=\n").is_broken());
        assert!(parse_egl_selftest("DISPATCH_ERROR=no such file\n").is_broken());
    }

    /// Under `cargo test`, `probe_egl_runtime`'s `current_exe` is the TEST binary, so
    /// the child prints no `EXTENSIONS=` line — an infrastructure failure of the probe,
    /// and the verdict must be `Indeterminate`.
    #[test]
    fn a_self_test_that_cannot_run_yields_indeterminate_from_the_live_probe() {
        let v = probe_egl_runtime(None);
        assert!(
            v.is_indeterminate(),
            "a self-test child that produces no verdict must be Indeterminate, got {v:?}"
        );
    }

    // ── installer integrity (#476) ──────────────────────────────────────────

    /// The reviewed digest compiled into the agent outranks everything: it is the only
    /// control that covers the FIRST provision of a version on a fresh host.
    #[test]
    fn a_reviewed_digest_decides_and_a_mismatch_is_refused() {
        let mut pins = DigestPins::new();
        // A stale host pin cannot override the reviewed one.
        pins.insert("610.57.04".into(), "bbbb".into());

        assert_eq!(
            check_installer_digest(Some("aaaa"), &pins, "610.57.04", "aaaa", false, false).unwrap(),
            Trust::Reviewed
        );

        let err = check_installer_digest(Some("aaaa"), &pins, "610.57.04", "bbbb", true, true)
            .expect_err("a payload that misses the reviewed digest MUST be refused");
        let msg = format!("{err:#}");
        assert!(
            msg.contains("REFUSING") && msg.contains("aaaa") && msg.contains("bbbb"),
            "{msg}"
        );
        // Neither operator escape hatch may override a reviewed-digest mismatch.
    }

    /// Fail closed: an unreviewed, unpinned version is a refusal, not a new pin. Silent
    /// trust-on-first-use let a substituted `.run` — a shell script run with the agent's
    /// privileges — become the pin.
    #[test]
    fn an_unpinned_version_is_refused_unless_the_operator_says_otherwise() {
        let pins = DigestPins::new();
        assert!(
            reviewed_digest("610.57.04").is_none(),
            "test assumes no reviewed pin"
        );

        let err = check_installer_digest(None, &pins, "610.57.04", "aaaa", false, false)
            .expect_err("an unvouched-for payload MUST be refused");
        let msg = format!("{err:#}");
        assert!(msg.contains("REFUSING"), "{msg}");
        // The refusal has to be actionable: every way out is named.
        for hint in [
            STAGED_RUN_VAR,
            "REVIEWED_DRIVER_DIGESTS",
            "QUASAR_NVIDIA_DRIVER_TRUST_ON_FIRST_USE",
            "QUASAR_NVIDIA_DRIVER_VOLUME=0",
        ] {
            assert!(msg.contains(hint), "the refusal must name {hint}: {msg}");
        }

        // Both opt-ins are the operator's, and each is enough on its own.
        assert_eq!(
            check_installer_digest(None, &pins, "610.57.04", "aaaa", true, false).unwrap(),
            Trust::TrustOnFirstUse
        );
        assert_eq!(
            check_installer_digest(None, &pins, "610.57.04", "aaaa", false, true).unwrap(),
            Trust::OperatorStaged
        );
    }

    /// Once a host has accepted a payload for a version, a change for that same version
    /// is refused — including on the operator-staged path.
    #[test]
    fn a_changed_digest_for_a_host_pinned_version_is_refused() {
        let mut pins = DigestPins::new();
        pins.insert("610.57.04".into(), "aaaa".into());

        assert_eq!(
            check_installer_digest(None, &pins, "610.57.04", "aaaa", false, false).unwrap(),
            Trust::HostPinned
        );

        for (tofu, staged) in [(false, false), (true, false), (false, true), (true, true)] {
            let err = check_installer_digest(None, &pins, "610.57.04", "bbbb", tofu, staged)
                .expect_err("a changed digest for a pinned version MUST be refused");
            let msg = format!("{err:#}");
            assert!(
                msg.contains("REFUSING") && msg.contains("aaaa") && msg.contains("bbbb"),
                "{msg}"
            );
        }

        // A different driver version is a different pin, and still needs its own opt-in.
        assert!(check_installer_digest(None, &pins, "575.64", "cccc", false, false).is_err());
        assert!(check_installer_digest(None, &pins, "575.64", "cccc", true, false).is_ok());
    }

    #[test]
    fn digest_pins_persist_in_the_volume_across_provisions() {
        let t = Tmp::new("pins");
        assert!(read_digest_pins(&t.0).unwrap().is_empty());

        write_digest_pin(&t.0, "610.57.04", "aaaa").unwrap();
        write_digest_pin(&t.0, "575.64", "cccc").unwrap();
        let pins = read_digest_pins(&t.0).unwrap();
        assert_eq!(pins.get("610.57.04").map(String::as_str), Some("aaaa"));
        assert_eq!(pins.get("575.64").map(String::as_str), Some("cccc"));

        // A re-provision of one version must not drop the other version's pin.
        assert!(check_installer_digest(None, &pins, "575.64", "dddd", true, true).is_err());
    }

    /// Fail CLOSED on a corrupt pin file. Degrading it to "no pins" is how an integrity
    /// check quietly stops checking, and it re-pins from whatever arrives next.
    #[test]
    fn a_corrupt_pin_file_is_a_refusal_not_an_empty_pin_set() {
        let t = Tmp::new("corrupt-pins");
        write_digest_pin(&t.0, "610.57.04", "aaaa").unwrap();
        fs::write(t.0.join(layout::DIGESTS), b"{not json").unwrap();

        let err = read_digest_pins(&t.0).expect_err("a corrupt pin file MUST NOT read as empty");
        let msg = format!("{err:#}");
        assert!(msg.contains("REFUSING"), "{msg}");
        assert!(
            msg.contains(layout::DIGESTS),
            "the refusal must name the file to inspect: {msg}"
        );

        // An ABSENT file is still the honest "nothing pinned yet".
        fs::remove_file(t.0.join(layout::DIGESTS)).unwrap();
        assert!(read_digest_pins(&t.0).unwrap().is_empty());
    }

    /// Every reviewed entry must be a real sha256 for a real driver version, or the
    /// table silently stops matching anything.
    #[test]
    fn the_reviewed_digest_table_is_well_formed() {
        for (version, digest) in REVIEWED_DRIVER_DIGESTS {
            assert_eq!(
                parse_driver_version(version).as_deref(),
                Some(*version),
                "{version} is not a driver version this agent can resolve"
            );
            assert_eq!(digest.len(), 64, "{version}: {digest} is not a sha256");
            assert!(
                digest
                    .chars()
                    .all(|c| c.is_ascii_hexdigit() && !c.is_uppercase()),
                "{version}: {digest} must be lowercase hex"
            );
            assert_eq!(reviewed_digest(version), Some(*digest));
        }
    }

    // ── failure backoff + resource preflight (#477) ─────────────────────────

    /// Without this, an agent crash-looping for an unrelated reason is a
    /// repeated 350 MB download against download.nvidia.com.
    #[test]
    fn repeated_failures_back_off_and_a_version_change_resets_them() {
        let none = Attempts::default();
        assert!(backoff_remaining(&none, "610.57.04", 1_000).is_none());

        let mut a = Attempts {
            version: "610.57.04".into(),
            attempts: 1,
            last_attempt_unix: 1_000,
            last_error: "boom".into(),
        };
        // Straight after the first failure: wait the base interval.
        let w1 = backoff_remaining(&a, "610.57.04", 1_000).expect("must back off");
        assert_eq!(w1, PROVISION_BACKOFF_BASE);
        // Once the interval has elapsed: free to retry.
        assert!(
            backoff_remaining(&a, "610.57.04", 1_000 + PROVISION_BACKOFF_BASE.as_secs()).is_none()
        );

        // It grows.
        a.attempts = 3;
        let w3 = backoff_remaining(&a, "610.57.04", 1_000).unwrap();
        assert_eq!(w3, PROVISION_BACKOFF_BASE * 4);

        // …and is capped, rather than becoming "never again".
        a.attempts = 99;
        assert_eq!(
            backoff_remaining(&a, "610.57.04", 1_000).unwrap(),
            PROVISION_BACKOFF_MAX
        );

        // A driver upgrade is a different problem: the old version's failures must not
        // hold the new one hostage.
        assert!(backoff_remaining(&a, "615.10", 1_000).is_none());
    }

    #[test]
    fn attempts_are_counted_before_the_download_and_cleared_on_success() {
        let t = Tmp::new("attempts");
        assert_eq!(read_attempts(&t.0).attempts, 0);

        note_attempt(&t.0, "610.57.04");
        note_attempt(&t.0, "610.57.04");
        let a = read_attempts(&t.0);
        assert_eq!(a.attempts, 2);
        assert_eq!(a.version, "610.57.04");

        note_failure(&t.0, "extract failed");
        assert_eq!(read_attempts(&t.0).last_error, "extract failed");

        // A new driver version restarts the count at 1.
        note_attempt(&t.0, "615.10");
        assert_eq!(read_attempts(&t.0).attempts, 1);

        clear_attempts(&t.0);
        assert_eq!(read_attempts(&t.0).attempts, 0);
    }

    /// 2 GB+ is written inside the volume, i.e. into the docker data root.
    #[test]
    fn a_full_filesystem_is_refused_before_anything_is_downloaded() {
        assert!(free_space_verdict(REQUIRED_FREE_BYTES).is_ok());
        let err = free_space_verdict(REQUIRED_FREE_BYTES - 1).expect_err("must refuse");
        assert!(format!("{err:#}").contains("free space"), "{err:#}");
        // A real path answers, a nonexistent one does not; `check_free_space` fails
        // open on `None`.
        let t = Tmp::new("statvfs");
        assert!(free_bytes(&t.0).is_some());
        assert!(free_bytes(Path::new("/definitely/not/a/path/quasar")).is_none());
    }

    /// Cleaning scratch only on success leaks ~1.8 GB onto the filesystem whose
    /// exhaustion most likely caused the failure.
    #[test]
    fn scratch_is_removed_on_the_failure_path_too() {
        let t = Tmp::new("scratch");
        let scratch = t.0.join(layout::SCRATCH);
        fs::create_dir_all(scratch.join("NVIDIA-Linux-x86_64-610.57.04")).unwrap();
        fs::write(scratch.join("big.run"), b"payload").unwrap();

        let failed: Result<()> = (|| {
            let _guard = ScratchGuard(scratch.clone());
            assert!(scratch.is_dir());
            bail!("extract failed halfway through")
        })();
        assert!(failed.is_err());
        assert!(
            !scratch.exists(),
            "the scratch tree must be gone after an early return, not only after success"
        );
    }

    // ── download-path correctness ───────────────────────────────────────────

    #[test]
    fn a_truncated_download_is_refused_rather_than_executed() {
        assert!(verify_complete(400, Some(400)).is_ok());
        // No Content-Length ⇒ nothing to compare against.
        assert!(verify_complete(400, None).is_ok());
        let err = verify_complete(399, Some(400)).expect_err("a short body must be refused");
        let msg = format!("{err:#}");
        assert!(msg.contains("short download"), "{msg}");
        assert!(
            msg.contains("399") && msg.contains("400"),
            "the error must name what arrived vs what was promised: {msg}"
        );
    }

    /// The Location-resolution half; the host pin per hop is
    /// `only_https_download_nvidia_com_is_accepted`.
    #[test]
    fn redirect_locations_resolve_absolutely_and_stay_refusable() {
        let base = run_url("610.57.04");
        assert_eq!(
            join_redirect(&base, "https://download.nvidia.com/cdn/x.run").unwrap(),
            "https://download.nvidia.com/cdn/x.run"
        );
        assert_eq!(
            join_redirect(&base, "/cdn/x.run").unwrap(),
            "https://download.nvidia.com/cdn/x.run"
        );
        // An off-host hop resolves, then validate_url refuses it: the pin applies per
        // hop, not only to hop 0.
        let off = join_redirect(&base, "https://evil.example/x.run").unwrap();
        assert!(validate_url(&off).is_err());
        // Shapes refused rather than resolved wrongly.
        assert!(join_redirect(&base, "//evil.example/x.run").is_err());
        assert!(join_redirect(&base, "x.run").is_err());
    }

    #[test]
    fn an_unrecognisable_extract_tree_is_an_error_not_an_empty_volume() {
        let t = Tmp::new("junk");
        t.file("README", "nothing here");
        assert!(classify_extract_tree(&t.0).is_err());
    }

    #[test]
    fn populate_places_libs_and_writes_absolute_discovery_configs() {
        let src = fake_extract("populate-src");
        let vol = Tmp::new("populate-vol");
        let pop = classify_extract_tree(&src.0).unwrap();
        let (n64, n32) = populate_volume(&vol.0, &src.0, &pop, "610.57.04").unwrap();
        assert_eq!((n64, n32), (10, 3));

        assert!(vol.0.join("lib64/libEGL_nvidia.so.610.57.04").is_file());
        // …and the dispatch layer is absent from the written volume, not just the plan.
        assert!(!vol.0.join("lib64/libEGL.so.610.57.04").exists());
        assert!(!vol.0.join("lib64/libGLdispatch.so.0").exists());
        assert!(!vol.0.join("lib32/libGLdispatch.so.0").exists());
        assert!(vol.0.join("lib32/libGLX_nvidia.so.610.57.04").is_file());
        assert!(vol.0.join("gbm/nvidia-drm_gbm.so").is_file());

        // The EGL vendor json must carry an absolute path into the volume: it is
        // dlopen'd directly.
        let egl: serde_json::Value =
            serde_json::from_str(&fs::read_to_string(vol.0.join(layout::EGL_VENDOR_JSON)).unwrap())
                .unwrap();
        let p = egl["ICD"]["library_path"].as_str().unwrap();
        assert!(p.starts_with(vol.0.to_str().unwrap()), "{p}");
        assert!(p.ends_with("lib64/libEGL_nvidia.so.610.57.04"), "{p}");

        // The Vulkan ICD is the installer's own file with only the path made absolute:
        // its api_version / file_format_version must survive untouched.
        let icd: serde_json::Value =
            serde_json::from_str(&fs::read_to_string(vol.0.join(layout::VULKAN_ICD_JSON)).unwrap())
                .unwrap();
        assert_eq!(icd["file_format_version"], "1.0.1");
        assert_eq!(icd["ICD"]["api_version"], "1.4.341");
        let icd_path = icd["ICD"]["library_path"].as_str().unwrap();
        assert!(icd_path.starts_with(vol.0.to_str().unwrap()), "{icd_path}");
        assert!(icd_path.ends_with("lib64/libGLX_nvidia.so.0"), "{icd_path}");

        // External-platform configs are rewritten, not copied verbatim.
        let wl: serde_json::Value = serde_json::from_str(
            &fs::read_to_string(vol.0.join("egl_external_platform.d/10_nvidia_wayland.json"))
                .unwrap(),
        )
        .unwrap();
        assert!(wl["ICD"]["library_path"]
            .as_str()
            .unwrap()
            .starts_with(vol.0.to_str().unwrap()));

        let conf = fs::read_to_string(vol.0.join(layout::LD_CONF)).unwrap();
        assert!(conf.contains("lib64"));
        assert!(conf.contains("lib32"));
    }

    /// A re-provision must not leave the old version's libraries behind: ldconfig would
    /// index both and the loader could bind a stale eglcore against a new module.
    #[test]
    fn reprovision_clears_the_previous_library_set() {
        let src = fake_extract("reprov-src");
        let vol = Tmp::new("reprov-vol");
        fs::create_dir_all(vol.0.join("lib64")).unwrap();
        fs::write(vol.0.join("lib64/libEGL_nvidia.so.570.86"), "old").unwrap();

        let pop = classify_extract_tree(&src.0).unwrap();
        populate_volume(&vol.0, &src.0, &pop, "610.57.04").unwrap();
        assert!(!vol.0.join("lib64/libEGL_nvidia.so.570.86").exists());
        assert!(vol.0.join("lib64/libEGL_nvidia.so.610.57.04").is_file());
    }

    #[test]
    fn external_platform_rewrite_absolutises_only_bare_sonames() {
        let dir = Path::new("/vol/lib64");
        let out = rewrite_icd_library_path_body(
            r#"{"ICD":{"library_path":"libnvidia-egl-wayland.so.1"}}"#,
            dir,
        )
        .unwrap();
        assert!(out.contains("/vol/lib64/libnvidia-egl-wayland.so.1"));

        // An already-absolute path is left alone.
        let out = rewrite_icd_library_path_body(
            r#"{"ICD":{"library_path":"/usr/lib64/libfoo.so.1"}}"#,
            dir,
        )
        .unwrap();
        assert!(out.contains("/usr/lib64/libfoo.so.1"));
        assert!(!out.contains("/vol/lib64/usr"));

        assert!(rewrite_icd_library_path_body("{}", dir).is_err());
    }

    // ── env injection safety ────────────────────────────────────────────────

    #[test]
    fn process_env_preserves_the_system_search_paths() {
        let vol = Tmp::new("env");
        fs::create_dir_all(vol.0.join(layout::EGL_VENDOR_DIR)).unwrap();
        fs::create_dir_all(vol.0.join(layout::EGL_EXTERNAL_DIR)).unwrap();
        fs::create_dir_all(vol.0.join(layout::VULKAN_ICD_DIR)).unwrap();
        fs::write(vol.0.join(layout::VULKAN_ICD_JSON), "{}").unwrap();

        let env: BTreeMap<String, String> = process_env(&vol.0).into_iter().collect();

        // glvnd's *_DIRS replaces the default search, so the system dirs must be
        // carried along or Mesa disappears on a host that also has it.
        let dirs = &env["__EGL_VENDOR_LIBRARY_DIRS"];
        assert!(dirs.starts_with(vol.0.to_str().unwrap()), "{dirs}");
        assert!(dirs.contains("/usr/share/glvnd/egl_vendor.d"), "{dirs}");
        assert!(dirs.contains("/etc/glvnd/egl_vendor.d"), "{dirs}");

        // The FILENAMES form would replace the search with one file; never emitted.
        assert!(!env.contains_key("__EGL_VENDOR_LIBRARY_FILENAMES"));

        // Vulkan: the ADD form, never the replacing forms.
        assert!(env.contains_key("VK_ADD_DRIVER_FILES"));
        assert!(!env.contains_key("VK_ICD_FILENAMES"));
        assert!(!env.contains_key("VK_DRIVER_FILES"));

        // LD_LIBRARY_PATH can only come from compose (the loader latches it at exec),
        // so it must never appear here pretending to work.
        assert!(!env.contains_key("LD_LIBRARY_PATH"));

        // GBM_BACKENDS_PATH replaces Mesa's dir: only when a backend is present.
        assert!(!env.contains_key("GBM_BACKENDS_PATH"));
        fs::create_dir_all(vol.0.join(layout::GBM_DIR)).unwrap();
        fs::write(vol.0.join("gbm/nvidia-drm_gbm.so"), "x").unwrap();
        let env: BTreeMap<String, String> = process_env(&vol.0).into_iter().collect();
        assert_eq!(
            env["GBM_BACKENDS_PATH"],
            vol.0.join("gbm").to_str().unwrap()
        );
    }

    #[test]
    fn process_env_is_empty_for_an_unpopulated_volume() {
        let vol = Tmp::new("env-empty");
        assert!(process_env(&vol.0).is_empty());
    }

    // ── app-container wiring ────────────────────────────────────────────────

    fn info(host: Option<&str>, lib32_count: usize) -> VolumeInfo {
        VolumeInfo {
            local: PathBuf::from(VOLUME_MOUNT),
            host: host.map(PathBuf::from),
            name: Some("deploy_quasar-nvidia-driver".into()),
            manifest: Manifest {
                lib32_count,
                ..manifest("610.57.04")
            },
        }
    }

    /// A host with its own driver must produce zero extra arguments.
    #[test]
    fn native_driver_host_gets_no_app_container_changes() {
        assert!(app_container_args(None, "/opt/quasar/nvidia-driver", "").is_empty());
        assert!(lib32_host_path(None).is_none());
    }

    #[test]
    fn provisioned_host_mounts_the_volume_and_wires_discovery() {
        let i = info(
            Some("/var/lib/docker/volumes/deploy_quasar-nvidia-driver/_data"),
            12,
        );
        let args = app_container_args(Some(&i), "/opt/quasar/nvidia-driver", "");
        let joined = args.join(" ");
        assert!(joined.contains(
            "--mount type=volume,src=deploy_quasar-nvidia-driver,dst=/opt/quasar/nvidia-driver,readonly"
        ), "{joined}");
        assert!(
            joined.contains("LD_LIBRARY_PATH=/opt/quasar/nvidia-driver/lib64"),
            "{joined}"
        );
        assert!(
            joined.contains(
                "__EGL_VENDOR_LIBRARY_DIRS=/opt/quasar/nvidia-driver/glvnd/egl_vendor.d:/etc/glvnd"
            ),
            "{joined}"
        );
        assert!(
            joined.contains(
                "VK_ADD_DRIVER_FILES=/opt/quasar/nvidia-driver/vulkan/icd.d/nvidia_icd.json"
            ),
            "{joined}"
        );
    }

    /// `-e LD_LIBRARY_PATH=` REPLACES the image's value, so the image's own entry must
    /// be appended.
    #[test]
    fn app_container_ld_library_path_appends_the_images_own_value() {
        let i = info(Some("/host/vol"), 12);
        let args = app_container_args(
            Some(&i),
            "/opt/quasar/nvidia-driver",
            "/opt/app/lib:/usr/local/lib",
        );
        let ld = args
            .iter()
            .find(|a| a.starts_with("LD_LIBRARY_PATH="))
            .expect("LD_LIBRARY_PATH arg");
        assert_eq!(
            ld,
            "LD_LIBRARY_PATH=/opt/quasar/nvidia-driver/lib64:/opt/app/lib:/usr/local/lib"
        );
    }

    /// An app container must get GBM discovery, not just EGL/Vulkan — see
    /// [`app_container_args`] for what a missing `GBM_BACKENDS_PATH` costs.
    #[test]
    fn app_container_gets_the_gbm_backend_path() {
        let vol = Tmp::new("appgbm");
        vol.file(&format!("{}/nvidia-drm_gbm.so", layout::GBM_DIR), "elf");
        let i = VolumeInfo {
            local: vol.0.clone(),
            host: Some(PathBuf::from("/host/vol")),
            name: None,
            manifest: manifest("610.57.04"),
        };
        let args = app_container_args(Some(&i), "/opt/quasar/nvidia-driver", "");
        assert!(
            args.iter()
                .any(|a| a == "GBM_BACKENDS_PATH=/opt/quasar/nvidia-driver/gbm"),
            "{args:?}"
        );
    }

    /// `GBM_BACKENDS_PATH` REPLACES Mesa's backend directory, so a volume with no
    /// backend must not get the variable. Mirrors `process_env`'s guard.
    #[test]
    fn app_container_omits_gbm_path_when_the_volume_has_no_backend() {
        let vol = Tmp::new("appnogbm");
        let i = VolumeInfo {
            local: vol.0.clone(),
            host: Some(PathBuf::from("/host/vol")),
            name: None,
            manifest: manifest("610.57.04"),
        };
        let args = app_container_args(Some(&i), "/opt/quasar/nvidia-driver", "");
        assert!(
            !args.iter().any(|a| a.starts_with("GBM_BACKENDS_PATH=")),
            "{args:?}"
        );
    }

    /// Docker can attach a named volume even when its host storage path is unavailable.
    #[test]
    fn named_volume_does_not_require_a_host_storage_path() {
        let i = info(None, 12);
        assert!(!app_container_args(Some(&i), "/opt/quasar/nvidia-driver", "").is_empty());
        assert!(lib32_host_path(Some(&i)).is_none());
    }

    #[test]
    fn lib32_path_feeds_the_existing_375_mount_mechanism() {
        let i = info(Some("/host/vol"), 12);
        assert_eq!(
            lib32_host_path(Some(&i)).as_deref(),
            Some("/host/vol/lib32")
        );
        // …and is withheld with no 32-bit tree, so the mount is not pointed at an
        // empty directory.
        let none = info(Some("/host/vol"), 0);
        assert!(lib32_host_path(Some(&none)).is_none());
    }

    // ── lock ────────────────────────────────────────────────────────────────

    /// The mechanism is `artifact::tests`; this asserts only that the lock stays where
    /// consumers and the documented remediation expect it.
    #[test]
    fn the_provisioning_lock_lives_in_the_volume() {
        let t = Tmp::new("lock");
        let l = acquire_lock(&t.0).unwrap();
        let p = t.0.join(layout::LOCK);
        assert!(p.is_file());
        assert!(
            acquire_lock(&t.0).is_err(),
            "a second concurrent provision must be refused"
        );
        drop(l);
        assert!(!p.exists());
    }

    // ── misc ────────────────────────────────────────────────────────────────

    #[test]
    fn overlay_digests_are_not_container_ids() {
        let layer = "b".repeat(64);
        let id = "a".repeat(64);
        let body = format!("1 0 0:1 / / rw - overlay overlay rw,lowerdir=/layers/{layer}/diff\n2 1 8:1 /var/lib/docker/containers/{id}/hosts /etc/hosts rw - ext4 /dev/sda rw\n");
        assert_eq!(parse_container_id_from_mountinfo(&body), Some(id));
        assert_eq!(
            parse_container_id_from_mountinfo(&format!(
                "1 0 0:1 /layers/{layer}/diff /opt/data rw - ext4 /dev/sda rw"
            )),
            None
        );
    }

    #[test]
    fn unreadable_pin_store_does_not_reset_trust() {
        let dir = Tmp::new("pins-is-directory");
        std::fs::create_dir_all(dir.0.join(layout::DIGESTS)).unwrap();
        assert!(read_digest_pins(&dir.0).is_err());
    }

    #[test]
    fn container_id_is_recovered_from_mountinfo() {
        let id = "a".repeat(64);
        let body = format!(
            "1234 1200 0:59 / / rw - overlay overlay rw\n\
             1240 1234 0:60 /var/lib/docker/containers/{id}/resolv.conf /etc/resolv.conf rw - ext4 /dev/sda1 rw\n"
        );
        assert_eq!(
            parse_container_id_from_mountinfo(&body).as_deref(),
            Some(id.as_str())
        );
        assert_eq!(parse_container_id_from_mountinfo("1 2 0:1 / / rw\n"), None);
    }

    #[test]
    fn version_is_recoverable_from_the_url_for_the_404_message() {
        assert_eq!(
            version_from_url(&run_url("610.57.04")).as_deref(),
            Some("610.57.04")
        );
    }

    #[test]
    fn describe_names_the_volume_and_version() {
        let d = describe(&manifest("610.57.04"));
        assert!(d.contains("driver volume"));
        assert!(d.contains("610.57.04"));
    }
}
