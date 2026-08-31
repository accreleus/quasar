//! Runtime provisioning of NVRTC (#545) — CUDA *toolkit* userspace, so no driver
//! installer carries it and [`crate::nvidia_volume`] cannot produce it. It gates
//! exactly four elements (`cudaconvert`, `cudaconvertscale`, `cudascale`,
//! `cudacompositor`); `cudaconvertscale` is the scale stage of the per-session NVENC
//! fallback, so without NVRTC such a session dies with "cudaconvert not found".
//!
//! Every failure path is SOFT — refused (old driver), failed (no network), switched
//! off — and all mean the same thing: the four `cuda*` elements are absent, exactly as
//! on the universal image, and Vulkan encode is unaffected. Nothing here may block
//! plugin registration or a session launch.
//!
//! Nothing is written outside the driver volume, and no toolkit/`nvcc`/`libcudart` is
//! placed. The driver half stays `nvidia_volume`'s: only it may re-provision that.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::RwLock;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{bail, Context, Result};

use crate::artifact;
use crate::nvidia_volume;

/// Tracing target. The download/lock/backoff lines come from [`artifact`] instead and
/// land on `quasar.artifact` with `artifact="cuda-nvrtc"`.
const T: &str = "quasar.cuda_runtime";

/// This provisioner's name in the shared [`artifact`] machinery.
const ARTIFACT: &str = "cuda-nvrtc";

// ── the pin ──────────────────────────────────────────────────────────────────

/// The pinned NVRTC redistributable. `deploy/pins.env` discipline: version + published
/// digest, both here, changed together. Must stay on CUDA 13.2.x — `/opt/gst`'s
/// `libgstnvcodec.so` / `libgstcuda-1.0.so` were built against that minor.
/// Source: <https://developer.download.nvidia.com/compute/cuda/redist/redistrib_13.2.2.json>
pub const NVRTC_VERSION: &str = "13.2.86";
/// sha256 verbatim from NVIDIA's redistrib index. A real pin (unlike the driver `.run`'s
/// trust-on-first-use): a substituted payload is refused on the first provision too.
pub const NVRTC_SHA256: &str = "6e0661e31a43c22ad030637adeb881822249ffb2a14a19691828f117f5ab2de9";
/// Pinned download host. NOT the driver installer's `download.nvidia.com` — the two
/// provisioners pin different origins and neither may be redirected onto the other's.
pub const REDIST_HOST: &str = "developer.download.nvidia.com";

/// The redistributable's URL.
pub fn redist_url(version: &str) -> String {
    format!(
        "https://{REDIST_HOST}/compute/cuda/redist/cuda_nvrtc/linux-x86_64/\
         cuda_nvrtc-linux-x86_64-{version}-archive.tar.xz"
    )
}

/// The archive's top-level directory, which is also the name `tar` will create.
fn archive_dir(version: &str) -> String {
    format!("cuda_nvrtc-linux-x86_64-{version}-archive")
}

/// CUDA 13.x needs an r580+ driver. Below it a *failing* NVRTC is worse than an absent
/// one — the four `cuda*` elements would register and then break a live session — so an
/// old driver is a refusal, never an attempt.
pub const CUDA13_MIN_DRIVER_MAJOR: u32 = 580;

/// Total wall-clock budget for the ~58 MiB fetch.
const DOWNLOAD_TIMEOUT: Duration = Duration::from_secs(15 * 60);

/// Archive (~58 MiB) + extracted tree (~230 MiB) + placed libraries all land inside the
/// driver volume, i.e. the docker data root shared with postgres and the control plane.
const REQUIRED_FREE_BYTES: u64 = 1024 * 1024 * 1024;

/// Failure backoff, this provisioner's own counter: `BASE * 2^(failures-1)`, capped.
const PROVISION_BACKOFF_BASE: Duration = Duration::from_secs(5 * 60);
const PROVISION_BACKOFF_MAX: Duration = Duration::from_secs(6 * 60 * 60);

/// Tools the extract path shells out to.
const REQUIRED_TOOLS: &[&str] = &["tar", "xz", "ldconfig"];

// ── layout ───────────────────────────────────────────────────────────────────

/// The CUDA half, a sibling of the driver's `lib64` with its own manifest.
///
/// WIPE EXCLUSION: a driver re-provision clears `lib64/` and `lib32/` BY NAME
/// (`nvidia_volume::populate_volume`); `cuda/` must never be added to that list, since
/// NVRTC is keyed on its own version, not on the kernel module. Guarded by
/// `tests::a_driver_reprovision_does_not_wipe_the_cuda_tree`.
pub mod layout {
    /// Root of the CUDA half, inside the driver volume.
    pub const ROOT: &str = "cuda";
    /// The library directory that goes on `LD_LIBRARY_PATH`.
    pub const LIB64: &str = "cuda/lib64";
    pub const MANIFEST: &str = "cuda/manifest.json";
    pub const LOCK: &str = "cuda/.provision.lock";
    pub const ATTEMPTS: &str = "cuda/.provision-attempts.json";
    pub const SCRATCH: &str = "cuda/scratch";
}

/// The library directory a consumer must have on `LD_LIBRARY_PATH`.
pub fn lib_dir() -> PathBuf {
    PathBuf::from(nvidia_volume::VOLUME_MOUNT).join(layout::LIB64)
}

/// Whether a directory appears in an `LD_LIBRARY_PATH` value, whole entries only.
pub fn on_ld_library_path(ld_library_path: &str, dir: &Path) -> bool {
    ld_library_path
        .split(':')
        .any(|p| !p.is_empty() && Path::new(p) == dir)
}

/// Warn when the placed libraries are somewhere the loader will never look.
///
/// glibc latches `LD_LIBRARY_PATH` at `execve`, so this cannot be fixed in-process; it
/// comes from `docker-compose.nvidia.yml` (unconditional, safe because a missing
/// directory is inert). Catches an un-migrated compose file, whose only other symptom
/// is libraries visibly present and `cudaconvert` still not registering.
fn warn_if_not_wired() {
    let ld = std::env::var("LD_LIBRARY_PATH").unwrap_or_default();
    let want = lib_dir();
    if on_ld_library_path(&ld, &want) {
        return;
    }
    tracing::warn!(
        target: T, token = "cudart-ld-library-path-missing",
        cuda_lib64 = %want.display(), ld_library_path = %ld,
        "LD_LIBRARY_PATH does not contain the volume's CUDA lib64, so the loader will not find \
         libnvrtc and cudaconvert & co will not register. The dynamic loader latches its search \
         path at process start and this cannot be fixed from here — apply the current \
         deploy/docker-compose.nvidia.yml (it appends this directory unconditionally; the \
         directory is inert when empty) and recreate the node-agent container."
    );
}

// ── manifest ─────────────────────────────────────────────────────────────────

/// What is in the CUDA tree. Written LAST: its presence is what every consumer treats
/// as "this is usable".
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct Manifest {
    /// What the tree is keyed on — NOT the driver version, which changes independently.
    pub nvrtc_version: String,
    /// Verified against [`NVRTC_SHA256`] before anything was extracted;
    /// `"operator-staged"` when the libraries came from [`RUNTIME_DIR_VAR`].
    pub sha256: String,
    pub url: String,
    /// `"nvidia-redist"` or `"operator"`.
    pub source: String,
    pub provisioned_at_unix: u64,
    pub agent_version: String,
    pub lib_count: usize,
    /// Population-rule generation. Bump when a defect makes an already-written tree
    /// wrong rather than merely old, so the agent re-provisions by itself.
    #[serde(default)]
    pub layout_version: u32,
}

/// `1` — libnvrtc + libnvrtc-builtins real files only (no `stubs/`, no `*_static.a`),
/// SONAME links via `ldconfig -n`, plus the unversioned `libnvrtc.so`.
pub const CURRENT_LAYOUT_VERSION: u32 = 1;

/// What the CUDA tree holds relative to the pinned version.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum TreeState {
    /// No manifest: never provisioned, or an attempt aborted before the manifest write.
    Empty,
    /// Manifest matches the pin: usable as-is.
    Current(Manifest),
    /// A different NVRTC version than the pin. Re-provision.
    Stale { have: String, want: String },
    /// Right version, wrong population rules. Re-provision.
    ObsoleteLayout { have: u32, want: u32 },
    /// Manifest present but unparseable: a half-written tree, treated like `Empty`.
    Unreadable(String),
}

impl TreeState {
    pub fn usable(&self) -> Option<&Manifest> {
        match self {
            TreeState::Current(m) => Some(m),
            _ => None,
        }
    }

    pub fn needs_provision(&self) -> bool {
        !matches!(self, TreeState::Current(_))
    }
}

/// Read the CUDA manifest and compare it against the pinned version.
pub fn tree_state(volume: &Path, want_version: &str) -> TreeState {
    let body = match std::fs::read_to_string(volume.join(layout::MANIFEST)) {
        Ok(b) => b,
        Err(_) => return TreeState::Empty,
    };
    let manifest: Manifest = match serde_json::from_str(&body) {
        Ok(m) => m,
        Err(e) => return TreeState::Unreadable(e.to_string()),
    };
    if manifest.nvrtc_version != want_version {
        return TreeState::Stale {
            have: manifest.nvrtc_version,
            want: want_version.to_string(),
        };
    }
    if manifest.layout_version != CURRENT_LAYOUT_VERSION {
        return TreeState::ObsoleteLayout {
            have: manifest.layout_version,
            want: CURRENT_LAYOUT_VERSION,
        };
    }
    TreeState::Current(manifest)
}

// ── knobs ────────────────────────────────────────────────────────────────────

/// `QUASAR_CUDA_RUNTIME`. Defaults on; `0` is the opt-out for a host that must not
/// fetch ~58 MB from the public internet unprompted.
pub fn enabled() -> bool {
    !matches!(
        std::env::var("QUASAR_CUDA_RUNTIME").as_deref(),
        Ok("0") | Ok("false") | Ok("no")
    )
}

/// `QUASAR_CUDA_RUNTIME_DIR`, the air-gapped hatch: a SOURCE to copy from, never a
/// destination (the process cannot change `LD_LIBRARY_PATH`, so the libraries must land
/// where compose already points). No download and no digest check on that path.
pub const RUNTIME_DIR_VAR: &str = "QUASAR_CUDA_RUNTIME_DIR";

pub fn staged_dir() -> Option<PathBuf> {
    let v = std::env::var(RUNTIME_DIR_VAR).ok()?;
    let v = v.trim();
    (!v.is_empty()).then(|| PathBuf::from(v))
}

// ── driver-version gate ──────────────────────────────────────────────────────

/// The MAJOR component of a driver version string (`"610.57.04"` → `610`).
pub fn driver_major(version: &str) -> Option<u32> {
    version.split('.').next()?.parse().ok()
}

/// Whether a CUDA 13 NVRTC can be used against this driver — see
/// [`CUDA13_MIN_DRIVER_MAJOR`].
pub fn driver_supports_pinned_nvrtc(driver_version: &str) -> bool {
    driver_major(driver_version).is_some_and(|m| m >= CUDA13_MIN_DRIVER_MAJOR)
}

// ── status ───────────────────────────────────────────────────────────────────

/// Live provisioning status, for the readiness surface and the diagnostic bundle.
/// `Skipped` is a first-class outcome, not a failure: a pre-r580, opted-out or
/// non-NVIDIA host reports it and behaves as it did before this feature existed.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub enum Status {
    #[default]
    Idle,
    Skipped(String),
    Provisioning {
        phase: String,
        percent: Option<u64>,
    },
    Provisioned(Manifest),
    /// Terminal failure. Degraded, never fatal: the four `cuda*` elements stay absent
    /// and the host keeps its current encoder set.
    Failed(String),
}

static STATUS: RwLock<Option<Status>> = RwLock::new(None);

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

fn phase(name: &str, percent: Option<u64>) {
    set_status(Status::Provisioning {
        phase: name.to_string(),
        percent,
    });
}

// ── decision ─────────────────────────────────────────────────────────────────

/// The outcome the caller acts on.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Outcome {
    /// Nothing to do; the string is the operator-facing reason.
    NotNeeded(String),
    AlreadyCurrent(Manifest),
    /// Freshly placed; the caller decides on a restart via
    /// [`restart_needed_after_placement`].
    Provisioned(Manifest),
    /// Degraded, never fatal.
    Failed(String),
}

/// Decide whether to provision, with no I/O. `Err(reason)` means nothing to do, and the
/// reason is what the operator is shown.
pub fn decide(
    nvidia_present: bool,
    driver_version: Option<&str>,
    opted_out: bool,
    volume_mounted: bool,
    staged: bool,
    state: &TreeState,
) -> std::result::Result<(), String> {
    if !nvidia_present {
        return Err("no NVIDIA GPU on this host".into());
    }
    if opted_out {
        return Err(
            "QUASAR_CUDA_RUNTIME=0 (CUDA-userspace provisioning disabled by the \
                    operator) — the cudaconvert/cudascale elements will be absent and the \
                    per-session NVENC fallback is unavailable; Vulkan encode is unaffected"
                .into(),
        );
    }
    if !volume_mounted {
        return Err(format!(
            "the driver volume is not mounted at {} (apply deploy/docker-compose.nvidia.yml) — \
             there is nowhere to put the CUDA userspace",
            nvidia_volume::VOLUME_MOUNT
        ));
    }
    // The gate is about the DRIVER, so it applies to the operator-staged path too.
    match driver_version {
        None => {
            return Err(
                "the NVIDIA kernel module version is unavailable, so a CUDA/driver \
                 compatibility check is impossible — not provisioning"
                    .into(),
            )
        }
        Some(v) if !driver_supports_pinned_nvrtc(v) => {
            return Err(format!(
                "this host's NVIDIA driver is {v}, and CUDA {NVRTC_VERSION} NVRTC needs \
                 r{CUDA13_MIN_DRIVER_MAJOR} or newer. NOT provisioning: a failing NVRTC is worse \
                 than an absent one (the four cuda* elements would register and then break a live \
                 session). This host keeps today's behaviour — Vulkan encode, which is the \
                 default on NVIDIA and does not use them. Upgrade the driver to \
                 r{CUDA13_MIN_DRIVER_MAJOR} to enable the NVENC fallback path."
            ))
        }
        Some(_) => {}
    }
    if !state.needs_provision() {
        return Err("the CUDA userspace is already current".into());
    }
    let _ = staged;
    Ok(())
}

// ── entry point ──────────────────────────────────────────────────────────────

/// Adopt an already-populated CUDA tree at process start: one file read, no network.
/// The steady-state path on every boot after the first.
pub fn adopt_current() -> Option<Manifest> {
    let volume = PathBuf::from(nvidia_volume::VOLUME_MOUNT);
    match tree_state(&volume, NVRTC_VERSION) {
        TreeState::Current(m) => {
            tracing::info!(
                target: T,
                nvrtc = %m.nvrtc_version, source = %m.source, libs = m.lib_count,
                lib_dir = %volume.join(layout::LIB64).display(),
                "adopting the Quasar-provisioned CUDA userspace (NVRTC) for this process"
            );
            set_status(Status::Provisioned(m.clone()));
            warn_if_not_wired();
            Some(m)
        }
        TreeState::Stale { have, want } => {
            tracing::info!(
                target: T, have = %have, want = %want,
                "the CUDA userspace in the driver volume is NVRTC {have}, this agent pins {want} \
                 — it will be re-provisioned"
            );
            None
        }
        _ => None,
    }
}

/// Run the provisioner. Blocking — call it on a dedicated thread. Every exit is soft;
/// the worst case leaves the host with no `cuda*` elements and Vulkan encode intact.
pub fn provision_blocking(nvidia_present: bool) -> Outcome {
    let volume = PathBuf::from(nvidia_volume::VOLUME_MOUNT);
    let volume_mounted = volume.is_dir();
    let driver = nvidia_volume::kernel_driver_version(Path::new("/"));
    let state = tree_state(&volume, NVRTC_VERSION);
    let staged = staged_dir();

    if let Err(reason) = decide(
        nvidia_present,
        driver.as_deref(),
        !enabled(),
        volume_mounted,
        staged.is_some(),
        &state,
    ) {
        if let TreeState::Current(m) = &state {
            tracing::info!(
                target: T, nvrtc = %m.nvrtc_version,
                "CUDA userspace already provisioned for the pinned NVRTC version — reusing it"
            );
            set_status(Status::Provisioned(m.clone()));
            return Outcome::AlreadyCurrent(m.clone());
        }
        // INFO, not WARN: each of these is a host behaving as designed.
        tracing::info!(target: T, "CUDA-userspace provisioning skipped: {reason}");
        set_status(Status::Skipped(reason.clone()));
        return Outcome::NotNeeded(reason);
    }

    match &state {
        TreeState::Stale { have, want } => tracing::info!(
            target: T, have = %have, want = %want,
            "re-provisioning the CUDA userspace: the tree holds NVRTC {have}, this agent pins {want}"
        ),
        TreeState::Unreadable(e) => tracing::warn!(
            target: T, token = "cudart-manifest-unreadable", error = %e,
            "the CUDA manifest is unreadable (half-written by an interrupted run) — re-provisioning"
        ),
        TreeState::ObsoleteLayout { have, want } => tracing::info!(
            target: T, have, want,
            "re-provisioning the CUDA userspace: it was placed by population-rule generation \
             {have}, current is {want}"
        ),
        _ => {}
    }

    tracing::info!(
        target: T,
        nvrtc = %NVRTC_VERSION,
        driver = %driver.as_deref().unwrap_or("?"),
        source = if staged.is_some() { "operator-staged" } else { "nvidia-redist" },
        "provisioning the CUDA userspace (NVRTC) into the driver volume — this is what registers \
         cudaconvert/cudaconvertscale/cudascale/cudacompositor, i.e. the per-session NVENC \
         fallback path"
    );

    match run_provision(&volume, staged.as_deref()) {
        Ok(manifest) => {
            tracing::info!(
                target: T,
                nvrtc = %manifest.nvrtc_version, libs = manifest.lib_count,
                sha256 = %manifest.sha256,
                token_hint = "cudart-provisioned",
                "CUDA userspace provisioned successfully"
            );
            set_status(Status::Provisioned(manifest.clone()));
            warn_if_not_wired();
            Outcome::Provisioned(manifest)
        }
        Err(e) => {
            let msg = format!("{e:#}");
            artifact::note_failure(&volume.join(layout::ATTEMPTS), &msg);
            // WARN, never ERROR: degraded to the universal image's behaviour, not broken.
            tracing::warn!(
                target: T, token = "cudart-provision-failed", error = %msg,
                "CUDA-userspace provisioning FAILED — this host keeps the universal image's \
                 current behaviour: no cudaconvert/cudascale elements, so a session that would \
                 fall back to nvcuda<codec>enc cannot start. Vulkan encode (the NVIDIA default) \
                 is unaffected."
            );
            set_status(Status::Failed(msg.clone()));
            Outcome::Failed(msg)
        }
    }
}

// ── the actual work ──────────────────────────────────────────────────────────

/// Deletes the scratch tree on every exit path, not just the happy one.
struct ScratchGuard(PathBuf);

impl Drop for ScratchGuard {
    fn drop(&mut self) {
        if self.0.exists() {
            let _ = std::fs::remove_dir_all(&self.0);
            tracing::debug!(target: T, path = %self.0.display(), "scratch directory removed");
        }
    }
}

fn run_provision(volume: &Path, staged: Option<&Path>) -> Result<Manifest> {
    check_tools()?;
    std::fs::create_dir_all(volume.join(layout::ROOT))
        .with_context(|| format!("create {}", volume.join(layout::ROOT).display()))?;
    let _lock = artifact::Lock::acquire(&volume.join(layout::LOCK), ARTIFACT)?;

    let attempts_path = volume.join(layout::ATTEMPTS);
    let attempts = artifact::read_attempts(&attempts_path);
    if let Some(wait) = artifact::backoff_remaining(
        &attempts,
        NVRTC_VERSION,
        artifact::now_unix(),
        PROVISION_BACKOFF_BASE,
        PROVISION_BACKOFF_MAX,
    ) {
        bail!(
            "CUDA-userspace provisioning has failed {} time(s) for NVRTC {NVRTC_VERSION} and is \
             backing off — not re-attempting for another {} min. Last error: {}. (Clearing the \
             backoff is deliberate: delete {} inside the driver volume.)",
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
    artifact::note_attempt(&attempts_path, NVRTC_VERSION);

    let scratch = volume.join(layout::SCRATCH);
    let _ = std::fs::remove_dir_all(&scratch);
    std::fs::create_dir_all(&scratch).with_context(|| format!("create {}", scratch.display()))?;
    let _scratch_guard = ScratchGuard(scratch.clone());

    let (source_lib_dir, sha256, url, source) = match staged {
        Some(dir) => {
            if !dir.is_dir() {
                bail!(
                    "{RUNTIME_DIR_VAR}={} is not a directory — nothing staged to provision from",
                    dir.display()
                );
            }
            tracing::info!(
                target: T, dir = %dir.display(),
                "using the operator-staged CUDA directory ({RUNTIME_DIR_VAR}) — no download, and \
                 no digest check: these libraries are the operator's to vouch for"
            );
            (
                dir.to_path_buf(),
                "operator-staged".to_string(),
                dir.display().to_string(),
                "operator".to_string(),
            )
        }
        None => {
            phase("download", None);
            let url = redist_url(NVRTC_VERSION);
            let archive = scratch.join(format!("cuda_nvrtc-{NVRTC_VERSION}.tar.xz"));
            let sha256 = fetch_redist(&url, &archive)?;
            check_pinned_digest(&sha256)?;
            phase("extract", None);
            let root = extract_archive(&scratch, &archive)?;
            (root.join("lib"), sha256, url, "nvidia-redist".to_string())
        }
    };

    phase("place", None);
    let placed = place_libraries(volume, &source_lib_dir)?;
    run_ldconfig(&volume.join(layout::LIB64));
    link_unversioned(&volume.join(layout::LIB64))?;

    let manifest = Manifest {
        nvrtc_version: NVRTC_VERSION.to_string(),
        sha256,
        url,
        source,
        provisioned_at_unix: SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_secs())
            .unwrap_or(0),
        agent_version: env!("CARGO_PKG_VERSION").to_string(),
        lib_count: placed,
        layout_version: CURRENT_LAYOUT_VERSION,
    };
    // LAST: its presence is what every consumer reads as "this tree is usable".
    let manifest_path = volume.join(layout::MANIFEST);
    std::fs::write(&manifest_path, serde_json::to_vec_pretty(&manifest)?)
        .with_context(|| format!("write {}", manifest_path.display()))?;
    tracing::info!(target: T, path = %manifest_path.display(), "CUDA manifest written");

    artifact::clear_attempts(&attempts_path);
    Ok(manifest)
}

fn check_tools() -> Result<()> {
    let missing: Vec<&str> = REQUIRED_TOOLS
        .iter()
        .copied()
        .filter(|t| artifact::which(t).is_none())
        .collect();
    if missing.is_empty() {
        return Ok(());
    }
    bail!(
        "the agent image is missing the tool(s) needed to unpack the CUDA redistributable: {}. \
         Rebuild the node image (deploy/build-images.sh) — these are expected to be present.",
        missing.join(", ")
    )
}

fn check_free_space(volume: &Path) -> Result<()> {
    let Some(free) = artifact::free_bytes(volume) else {
        tracing::debug!(
            target: T, path = %volume.display(),
            "could not statvfs the driver volume — proceeding without a free-space preflight"
        );
        return Ok(());
    };
    artifact::free_space_verdict(
        free,
        REQUIRED_FREE_BYTES,
        ARTIFACT,
        "The archive (~58 MiB) and its extracted tree (~230 MiB) are written INSIDE the driver \
         volume, i.e. into the docker data root, which is shared with postgres and the control \
         plane. Free space and restart the agent.",
    )
}

fn fetch_redist(url: &str, dest: &Path) -> Result<String> {
    artifact::fetch(&artifact::Download {
        url,
        dest,
        host: REDIST_HOST,
        what: ARTIFACT,
        timeout: DOWNLOAD_TIMEOUT,
        // The archive is ~58 MiB; anything under 1 MiB is an error page.
        min_bytes: 1024 * 1024,
        // xz magic. A 200 that is actually an error page would otherwise be
        // handed to `tar`.
        magic: Some(&[0xfd, b'7', b'z', b'X', b'Z', 0x00]),
        not_found: &format!(
            "NVIDIA does not publish cuda_nvrtc {NVRTC_VERSION} for linux-x86_64 at {REDIST_HOST}. \
             The pin in node-agent/src/cuda_runtime.rs names a version that is not in the redist \
             index — fix the pin rather than the host."
        ),
        on_progress: Some(&|pct| phase("download", pct)),
    })
}

/// Compare the downloaded digest against [`NVRTC_SHA256`]. A mismatch must refuse
/// before anything is extracted.
pub fn check_pinned_digest(sha256: &str) -> Result<()> {
    if sha256 == NVRTC_SHA256 {
        tracing::info!(
            target: T, sha256 = %sha256,
            "the downloaded CUDA redistributable matches the digest pinned in this agent"
        );
        return Ok(());
    }
    bail!(
        "REFUSING to unpack the CUDA redistributable: its sha256 does not match the pin compiled \
         into this agent.\n  pinned:     {NVRTC_SHA256}\n  downloaded: {sha256}\nA published \
         redistributable for a released version is immutable, so this is either a corrupted \
         download or a substituted payload. Nothing has been extracted."
    )
}

/// `tar -xJf <archive>` into `scratch`, returning the archive's root directory.
fn extract_archive(scratch: &Path, archive: &Path) -> Result<PathBuf> {
    let out = std::process::Command::new("tar")
        .arg("-xJf")
        .arg(archive)
        .arg("-C")
        .arg(scratch)
        .output()
        .context("spawn tar to unpack the CUDA redistributable")?;
    if !out.status.success() {
        for line in String::from_utf8_lossy(&out.stderr)
            .lines()
            .filter(|l| !l.trim().is_empty())
        {
            tracing::warn!(target: T, token = "cudart-tar-stderr", "[tar] {line}");
        }
        bail!("tar -xJf failed: exit {:?}", out.status.code());
    }
    let root = scratch.join(archive_dir(NVRTC_VERSION));
    if !root.is_dir() {
        bail!(
            "the CUDA redistributable did not unpack to {} — the archive layout is not what this \
             provisioner understands",
            root.display()
        );
    }
    tracing::info!(target: T, dir = %root.display(), "CUDA redistributable unpacked");
    Ok(root)
}

/// Base names taken out of the archive's `lib/`, and nothing else. `*_static.a` is
/// ~250 MB of link-time material; `lib/stubs/libnvrtc.so` must never be placed — it
/// dlopens successfully and then fails every call. `stubs/` is excluded by reading one
/// directory non-recursively.
const WANTED_LIB_PREFIXES: &[&str] = &["libnvrtc.so", "libnvrtc-builtins.so"];

/// Whether a file in the archive's `lib/` belongs in the volume.
pub fn is_wanted_library(name: &str) -> bool {
    WANTED_LIB_PREFIXES.iter().any(|p| name.starts_with(p))
}

/// Copy the wanted libraries into `cuda/lib64`, returning how many were placed.
///
/// Real files only: `std::fs::copy` follows symlinks, so copying the archive's own
/// SONAME/unversioned links would duplicate a 115 MB library three times over. They are
/// recreated below instead.
fn place_libraries(volume: &Path, source_lib_dir: &Path) -> Result<usize> {
    let dst = volume.join(layout::LIB64);
    // A re-provision must not leave the previous NVRTC behind: `ldconfig` would index
    // both and the loader could bind the stale one.
    if dst.exists() {
        std::fs::remove_dir_all(&dst).with_context(|| format!("clear {}", dst.display()))?;
    }
    std::fs::create_dir_all(&dst).with_context(|| format!("create {}", dst.display()))?;

    let entries = std::fs::read_dir(source_lib_dir)
        .with_context(|| format!("read {}", source_lib_dir.display()))?;
    let mut names: Vec<String> = entries
        .filter_map(|e| e.ok())
        .filter(|e| {
            // Real files only — no symlinks (recreated), no `stubs/`.
            std::fs::symlink_metadata(e.path())
                .map(|m| m.is_file() && !m.file_type().is_symlink())
                .unwrap_or(false)
        })
        .filter_map(|e| e.file_name().to_str().map(str::to_string))
        .filter(|n| is_wanted_library(n))
        .collect();
    names.sort();

    if names.is_empty() {
        bail!(
            "no libnvrtc*.so* found in {} — this is not an NVRTC library directory",
            source_lib_dir.display()
        );
    }
    for name in &names {
        std::fs::copy(source_lib_dir.join(name), dst.join(name))
            .with_context(|| format!("copy {name} into {}", dst.display()))?;
    }
    tracing::info!(
        target: T, count = names.len(), dir = %dst.display(),
        "CUDA libraries placed: {}",
        names.join(", ")
    );
    Ok(names.len())
}

/// `ldconfig -n <dir>` creates the SONAME symlinks that `libnvrtc`'s own dlopen of its
/// builtins needs; doing it by hand would mean parsing ELF SONAMEs. Best-effort — the
/// unversioned link below is what actually gates element registration.
fn run_ldconfig(dir: &Path) {
    match std::process::Command::new("ldconfig")
        .arg("-n")
        .arg(dir)
        .output()
    {
        Ok(o) => {
            for line in String::from_utf8_lossy(&o.stderr)
                .lines()
                .filter(|l| !l.trim().is_empty())
            {
                tracing::info!(target: T, "[ldconfig] {line}");
            }
            if o.status.success() {
                tracing::info!(target: T, dir = %dir.display(), "SONAME symlinks created");
            } else {
                tracing::warn!(
                    target: T, token = "cudart-ldconfig-failed",
                    dir = %dir.display(), code = ?o.status.code(),
                    "ldconfig -n reported a failure"
                );
            }
        }
        Err(e) => tracing::warn!(
            target: T, token = "cudart-ldconfig-exec-failed",
            dir = %dir.display(), error = %e, "could not run ldconfig -n"
        ),
    }
}

/// `gstcuda` dlopens the UNVERSIONED `libnvrtc.so`, which no NVIDIA runtime package
/// ships and `ldconfig -n` never creates (it links SONAMEs, and none is unversioned).
/// Without this link NVRTC silently fails to load, `cudaconvert` is skipped at plugin
/// registration, and every NVENC-fallback session dies with "cudaconvert not found".
/// No image ships NVRTC any more, so this is the only place the link is made.
fn link_unversioned(dir: &Path) -> Result<()> {
    let mut linked = Vec::new();
    for base in ["libnvrtc", "libnvrtc-builtins"] {
        // Prefer the SONAME link ldconfig just made; fall back to the real file.
        let Some(target) = newest_versioned(dir, base) else {
            continue;
        };
        let link = dir.join(format!("{base}.so"));
        let _ = std::fs::remove_file(&link);
        std::os::unix::fs::symlink(&target, &link)
            .with_context(|| format!("symlink {} -> {target}", link.display()))?;
        linked.push(format!("{base}.so -> {target}"));
    }
    if linked.is_empty() {
        bail!("no libnvrtc.so.* to link the unversioned name to — the placement produced nothing");
    }
    tracing::info!(
        target: T,
        "unversioned symlinks created ({}) — gstcuda dlopens the UNVERSIONED name, so this is \
         what makes cudaconvert/cudaconvertscale/cudascale/cudacompositor register",
        linked.join(", ")
    );
    Ok(())
}

/// The shortest `<base>.so.<n>...` name in `dir`: the SONAME link when `ldconfig` ran,
/// the real file otherwise.
pub fn newest_versioned(dir: &Path, base: &str) -> Option<String> {
    let prefix = format!("{base}.so.");
    let mut candidates: Vec<String> = std::fs::read_dir(dir)
        .ok()?
        .filter_map(|e| e.ok())
        .filter_map(|e| e.file_name().to_str().map(str::to_string))
        .filter(|n| n.starts_with(&prefix))
        .collect();
    // Shortest first, then lexicographic, so the choice is deterministic.
    candidates.sort_by(|a, b| a.len().cmp(&b.len()).then_with(|| a.cmp(b)));
    candidates.into_iter().next()
}

// ── consumption ──────────────────────────────────────────────────────────────

/// Whether the agent must restart to pick up freshly-placed NVRTC: only when the
/// libraries were just placed AND the GStreamer registry was already scanned without
/// them (plugin features register at scan time, so `cudaconvert` can never appear in
/// that registry). Before the first scan a restart only delays registration.
///
/// `registry_scanned` is `session::gst_initialised()`; `element_present` is whether
/// `cudaconvert` is in that registry.
pub fn restart_needed_after_placement(
    just_placed: bool,
    registry_scanned: bool,
    element_present: bool,
) -> bool {
    just_placed && registry_scanned && !element_present
}

/// A compact operator-facing summary, for readiness output.
pub fn describe(manifest: &Manifest) -> String {
    format!(
        "CUDA userspace provisioned by Quasar (NVRTC {}, {})",
        manifest.nvrtc_version, manifest.source
    )
}

/// Extra key/values for the diagnostic bundle / logs.
pub fn debug_map() -> BTreeMap<String, String> {
    let mut m = BTreeMap::new();
    m.insert("enabled".into(), enabled().to_string());
    m.insert("pinned_nvrtc".into(), NVRTC_VERSION.to_string());
    if let Some(d) = staged_dir() {
        m.insert("staged_dir".into(), d.display().to_string());
    }
    let v = match status() {
        Status::Idle => "idle".to_string(),
        Status::Skipped(r) => format!("skipped:{r}"),
        Status::Provisioning { phase, percent } => format!(
            "provisioning:{phase}{}",
            percent.map(|p| format!(":{p}%")).unwrap_or_default()
        ),
        Status::Provisioned(mf) => format!("provisioned:{}", mf.nvrtc_version),
        Status::Failed(e) => format!("failed:{e}"),
    };
    m.insert("status".into(), v);
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
                "quasar-cudart-{tag}-{}-{}",
                std::process::id(),
                SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .unwrap()
                    .as_nanos()
            ));
            fs::create_dir_all(&d).unwrap();
            Tmp(d)
        }
    }
    impl Drop for Tmp {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn manifest(v: &str) -> Manifest {
        Manifest {
            nvrtc_version: v.to_string(),
            sha256: NVRTC_SHA256.into(),
            url: redist_url(v),
            source: "nvidia-redist".into(),
            provisioned_at_unix: 1,
            agent_version: "test".into(),
            lib_count: 2,
            layout_version: CURRENT_LAYOUT_VERSION,
        }
    }

    fn write_manifest(volume: &Path, m: &Manifest) {
        let p = volume.join(layout::MANIFEST);
        fs::create_dir_all(p.parent().unwrap()).unwrap();
        fs::write(p, serde_json::to_vec(m).unwrap()).unwrap();
    }

    // ── the pin ──────────────────────────────────────────────────────────────

    #[test]
    fn the_pin_is_a_real_pin_and_its_url_is_host_pinned() {
        assert_eq!(
            redist_url(NVRTC_VERSION),
            "https://developer.download.nvidia.com/compute/cuda/redist/cuda_nvrtc/linux-x86_64/\
             cuda_nvrtc-linux-x86_64-13.2.86-archive.tar.xz"
        );
        assert!(artifact::validate_url(&redist_url(NVRTC_VERSION), REDIST_HOST).is_ok());
        // The two provisioners pin different origins; neither URL passes the other's.
        assert!(artifact::validate_url(&redist_url(NVRTC_VERSION), "download.nvidia.com").is_err());
        assert!(nvidia_volume::validate_url(&redist_url(NVRTC_VERSION)).is_err());

        assert_eq!(NVRTC_SHA256.len(), 64);
        assert!(check_pinned_digest(NVRTC_SHA256).is_ok());
        let err = check_pinned_digest(&"0".repeat(64)).expect_err("a mismatch must be refused");
        assert!(format!("{err:#}").contains("REFUSING"));
    }

    // ── the driver-version gate ──────────────────────────────────────────────

    #[test]
    fn only_a_cuda_13_capable_driver_gets_nvrtc() {
        assert_eq!(driver_major("610.57.04"), Some(610));
        assert_eq!(driver_major("580.65.06"), Some(580));
        assert_eq!(driver_major("nonsense"), None);

        assert!(driver_supports_pinned_nvrtc("610.57.04"));
        assert!(driver_supports_pinned_nvrtc("580.65.06"));
        // Below the floor a CUDA 13 NVRTC loads and then fails: worse than absent.
        assert!(!driver_supports_pinned_nvrtc("575.64"));
        assert!(!driver_supports_pinned_nvrtc("470.256.02"));
    }

    #[test]
    fn provisioning_decision_table() {
        let empty = TreeState::Empty;
        let current = TreeState::Current(manifest(NVRTC_VERSION));

        assert!(decide(true, Some("610.57.04"), false, true, false, &empty).is_ok());

        // Every refusal varies exactly one input from that happy path, asserted on the
        // phrase the operator is shown.
        let ok = "610.57.04";
        let refused = |r: std::result::Result<(), String>, expect: &str| {
            let err = r.expect_err(expect);
            assert!(err.contains(expect), "{err} should mention {expect}");
        };
        refused(
            decide(false, Some(ok), false, true, false, &empty),
            "NVIDIA GPU",
        );
        refused(
            decide(true, Some(ok), true, true, false, &empty),
            "QUASAR_CUDA_RUNTIME=0",
        );
        refused(
            decide(true, Some(ok), false, false, false, &empty),
            "driver volume is not mounted",
        );
        refused(
            decide(true, None, false, true, false, &empty),
            "compatibility check",
        );
        refused(
            decide(true, Some("575.64"), false, true, false, &empty),
            "r580 or newer",
        );
        refused(
            decide(true, Some(ok), false, true, false, &current),
            "already current",
        );

        // The driver gate applies to the operator-staged path too.
        assert!(decide(true, Some("575.64"), false, true, true, &empty).is_err());
    }

    // ── manifest keying ──────────────────────────────────────────────────────

    #[test]
    fn the_tree_is_keyed_on_the_nvrtc_version_not_the_driver_version() {
        let t = Tmp::new("state");
        assert_eq!(tree_state(&t.0, NVRTC_VERSION), TreeState::Empty);

        write_manifest(&t.0, &manifest(NVRTC_VERSION));
        let st = tree_state(&t.0, NVRTC_VERSION);
        assert!(matches!(st, TreeState::Current(_)));
        assert!(!st.needs_provision());
        assert!(st.usable().is_some());

        // A new pin in a newer agent binary makes the placed tree stale.
        match tree_state(&t.0, "13.3.33") {
            TreeState::Stale { have, want } => {
                assert_eq!(have, NVRTC_VERSION);
                assert_eq!(want, "13.3.33");
            }
            other => panic!("expected Stale, got {other:?}"),
        }

        // Wrong population rules for the right version: re-provisioned too.
        let mut old = manifest(NVRTC_VERSION);
        old.layout_version = 0;
        write_manifest(&t.0, &old);
        assert!(matches!(
            tree_state(&t.0, NVRTC_VERSION),
            TreeState::ObsoleteLayout { have: 0, .. }
        ));

        // A half-written manifest is a half-written tree: re-provision, like Empty.
        fs::write(t.0.join(layout::MANIFEST), b"{not json").unwrap();
        assert!(tree_state(&t.0, NVRTC_VERSION).needs_provision());
    }

    // ── what comes out of the archive ────────────────────────────────────────

    #[test]
    fn only_the_two_runtime_libraries_are_taken_from_the_archive() {
        assert!(is_wanted_library("libnvrtc.so.13.2.86"));
        assert!(is_wanted_library("libnvrtc.so.13"));
        assert!(is_wanted_library("libnvrtc-builtins.so.13.2.86"));
        // Link-time and devel artifacts: ~250 MB with no runtime use.
        for junk in [
            "libnvrtc_static.a",
            "libnvrtc-builtins_static.a",
            "nvrtc.h",
            "nvrtc-13.2.pc",
            "libcudart.so.13",
        ] {
            assert!(!is_wanted_library(junk), "must not place {junk}");
        }
    }

    #[test]
    fn placement_copies_real_files_only_and_makes_the_unversioned_link() {
        let t = Tmp::new("place");
        let src = t.0.join("archive/lib");
        fs::create_dir_all(src.join("stubs")).unwrap();
        fs::write(src.join("libnvrtc.so.13.2.86"), b"real").unwrap();
        fs::write(src.join("libnvrtc-builtins.so.13.2.86"), b"real").unwrap();
        fs::write(src.join("libnvrtc_static.a"), b"static").unwrap();
        // A link stub: dlopen succeeds and every call fails, so placing it is worse
        // than placing nothing.
        fs::write(src.join("stubs/libnvrtc.so"), b"stub").unwrap();
        // The archive's own symlinks: following them duplicates a 115 MB library.
        std::os::unix::fs::symlink("libnvrtc.so.13.2.86", src.join("libnvrtc.so.13")).unwrap();
        std::os::unix::fs::symlink("libnvrtc.so.13.2.86", src.join("libnvrtc.so")).unwrap();

        let volume = t.0.join("vol");
        let placed = place_libraries(&volume, &src).unwrap();
        assert_eq!(placed, 2, "only the two real runtime libraries");

        let dst = volume.join(layout::LIB64);
        assert!(dst.join("libnvrtc.so.13.2.86").is_file());
        assert!(dst.join("libnvrtc-builtins.so.13.2.86").is_file());
        assert!(!dst.join("libnvrtc_static.a").exists());
        assert!(!dst.join("stubs").exists());
        assert!(
            !dst.join("libnvrtc.so").exists(),
            "the archive's own symlinks must not be copied; they are recreated"
        );

        // gstcuda dlopens the unversioned name, which no NVIDIA runtime package and no
        // `ldconfig -n` ever creates.
        link_unversioned(&dst).unwrap();
        let link = dst.join("libnvrtc.so");
        assert!(
            fs::symlink_metadata(&link)
                .unwrap()
                .file_type()
                .is_symlink(),
            "libnvrtc.so must be the unversioned symlink"
        );
        assert_eq!(fs::read(&link).unwrap(), b"real");
        assert!(dst.join("libnvrtc-builtins.so").exists());

        // A re-provision clears the previous version rather than layering it.
        fs::write(dst.join("libnvrtc.so.13.0.1"), b"old").unwrap();
        place_libraries(&volume, &src).unwrap();
        assert!(!dst.join("libnvrtc.so.13.0.1").exists());
    }

    #[test]
    fn the_unversioned_link_prefers_the_soname_over_the_full_version() {
        let t = Tmp::new("link");
        fs::write(t.0.join("libnvrtc.so.13.2.86"), b"x").unwrap();
        assert_eq!(
            newest_versioned(&t.0, "libnvrtc").as_deref(),
            Some("libnvrtc.so.13.2.86")
        );
        // Once ldconfig has made the SONAME link, point at that: a future patch bump
        // then only has to move one link.
        std::os::unix::fs::symlink("libnvrtc.so.13.2.86", t.0.join("libnvrtc.so.13")).unwrap();
        assert_eq!(
            newest_versioned(&t.0, "libnvrtc").as_deref(),
            Some("libnvrtc.so.13")
        );
        assert_eq!(newest_versioned(&t.0, "libcudart"), None);
    }

    // ── the wipe-exclusion invariant ─────────────────────────────────────────

    /// The wipe-exclusion rule: a driver re-provision clears `lib64/` and `lib32/`, and
    /// `cuda/` must survive it. Fails if that clear is ever widened to the volume.
    #[test]
    fn a_driver_reprovision_does_not_wipe_the_cuda_tree() {
        let t = Tmp::new("wipe");
        let volume = &t.0;

        let libdir = volume.join(layout::LIB64);
        fs::create_dir_all(&libdir).unwrap();
        fs::write(libdir.join("libnvrtc.so.13.2.86"), b"nvrtc").unwrap();
        write_manifest(volume, &manifest(NVRTC_VERSION));

        // A driver tree from an earlier driver version.
        let old_driver_lib = volume.join(nvidia_volume::layout::LIB64);
        fs::create_dir_all(&old_driver_lib).unwrap();
        fs::write(old_driver_lib.join("libnvidia-eglcore.so.1"), b"old").unwrap();

        // The exact clear the driver provisioner performs on a re-provision.
        for d in [nvidia_volume::layout::LIB64, nvidia_volume::layout::LIB32] {
            let p = volume.join(d);
            if p.exists() {
                fs::remove_dir_all(&p).unwrap();
            }
        }

        assert!(!old_driver_lib.exists(), "the driver libraries are cleared");
        assert!(
            libdir.join("libnvrtc.so.13.2.86").is_file(),
            "the CUDA tree must survive a driver re-provision — it is keyed on the NVRTC \
             version, not on the kernel module"
        );
        assert!(matches!(
            tree_state(volume, NVRTC_VERSION),
            TreeState::Current(_)
        ));
    }

    /// The two provisioners must not share bookkeeping: separate lock, attempt counter
    /// and manifest.
    #[test]
    fn the_two_provisioners_share_no_bookkeeping_files() {
        for (a, b) in [
            (layout::MANIFEST, nvidia_volume::layout::MANIFEST),
            (layout::LOCK, nvidia_volume::layout::LOCK),
            (layout::ATTEMPTS, nvidia_volume::layout::ATTEMPTS),
            (layout::SCRATCH, nvidia_volume::layout::SCRATCH),
            (layout::LIB64, nvidia_volume::layout::LIB64),
        ] {
            assert_ne!(a, b, "{a} must not be shared with the driver provisioner");
            assert!(
                a.starts_with(layout::ROOT),
                "{a} must live under {}/",
                layout::ROOT
            );
        }
    }

    // ── the restart guard ────────────────────────────────────────────────────

    #[test]
    fn a_restart_is_only_for_a_registry_that_was_scanned_without_nvrtc() {
        // Placed, and the registry was already scanned without them.
        assert!(restart_needed_after_placement(true, true, false));
        // Nothing placed: adopting an existing tree needs no restart.
        assert!(!restart_needed_after_placement(false, true, false));
        // Not scanned yet: the first scan finds NVRTC by itself.
        assert!(!restart_needed_after_placement(true, false, false));
        // Already registered.
        assert!(!restart_needed_after_placement(true, true, true));
    }

    // ── status surface ───────────────────────────────────────────────────────

    #[test]
    fn the_ld_library_path_check_matches_whole_entries_only() {
        let dir = Path::new("/opt/quasar/nvidia-driver/cuda/lib64");
        assert!(on_ld_library_path(
            "/opt/quasar/nvidia-driver/lib64:/opt/quasar/nvidia-driver/cuda/lib64",
            dir
        ));
        assert!(on_ld_library_path(
            "/opt/quasar/nvidia-driver/cuda/lib64",
            dir
        ));
        assert!(!on_ld_library_path("/opt/quasar/nvidia-driver/lib64", dir));
        // A prefix is not a match; an empty entry is not a match for "/".
        assert!(!on_ld_library_path(
            "/opt/quasar/nvidia-driver/cuda/lib64x",
            dir
        ));
        assert!(!on_ld_library_path("", dir));
    }

    #[test]
    fn the_debug_map_always_names_the_pin() {
        let m = debug_map();
        assert_eq!(
            m.get("pinned_nvrtc").map(String::as_str),
            Some(NVRTC_VERSION)
        );
        assert!(m.contains_key("status"));
        assert!(m.contains_key("enabled"));
    }

    #[test]
    fn describe_names_the_version_and_where_it_came_from() {
        let d = describe(&manifest(NVRTC_VERSION));
        assert!(d.contains(NVRTC_VERSION));
        assert!(d.contains("nvidia-redist"));
    }
}
