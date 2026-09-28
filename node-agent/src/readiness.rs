//! Host readiness probe (first-run-experience spec S1): latent host gaps turned into named
//! checks with an exact remediation command, reported on `capacity.readiness`.
//!
//! Advisory, never a gate. A failing check must never block registration or launch, or change
//! scheduling — the checks read a proxy for a capability, and refusing to run sessions on a
//! false negative is worse than showing a red card.
//!
//! Capability checks read the AGENT CONTAINER's filesystem, not the host's: the compositor and
//! encoders run here, so what matters is what the container runtime actually injected. The only
//! host-side read is `/etc/os-release` (via `/host`), used purely to pick remediation wording;
//! its absence degrades to generic wording, never a failed check.

/// `owner_conflict` on an owned install.
pub mod owner_conflict;
/// The update-path checks (preflight ids), with their collectors.
pub mod platform_update;
pub mod report;
pub mod runtime_facts;
pub mod storage;

use std::path::{Path, PathBuf};

use crate::messages::ReadinessCheck;
use crate::session::container;

/// Check statuses. `&'static str`, not an enum: they cross the wire and the control plane
/// stores them opaquely, so a new status must be additive on both sides. Open enum per
/// `protocol/agent-api.md` — a consumer passes an unrecognised status through.
pub const PASS: &str = "pass";
pub const FAIL: &str = "fail";
pub const SKIP: &str = "skip";
/// The NVIDIA driver volume is being materialised right now.
pub const PROVISIONING: &str = "provisioning";
/// A named risk that is never `fail` (#483): detection can prove a default-deny posture is
/// active, never that it actually drops the agent's ICE UDP.
pub const WARN: &str = "warn";
/// Indeterminate: a host probe could not be concluded. Never blocks, never clears a block
/// (protocol/agent-api.md `readiness`).
pub const UNKNOWN: &str = "unknown";
/// The hardware does not provide this capability (#311, amendment 12 addendum): a codec
/// the GPU has no encoder for. Not a fault, never blocks, and definitive — retained like
/// `pass`/`fail`, never replaced by an indeterminate run.
pub const UNSUPPORTED: &str = "unsupported";

/// Where the host's `/etc/os-release` is bind-mounted in the agent container
/// (reference compose). Absent ⇒ generic remediation wording.
const HOST_ROOT: &str = "/host";

/// Directories searched for `libnvidia-eglcore.so*`, relative to the probe root.
/// Covers Fedora/RHEL (`usr/lib64`), Debian/Ubuntu multiarch
/// (`usr/lib/x86_64-linux-gnu`) and the plain `usr/lib` layout.
const LIB_DIRS: &[&str] = &[
    "usr/lib64",
    "usr/lib",
    "usr/lib/x86_64-linux-gnu",
    "usr/lib/aarch64-linux-gnu",
    "lib64",
    "lib",
];

/// glvnd EGL vendor-config directories, relative to the probe root. `/usr/share`
/// is where the driver package installs it; `/etc` is the admin override
/// location and the one `nvidia-ctk` writes into on some layouts.
const EGL_VENDOR_DIRS: &[&str] = &["usr/share/glvnd/egl_vendor.d", "etc/glvnd/egl_vendor.d"];

/// What the host's encoder codec probe said (#493d). Three states, not two: `NotProbed`
/// (pre-registration call sites) must stay silent; `Failed`/`Probed(empty)` are loud, since a
/// GPU host that can encode nothing fails every launch.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub enum CodecProbe {
    /// The probe has not run at this call site. Reports `skip`.
    #[default]
    NotProbed,
    /// The probe ran and could not even initialise GStreamer.
    Failed,
    /// The probe ran; these are the codecs the host advertised (possibly none).
    Probed(Vec<String>),
}

/// Where the Quasar NVIDIA driver volume is mounted inside the agent container.
/// Mirrors `nvidia_volume::VOLUME_MOUNT` but expressed relative to a probe root
/// (leading `/` stripped) so tests can plant a fake one.
const NVIDIA_VOLUME_REL: &str = "opt/quasar/nvidia-driver";

/// Inputs to [`probe`]. Every path is a ROOT so the probe is testable against a fake
/// filesystem — no check may ever read a literal `/` path.
#[derive(Debug, Clone)]
pub struct ProbeEnv {
    /// The agent's own filesystem root (`/` in production, a tempdir in tests).
    pub root: PathBuf,
    /// Root under which the HOST's `/etc/os-release` is visible; falls back to `root`.
    pub host_root: PathBuf,
    /// NVIDIA GPU present. The NVIDIA checks `skip` (never `fail`) when false — an AMD box is
    /// not unready for lacking NVIDIA libraries.
    pub nvidia: bool,
    /// ANY GPU, any vendor. The vendor-neutral sanity checks key off this rather than
    /// `nvidia`: no render node / no codecs is a failure whoever made the card.
    pub gpu_present: bool,
    /// `QUASAR_APP_PUID`, or `None` when unset. Used only to predict whether the APP container
    /// could open the `/dev/dri` nodes — the agent runs as root and is never the one that fails.
    pub app_uid: Option<u32>,
    /// `QUASAR_APP_PGID`, used only to sharpen the message.
    pub app_gid: Option<u32>,
    /// Where the Quasar NVIDIA driver volume is mounted in THIS container.
    pub nvidia_volume_root: PathBuf,
    /// The loaded kernel module's version (`/sys/module/nvidia/version`) — what the volume's
    /// userspace has to match.
    pub kernel_driver_version: Option<String>,
    /// Tri-state on purpose: "never ran" and "ran and found nothing" are opposite answers, and
    /// collapsing them into `None` is how a zero-codec host reads as fine.
    pub host_codecs: CodecProbe,
    /// The startup-probed 32-bit NVIDIA driver-lib dir (`""` = none). Reuses the #375 probe
    /// result rather than re-running it — it costs a throwaway container per run.
    pub nvidia_lib32_path: String,
    /// Driver-volume provisioner state. Provisioned turns the three NVIDIA checks green;
    /// in-flight reports [`PROVISIONING`]; failed reports the specific error and retry policy.
    pub nvidia_volume: VolumeView,
    /// Can the runtime pass the provisioned driver into sibling app containers?
    pub driver_mount_error: Option<String>,
    /// This refresh's sibling-mount inspection. Indeterminate is a busy or timed-out
    /// client, not evidence the mounts are wrong.
    pub container_mounts: MountObservation,
    /// Does the EGL stack this container loads actually WORK, as opposed to being present on
    /// disk? A file-presence pass that is green while the compositor cannot init EGL sends the
    /// operator elsewhere, so this runtime verdict VETOES it (loop-3 guard).
    pub egl_runtime: crate::nvidia_volume::EglRuntime,
    /// Firewall detection's answer, computed once at [`ProbeEnv::live`] so every reader sees
    /// the same instant and the subprocess cost is paid once, not per check.
    /// RH-07 #403: the latest real-traffic evidence of the WebRTC media path.
    pub media: Option<crate::session::media_evidence::Evidence>,
    /// On an owned install, what its recovery actor answered this refresh
    /// (platform_update.rs); `None` on a host with no recovery actor.
    pub recovery_actor: Option<platform_update::ActorView>,
    pub health: platform_update::HealthOwner,
    /// This agent's own `/health` identity, to compare against who answers.
    pub self_identity: platform_update::HealthIdentity,
    /// On an owned install, the owner conflicts its recovery actor reported this refresh
    /// (the inner `None`: it did not answer); `None` on any other install.
    pub owner_conflicts: Option<owner_conflict::Observed>,
    /// The storage roots and their free space (#253), read once per probe.
    pub storage: storage::StorageView,
    /// The container engine as one inspection saw it (#254), read once per probe.
    pub runtime: runtime_facts::RuntimeView,
}

/// The driver-volume provisioner's state, as readiness sees it. Plain data, not a live call
/// into `nvidia_volume`, so the probe stays pure w.r.t. `ProbeEnv`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub enum VolumeView {
    /// No volume in play: host driver fine, host not NVIDIA, not mounted, or opted out.
    #[default]
    None,
    Provisioning {
        phase: String,
        percent: Option<u64>,
    },
    /// Populated and matching the loaded kernel module. `root` is the volume's path inside THIS
    /// container, so checks confirm the files are there rather than trusting a manifest.
    Provisioned {
        root: PathBuf,
        version: String,
    },
    /// Terminal failure; the string is the real error.
    Failed(String),
}

impl VolumeView {
    /// Build the view from the live provisioner state.
    pub fn live() -> VolumeView {
        use crate::nvidia_volume::{self, Status};
        match nvidia_volume::status() {
            Status::Idle => VolumeView::None,
            Status::Provisioning { phase, percent } => VolumeView::Provisioning { phase, percent },
            Status::Provisioned(m) => VolumeView::Provisioned {
                root: nvidia_volume::current()
                    .map(|i| i.local)
                    .unwrap_or_else(|| PathBuf::from(nvidia_volume::VOLUME_MOUNT)),
                version: m.driver_version,
            },
            Status::Failed(e) => VolumeView::Failed(e),
        }
    }
}

impl ProbeEnv {
    /// Production environment: probe the agent's own filesystem, read
    /// `/host/etc/os-release` when the compose mount is present.
    pub fn live(nvidia: bool, nvidia_lib32_path: &str) -> Self {
        // #274: the engine is asked FIRST, and under its own small budget. It used to be
        // the last field built, behind the self-mount inspection, the sibling EGL probe
        // and the image-storage lookup — each bounded only by the 30 s client deadline,
        // ~100 s serially on a hung daemon, which put the failing `runtime_endpoint` past
        // the control plane's readiness staleness window. When the engine gave a
        // definitive "not usable" answer, every one of those calls would pay its full
        // deadline to report exactly what it reports when skipped, so they are skipped.
        let runtime = runtime_facts::RuntimeView::live();
        let engine_answered = runtime.engine_answered();
        if nvidia && engine_answered {
            crate::nvidia_volume::retry_mount_resolution();
        }
        let host_root =
            if is_containerized() || Path::new(HOST_ROOT).join("etc/os-release").exists() {
                PathBuf::from(HOST_ROOT)
            } else {
                PathBuf::from("/")
            };
        // One status read on an owned install: whether the actor answers, its owner
        // conflicts, and whether its identity moved since `register`.
        let owned = crate::buildinfo::owned_socket().map(|socket| {
            let (facts, conflicts) = crate::buildinfo::observe_owned(&socket);
            crate::buildinfo::note_observed(&facts);
            let actor = platform_update::ActorView {
                socket,
                answered: facts.updater_present == Some(true),
                version: facts.recovery_actor_version.clone(),
            };
            (actor, conflicts)
        });
        ProbeEnv {
            root: PathBuf::from("/"),
            host_root,
            nvidia,
            // Caller refines this with `with_gpu_present` once capacity
            // detection has run; an NVIDIA host trivially has a GPU.
            gpu_present: nvidia,
            app_uid: env_u32("QUASAR_APP_PUID"),
            app_gid: env_u32("QUASAR_APP_PGID"),
            nvidia_volume_root: PathBuf::from("/").join(NVIDIA_VOLUME_REL),
            kernel_driver_version: crate::nvidia_volume::kernel_driver_version(Path::new("/")),
            host_codecs: CodecProbe::NotProbed,
            nvidia_lib32_path: nvidia_lib32_path.to_string(),
            nvidia_volume: VolumeView::live(),
            // A native (non-containerized) agent has no sibling mounts to validate and
            // must keep passing this check whatever the engine is doing — the skip stands
            // in for the inspection, never for `is_containerized`.
            container_mounts: live_mount_observation(&runtime),
            driver_mount_error: crate::nvidia_volume::mount_resolution_error().or_else(|| crate::nvidia_volume::current().and_then(|info| {
                if info.host.is_none() && info.name.is_none() {
                    Some(format!("The agent can read its NVIDIA driver volume but cannot resolve its Docker mount. App launches are blocked; check Docker socket and identity inspection, or set {} to the host directory already mounted at /opt/quasar/nvidia-driver.", crate::nvidia_volume::HOST_PATH_ENV))
                } else { None }
            })),
            // NVIDIA only: on AMD/Intel the EGL stack is Mesa's and none of this module's
            // remediation applies, so the subprocess (and a confusing red row) buys nothing.
            egl_runtime: match (nvidia, engine_answered) {
                (true, true) => crate::nvidia_volume::probe_egl_runtime(
                    crate::nvidia_volume::vendor_lib_for_selftest().as_deref(),
                ),
                // The sibling probe needs the engine to launch a container; the engine
                // just said it cannot. Same verdict it reaches the slow way.
                (true, false) => crate::nvidia_volume::EglRuntime::Indeterminate {
                    detail: "the container engine did not answer this refresh".into(),
                },
                (false, _) => crate::nvidia_volume::EglRuntime::Unknown,
            },
            // Vendor/GPU-independent: a firewall problem is as real on a GPU-less box.
            media: crate::session::media_evidence::latest(),
            recovery_actor: owned.as_ref().map(|(actor, _)| actor.clone()),
            health: platform_update::collect_health(crate::health::addr_from_env()),
            self_identity: platform_update::HealthIdentity {
                node: crate::logging::host_name().to_string(),
                pid: std::process::id(),
            },
            owner_conflicts: owned.map(|(_, conflicts)| conflicts),
            storage: storage::StorageView::live(engine_answered),
            runtime,
        }
    }

    /// Hand the probe capacity detection's vendor-neutral GPU answer.
    pub fn with_gpu_present(mut self, gpu_present: bool) -> Self {
        self.gpu_present = gpu_present;
        self
    }

    /// Hand the probe the already-paid codec probe result. `None` means the probe ran and
    /// GStreamer would not initialise ([`CodecProbe::Failed`]), never "unknown".
    pub fn with_codec_probe(mut self, codecs: Option<&[String]>) -> Self {
        self.host_codecs = match codecs {
            Some(c) => CodecProbe::Probed(c.to_vec()),
            None => CodecProbe::Failed,
        };
        self
    }
}

/// Parse a small unsigned env var, ignoring garbage: a typo must never make a check lie.
fn env_u32(key: &str) -> Option<u32> {
    std::env::var(key).ok()?.trim().parse().ok()
}

/// The distro family, used only to choose remediation wording.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Distro {
    Fedora,
    Debian,
    Arch,
    Unknown,
}

/// `ID` wins; `ID_LIKE` is the fallback so derivatives (Nobara, Pop, CachyOS) get the right
/// package manager without an entry each.
pub fn parse_distro(os_release: &str) -> Distro {
    let value = |key: &str| -> Option<String> {
        os_release.lines().find_map(|line| {
            let rest = line.strip_prefix(key)?.strip_prefix('=')?;
            Some(rest.trim().trim_matches('"').to_ascii_lowercase())
        })
    };
    let ids = [value("ID"), value("ID_LIKE")];
    for token in ids.iter().flatten().flat_map(|v| {
        v.split_whitespace()
            .map(str::to_string)
            .collect::<Vec<String>>()
    }) {
        match token.as_str() {
            "fedora" | "rhel" | "centos" | "nobara" | "bazzite" => return Distro::Fedora,
            "debian" | "ubuntu" => return Distro::Debian,
            "arch" | "archlinux" => return Distro::Arch,
            _ => {}
        }
    }
    Distro::Unknown
}

fn detect_distro(env: &ProbeEnv) -> Distro {
    match std::fs::read_to_string(env.host_root.join("etc/os-release")) {
        Ok(body) => parse_distro(&body),
        Err(_) => Distro::Unknown,
    }
}

fn is_containerized() -> bool {
    Path::new("/.dockerenv").exists() || Path::new("/run/.containerenv").exists()
}

/// What `host_container_mounts` says when the engine refused the inspection for a
/// reason that is evidence: permission denied, a missing socket, a bad endpoint.
/// A busy client or a timeout is not this string.
pub(crate) const ENGINE_MOUNT_INSPECTION_FAILED: &str =
    "Docker could not inspect the agent's mounts; check socket access";

const MOUNT_CHECK_REMEDIATION: &str = "Use the generated bind mounts at identical host/container paths. Fix the Docker socket or mount configuration, then recreate the agent; checks refresh automatically.";

const MOUNT_INDETERMINATE_SUMMARY: &str =
    "The runtime client was busy or timed out this refresh; the last mount result stands";

/// One sibling-mount inspection. [`MountObservation::Indeterminate`] is not evidence
/// the mounts are wrong, so a launch does not refuse on it (ADR 0005).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum MountObservation {
    /// Mounts agree, or this agent is not a container.
    Agree,
    /// A mismatch, or an engine refusal that is evidence.
    Fail(String),
    /// Busy, cancelled, or timed out. Not evidence.
    Indeterminate,
}

pub(crate) fn classify_mount_runtime(kind: crate::runtime::ErrorKind) -> MountObservation {
    match kind {
        crate::runtime::ErrorKind::Busy
        | crate::runtime::ErrorKind::Cancelled
        | crate::runtime::ErrorKind::Timeout => MountObservation::Indeterminate,
        _ => MountObservation::Fail(ENGINE_MOUNT_INSPECTION_FAILED.into()),
    }
}

fn live_mount_observation(runtime: &runtime_facts::RuntimeView) -> MountObservation {
    if !is_containerized() {
        return MountObservation::Agree;
    }
    // The endpoint inspection already spent the budget. A timeout must not start
    // another one, and it is not evidence the mounts are wrong.
    if !runtime.engine_answered() {
        return if runtime.is_inspection_timeout() {
            MountObservation::Indeterminate
        } else {
            MountObservation::Fail(ENGINE_MOUNT_INSPECTION_FAILED.into())
        };
    }
    sibling_mount_observation()
}

/// Docker resolves app bind sources in the host namespace, not the agent's.
/// `Some` only when this inspection is evidence of a problem. A busy or timed-out
/// client returns `None`, so a launch is not refused on it.
pub(crate) fn sibling_mount_error() -> Option<String> {
    match sibling_mount_observation() {
        MountObservation::Fail(error) => Some(error),
        MountObservation::Agree | MountObservation::Indeterminate => None,
    }
}

fn sibling_mount_observation() -> MountObservation {
    if !is_containerized() {
        return MountObservation::Agree;
    }
    let Some(id) = crate::nvidia_volume::self_container_id() else {
        return MountObservation::Fail(
            "Cannot identify the agent container to validate app mounts".into(),
        );
    };
    let runtime = match crate::runtime::configured() {
        Ok(runtime) => runtime,
        Err(error) => return classify_mount_runtime(error.kind),
    };
    let container = match runtime
        .inspect_container_within(id, crate::runtime::ENGINE_INSPECTION_BUDGET)
        .wait()
    {
        Ok(Some(container)) => container,
        Ok(None) => return MountObservation::Fail(ENGINE_MOUNT_INSPECTION_FAILED.into()),
        Err(error) => return classify_mount_runtime(error.kind),
    };
    let mut paths =
        vec![std::env::var("XDG_RUNTIME_DIR").unwrap_or_else(|_| "/run/quasar-agent".into())];
    let home = std::env::var("QUASAR_HOME_ROOT").unwrap_or_default();
    if !home.is_empty() {
        paths.push(home.clone());
        paths.push(
            template_root_for(Path::new(&home))
                .to_string_lossy()
                .into_owned(),
        );
    }
    match validate_sibling_mounts(&container.mounts, &paths) {
        None => MountObservation::Agree,
        Some(error) => MountObservation::Fail(error),
    }
}

/// `QUASAR_TEMPLATE_ROOT`, or the sibling-of-homes default (`{home}/../templates`).
pub(super) fn template_root_for(home: &Path) -> PathBuf {
    std::env::var("QUASAR_TEMPLATE_ROOT")
        .ok()
        .filter(|p| !p.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            home.parent()
                .unwrap_or(Path::new("/var/lib/quasar"))
                .join("templates")
        })
}

fn validate_sibling_mounts(mounts: &[crate::runtime::Mount], paths: &[String]) -> Option<String> {
    let broken: Vec<_> = paths
        .iter()
        .filter(|path| {
            !mounts.iter().any(|mount| {
                mount.kind == crate::runtime::MountKind::Bind
                    && mount
                        .source
                        .as_ref()
                        .is_some_and(|source| source.0 == Path::new(path))
                    && mount.destination == **path
                    && mount.read_only == Some(false)
            })
        })
        .cloned()
        .collect();
    if broken.is_empty() {
        None
    } else {
        Some(format!("Agent directories do not have matching writable host bind mounts: {}. Apps would receive different or inaccessible files.", broken.join(", ")))
    }
}

/// Run the full check set. Pure w.r.t. `env` (no global state, network, or container launches)
/// so it is cheap to re-run on every capacity report.
pub fn probe(env: &ProbeEnv) -> Vec<ReadinessCheck> {
    let mut checks = probe_all(env);
    checks.extend(owner_conflict::check(
        env.owner_conflicts.is_some(),
        env.owner_conflicts.as_ref().unwrap_or(&None),
    ));
    checks
}

fn probe_all(env: &ProbeEnv) -> Vec<ReadinessCheck> {
    let distro = detect_distro(env);
    vec![
        runtime_facts::check_runtime_endpoint(&env.runtime),
        runtime_facts::check_runtime_api_version(&env.runtime),
        runtime_facts::check_runtime_capabilities(&env.runtime),
        runtime_facts::check_runtime_cdi(&env.runtime),
        runtime_facts::check_runtime_engine(&env.runtime),
        runtime_facts::check_engine_healthchecks(&env.runtime),
        // Runtime veto: files present but the stack not loading must never read green.
        veto_if_egl_broken(check_nvidia_egl_vendor(env, distro), env),
        veto_if_egl_broken(check_nvidia_eglcore(env, distro), env),
        check_nvidia_lib32(env, distro),
        check_render_node(env, distro),
        check_uinput(env, distro),
        check_user_namespaces(env, distro),
        check_app_apparmor_profile(env),
        // ── GPU host post-boot sanity (#493) ─────────────────────────────────
        check_host_render_node(env, distro),
        check_dri_node_app_access(env, distro),
        check_driver_volume_version(env, distro),
        match &env.driver_mount_error {
            Some(error) => fail("nvidia_driver_mount", error.clone(), "Check Docker socket and container mount inspection, or set QUASAR_NVIDIA_DRIVER_HOST_PATH to the host directory already mounted at /opt/quasar/nvidia-driver. Explicit paths must pass the same-directory sibling check. Recreate the agent after changing environment settings; reinstalling drivers will not fix mount resolution.".to_string()),
            None => skip("nvidia_driver_mount", "No unresolved NVIDIA app driver mount"),
        },
        check_encoder_codecs(env, distro),
        check_vulkan_av1_compatibility(env),
        check_xid_visibility(env),
        // Applies to every host, GPU or not — not part of the sanity family.
        check_media_reachability(env, distro),
        host_container_mounts_check(&env.container_mounts),
        storage::check_homes_root_writable(&env.storage, storage::WriteIdentity::from_env_pair(env.app_uid, env.app_gid)),
        storage::check_homes_free_space(&env.storage),
        storage::check_template_free_space(&env.storage),
        storage::check_image_free_space(&env.storage),
        // The update path: what preflight reads about this host.
        platform_update::check_updater_socket(env.recovery_actor.as_ref()),
        platform_update::check_health_addr_bindable(&env.health, &env.self_identity),
    ]
}

fn check_vulkan_av1_compatibility(env: &ProbeEnv) -> ReadinessCheck {
    use crate::encoder_compatibility::{self as compatibility, Av1Compatibility};
    match compatibility::inspect(&env.root) {
        Av1Compatibility::KnownCorrupt => warn_check(
            compatibility::CHECK_ID, compatibility::SUMMARY.into(), compatibility::REMEDIATION.into(),
        ),
        Av1Compatibility::Validated => pass(compatibility::CHECK_ID,
            "RTX 5090 with NVIDIA 610.57.04: Vulkan AV1 was validated on this GPU/driver combination. Codec availability still depends on the configured encoder and installed elements.".into()),
        Av1Compatibility::Unknown => skip(compatibility::CHECK_ID,
            "No recorded Vulkan AV1 compatibility result for this GPU/driver combination; this is not a visual-quality certification"),
    }
}

/// Can this agent see the kernel's GPU fault records? Usually `skip` and never `fail`:
/// `/dev/kmsg` needs a mapping plus `CAP_SYSLOG`, and an operator who keeps that off is not
/// misconfigured. It is a check because otherwise its absence is indistinguishable from "no
/// Xid ever happened here".
fn check_xid_visibility(env: &ProbeEnv) -> ReadinessCheck {
    const ID: &str = "xid_visibility";
    if !env.gpu_present {
        return skip(ID, "no GPU on this host — nothing would report an Xid");
    }
    let path = env
        .root
        .join(crate::gpu_kmsg::KMSG_PATH.trim_start_matches('/'));
    match std::fs::File::open(&path) {
        Ok(_) => pass(
            ID,
            format!(
                "{} is readable — GPU Xid / amdgpu fault records are reported as \
                 `host.xid` / `host.gpu_fault` trace events",
                crate::gpu_kmsg::KMSG_PATH
            ),
        ),
        // Amendment 17: an optional diagnostic the host was not prepared to grant. The
        // summary names the setting; a skip asks nothing of the operator.
        Err(e) => ReadinessCheck {
            id: ID.to_string(),
            status: SKIP.to_string(),
            summary: format!(
                "GPU fault messages are not collected: {} is not readable here ({e}). This \
                 diagnostic is optional; a host that keeps the kernel log restricted \
                 (kernel.dmesg_restrict=1, the usual default) does not grant it, and host \
                 preparation's --allow-kernel-log does. Faults can still be read by hand \
                 in the host's dmesg",
                crate::gpu_kmsg::KMSG_PATH
            ),
            remediation: String::new(),
            observed_at: None,
            source: Some("local".to_string()),
            blocks: None,
        },
    }
}

/// The GPU host post-boot sanity family (#493): host states that leave every observable Quasar
/// surface green while every session dies. Logged at ERROR with a grep token, not WARN.
pub const SANITY_CHECK_IDS: &[&str] = &[
    "host_render_node",
    "dri_node_app_access",
    "driver_volume_version",
    "encoder_codecs",
];

/// The grep token every post-boot-sanity failure line carries.
pub const SANITY_LOG_TOKEN: &str = "gpu-host-sanity";

fn is_sanity_check(id: &str) -> bool {
    SANITY_CHECK_IDS.contains(&id)
}

/// Which NVIDIA capability gaps the check set found, driving the driver-volume provisioner's
/// trigger. Runs the SAME check functions the card shows, so provisioner and operator can never
/// disagree. A `provisioning`/`pass` check is not a gap — re-triggering on one already being
/// fixed is how a download loop happens.
///
/// Safety property (#475): provisioning is FILE-PRESENCE-TRIGGERED ONLY. This reads the checks
/// *before* [`veto_if_egl_broken`], so a gap only ever means "the files are missing from this
/// container". A runtime EGL verdict may redden a row but must never trigger a 350 MB download
/// and the `restart_for_egl` exit that kills every live session. Cost: a files-present but
/// broken EGL host is not auto-re-provisioned; a present-but-wrong volume is caught instead by
/// [`crate::nvidia_volume::VolumeState`] (`Stale`/`ObsoleteLayout` refused by `adopt_current`).
pub fn nvidia_gap(env: &ProbeEnv) -> crate::nvidia_volume::Gap {
    let distro = detect_distro(env);
    let missing = |c: ReadinessCheck| c.status == FAIL;
    crate::nvidia_volume::Gap {
        egl: missing(check_nvidia_egl_vendor(env, distro))
            || missing(check_nvidia_eglcore(env, distro)),
        lib32: missing(check_nvidia_lib32(env, distro)),
    }
}

/// Emit the one-shot startup block; returns the FAIL count (WARN is not counted).
pub fn log_report(checks: &[ReadinessCheck]) -> usize {
    for c in checks.iter().filter(|c| c.status == PROVISIONING) {
        tracing::info!(check = %c.id, "host readiness: {}", c.summary);
    }
    // Unconditional, before the zero-failure early return: a warning must never go quiet
    // just because the host has no hard failures.
    for c in checks.iter().filter(|c| c.status == WARN) {
        tracing::warn!(
            token = "readiness-check-warn",
            check = %c.id,
            "host readiness WARN: {} — remediation: {}",
            c.summary,
            c.remediation
        );
    }
    // An indeterminate host probe is not a failure, but an operator should see it.
    for c in checks.iter().filter(|c| c.status == UNKNOWN) {
        tracing::warn!(
            token = "readiness-check-unknown",
            check = %c.id,
            "host readiness UNKNOWN: {} — remediation: {}",
            c.summary,
            c.remediation
        );
    }
    let failed = checks.iter().filter(|c| c.status == FAIL).count();
    let provisioning = checks.iter().filter(|c| c.status == PROVISIONING).count();
    if failed == 0 {
        // Mid-provision must not summarise as "all checks passed or skipped" — that is the
        // reassuring-but-false line this module exists to prevent.
        if provisioning > 0 {
            tracing::info!(
                checks = checks.len(),
                provisioning,
                "host readiness: no failures; {provisioning} check(s) are being remediated \
                 automatically and are not usable yet"
            );
        } else {
            tracing::info!(
                checks = checks.len(),
                "host readiness: all checks passed or skipped"
            );
        }
        return 0;
    }
    tracing::warn!(
        token = "readiness-checks-failed",
        failed,
        checks = checks.len(),
        "host readiness: {failed} check(s) FAILED — sessions may fail in ways that look unrelated. \
         Admin -> Hosts -> this host shows the same list with remediation."
    );
    for c in checks.iter().filter(|c| c.status == FAIL) {
        // The sanity family means "every session on this host will fail", not "a capability
        // may be degraded" — ERROR plus the fixed grep token, fault and fix on one line.
        if is_sanity_check(&c.id) {
            tracing::error!(
                token = "readiness-sanity-failed",
                check = %c.id,
                "{SANITY_LOG_TOKEN} FAIL [{}]: {} — RUN: {}",
                c.id,
                c.summary,
                c.remediation
            );
            continue;
        }
        tracing::warn!(
            token = "readiness-check-failed",
            check = %c.id,
            "host readiness FAIL: {} — remediation: {}",
            c.summary,
            c.remediation
        );
    }
    failed
}

// ── boot-time gate (#98) ─────────────────────────────────────────────────────

/// A boot-time sanity fault, already resolved into a log token and the check's own wording.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BootFault {
    /// Grep token for the log line. Distinct per race, so an operator (and `redeploy.sh`)
    /// can tell "wait for a restart" from "regenerate the CDI spec" without reading prose.
    pub token: &'static str,
    pub check: String,
    pub summary: String,
    pub remediation: String,
}

/// What a boot-time readiness report means for the process.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum BootAction {
    /// No boot fault this process can act on. Includes every GPU-less host.
    Continue,
    /// The host has a render node this container cannot see. A device list is fixed at
    /// container CREATE, so only a fresh start picks it up: exit non-zero and let the
    /// restart policy do it.
    ExitForRetry(BootFault),
    /// A sanity fault no restart can fix. Stay up so the readiness card carries the
    /// remediation, and name the fix once in the log.
    Stay(BootFault),
}

/// One token per condition; `redeploy.sh` classifies deploys by them and the log sites in
/// `agent.rs` repeat them as literals (the log-convention test wants a literal first field).
pub const BOOT_RENDER_NODE_TOKEN: &str = "boot-render-node-missing";
pub const BOOT_RENDER_NODE_DEFERRED_TOKEN: &str = "boot-render-node-retry-deferred";
pub const BOOT_RENDER_NODE_SPENT_TOKEN: &str = "boot-render-node-retries-spent";
pub const BOOT_RENDER_NODE_UNOPENABLE_TOKEN: &str = "boot-render-node-unopenable";
pub const BOOT_DRI_MODES_TOKEN: &str = "boot-dri-modes-stale-cdi";
pub const BOOT_HOST_RENDER_NODE_TOKEN: &str = "boot-host-render-node-missing";

/// Does this container have a `/dev/dri/renderD*` node at all? The gate's own read: the
/// `render_node` check FAILs both for "no node here" and "node here but not openable", and
/// only the first is fixed by a fresh container start. Kept out of `ReadinessCheck` — that
/// shape is the agent-api contract.
pub fn container_has_render_node(root: &Path) -> bool {
    !dir_entries_matching(&root.join("dev/dri"), |n| n.starts_with("renderD")).is_empty()
}

/// Inputs to [`boot_action`]. All of them, so the decision stays pure and testable with no
/// devices: the caller reads the world once and this function only decides.
pub struct BootInputs<'a> {
    pub checks: &'a [ReadinessCheck],
    /// Capacity's vendor-neutral answer (`/sys/class/drm` card* with a known vendor id and
    /// readable VRAM). False ⇒ never exit: a GPU-less host is not broken, it is small.
    pub gpu_present: bool,
    /// [`container_has_render_node`]'s answer. True with `render_node` FAIL means the node is
    /// here and unopenable (mode, group, device cgroup), which a restart reproduces exactly.
    pub container_has_render_node: bool,
    /// A driver-volume / CUDA-runtime provision is materialising right now. Exiting would
    /// kill a hundreds-of-MB download mid-flight, so it defers the retry to the next boot.
    pub provision_in_flight: bool,
    /// Consecutive boots that have already exited for this fault. Past
    /// [`BOOT_EXIT_MAX_ATTEMPTS`] the retry has proven useless, so stop looping and stay
    /// visible instead.
    pub prior_exits: u32,
}

/// Retries before the loop is declared useless. Docker's restart policy backs off on its own;
/// this bound is what stops a permanently-unfixable fault from hiding the readiness card
/// behind a container that never stays up.
pub const BOOT_EXIT_MAX_ATTEMPTS: u32 = 5;

/// The boot decision, pure over an already-taken readiness report.
///
/// Exit is reserved for the one fault a fresh container start actually fixes: the host kernel
/// made a render node (`host_render_node` PASS) and this container has NONE
/// (`container_has_render_node` false). The other readings of a `render_node` FAIL each get a
/// `Stay`: a synthetic-capacity dev host, the GSP-firmware case (only a host reboot fixes it),
/// and a node that is present but unopenable (mode/group/cgroup — a restart reproduces it).
pub fn boot_action(input: BootInputs<'_>) -> BootAction {
    fn find<'a>(checks: &'a [ReadinessCheck], id: &str) -> Option<&'a ReadinessCheck> {
        checks.iter().find(|c| c.id == id)
    }
    fn has_status(checks: &[ReadinessCheck], id: &str, status: &str) -> bool {
        find(checks, id).is_some_and(|c| c.status == status)
    }
    fn fault(checks: &[ReadinessCheck], id: &str, token: &'static str) -> Option<BootFault> {
        find(checks, id).map(|c| BootFault {
            token,
            check: c.id.clone(),
            summary: c.summary.clone(),
            remediation: c.remediation.clone(),
        })
    }
    let checks = input.checks;
    if !input.gpu_present {
        return BootAction::Continue;
    }
    // Ordered before the render-node arm: a stale CDI spec reproduces root-only nodes at every
    // container create, so restarting for it loops forever on the same spec. A delayed re-probe
    // is no better — the runtime creates these nodes from the spec at container create and no
    // udev runs in here, so their modes cannot change under a live process.
    if has_status(checks, "dri_node_app_access", FAIL) {
        if let Some(f) = fault(checks, "dri_node_app_access", BOOT_DRI_MODES_TOKEN) {
            return BootAction::Stay(f);
        }
    }
    if has_status(checks, "render_node", FAIL) {
        if !has_status(checks, "host_render_node", PASS) {
            // The kernel never made a node. A restart cannot conjure one; the check's own
            // initramfs/GSP remediation is the fix, and it needs the card to stay up.
            return fault(checks, "host_render_node", BOOT_HOST_RENDER_NODE_TOKEN)
                .map(BootAction::Stay)
                .unwrap_or(BootAction::Continue);
        }
        if input.container_has_render_node {
            // Present but not openable: the device list is fine, the permissions are not, and
            // a fresh container reproduces both. Its remediation names the mode/cgroup fix.
            return fault(checks, "render_node", BOOT_RENDER_NODE_UNOPENABLE_TOKEN)
                .map(BootAction::Stay)
                .unwrap_or(BootAction::Continue);
        }
        let Some(f) = fault(checks, "render_node", BOOT_RENDER_NODE_TOKEN) else {
            return BootAction::Continue;
        };
        if input.provision_in_flight {
            return BootAction::Stay(BootFault {
                token: BOOT_RENDER_NODE_DEFERRED_TOKEN,
                ..f
            });
        }
        if input.prior_exits >= BOOT_EXIT_MAX_ATTEMPTS {
            return BootAction::Stay(BootFault {
                token: BOOT_RENDER_NODE_SPENT_TOKEN,
                ..f
            });
        }
        return BootAction::ExitForRetry(f);
    }
    BootAction::Continue
}

// ── individual checks ────────────────────────────────────────────────────────

/// These constructors make proxy checks: source `local`, never `blocks`. A check that
/// rests on evidence adds its own after construction (runtime_facts, storage, host_probe).
fn pass(id: &str, summary: String) -> ReadinessCheck {
    ReadinessCheck {
        id: id.to_string(),
        status: PASS.to_string(),
        summary,
        remediation: String::new(),
        observed_at: None,
        source: Some("local".to_string()),
        blocks: None,
    }
}

fn skip(id: &str, summary: &str) -> ReadinessCheck {
    ReadinessCheck {
        id: id.to_string(),
        status: SKIP.to_string(),
        summary: summary.to_string(),
        remediation: String::new(),
        observed_at: None,
        source: Some("local".to_string()),
        blocks: None,
    }
}

fn fail(id: &str, summary: String, remediation: String) -> ReadinessCheck {
    ReadinessCheck {
        id: id.to_string(),
        status: FAIL.to_string(),
        summary,
        remediation,
        observed_at: None,
        source: Some("local".to_string()),
        blocks: None,
    }
}

/// Advisory but actionable: carries a remediation, because the exact command is the whole
/// point of a check whose finding is "look at this, but it might be fine".
fn host_container_mounts_check(observation: &MountObservation) -> ReadinessCheck {
    let check = match observation {
        MountObservation::Agree => pass(
            "host_container_mounts",
            "Required sibling-container paths agree with their host bind mounts".to_string(),
        ),
        MountObservation::Fail(error) => fail(
            "host_container_mounts",
            error.clone(),
            MOUNT_CHECK_REMEDIATION.to_string(),
        ),
        MountObservation::Indeterminate => {
            unknown("host_container_mounts", MOUNT_INDETERMINATE_SUMMARY)
        }
    };
    debug_assert!(
        check.blocks.is_none(),
        "host_container_mounts is a local check"
    );
    check
}

fn unknown(id: &str, summary: &str) -> ReadinessCheck {
    ReadinessCheck {
        id: id.to_string(),
        status: UNKNOWN.to_string(),
        summary: summary.to_string(),
        remediation: "The agent retries on the next refresh.".to_string(),
        observed_at: None,
        source: Some("local".to_string()),
        blocks: None,
    }
}

/// Advisory but actionable: carries a remediation, because the exact command is the whole
/// point of a check whose finding is "look at this, but it might be fine".
fn warn_check(id: &str, summary: String, remediation: String) -> ReadinessCheck {
    ReadinessCheck {
        id: id.to_string(),
        status: WARN.to_string(),
        summary,
        remediation,
        observed_at: None,
        source: Some("local".to_string()),
        blocks: None,
    }
}

fn provisioning(id: &str, summary: String) -> ReadinessCheck {
    ReadinessCheck {
        id: id.to_string(),
        status: PROVISIONING.to_string(),
        summary,
        // Empty on purpose: a `dnf install` line next to "we are fixing this for you" is how
        // an operator ends up doing both.
        remediation: String::new(),
        observed_at: None,
        source: Some("local".to_string()),
        blocks: None,
    }
}

/// Remediation for "the files are there but the stack does not load". Not a package-install
/// problem, so the usual `dnf` line would tell the operator to install what is installed.
fn egl_runtime_remediation(env: &ProbeEnv) -> String {
    let base = "The NVIDIA EGL libraries are present but the EGL stack this container loads does \
                not work, so no session can start. Check which libEGL.so.1 the loader resolves \
                (`docker exec <agent> /usr/local/bin/quasar-node-agent egl-selftest`); it must be \
                the IMAGE's libglvnd dispatcher, not a driver-supplied libEGL. A directory on \
                LD_LIBRARY_PATH that contains its own libEGL.so.* will shadow it.";
    match &env.nvidia_volume {
        VolumeView::Provisioned { .. } => format!(
            "{base}\nThe Quasar driver volume is in play. Re-provision it from scratch: \
             `docker compose down quasar-node-agent && docker volume rm <project>_quasar-nvidia-driver` \
             then bring the agent back up. Set QUASAR_NVIDIA_DRIVER_VOLUME=0 to fall back to \
             host driver packages instead."
        ),
        _ => base.to_string(),
    }
}

/// Veto a file-presence PASS when the EGL stack does not load (the loop-3 guard).
///
/// ONLY `EglRuntime::Broken` vetoes. `Indeterminate` (self-test timed out, killed, never ran)
/// falls through untouched — it is the absence of a verdict, and reddening a healthy host
/// because a subprocess was slow is worse than the failure this guard catches.
fn veto_if_egl_broken(check: ReadinessCheck, env: &ProbeEnv) -> ReadinessCheck {
    if check.status != PASS {
        return check;
    }
    let crate::nvidia_volume::EglRuntime::Broken { detail, loaded } = &env.egl_runtime else {
        return check;
    };
    fail(
        &check.id,
        format!(
            "{} — but the EGL stack does not load: {detail}{}",
            check.summary,
            loaded
                .as_deref()
                .map(|l| format!(" (resolved libEGL.so.1 = {l})"))
                .unwrap_or_default()
        ),
        egl_runtime_remediation(env),
    )
}

/// Shared tail of the three NVIDIA checks: the host lacks the capability, so the answer depends
/// on the driver volume. `volume_hit` re-reads the volume's real contents rather than trusting
/// the manifest — a manifest says what was written, the card answers whether it is there.
fn nvidia_gap_outcome(
    id: &str,
    env: &ProbeEnv,
    distro: Distro,
    fail_summary: &str,
    extra_remediation: &str,
    volume_hit: impl Fn(&Path) -> Option<String>,
) -> ReadinessCheck {
    let manual = |lead: &str| {
        let hint = nvidia_install_hint(distro);
        if lead.is_empty() {
            format!("{hint}\n{NVIDIA_RESTART_NOTE}")
        } else {
            format!("{lead}\n{hint}\n{NVIDIA_RESTART_NOTE}")
        }
    };
    match &env.nvidia_volume {
        VolumeView::Provisioned { root, version } => match volume_hit(root) {
            Some(where_) => pass(
                id,
                format!("{where_} — provisioned by Quasar (driver volume, v{version})"),
            ),
            None => fail(
                id,
                format!(
                    "{fail_summary} (the Quasar driver volume v{version} is provisioned but does \
                     not carry it)"
                ),
                manual(extra_remediation),
            ),
        },
        VolumeView::Provisioning { phase, percent } => provisioning(
            id,
            match percent {
                Some(p) => format!(
                    "Quasar is provisioning the matching NVIDIA driver userspace into a local \
                     volume ({phase}, {p}%) — no action needed; the agent restarts itself when \
                     it finishes"
                ),
                None => format!(
                    "Quasar is provisioning the matching NVIDIA driver userspace into a local \
                     volume ({phase}) — no action needed; the agent restarts itself when it \
                     finishes"
                ),
            },
        ),
        VolumeView::Failed(err) => fail(
            id,
            format!("Automatic NVIDIA driver-volume provisioning is waiting to retry: {err}"),
            "Resolve the specific error above. Quasar retries automatically; do not replace the host driver merely because provisioning failed.".to_string(),
        ),
        VolumeView::None => fail(id, fail_summary.to_string(), manual(extra_remediation)),
    }
}

/// Suffix on every NVIDIA remediation: driver libs are injected at container CREATE time, so a
/// host-side install stays invisible until the agent container is recreated.
const NVIDIA_RESTART_NOTE: &str =
    "Then recreate the node-agent container (Admin -> Hosts -> Restart agent, or \
     `docker compose up -d --force-recreate quasar-node-agent`) — driver libraries are \
     injected at container creation, so an in-place host install is not picked up until then.";

fn nvidia_install_hint(distro: Distro) -> String {
    match distro {
        Distro::Fedora => "sudo dnf install -y nvidia-driver-libs nvidia-driver-libs.i686 \
             egl-wayland libnvidia-egl-wayland && sudo nvidia-ctk cdi generate \
             --output=/etc/cdi/nvidia.yaml"
            .to_string(),
        Distro::Debian => {
            "sudo apt install -y libnvidia-egl-wayland1 libnvidia-gl-<driver-version> \
             libnvidia-gl-<driver-version>:i386 && sudo nvidia-ctk cdi generate \
             --output=/etc/cdi/nvidia.yaml"
                .to_string()
        }
        Distro::Arch => "sudo pacman -S --needed nvidia-utils lib32-nvidia-utils egl-wayland && \
             sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml"
            .to_string(),
        Distro::Unknown => "Install your distribution's NVIDIA *graphics* (EGL/GL) driver \
             packages — a CUDA-only install is not enough — including the 32-bit \
             (i686/i386/lib32) variant, then regenerate the container-runtime device spec \
             (`nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml`)."
            .to_string(),
    }
}

/// `10_nvidia.json` — the glvnd vendor config. Without it EGL enumerates no
/// NVIDIA vendor at all and the compositor panics on display creation (#462).
fn check_nvidia_egl_vendor(env: &ProbeEnv, distro: Distro) -> ReadinessCheck {
    const ID: &str = "nvidia_egl_vendor_json";
    if !env.nvidia {
        return skip(ID, "no NVIDIA GPU detected on this host");
    }
    let found = EGL_VENDOR_DIRS
        .iter()
        .filter_map(|d| dir_entry_matching(&env.root.join(d), |n| n.contains("nvidia")))
        .next();
    if let Some(path) = found {
        return pass(ID, format!("EGL vendor config present ({path})"));
    }
    nvidia_gap_outcome(
        ID,
        env,
        distro,
        "no NVIDIA EGL vendor config (10_nvidia.json) — the driver is installed \
         CUDA-only, so the session compositor will crash on startup",
        "",
        |root| {
            dir_entry_matching(
                &root.join(crate::nvidia_volume::layout::EGL_VENDOR_DIR),
                |n| n.contains("nvidia"),
            )
            .map(|p| format!("EGL vendor config present ({p})"))
        },
    )
}

/// `libnvidia-eglcore.so*`, the EGL implementation behind the vendor json. Present-json /
/// absent-library is a real state (stale `10_nvidia.json` from a partial uninstall).
fn check_nvidia_eglcore(env: &ProbeEnv, distro: Distro) -> ReadinessCheck {
    const ID: &str = "nvidia_eglcore_library";
    if !env.nvidia {
        return skip(ID, "no NVIDIA GPU detected on this host");
    }
    let found = LIB_DIRS
        .iter()
        .filter_map(|d| {
            dir_entry_matching(&env.root.join(d), |n| n.starts_with("libnvidia-eglcore.so"))
        })
        .next();
    if let Some(path) = found {
        return pass(ID, format!("libnvidia-eglcore resolvable ({path})"));
    }
    nvidia_gap_outcome(
        ID,
        env,
        distro,
        "libnvidia-eglcore is not resolvable — the NVIDIA EGL/GL runtime is missing \
         from this container, so hardware compositing and encode will fail",
        "",
        |root| {
            dir_entry_matching(&root.join(crate::nvidia_volume::layout::LIB64), |n| {
                n.starts_with("libnvidia-eglcore.so")
            })
            .map(|p| format!("libnvidia-eglcore resolvable ({p})"))
        },
    )
}

/// 32-bit GL, from the startup probe result rather than a re-probe (see
/// [`ProbeEnv::nvidia_lib32_path`]). Steam's native client is 32-bit.
fn check_nvidia_lib32(env: &ProbeEnv, distro: Distro) -> ReadinessCheck {
    const ID: &str = "nvidia_lib32_gl";
    if !env.nvidia {
        return skip(ID, "no NVIDIA GPU detected on this host");
    }
    if !env.nvidia_lib32_path.is_empty() {
        return pass(
            ID,
            format!("32-bit NVIDIA GL present ({})", env.nvidia_lib32_path),
        );
    }
    nvidia_gap_outcome(
        ID,
        env,
        distro,
        "no 32-bit NVIDIA GL libraries on the host — 32-bit apps (the native Steam \
         client) cannot render and exit before producing any video",
        "Install the 32-bit NVIDIA driver libraries.",
        |root| {
            dir_entry_matching(&root.join(crate::nvidia_volume::layout::LIB32), |n| {
                n.starts_with("libGLX_nvidia.so")
            })
            .map(|p| format!("32-bit NVIDIA GL present ({p})"))
        },
    )
}

/// How many DRM render nodes the HOST kernel created. `/sys/class/drm` is the kernel's own
/// view and needs no extra mount, so it answers independently of what the container runtime
/// injected into `/dev/dri`.
fn host_render_node_count(env: &ProbeEnv) -> usize {
    dir_entries_matching(&env.root.join("sys/class/drm"), |n| {
        n.starts_with("renderD")
    })
    .len()
}

/// A DRM render node the agent can actually OPEN. Existence is not enough: a passed-through
/// node whose cgroup or mode denies the open reads identically to "no GPU" inside GStreamer.
fn check_render_node(env: &ProbeEnv, _distro: Distro) -> ReadinessCheck {
    const ID: &str = "render_node";
    let dri = env.root.join("dev/dri");
    let nodes = dir_entries_matching(&dri, |n| n.starts_with("renderD"));
    if nodes.is_empty() {
        // The host's own view separates "this box has no GPU node" from the #98 boot race
        // (container created in the second before nvidia_drm made the node). Only the second
        // one is fixed by a fresh container start, and [`boot_action`] keys on this wording's
        // check pair, not on the prose.
        if host_render_node_count(env) > 0 {
            return fail(
                ID,
                "the host kernel HAS a DRM render node but none is visible to the agent — the \
                 container was created before the device existed, and a device list is fixed \
                 at container creation, so this process can never pick it up"
                    .to_string(),
                "Nothing to do by hand: the agent exits so the container restart policy starts \
                 a fresh container that re-enumerates /dev/dri. If it repeats every boot, the \
                 pass-through itself is missing — confirm `devices: [/dev/dri]` (or `gpus: all`) \
                 on the node-agent service in deploy/docker-compose.yml."
                    .to_string(),
            );
        }
        return fail(
            ID,
            "no DRM render node (/dev/dri/renderD*) is visible to the agent — hardware \
             encode is unavailable"
                .to_string(),
            "Confirm the host has a GPU with a kernel driver bound (`ls /dev/dri`), then \
             confirm the node-agent service passes it through: `devices: [/dev/dri]` in \
             deploy/docker-compose.yml. On an NVIDIA host also regenerate the CDI spec \
             (`sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml`)."
                .to_string(),
        );
    }

    // ANY node opening is a pass. Testing only the lexicographically first one fails a healthy
    // multi-GPU host whose renderD128 is an unopenable iGPU while renderD129 can encode.
    let mut errors: Vec<String> = Vec::new();
    for path in &nodes {
        match std::fs::OpenOptions::new().read(true).open(path) {
            Ok(_) => return pass(ID, format!("render node open-able ({path})")),
            Err(e) => errors.push(format!("{path}: {e}")),
        }
    }
    // List every candidate: on a multi-GPU box "renderD128 failed" sends the operator after
    // the wrong device.
    fail(
        ID,
        format!(
            "no DRM render node could be opened by the agent ({} present: {})",
            nodes.len(),
            errors.join("; ")
        ),
        "The agent process lacks access to every render node it can see. Check their \
         group/mode on the host (`ls -l /dev/dri`) and that no seccomp/device-cgroup rule \
         is blocking them for the node-agent container."
            .to_string(),
    )
}

/// `/dev/uinput` — virtual keyboard/mouse/gamepad injection. Without it a
/// session streams video that ignores every input event.
fn check_uinput(env: &ProbeEnv, _distro: Distro) -> ReadinessCheck {
    const ID: &str = "uinput";
    let path = env.root.join("dev/uinput");
    if !path.exists() {
        return fail(
            ID,
            "/dev/uinput is not visible to the agent — virtual keyboard, mouse and gamepad \
             injection will fail and sessions will not respond to input"
                .to_string(),
            "Load the module on the host (`sudo modprobe uinput`; persist it with \
             `echo uinput | sudo tee /etc/modules-load.d/uinput.conf`) and confirm the \
             node-agent service lists `/dev/uinput` under `devices:` in \
             deploy/docker-compose.yml."
                .to_string(),
        );
    }
    // WRITE, not read: injection is `write()`s plus a `UI_DEV_CREATE` ioctl, and a read-only
    // open succeeds on exactly the nodes where input silently does nothing.
    match std::fs::OpenOptions::new().write(true).open(&path) {
        Ok(_) => pass(ID, "/dev/uinput present and writable".to_string()),
        Err(e) => fail(
            ID,
            format!("/dev/uinput exists but is not writable by the agent: {e}"),
            "Input injection needs WRITE access to /dev/uinput, not just read. Check its \
             owner/mode on the host (`ls -l /dev/uinput`; it should be root-writable) and \
             the node-agent container's device cgroup rules."
                .to_string(),
        ),
    }
}

/// Unprivileged user namespaces, which `bwrap` and app-image sandboxes need to create. A
/// kernel with these disabled fails app startup with an error that never reaches the operator.
///
/// This reads the AGENT's context, not an app container's, and that is the honest limit of
/// what it can do (#76): every knob it reads is a HOST KERNEL setting, shared by both — a
/// container has no private copy of `kernel.apparmor_restrict_unprivileged_userns` — so for
/// these three the agent's answer IS the app's. The one thing that does diverge is the LSM
/// profile applied per container at `docker run`, and probing that from here would mean
/// launching a throwaway container on every capacity report. [`check_app_apparmor_profile`]
/// reports that half directly instead.
fn check_user_namespaces(env: &ProbeEnv, distro: Distro) -> ReadinessCheck {
    const ID: &str = "user_namespaces";

    // Debian-family kernels carry a second, decisive gate, checked first: a box can advertise
    // `user.max_user_namespaces=15000` and still refuse every unprivileged
    // `clone(CLONE_NEWUSER)`. Reading only the first knob calls that host ready.
    let clone_knob = env.root.join("proc/sys/kernel/unprivileged_userns_clone");
    if let Ok(body) = std::fs::read_to_string(&clone_knob) {
        if body.trim() == "0" {
            return fail(
                ID,
                "unprivileged user namespaces are disabled by                  kernel.unprivileged_userns_clone — sandboxed app launchers (bwrap, Steam's                  container runtime) cannot start and the app exits before producing video"
                    .to_string(),
                "sudo sysctl -w kernel.unprivileged_userns_clone=1 &&                  echo kernel.unprivileged_userns_clone=1 | sudo tee                  /etc/sysctl.d/99-quasar-userns.conf"
                    .to_string(),
            );
        }
    }

    // Ubuntu 24.04+ replaced the Debian clone knob with an AppArmor-backed one, and it is
    // the gate that actually decides whether Steam's bwrap/pressure-vessel bootstrap can
    // start (#76). It is a HOST kernel setting: the app container's own distro is
    // irrelevant, because a container shares the host's kernel and its LSM. Reading only
    // the two knobs above calls an Ubuntu host ready while every Steam-class app dies with
    // "Steam now requires user namespaces to be enabled".
    let apparmor_knob = env
        .root
        .join("proc/sys/kernel/apparmor_restrict_unprivileged_userns");
    if let Ok(body) = std::fs::read_to_string(&apparmor_knob) {
        if body.trim() == "1" {
            return fail(
                ID,
                "the host restricts unprivileged user namespaces through AppArmor \
                 (kernel.apparmor_restrict_unprivileged_userns=1, the Ubuntu 24.04+ default) — \
                 sandboxed app launchers (bwrap, Steam's container runtime) cannot create the \
                 namespace they need and the app exits before producing video, however much \
                 user.max_user_namespaces allows"
                    .to_string(),
                "sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 && \
                 echo kernel.apparmor_restrict_unprivileged_userns=0 | sudo tee \
                 /etc/sysctl.d/99-quasar-userns.conf"
                    .to_string(),
            );
        }
    }

    let path = env.root.join("proc/sys/user/max_user_namespaces");
    let Ok(body) = std::fs::read_to_string(&path) else {
        // Not every kernel exposes the knob; absence must never read as a failure. A present,
        // non-zero clone gate is already a positive answer.
        if clone_knob.exists() {
            return pass(
                ID,
                "user namespaces available (kernel.unprivileged_userns_clone enabled)".to_string(),
            );
        }
        return skip(
            ID,
            "kernel does not expose user.max_user_namespaces — cannot determine sandbox support",
        );
    };
    let max: u64 = body.trim().parse().unwrap_or(0);
    if max > 0 {
        return pass(ID, format!("user namespaces available (max {max})"));
    }
    let hint = match distro {
        Distro::Debian => "sudo sysctl -w user.max_user_namespaces=15000              kernel.unprivileged_userns_clone=1 && printf              'user.max_user_namespaces=15000\\nkernel.unprivileged_userns_clone=1\\n' |              sudo tee /etc/sysctl.d/99-quasar-userns.conf"
            .to_string(),
        _ => "sudo sysctl -w user.max_user_namespaces=15000 &&              echo user.max_user_namespaces=15000 | sudo tee /etc/sysctl.d/99-quasar-userns.conf"
            .to_string(),
    };
    fail(
        ID,
        "unprivileged user namespaces are disabled — sandboxed app launchers (bwrap,          Steam's container runtime) cannot start and the app exits before producing video"
            .to_string(),
        hint,
    )
}

/// Is the scoped `quasar-app` AppArmor profile loaded, so app containers can be confined by
/// it instead of running `apparmor=unconfined` (#76)?
///
/// Never a `fail`: without the profile the agent falls back to unconfined and sessions run
/// exactly as they did before, so this is a security posture the operator should see, not a
/// broken host. `skip` on a non-AppArmor host — an SELinux box gets no `apparmor=` flag at
/// all and nothing to load.
fn check_app_apparmor_profile(env: &ProbeEnv) -> ReadinessCheck {
    let over = container::app_apparmor_override();
    let name = match over.as_deref() {
        Some("unconfined") | None => container::APP_APPARMOR_PROFILE,
        Some(n) => n,
    };
    app_apparmor_check(
        container::host_uses_apparmor_in(&env.root),
        container::apparmor_profile_state(&env.root, name),
        over.as_deref(),
    )
}

/// The verdict, as a pure function of the same three inputs
/// [`container::app_apparmor_choice`] decides the launch flag from, so the card and the
/// launch cannot disagree.
fn app_apparmor_check(
    host_uses_apparmor: bool,
    state: container::AppArmorProfileState,
    override_name: Option<&str>,
) -> ReadinessCheck {
    use container::AppArmorProfileState as S;
    const ID: &str = "app_apparmor_profile";
    let profile = container::APP_APPARMOR_PROFILE;
    let load = container::APP_APPARMOR_LOAD_CMD;

    if !host_uses_apparmor {
        return skip(
            ID,
            "this host does not enforce AppArmor — app containers carry no AppArmor profile \
             and there is nothing to load",
        );
    }
    if override_name == Some("unconfined") {
        return warn_check(
            ID,
            "app containers run apparmor-unconfined because QUASAR_APP_APPARMOR_PROFILE is \
             set to `unconfined`: they keep none of docker-default's protections"
                .to_string(),
            format!(
                "Deliberate, and the escape hatch for a title the profile breaks. To go back \
                 to the scoped profile, unset QUASAR_APP_APPARMOR_PROFILE in the agent's \
                 environment and make sure {profile} is loaded: {load}"
            ),
        );
    }
    let named = override_name.unwrap_or(profile);
    match state {
        S::Loaded => pass(
            ID,
            format!("app containers are confined by the {named} AppArmor profile"),
        ),
        // Loaded but only logging. The launch still passes the profile, so nothing is worse
        // off than unconfined — but "confined" would be a false pass, which is the exact
        // class of bug #76 was filed for.
        S::Complain => warn_check(
            ID,
            format!(
                "the {named} AppArmor profile is loaded but not in enforce mode (complain): \
                 app containers launch with it, so every rule it would have applied is only \
                 logged to the kernel audit and nothing is denied"
            ),
            format!(
                "Reload it in enforce mode — `apparmor_parser -r` sets enforce unless the \
                 profile was put in complain by `aa-complain` or a symlink in \
                 /etc/apparmor.d/force-complain: {load}"
            ),
        ),
        S::NotLoaded if override_name.is_some() => warn_check(
            ID,
            format!(
                "QUASAR_APP_APPARMOR_PROFILE names the {named} AppArmor profile, which is not \
                 loaded on this host — the container runtime refuses a launch against a \
                 profile it cannot find, so every session here will fail to start"
            ),
            format!("Load {named} on the host, or unset the variable to fall back to {profile}/unconfined: {load}"),
        ),
        S::NotLoaded => warn_check(
            ID,
            format!(
                "the {profile} AppArmor profile is not loaded, so app containers run \
                 apparmor-unconfined: sessions work, but they keep none of docker-default's \
                 protections (no /proc or /sys write denies, no capability or ptrace \
                 mediation)"
            ),
            format!("Load it on the host — the agent must not load kernel policy itself: {load}"),
        ),
        // Not "no profile": the agent cannot see the list at all, so it keeps the safe
        // fallback and says which mount is missing rather than guessing.
        S::Unknown => warn_check(
            ID,
            format!(
                "cannot tell whether the {profile} AppArmor profile is loaded — the kernel's \
                 profile list does not read from in here, so app containers take the safe \
                 fallback and run apparmor-unconfined"
            ),
            format!(
                "The agent container needs the host's securityfs to answer this: \
                 `/sys/kernel/security:/host/sys/kernel/security:ro` under the node-agent \
                 service's `volumes:` (the shipped deploy/docker-compose.yml has it — a stack \
                 that predates it needs the agent recreated). Then load the profile: {load}"
            ),
        ),
    }
}

// ── GPU host post-boot sanity (#493) ─────────────────────────────────────────
//
// Host states where every surface reads green (nvidia-smi, the codec probe, registration)
// while every session dies with an error pointing at Quasar or gamescope.
//
// These never mark the host unschedulable, even though two of them predict total session
// failure: the module contract is advisory-never-a-gate, both predictions are inferences (the
// app-uid one reasons about a container that does not exist yet), and a wrong `unschedulable`
// on a healthy host is worse than a diagnosable failure with a remediation next to it.

/// (a) Does the HOST kernel have a DRM render node at all?
///
/// Post-reboot the NVIDIA modules can load from the initramfs before `/lib/firmware` mounts;
/// GSP firmware load fails `-2` and `nvidia_drm` never creates `/dev/dri/renderD128`. CUDA/NVML
/// recover, so nvidia-smi and the codec probe both pass while every session dies.
///
/// Read from sysfs, not `/dev`: `/sys/class/drm` is the host kernel's own view and needs no
/// extra mount, so it answers "did the kernel create one" independently of what the container
/// runtime injected ([`check_render_node`] covers that). Absent sysfs is `skip`, never a
/// failure — no answer is not a bad answer.
fn check_host_render_node(env: &ProbeEnv, distro: Distro) -> ReadinessCheck {
    const ID: &str = "host_render_node";
    if !env.gpu_present {
        return skip(ID, "no GPU detected on this host");
    }
    let drm = env.root.join("sys/class/drm");
    if std::fs::read_dir(&drm).is_err() {
        return skip(
            ID,
            "/sys/class/drm is not readable from the agent — cannot determine whether the \
             host kernel created a render node",
        );
    }
    let count = host_render_node_count(env);
    if count > 0 {
        return pass(
            ID,
            format!("host kernel created {count} DRM render node(s)"),
        );
    }
    let initramfs = match distro {
        Distro::Debian => "sudo update-initramfs -u -k all",
        Distro::Arch => "sudo mkinitcpio -P",
        _ => "sudo dracut -f",
    };
    fail(
        ID,
        "the host kernel has NO DRM render node (/sys/class/drm has no renderD*) even though \
         a GPU was detected — the DRM driver never created one, so every session will fail \
         while nvidia-smi and the codec probe still report success"
            .to_string(),
        format!(
            "On NVIDIA this is the post-reboot GSP firmware race: the modules loaded from the \
             initramfs before /lib/firmware was available, GSP firmware load failed with -2 and \
             nvidia_drm never created the node. Rebuild the initramfs and reboot the HOST: \
             `{initramfs} && sudo reboot`. Confirm afterwards with `ls /dev/dri` (a renderD* node \
             must be present) and `dmesg | grep -i gsp` (no firmware -2 errors)."
        ),
    )
}

/// (b) Can the APP container's uid open the `/dev/dri` nodes this container was handed? (#491)
///
/// `nvidia-cdi-refresh.service` can run before udev applies group ownership and bake
/// `fileMode: 384` (0600) with no gid into the CDI spec; Docker then reproduces root-only nodes
/// in every GPU container indefinitely, with the HOST nodes still correct. The agent is root so
/// it never notices; the app drops to `QUASAR_APP_PUID` and gamescope dies ~30s in.
///
/// The app's supplementary groups are not a guess: the launcher hands it one `--group-add`
/// per DRM-node group ([`crate::session::container::dri_group_granted`]), so this asks the
/// question the app will actually face. Probing as root instead false-passed hermes, whose
/// 0660 root:render node left RADV with permission denied and gamescope dead.
fn check_dri_node_app_access(env: &ProbeEnv, _distro: Distro) -> ReadinessCheck {
    const ID: &str = "dri_node_app_access";
    if !env.gpu_present {
        return skip(ID, "no GPU detected on this host");
    }
    if env.app_uid == Some(0) {
        return skip(
            ID,
            "app containers run as root (QUASAR_APP_PUID=0) — device modes cannot exclude them",
        );
    }
    let dri = env.root.join("dev/dri");
    let nodes = dir_entries_matching(&dri, |n| n.starts_with("renderD") || n.starts_with("card"));
    if nodes.is_empty() {
        // Already covered loudly by `render_node`; saying it twice hides the real fault.
        return skip(
            ID,
            "no /dev/dri nodes are visible to the agent (see render_node)",
        );
    }
    let unusable: Vec<String> = nodes
        .iter()
        .filter(|p| !openable_by_app(Path::new(p), env))
        .map(|p| describe_node(Path::new(p)))
        .collect();
    if unusable.is_empty() {
        return pass(
            ID,
            format!(
                "all {} /dev/dri node(s) are openable by the app user{}",
                nodes.len(),
                match env.app_uid {
                    Some(u) => format!(" (uid {u})"),
                    None => String::new(),
                }
            ),
        );
    }
    let remediation = if env.nvidia {
        "The boot-time CDI spec baked the wrong device modes (nvidia-cdi-refresh.service ran \
         before udev applied group ownership). Regenerate it on the HOST and recreate the \
         containers: `sudo nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml && \
         docker compose up -d --force-recreate`. A correct spec carries `fileMode: 438` (0666) \
         and a gid for each /dev/dri entry."
    } else {
        "Give the node a non-root owning group with group rw (the distro default is \
         `0660 root:render` for renderD* and `0660 root:video` for card*, both of which the \
         launcher grants numerically), or make it world-rw. Check the HOST with \
         `ls -l /dev/dri` and the udev rules that set it."
    };
    fail(
        ID,
        format!(
            "{} /dev/dri node(s) in this container cannot be opened by the unprivileged app \
             user{}, even with the DRM groups the launcher grants: {} — gamescope will fail with \
             `vulkan: physical device has no primary node` and the app container will exit 1 \
             about 30s after launch, while the HOST's own nodes look correct",
            unusable.len(),
            match env.app_uid {
                Some(u) => format!(" (QUASAR_APP_PUID={u})"),
                None => String::new(),
            },
            unusable.join(", ")
        ),
        remediation.to_string(),
    )
}

/// `renderD128 (mode 0660 uid 0 gid 991)` — enough to see which bit is missing without a
/// second trip to the host.
fn describe_node(path: &Path) -> String {
    use std::os::unix::fs::MetadataExt;
    let name = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_else(|| path.to_string_lossy().into_owned());
    match std::fs::metadata(path) {
        Ok(md) => format!(
            "{name} (mode {:04o} uid {} gid {})",
            md.mode() & 0o7777,
            md.uid(),
            md.gid()
        ),
        Err(_) => name,
    }
}

/// Could a process running as the app user open `path` read-write? Fails open by construction:
/// only a node whose owner, group and other bits all exclude the app says no.
fn openable_by_app(path: &Path, env: &ProbeEnv) -> bool {
    use std::os::unix::fs::MetadataExt;
    let Ok(md) = std::fs::metadata(path) else {
        // Unreadable metadata is not evidence of a bad mode.
        return true;
    };
    let mode = md.mode();
    let group_rw = mode & 0o060 == 0o060;
    let other_rw = mode & 0o006 == 0o006;
    let owner_rw = mode & 0o600 == 0o600;
    // Group access counts only when the app is IN that group: either PGID, or a gid the
    // launcher passes as `--group-add`.
    let group_reachable = env.app_gid == Some(md.gid())
        || crate::session::container::dri_group_granted(mode, md.gid());
    other_rw || (group_rw && group_reachable) || (owner_rw && env.app_uid == Some(md.uid()))
}

/// (c) Does the Quasar NVIDIA driver volume match the RUNNING kernel driver?
///
/// Provisioning is file-presence-triggered, so after a driver downgrade the volume keeps the
/// old libraries and the encoder advertises zero codecs while readiness reads all-clear. This
/// check only REPORTS: adding a second re-provision trigger path is how a download loop is
/// built (#475).
fn check_driver_volume_version(env: &ProbeEnv, _distro: Distro) -> ReadinessCheck {
    const ID: &str = "driver_volume_version";
    if !env.nvidia {
        return skip(ID, "no NVIDIA GPU detected on this host");
    }
    let manifest_path = env.nvidia_volume_root.join("manifest.json");
    let Ok(body) = std::fs::read_to_string(&manifest_path) else {
        return skip(
            ID,
            "no Quasar NVIDIA driver volume on this host (host driver packages are in use)",
        );
    };
    let Some(kernel) = env.kernel_driver_version.as_deref() else {
        return skip(
            ID,
            "the NVIDIA kernel module version is unreadable (/sys/module/nvidia/version) — \
             cannot compare it against the driver volume",
        );
    };
    let volume_version = serde_json::from_str::<serde_json::Value>(&body)
        .ok()
        .and_then(|v| v["driver_version"].as_str().map(str::to_string));
    let Some(volume_version) = volume_version else {
        return fail(
            ID,
            "the NVIDIA driver volume's manifest.json is unreadable or carries no \
             driver_version — the volume is half-written and the libraries it injects cannot \
             be trusted"
                .to_string(),
            driver_volume_reprovision_remediation(),
        );
    };
    if volume_version == kernel {
        return pass(
            ID,
            format!("driver volume matches the running kernel module (v{kernel})"),
        );
    }
    fail(
        ID,
        format!(
            "the Quasar NVIDIA driver volume was built for driver v{volume_version} but the \
             RUNNING kernel module is v{kernel} — the injected userspace does not match the \
             driver, so the encoder can end up advertising ZERO codecs while nvidia-smi and the \
             host look healthy. The volume is never repaired on its own: provisioning only \
             triggers on missing files, and these files are present (just wrong)"
        ),
        driver_volume_reprovision_remediation(),
    )
}

fn driver_volume_reprovision_remediation() -> String {
    "Delete the driver volume and let the agent rebuild it against the running driver: \
     `docker compose stop quasar-node-agent && docker volume rm \
     <project>_quasar-nvidia-driver && docker compose up -d quasar-node-agent` (find the exact \
     name with `docker volume ls | grep nvidia-driver`). To use the host's own driver packages \
     instead, set QUASAR_NVIDIA_DRIVER_VOLUME=0 and recreate the agent container."
        .to_string()
}

/// (d) A GPU host advertising no codecs is a failure, not a capability report: every launch on
/// it is rejected or dies at pipeline build.
fn check_encoder_codecs(env: &ProbeEnv, _distro: Distro) -> ReadinessCheck {
    const ID: &str = "encoder_codecs";
    if !env.gpu_present {
        return skip(ID, "no GPU detected on this host");
    }
    match &env.host_codecs {
        CodecProbe::NotProbed => skip(ID, "the encoder codec probe has not run yet"),
        CodecProbe::Probed(c) if !c.is_empty() => {
            pass(ID, format!("encoder advertises {}", c.join(", ")))
        }
        CodecProbe::Probed(_) => fail(
            ID,
            "this host has a GPU but the encoder advertises NO codecs — every session launch \
             on it will fail"
                .to_string(),
            ENCODER_CODECS_REMEDIATION.to_string(),
        ),
        CodecProbe::Failed => fail(
            ID,
            "this host has a GPU but the encoder codec probe could not initialise GStreamer — \
             no session can be encoded"
                .to_string(),
            ENCODER_CODECS_REMEDIATION.to_string(),
        ),
    }
}

const ENCODER_CODECS_REMEDIATION: &str =
    "Check the other checks on this card first — a driver-volume version mismatch \
     (driver_volume_version) or a missing render node (host_render_node) both produce exactly \
     this. Then confirm the encoder elements register inside the agent container: \
     `docker exec <agent> gst-inspect-1.0 nvh264enc` (NVIDIA) or `vah264enc` / `vah264lpenc` \
     (AMD/Intel), and check the agent log for the `codec support probed` line. `vainfo`, also in \
     the image, lists the VA entrypoints independently of GStreamer. On an Intel host the vulkan \
     elements additionally need `ANV_DEBUG=video-encode`, which this image bakes in — prefix the \
     gst-inspect with it by hand if the agent is still running an older image. Either way point \
     `GST_REGISTRY` at a scratch path for that gst-inspect: the image's registry was built with no \
     GPU present, so device-probing elements are absent from it by construction.";

// ── (#483, RH-07 #403) media reachability: evidence from real traffic ───────────
//
// The agent uses host networking, so a host firewall that drops its ICE UDP leaves every other
// surface healthy while sessions never carry video. This used to be judged by reading the
// firewall's rules, which needed NET_ADMIN in the host's network namespace (a rootless engine
// cannot grant it) and was a proxy even then. It is now judged by real traffic: a remote peer's
// connectivity checks arriving during a real session (`session::media_evidence`). Amendment 17
// fixes the rules: `source: runtime`, never `blocks`, and an inconclusive or absent result is
// `unknown` (never `warn`).

/// mDNS: Chrome sends `.local` hostnames as ICE candidates, so 5353/udp is half the media path.
const MDNS_PORT: u32 = 5353;

/// The kernel's own default ephemeral range — what the ICE sockets draw from when this host's
/// `ip_local_port_range` cannot be read.
const DEFAULT_MEDIA_PORTS: (u32, u32) = (32768, 60999);

fn check_media_reachability(env: &ProbeEnv, distro: Distro) -> ReadinessCheck {
    use crate::session::media_evidence::Evidence;
    const ID: &str = "media_reachability";
    let observed = |at: &std::time::SystemTime| {
        time::OffsetDateTime::from(*at)
            .replace_nanosecond(0)
            .ok()?
            .format(&time::format_description::well_known::Rfc3339)
            .ok()
    };
    let mut check = match &env.media {
        None => ReadinessCheck {
            id: ID.to_string(),
            status: UNKNOWN.to_string(),
            summary: "No session has shown yet whether browsers on other machines can reach \
                      this host's WebRTC media; the next session that connects, or fails to, \
                      answers it"
                .to_string(),
            remediation: String::new(),
            observed_at: None,
            source: None,
            blocks: None,
        },
        Some(Evidence::Reached { at, peer }) => {
            let mut c = pass(
                ID,
                format!(
                    "A browser on another machine ({peer}) reached this host's WebRTC media in a \
                     real session. That proves this peer's path, not that every port in the \
                     media range is open"
                ),
            );
            c.observed_at = observed(at);
            c
        }
        Some(Evidence::Blocked { at, offered }) => {
            let (lo, hi) = media_port_range(&env.root).unwrap_or(DEFAULT_MEDIA_PORTS);
            let mut c = fail(
                ID,
                format!(
                    "The last session's browser offered {offered} network candidate(s) and none of \
                     its traffic reached this host: WebRTC connection setup failed. A firewall on \
                     this host (or in front of it) is dropping inbound UDP {lo}-{hi} or \
                     UDP/{MDNS_PORT} (mDNS)"
                ),
                media_firewall_fix(distro, lo, hi),
            );
            c.observed_at = observed(at);
            c
        }
    };
    check.source = Some("runtime".to_string());
    check
}

/// The firewall fix for the media path, for the firewall the distribution ships by default.
/// The agent no longer reads the host's firewall (that needed NET_ADMIN), so it names both
/// common tools rather than guessing which one is active.
fn media_firewall_fix(distro: Distro, lo: u32, hi: u32) -> String {
    let firewalld = format!(
        "sudo firewall-cmd --permanent --add-port={lo}-{hi}/udp && sudo firewall-cmd \
         --permanent --add-service=mdns && sudo firewall-cmd --reload"
    );
    let ufw = format!("sudo ufw allow {lo}:{hi}/udp && sudo ufw allow 5353/udp");
    match distro {
        Distro::Debian => format!(
            "Allow inbound UDP {lo}-{hi} and 5353 (mDNS) to this host. With ufw: {ufw}. With \
             firewalld: {firewalld}. Restrict the source to your clients' network if you can."
        ),
        _ => format!(
            "Allow inbound UDP {lo}-{hi} and 5353 (mDNS) to this host. With firewalld: \
             {firewalld}. With ufw: {ufw}. Restrict the source to your clients' network if you \
             can."
        ),
    }
}

/// `/proc/sys/net/ipv4/ip_local_port_range`'s body (`"32768\t60999\n"`) as an inclusive range.
fn parse_ip_local_port_range(body: &str) -> Option<(u32, u32)> {
    let mut parts = body.split_whitespace();
    let lo: u32 = parts.next()?.parse().ok()?;
    let hi: u32 = parts.next()?.parse().ok()?;
    if lo == 0 || hi == 0 || lo > hi {
        return None;
    }
    Some((lo, hi))
}

/// The UDP window this host's ICE sockets draw from. `None` (not the kernel default) when it
/// could not be read, so callers can say so rather than assert a range they never saw.
fn media_port_range(root: &Path) -> Option<(u32, u32)> {
    std::fs::read_to_string(root.join("proc/sys/net/ipv4/ip_local_port_range"))
        .ok()
        .and_then(|body| parse_ip_local_port_range(&body))
}

// ── helpers ──────────────────────────────────────────────────────────────────

/// Entries in `dir` whose file name satisfies `pred`, sorted. Empty for an unreadable or absent
/// directory — both are the same answer to "is it there". Sorted because `read_dir` order is
/// filesystem-dependent and these paths appear verbatim in operator-facing summaries.
fn dir_entries_matching(dir: &Path, pred: impl Fn(&str) -> bool) -> Vec<String> {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return Vec::new();
    };
    let mut names: Vec<String> = entries
        .filter_map(|e| e.ok())
        .filter_map(|e| e.file_name().to_str().map(str::to_string))
        .filter(|n| pred(n))
        .collect();
    names.sort();
    names
        .into_iter()
        .map(|n| dir.join(n).to_string_lossy().into_owned())
        .collect()
}

/// The first matching entry, where presence alone is the question.
fn dir_entry_matching(dir: &Path, pred: impl Fn(&str) -> bool) -> Option<String> {
    dir_entries_matching(dir, pred).into_iter().next()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    /// A fake filesystem root. Every check reads through `ProbeEnv.root`, so a test never
    /// touches the real `/`.
    struct FakeRoot {
        dir: PathBuf,
    }

    impl FakeRoot {
        fn new(name: &str) -> Self {
            let dir = std::env::temp_dir().join(format!(
                "quasar-readiness-{name}-{}-{:?}",
                std::process::id(),
                std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_nanos()
            ));
            fs::create_dir_all(&dir).unwrap();
            FakeRoot { dir }
        }

        fn file(&self, rel: &str, body: &str) -> &Self {
            self.file_mode(rel, body, 0o666)
        }

        /// Fixtures stand in for device nodes, so the default is 0666 — the mode a healthy
        /// `/dev/dri/renderD*` carries. Tests that care about a mode set it explicitly.
        fn file_mode(&self, rel: &str, body: &str, mode: u32) -> &Self {
            use std::os::unix::fs::PermissionsExt;
            let p = self.dir.join(rel);
            fs::create_dir_all(p.parent().unwrap()).unwrap();
            fs::write(&p, body).unwrap();
            fs::set_permissions(&p, fs::Permissions::from_mode(mode)).unwrap();
            self
        }

        fn env(&self, nvidia: bool, lib32: &str) -> ProbeEnv {
            ProbeEnv {
                root: self.dir.clone(),
                host_root: self.dir.clone(),
                nvidia,
                gpu_present: nvidia,
                // Unprivileged by default — the case the /dev/dri mode check exists for.
                app_uid: Some(1000),
                app_gid: Some(1000),
                nvidia_volume_root: self.dir.join(NVIDIA_VOLUME_REL),
                kernel_driver_version: Some("610.57.04".to_string()),
                host_codecs: CodecProbe::Probed(vec!["h264".to_string()]),
                nvidia_lib32_path: lib32.to_string(),
                nvidia_volume: VolumeView::None,
                driver_mount_error: None,
                container_mounts: MountObservation::Agree,
                // `Unknown` means "not probed" and must never influence a verdict on its own.
                egl_runtime: crate::nvidia_volume::EglRuntime::Unknown,
                media: None,
                recovery_actor: None,
                health: platform_update::HealthOwner::default(),
                self_identity: platform_update::HealthIdentity {
                    node: "test".to_string(),
                    pid: 1,
                },
                owner_conflicts: None,
                storage: storage::StorageView::default(),
                runtime: runtime_facts::RuntimeView::NotObserved,
            }
        }

        fn env_vol(&self, volume: VolumeView) -> ProbeEnv {
            ProbeEnv {
                nvidia_volume: volume,
                ..self.env(true, "")
            }
        }
    }

    impl Drop for FakeRoot {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.dir);
        }
    }

    /// Fixtures are owned by whoever runs the tests, so a group-ownership case has to ask
    /// rather than assume a gid.
    fn fixture_gid(path: &Path) -> u32 {
        use std::os::unix::fs::MetadataExt;
        fs::metadata(path).unwrap().gid()
    }

    fn get<'a>(checks: &'a [ReadinessCheck], id: &str) -> &'a ReadinessCheck {
        checks
            .iter()
            .find(|c| c.id == id)
            .unwrap_or_else(|| panic!("no check {id} in {checks:?}"))
    }

    #[test]
    fn wrong_mount_source_is_not_treated_as_a_valid_app_path() {
        let paths = vec!["/run/quasar-agent".to_string()];
        let good = crate::runtime::Mount {
            kind: crate::runtime::MountKind::Bind,
            source: Some(crate::runtime::DaemonHostPath("/run/quasar-agent".into())),
            name: None,
            destination: "/run/quasar-agent".into(),
            read_only: Some(false),
        };
        assert!(validate_sibling_mounts(std::slice::from_ref(&good), &paths).is_none());
        let mut wrong = good.clone();
        wrong.source = Some(crate::runtime::DaemonHostPath(
            "/some/other/directory".into(),
        ));
        assert!(validate_sibling_mounts(&[wrong], &paths).is_some());
        let mut readonly = good;
        readonly.read_only = Some(true);
        assert!(validate_sibling_mounts(&[readonly], &paths).is_some());
        assert!(validate_sibling_mounts(&[], &paths).is_some());
    }

    /// A CUDA-only driver install (#462): EGL json, eglcore and 32-bit GL all missing, each
    /// named separately so the operator knows which package to install.
    #[test]
    fn cuda_only_nvidia_install_fails_the_three_nvidia_checks_separately() {
        let root = FakeRoot::new("cudaonly");
        root.file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n")
            .file("etc/os-release", "ID=fedora\n");
        let checks = probe(&root.env(true, ""));
        for id in [
            "nvidia_egl_vendor_json",
            "nvidia_eglcore_library",
            "nvidia_lib32_gl",
        ] {
            let c = get(&checks, id);
            assert_eq!(c.status, FAIL, "{id} should fail: {c:?}");
            assert!(!c.summary.is_empty(), "{id} needs a human summary");
            assert!(
                !c.remediation.is_empty(),
                "{id} must carry remediation text — the whole point of the check"
            );
        }
        // Non-NVIDIA checks are unaffected.
        assert_eq!(get(&checks, "render_node").status, PASS);
        assert_eq!(get(&checks, "uinput").status, PASS);
    }

    /// An unrecognised distro gets generic wording, never a wrong package manager.
    #[test]
    fn remediation_is_distro_aware() {
        let fedora = FakeRoot::new("fedora");
        fedora.file("etc/os-release", "ID=fedora\n");
        let f = get(&probe(&fedora.env(true, "")), "nvidia_lib32_gl")
            .remediation
            .clone();
        assert!(f.contains("dnf install"), "fedora remediation: {f}");
        assert!(
            f.contains("nvidia-driver-libs.i686"),
            "fedora remediation: {f}"
        );
        assert!(
            f.contains("nvidia-ctk cdi generate"),
            "fedora remediation: {f}"
        );

        let other = FakeRoot::new("other");
        other.file("etc/os-release", "ID=voidlinux\n");
        let o = get(&probe(&other.env(true, "")), "nvidia_lib32_gl")
            .remediation
            .clone();
        assert!(
            !o.contains("dnf "),
            "generic remediation must not name dnf: {o}"
        );
        assert!(
            !o.contains("pacman"),
            "generic remediation must not name pacman: {o}"
        );
        assert!(
            o.contains("32-bit"),
            "generic remediation should still say what to install: {o}"
        );

        let arch = FakeRoot::new("arch");
        arch.file("etc/os-release", "ID=cachyos\nID_LIKE=arch\n");
        let a = get(&probe(&arch.env(true, "")), "nvidia_lib32_gl")
            .remediation
            .clone();
        assert!(
            a.contains("lib32-nvidia-utils"),
            "ID_LIKE=arch remediation: {a}"
        );
    }

    #[test]
    fn distro_parsing_prefers_id_then_id_like() {
        assert_eq!(parse_distro("ID=fedora\n"), Distro::Fedora);
        assert_eq!(parse_distro("ID=ubuntu\nID_LIKE=debian\n"), Distro::Debian);
        assert_eq!(
            parse_distro("ID=\"pop\"\nID_LIKE=\"ubuntu debian\"\n"),
            Distro::Debian
        );
        assert_eq!(parse_distro("ID=nobara\nID_LIKE=fedora\n"), Distro::Fedora);
        assert_eq!(parse_distro("ID=voidlinux\n"), Distro::Unknown);
        assert_eq!(parse_distro(""), Distro::Unknown);
    }

    /// An AMD/Intel host must SKIP the NVIDIA checks, never fail them.
    #[test]
    fn non_nvidia_host_skips_the_nvidia_checks() {
        let root = FakeRoot::new("amd");
        root.file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n");
        let checks = probe(&root.env(false, ""));
        for id in [
            "nvidia_egl_vendor_json",
            "nvidia_eglcore_library",
            "nvidia_lib32_gl",
        ] {
            assert_eq!(get(&checks, id).status, SKIP, "{id} on a non-NVIDIA host");
        }
        assert_eq!(
            log_report(&checks),
            0,
            "a healthy AMD host logs no failures"
        );
    }

    /// A multi-GPU host whose FIRST render node is unusable but a later one works is healthy.
    #[test]
    fn any_openable_render_node_passes_on_a_multi_gpu_host() {
        use std::os::unix::fs::PermissionsExt;
        let root = FakeRoot::new("multi-gpu");
        root.file("dev/dri/renderD128", "")
            .file("dev/dri/renderD129", "")
            .file("dev/uinput", "");
        // renderD128 is present but unopenable; renderD129 is fine.
        fs::set_permissions(
            root.dir.join("dev/dri/renderD128"),
            fs::Permissions::from_mode(0o000),
        )
        .unwrap();

        let checks = probe(&root.env(false, ""));
        let c = get(&checks, "render_node");
        // Root ignores file modes, so renderD128 opens and the assertion is vacuous.
        if unsafe { libc::geteuid() } == 0 {
            assert_eq!(c.status, PASS, "{c:?}");
            return;
        }
        assert_eq!(
            c.status, PASS,
            "a usable second render node must satisfy the check: {c:?}"
        );
        assert!(
            c.summary.contains("renderD129"),
            "the summary should name the node that actually worked: {c:?}"
        );
    }

    /// When NO node opens, the failure names every candidate, not just the first.
    #[test]
    fn a_host_where_no_render_node_opens_fails_listing_every_candidate() {
        use std::os::unix::fs::PermissionsExt;
        if unsafe { libc::geteuid() } == 0 {
            return; // root opens anything; nothing to assert
        }
        let root = FakeRoot::new("no-usable-gpu");
        root.file("dev/dri/renderD128", "")
            .file("dev/dri/renderD129", "")
            .file("dev/uinput", "");
        for n in ["renderD128", "renderD129"] {
            fs::set_permissions(
                root.dir.join("dev/dri").join(n),
                fs::Permissions::from_mode(0o000),
            )
            .unwrap();
        }
        let checks = probe(&root.env(false, ""));
        let c = get(&checks, "render_node");
        assert_eq!(c.status, FAIL, "{c:?}");
        assert!(c.summary.contains("renderD128"), "{c:?}");
        assert!(
            c.summary.contains("renderD129"),
            "every candidate must be named, not just the first: {c:?}"
        );
    }

    #[test]
    fn missing_render_node_and_uinput_fail_with_remediation() {
        let root = FakeRoot::new("nodevices");
        root.file("proc/sys/user/max_user_namespaces", "15000\n");
        let checks = probe(&root.env(false, ""));
        for id in ["render_node", "uinput"] {
            let c = get(&checks, id);
            assert_eq!(c.status, FAIL, "{id}: {c:?}");
            assert!(c.remediation.contains("docker-compose.yml"), "{id}: {c:?}");
        }
        assert_eq!(log_report(&checks), 2);
    }

    /// Disabled user namespaces fail; an absent knob SKIPS.
    #[test]
    fn user_namespace_check_fails_on_zero_and_skips_when_unreadable() {
        let off = FakeRoot::new("userns-off");
        off.file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "0\n");
        assert_eq!(
            get(&probe(&off.env(false, "")), "user_namespaces").status,
            FAIL
        );

        let absent = FakeRoot::new("userns-absent");
        absent.file("dev/dri/renderD128", "").file("dev/uinput", "");
        assert_eq!(
            get(&probe(&absent.env(false, "")), "user_namespaces").status,
            SKIP
        );
    }

    /// #76: an Ubuntu 24.04+ host advertises a healthy `max_user_namespaces` and has no
    /// Debian clone knob at all, while AppArmor refuses every unprivileged `CLONE_NEWUSER`.
    /// That combination used to read as `pass` — on precisely the hosts where Steam-class
    /// apps cannot start.
    #[test]
    fn user_namespace_check_fails_when_apparmor_restricts_unprivileged_userns() {
        let ubuntu = FakeRoot::new("userns-apparmor");
        ubuntu
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n")
            .file(
                "proc/sys/kernel/apparmor_restrict_unprivileged_userns",
                "1\n",
            );
        let checks = probe(&ubuntu.env(false, ""));
        let c = get(&checks, "user_namespaces");
        assert_eq!(c.status, FAIL, "{c:?}");
        assert!(
            c.remediation
                .contains("kernel.apparmor_restrict_unprivileged_userns=0"),
            "the operator must be told the sysctl that fixes it: {c:?}"
        );

        // The same host with the restriction lifted is ready again.
        let lifted = FakeRoot::new("userns-apparmor-off");
        lifted
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n")
            .file(
                "proc/sys/kernel/apparmor_restrict_unprivileged_userns",
                "0\n",
            );
        assert_eq!(
            get(&probe(&lifted.env(false, "")), "user_namespaces").status,
            PASS
        );

        // A host that does not carry the knob at all (Fedora/SELinux) is unaffected.
        let fedora = FakeRoot::new("userns-no-apparmor-knob");
        fedora
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n");
        assert_eq!(
            get(&probe(&fedora.env(false, "")), "user_namespaces").status,
            PASS
        );
    }

    /// #76's second half: the scoped `quasar-app` profile is the difference between an
    /// AppArmor host confining app containers and running them with nothing at all, and only
    /// a human with root on the host can load it — so the card has to say which it is.
    #[test]
    fn app_apparmor_profile_check_reports_the_confinement_app_containers_will_get() {
        use container::AppArmorProfileState::*;

        // Not an AppArmor host: nothing to load, and no `apparmor=` flag is passed at all.
        assert_eq!(app_apparmor_check(false, NotLoaded, None).status, SKIP);

        let loaded = app_apparmor_check(true, Loaded, None);
        assert_eq!(loaded.status, PASS);
        assert!(loaded.summary.contains("quasar-app"), "{loaded:?}");

        // Never a failure: without the profile the agent falls back to unconfined and
        // sessions run exactly as they did before #76. Complain is loaded-but-enforcing-
        // nothing, which must not read as "confined".
        for state in [NotLoaded, Unknown, Complain] {
            let c = app_apparmor_check(true, state, None);
            assert_eq!(c.status, WARN, "{c:?}");
            assert!(
                c.remediation.contains("apparmor_parser -r"),
                "the load command is the whole point of the row: {c:?}"
            );
        }
        let complain = app_apparmor_check(true, Complain, None);
        assert!(
            complain.summary.contains("complain"),
            "the mode has to be named, not just 'not confined': {complain:?}"
        );
        // "cannot read the list" must name the missing mount, not send the operator to
        // re-load a profile that may already be there.
        assert!(app_apparmor_check(true, Unknown, None)
            .remediation
            .contains("/host/sys/kernel/security"));

        // Forced unconfined is a posture, not a fault — but it must not read as green.
        let forced = app_apparmor_check(true, Unknown, Some("unconfined"));
        assert_eq!(forced.status, WARN);
        assert!(forced.summary.contains("QUASAR_APP_APPARMOR_PROFILE"));

        // An override naming a profile the host does not have breaks every launch.
        let missing = app_apparmor_check(true, NotLoaded, Some("site-profile"));
        assert_eq!(missing.status, WARN);
        assert!(missing.summary.contains("site-profile"), "{missing:?}");
    }

    /// The wiring, end to end on a fake root: AppArmor detection and the profile list are
    /// read from the same tree the rest of the probe reads.
    #[test]
    fn app_apparmor_profile_check_reads_the_hosts_loaded_profile_list() {
        let selinux = FakeRoot::new("apparmor-selinux");
        selinux.file("dev/uinput", "");
        assert_eq!(
            get(&probe(&selinux.env(false, "")), "app_apparmor_profile").status,
            SKIP,
            "a Fedora/SELinux host has no AppArmor profile to load"
        );

        let ubuntu = FakeRoot::new("apparmor-loaded");
        ubuntu
            .file("dev/uinput", "")
            .file("sys/module/apparmor/parameters/enabled", "Y\n")
            .file(
                "host/sys/kernel/security/apparmor/profiles",
                "docker-default (enforce)\nquasar-app (enforce)\n",
            );
        assert_eq!(
            get(&probe(&ubuntu.env(false, "")), "app_apparmor_profile").status,
            PASS
        );

        let bare = FakeRoot::new("apparmor-not-loaded");
        bare.file("dev/uinput", "")
            .file("sys/module/apparmor/parameters/enabled", "Y\n")
            .file(
                "host/sys/kernel/security/apparmor/profiles",
                "docker-default (enforce)\n",
            );
        assert_eq!(
            get(&probe(&bare.env(false, "")), "app_apparmor_profile").status,
            WARN
        );

        // Loaded in complain mode: enforcing nothing, so not a pass.
        let complain = FakeRoot::new("apparmor-complain");
        complain
            .file("dev/uinput", "")
            .file("sys/module/apparmor/parameters/enabled", "Y\n")
            .file(
                "host/sys/kernel/security/apparmor/profiles",
                "docker-default (enforce)\nquasar-app (complain)\n",
            );
        let checks = probe(&complain.env(false, ""));
        let c = get(&checks, "app_apparmor_profile");
        assert_eq!(c.status, WARN, "{c:?}");
    }

    /// `max_user_namespaces` wide open while `kernel.unprivileged_userns_clone=0` refuses every
    /// unprivileged clone: reading only the first knob calls that host ready.
    #[test]
    fn userns_clone_gate_fails_even_when_max_user_namespaces_is_permissive() {
        let root = FakeRoot::new("userns-clone-off");
        root.file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n")
            .file("proc/sys/kernel/unprivileged_userns_clone", "0\n");
        let checks = probe(&root.env(false, ""));
        let c = get(&checks, "user_namespaces");
        assert_eq!(c.status, FAIL, "{c:?}");
        assert!(
            c.summary.contains("unprivileged_userns_clone"),
            "the failure must name the knob that is actually blocking: {c:?}"
        );
        assert!(
            c.remediation.contains("kernel.unprivileged_userns_clone=1"),
            "remediation must fix the RIGHT knob: {c:?}"
        );
    }

    /// A present, enabled clone gate is a positive answer even without `max_user_namespaces`.
    #[test]
    fn userns_clone_gate_enabled_passes_without_the_max_knob() {
        let root = FakeRoot::new("userns-clone-on");
        root.file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/kernel/unprivileged_userns_clone", "1\n");
        assert_eq!(
            get(&probe(&root.env(false, "")), "user_namespaces").status,
            PASS
        );
    }

    /// Injection WRITES to /dev/uinput: a read-only node must fail, since that is the host
    /// where input silently does nothing.
    #[test]
    fn uinput_check_requires_write_access_not_just_read() {
        use std::os::unix::fs::PermissionsExt;
        let root = FakeRoot::new("uinput-ro");
        root.file("dev/dri/renderD128", "").file("dev/uinput", "");
        let node = root.dir.join("dev/uinput");
        fs::set_permissions(&node, fs::Permissions::from_mode(0o444)).unwrap();

        let checks = probe(&root.env(false, ""));
        let c = get(&checks, "uinput");
        // Root opens anything, so assert only what is meaningful for the current uid.
        if unsafe { libc::geteuid() } == 0 {
            assert_eq!(c.status, PASS, "root can write regardless of mode: {c:?}");
        } else {
            assert_eq!(c.status, FAIL, "a read-only uinput node must fail: {c:?}");
            assert!(
                c.remediation.contains("WRITE"),
                "remediation must say write access is what's needed: {c:?}"
            );
        }

        // …and a writable node passes, with wording that says so.
        fs::set_permissions(&node, fs::Permissions::from_mode(0o666)).unwrap();
        let checks = probe(&root.env(false, ""));
        let c = get(&checks, "uinput");
        assert_eq!(c.status, PASS, "{c:?}");
        assert!(c.summary.contains("writable"), "{c:?}");
    }

    /// A stale `10_nvidia.json` with no library behind it fails the LIBRARY check, not nothing.
    #[test]
    fn vendor_json_without_the_library_fails_only_the_library_check() {
        let root = FakeRoot::new("stalejson");
        root.file("usr/share/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n");
        let checks = probe(&root.env(true, "/usr/lib"));
        assert_eq!(get(&checks, "nvidia_egl_vendor_json").status, PASS);
        assert_eq!(get(&checks, "nvidia_eglcore_library").status, FAIL);
    }

    /// Debian multiarch: the library lives under `/usr/lib/x86_64-linux-gnu`, not `/usr/lib64`.
    #[test]
    fn eglcore_is_found_in_the_debian_multiarch_dir() {
        let root = FakeRoot::new("multiarch");
        root.file("usr/lib/x86_64-linux-gnu/libnvidia-eglcore.so.570.86", "")
            .file("usr/share/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "");
        let checks = probe(&root.env(true, "/usr/lib32"));
        assert_eq!(get(&checks, "nvidia_eglcore_library").status, PASS);
    }

    // ── driver-volume integration (S1 revised) ──────────────────────────────

    /// The three NVIDIA ids the provisioner is responsible for.
    const NV_IDS: [&str; 3] = [
        "nvidia_egl_vendor_json",
        "nvidia_eglcore_library",
        "nvidia_lib32_gl",
    ];

    /// A CUDA-only host whose gap the driver volume FILLED reads green and says where the
    /// capability came from.
    #[test]
    fn a_provisioned_driver_volume_turns_the_nvidia_checks_green() {
        let root = FakeRoot::new("vol-ok");
        root.file("dev/dri/renderD128", "").file("dev/uinput", "");
        let vol = root.dir.join("vol");
        root.file("vol/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("vol/lib64/libnvidia-eglcore.so.610.57.04", "")
            .file("vol/lib32/libGLX_nvidia.so.610.57.04", "");

        let checks = probe(&root.env_vol(VolumeView::Provisioned {
            root: vol,
            version: "610.57.04".into(),
        }));
        for id in NV_IDS {
            let c = get(&checks, id);
            assert_eq!(c.status, PASS, "{id}: {c:?}");
            assert!(
                c.summary.contains("provisioned by Quasar"),
                "{id} must say the capability came from the driver volume: {c:?}"
            );
            assert!(c.summary.contains("610.57.04"), "{id}: {c:?}");
            assert!(c.remediation.is_empty(), "{id}: {c:?}");
        }
        assert_eq!(log_report(&checks), 0);
    }

    /// Mid-provision reports `provisioning`: not `fail` (nothing is wrong), not `pass` (it does
    /// not work yet).
    #[test]
    fn an_in_flight_provision_reports_the_provisioning_status_with_progress() {
        let root = FakeRoot::new("vol-inflight");
        root.file("dev/dri/renderD128", "").file("dev/uinput", "");
        let checks = probe(&root.env_vol(VolumeView::Provisioning {
            phase: "download".into(),
            percent: Some(40),
        }));
        for id in NV_IDS {
            let c = get(&checks, id);
            assert_eq!(c.status, PROVISIONING, "{id}: {c:?}");
            assert!(c.summary.contains("40%"), "{id}: {c:?}");
            assert!(
                c.remediation.is_empty(),
                "{id}: an operator must not be told to run dnf while we are fixing it: {c:?}"
            );
        }
        // Provisioning is not a failure — the WARN block must stay quiet.
        assert_eq!(log_report(&checks), 0);
    }

    /// When provisioning fails the card shows BOTH the real error and the manual remediation.
    #[test]
    fn a_failed_provision_keeps_the_manual_remediation_and_adds_the_error() {
        let root = FakeRoot::new("vol-failed");
        root.file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("etc/os-release", "ID=fedora\n");
        let checks = probe(&root.env_vol(VolumeView::Failed(
            "NVIDIA does not publish a .run installer for driver 610.57.04 (HTTP 404)".into(),
        )));
        for id in NV_IDS {
            let c = get(&checks, id);
            assert_eq!(c.status, FAIL, "{id}: {c:?}");
            assert!(c.summary.contains("HTTP 404"), "{id}: {c:?}");
            assert!(
                c.remediation.contains("retries automatically"),
                "{id} must explain recovery without blaming the host driver: {c:?}"
            );
        }
    }

    /// A host that already has the driver never credits the volume: CDI injection wins.
    #[test]
    fn a_native_driver_host_never_credits_the_volume() {
        let root = FakeRoot::new("vol-native");
        root.file("usr/share/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("usr/lib64/libnvidia-eglcore.so.610.57.04", "")
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "");
        let mut env = root.env_vol(VolumeView::Provisioned {
            root: root.dir.join("vol"),
            version: "610.57.04".into(),
        });
        env.nvidia_lib32_path = "/usr/lib".into();
        let checks = probe(&env);
        for id in NV_IDS {
            let c = get(&checks, id);
            assert_eq!(c.status, PASS, "{id}: {c:?}");
            assert!(
                !c.summary.contains("provisioned by Quasar"),
                "{id} must credit the host driver, not the volume: {c:?}"
            );
        }
    }

    /// A volume missing the 32-bit half (some installers ship none) fails THAT check only,
    /// with the manual remediation intact.
    #[test]
    fn a_volume_missing_the_32_bit_half_fails_only_that_check() {
        let root = FakeRoot::new("vol-no32");
        root.file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("etc/os-release", "ID=fedora\n")
            .file("vol/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("vol/lib64/libnvidia-eglcore.so.610.57.04", "");
        let checks = probe(&root.env_vol(VolumeView::Provisioned {
            root: root.dir.join("vol"),
            version: "610.57.04".into(),
        }));
        assert_eq!(get(&checks, "nvidia_egl_vendor_json").status, PASS);
        assert_eq!(get(&checks, "nvidia_eglcore_library").status, PASS);
        let c = get(&checks, "nvidia_lib32_gl");
        assert_eq!(c.status, FAIL, "{c:?}");
        assert!(c.remediation.contains("i686"), "{c:?}");
    }

    /// The trigger derives from the SAME check set the card shows, and an already-provisioning
    /// or passing check is not a gap — otherwise the agent re-downloads per capacity report.
    #[test]
    fn the_provisioner_gap_is_derived_from_the_failing_checks_only() {
        let root = FakeRoot::new("gap");
        root.file("dev/dri/renderD128", "").file("dev/uinput", "");

        let gap = nvidia_gap(&root.env(true, ""));
        assert!(gap.egl && gap.lib32, "CUDA-only host: both halves missing");

        let gap = nvidia_gap(&root.env_vol(VolumeView::Provisioning {
            phase: "download".into(),
            percent: None,
        }));
        assert!(
            !gap.any(),
            "an in-flight provision must not re-trigger provisioning"
        );

        let amd = FakeRoot::new("gap-amd");
        amd.file("dev/dri/renderD128", "").file("dev/uinput", "");
        assert!(
            !nvidia_gap(&amd.env(false, "")).any(),
            "skipped NVIDIA checks on an AMD host are not a gap"
        );

        // EGL present, 32-bit missing: lib32-only gap ⇒ no agent restart.
        let partial = FakeRoot::new("gap-lib32only");
        partial
            .file("usr/share/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("usr/lib64/libnvidia-eglcore.so.610.57.04", "")
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "");
        let gap = nvidia_gap(&partial.env(true, ""));
        assert!(!gap.egl && gap.lib32);
    }

    // ── #475: provisioning must never be triggerable by the runtime probe ───

    /// A healthy NVIDIA host whose EGL self-test could not answer. Nothing may follow: no red
    /// row, no gap, so no 350 MB download and no `restart_for_egl` exit killing live sessions.
    #[test]
    fn an_indeterminate_egl_probe_produces_no_gap_and_no_failure() {
        let root = FakeRoot::new("egl-indeterminate");
        root.file("usr/share/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("usr/lib64/libnvidia-eglcore.so.610.57.04", "")
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "");

        let mut env = root.env(true, "/usr/lib32");
        env.egl_runtime = crate::nvidia_volume::EglRuntime::Indeterminate {
            detail: "the EGL self-test did not finish within 20s".into(),
        };

        let checks = probe(&env);
        for id in [
            "nvidia_egl_vendor_json",
            "nvidia_eglcore_library",
            "nvidia_lib32_gl",
        ] {
            assert_eq!(
                get(&checks, id).status,
                PASS,
                "{id}: a probe that could not answer must not turn a healthy host red"
            );
        }
        assert!(
            !nvidia_gap(&env).any(),
            "A TIMEOUT MUST NEVER TRIGGER PROVISIONING (#475): a slow self-test on a host with a \
             complete driver used to become a 350 MB download and an agent restart that killed \
             every live session."
        );
    }

    /// Even a genuine `Broken` verdict cannot manufacture a gap while the files are present.
    /// The card still goes red (guarded by `a_broken_egl_stack_vetoes_a_file_presence_pass`),
    /// but red is where it stops.
    #[test]
    fn a_broken_egl_verdict_reddens_the_card_but_never_triggers_provisioning() {
        let root = FakeRoot::new("egl-broken-nogap");
        root.file("usr/share/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("usr/lib64/libnvidia-eglcore.so.610.57.04", "")
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "");

        let mut env = root.env(true, "/usr/lib32");
        env.egl_runtime = crate::nvidia_volume::EglRuntime::Broken {
            detail: "the loaded EGL library does not advertise EGL_EXT_device_enumeration".into(),
            loaded: Some("/usr/lib64/libEGL.so.610.57.04".into()),
        };

        // The operator is told, loudly.
        for id in ["nvidia_egl_vendor_json", "nvidia_eglcore_library"] {
            assert_eq!(get(&probe(&env), id).status, FAIL, "{id}");
        }
        // The agent does not act on it.
        assert!(
            !nvidia_gap(&env).any(),
            "the runtime veto is a diagnosis for the card, not a provisioning trigger — the gap \
             is only ever 'the FILES are missing from this container'"
        );

        // …and the CUDA-only host, where the files really are absent, still provisions.
        let cuda_only = FakeRoot::new("egl-cuda-only");
        cuda_only
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "");
        let mut env = cuda_only.env(true, "");
        env.egl_runtime = crate::nvidia_volume::EglRuntime::Broken {
            detail: "libEGL.so.1 could not be loaded at all".into(),
            loaded: None,
        };
        assert!(nvidia_gap(&env).egl);
    }

    // ── the loop-3 guard: green must mean "works", not "files exist" ────────

    /// Every file present (via the driver volume) but the EGL stack does not load: both EGL
    /// checks must go red, and the remediation must talk about the loader, not packages.
    #[test]
    fn a_broken_egl_stack_vetoes_a_file_presence_pass() {
        let root = FakeRoot::new("egl-veto");
        root.file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("etc/os-release", "ID=fedora\n")
            .file("vol/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("vol/lib64/libnvidia-eglcore.so.610.57.04", "")
            .file("vol/lib32/libGLX_nvidia.so.610.57.04", "");
        let mut env = root.env_vol(VolumeView::Provisioned {
            root: root.dir.join("vol"),
            version: "610.57.04".into(),
        });
        env.egl_runtime = crate::nvidia_volume::EglRuntime::Broken {
            detail: "the loaded EGL library does not advertise EGL_EXT_device_enumeration"
                .to_string(),
            loaded: Some("/opt/quasar/nvidia-driver/lib64/libEGL.so.610.57.04".to_string()),
        };
        let checks = probe(&env);

        for id in ["nvidia_egl_vendor_json", "nvidia_eglcore_library"] {
            let c = get(&checks, id);
            assert_eq!(
                c.status, FAIL,
                "{id} must NOT be green while the compositor cannot init EGL: {c:?}"
            );
            assert!(
                c.summary.contains("EGL_EXT_device_enumeration"),
                "{id} must name the actual defect: {c:?}"
            );
            assert!(
                c.summary.contains("libEGL.so.610.57.04"),
                "{id} must name the library that was wrongly resolved: {c:?}"
            );
            assert!(
                c.remediation.contains("egl-selftest"),
                "{id} remediation must point at the loader diagnosis: {c:?}"
            );
            assert!(
                !c.remediation.contains("dnf install"),
                "{id}: telling the operator to install already-installed packages is worse than \
                 saying nothing: {c:?}"
            );
        }
        // The 32-bit check is about app containers, not this process's loader.
        assert_eq!(get(&checks, "nvidia_lib32_gl").status, PASS);
    }

    /// `Ok` changes nothing, and `Unknown` (never probed) must never manufacture a failure.
    #[test]
    fn a_working_or_unprobed_egl_stack_leaves_the_checks_alone() {
        let root = FakeRoot::new("egl-ok");
        root.file("usr/share/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("usr/lib64/libnvidia-eglcore.so.610.57.04", "")
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "");

        let mut env = root.env(true, "/usr/lib");
        env.egl_runtime = crate::nvidia_volume::EglRuntime::Ok {
            loaded: "/usr/lib64/libEGL.so.1.1.0".into(),
        };
        for id in ["nvidia_egl_vendor_json", "nvidia_eglcore_library"] {
            assert_eq!(get(&probe(&env), id).status, PASS, "{id}");
        }

        env.egl_runtime = crate::nvidia_volume::EglRuntime::Unknown;
        for id in ["nvidia_egl_vendor_json", "nvidia_eglcore_library"] {
            assert_eq!(get(&probe(&env), id).status, PASS, "{id}");
        }
    }

    /// The veto only downgrades a PASS: it must not stamp on an in-flight `provisioning` row.
    #[test]
    fn the_egl_veto_only_downgrades_a_pass() {
        let root = FakeRoot::new("egl-veto-noop");
        root.file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("etc/os-release", "ID=fedora\n");
        let mut env = root.env_vol(VolumeView::Provisioning {
            phase: "download".into(),
            percent: Some(10),
        });
        env.egl_runtime = crate::nvidia_volume::EglRuntime::Broken {
            detail: "empty extension string".into(),
            loaded: None,
        };
        let checks = probe(&env);
        for id in ["nvidia_egl_vendor_json", "nvidia_eglcore_library"] {
            assert_eq!(
                get(&checks, id).status,
                PROVISIONING,
                "{id}: a provisioning row must survive the veto — of course EGL is broken, that \
                 is what we are fixing"
            );
        }
    }

    /// Ids are stable and unique: the admin card and the wizard key off them.
    #[test]
    fn every_check_has_a_stable_id_and_summary() {
        let root = FakeRoot::new("ids");
        let checks = probe(&root.env(true, ""));
        let mut ids: Vec<&str> = checks.iter().map(|c| c.id.as_str()).collect();
        let count = ids.len();
        ids.sort();
        ids.dedup();
        assert_eq!(ids.len(), count, "check ids must be unique");
        for c in &checks {
            assert!(!c.id.is_empty() && !c.summary.is_empty(), "{c:?}");
            assert!(
                // UNKNOWN: media_reachability before any session (amendment 17).
                matches!(
                    c.status.as_str(),
                    PASS | FAIL | SKIP | PROVISIONING | WARN | UNKNOWN
                ),
                "unknown status: {c:?}"
            );
        }
    }

    // ── GPU host post-boot sanity (#493) ─────────────────────────────────────

    #[test]
    fn invalid_explicit_driver_mount_is_a_visible_failure_with_override_remediation() {
        let root = FakeRoot::new("driver-host-override");
        let mut env = root.env(true, "");
        env.driver_mount_error = Some(
            "QUASAR_NVIDIA_DRIVER_HOST_PATH does not point to the same mounted directory".into(),
        );
        let checks = probe(&env);
        let check = get(&checks, "nvidia_driver_mount");
        assert_eq!(check.status, FAIL);
        assert!(check.summary.contains("same mounted directory"));
        assert!(check
            .remediation
            .contains(crate::nvidia_volume::HOST_PATH_ENV));
        assert!(!check.remediation.contains("overlay"));
    }

    #[test]
    fn av1_compatibility_refreshes_after_driver_change_without_blocking_readiness() {
        let root = FakeRoot::new("av1-driver-compatibility");
        root.file("sys/module/nvidia/version", "595.99.02\n")
            .file("sys/class/drm/renderD128/device/vendor", "0x10de\n")
            .file("sys/class/drm/renderD128/device/device", "0x2b85\n")
            .file("dev/dri/renderD128", "");
        let env = root.env(true, "");
        let checks = probe(&env);
        let check = get(&checks, crate::encoder_compatibility::CHECK_ID);
        assert_eq!(check.status, WARN);
        assert!(check.summary.contains("HEVC or H.264"));
        assert!(check.remediation.contains("does not substitute NVENC AV1"));
        root.file("sys/module/nvidia/version", "610.57.04\n");
        assert_eq!(check_vulkan_av1_compatibility(&env).status, PASS);
        root.file("sys/module/nvidia/version", "610.57.05\n");
        assert_eq!(check_vulkan_av1_compatibility(&env).status, SKIP);
        root.file("sys/module/nvidia/version", "595.99.02\n");
        std::fs::remove_file(root.dir.join("dev/dri/renderD128")).unwrap();
        assert_eq!(check_vulkan_av1_compatibility(&env).status, SKIP);
    }

    /// Codec advertisements are host-wide: an unknown GPU cannot hide an
    /// exposed affected GPU, regardless of render-node numbering. All NVIDIA
    /// GPUs share the loaded kernel driver version, so a known-bad 595 GPU and
    /// a validated 610 GPU cannot coexist in one real inspector snapshot.
    #[test]
    fn av1_compatibility_is_conservative_across_exposed_gpus() {
        use crate::encoder_compatibility::{inspect, Av1Compatibility};
        for affected_node in ["renderD128", "renderD130"] {
            let root = FakeRoot::new("av1-mixed-gpu");
            root.file("sys/module/nvidia/version", "595.99.02\n");
            for node in ["renderD128", "renderD129", "renderD130"] {
                root.file(&format!("dev/dri/{node}"), "")
                    .file(&format!("sys/class/drm/{node}/device/vendor"), "0x10de\n")
                    .file(
                        &format!("sys/class/drm/{node}/device/device"),
                        if node == affected_node {
                            "0x2b85\n"
                        } else {
                            "0x2684\n"
                        },
                    );
            }
            assert_eq!(
                inspect(&root.dir),
                Av1Compatibility::KnownCorrupt,
                "the unknown GPUs must not mask the affected {affected_node}"
            );
            assert_eq!(
                check_vulkan_av1_compatibility(&root.env(true, "")).status,
                WARN
            );

            // The same 5090 becomes validated after the driver changes; unknown
            // neighbouring devices must not erase that GPU-specific evidence.
            root.file("sys/module/nvidia/version", "610.57.04\n");
            assert_eq!(inspect(&root.dir), Av1Compatibility::Validated);
            root.file("sys/module/nvidia/version", "595.99.02\n");

            // sysfs lists host devices even when the container cannot use them.
            // A hidden affected GPU does not restrict this agent's codec set.
            std::fs::remove_file(root.dir.join(format!("dev/dri/{affected_node}"))).unwrap();
            assert_eq!(inspect(&root.dir), Av1Compatibility::Unknown);
        }
    }

    /// The probe-root-relative volume path must stay in lockstep with the provisioner's
    /// absolute mount, or the version check reads an empty directory and skips forever.
    #[test]
    fn the_volume_root_matches_the_provisioner_mount() {
        assert_eq!(
            PathBuf::from("/").join(NVIDIA_VOLUME_REL),
            PathBuf::from(crate::nvidia_volume::VOLUME_MOUNT)
        );
    }

    /// The post-reboot GSP race: sysfs has no render node while nvidia-smi and the codec probe
    /// both still pass.
    #[test]
    fn missing_host_render_node_fails_with_the_initramfs_remediation() {
        let root = FakeRoot::new("no-host-render");
        // sysfs exists (so the check is not skipped) but carries only a card.
        root.file("sys/class/drm/card0/dev", "226:0\n")
            .file("etc/os-release", "ID=fedora\n");
        let c = get(&probe(&root.env(true, "")), "host_render_node").clone();
        assert_eq!(c.status, FAIL, "{c:?}");
        assert!(c.remediation.contains("dracut -f"), "{c:?}");
        assert!(c.remediation.contains("reboot"), "{c:?}");

        // Present ⇒ pass; absent sysfs ⇒ skip, never a failure.
        let ok = FakeRoot::new("host-render-ok");
        ok.file("sys/class/drm/renderD128", "");
        assert_eq!(
            get(&probe(&ok.env(true, "")), "host_render_node").status,
            PASS
        );
        let nosysfs = FakeRoot::new("host-render-nosysfs");
        assert_eq!(
            get(&probe(&nosysfs.env(true, "")), "host_render_node").status,
            SKIP
        );
    }

    /// A CDI spec baked 0600 root:root: the uid-1000 app cannot open nodes the root agent can.
    /// Correct modes must not redden.
    #[test]
    fn root_only_dri_nodes_fail_for_an_unprivileged_app_uid() {
        let bad = FakeRoot::new("cdi-0600");
        bad.file_mode("dev/dri/renderD128", "", 0o600)
            .file_mode("dev/dri/card1", "", 0o600);
        let c = get(&probe(&bad.env(true, "")), "dri_node_app_access").clone();
        assert_eq!(c.status, FAIL, "{c:?}");
        assert!(c.remediation.contains("nvidia-ctk cdi generate"), "{c:?}");
        // The node and its mode/owner, so the operator need not go stat it again.
        assert!(c.summary.contains("renderD128 (mode 0600 uid"), "{c:?}");

        // Healthy: a world-rw render node, plus a 0660 card whose group the app is in.
        let good = FakeRoot::new("cdi-ok");
        good.file_mode("dev/dri/renderD128", "", 0o666)
            .file_mode("dev/dri/card1", "", 0o660);
        let mut env = good.env(true, "");
        env.app_gid = Some(fixture_gid(&good.dir.join("dev/dri/card1")));
        assert_eq!(get(&probe(&env), "dri_node_app_access").status, PASS);
    }

    /// A group with no WRITE bit is a group the launcher cannot make usable, and on an
    /// AMD host the CDI story is the wrong remediation to print.
    #[test]
    fn an_ungrantable_dri_node_fails_with_a_vendor_neutral_remediation() {
        let root = FakeRoot::new("dri-group-ro");
        root.file_mode("dev/dri/renderD128", "", 0o640);
        let mut env = root.env(false, "");
        env.gpu_present = true;
        env.app_gid = Some(fixture_gid(&root.dir.join("dev/dri/renderD128")));
        let c = get(&probe(&env), "dri_node_app_access").clone();
        assert_eq!(c.status, FAIL, "{c:?}");
        assert!(c.summary.contains("renderD128 (mode 0640 uid"), "{c:?}");
        assert!(c.remediation.contains("udev"), "{c:?}");
        assert!(!c.remediation.contains("nvidia-ctk"), "{c:?}");
    }

    /// A driver DOWNGRADE leaves the volume on the old version; provisioning is
    /// file-presence-triggered, so the check reports and does not re-provision.
    #[test]
    fn a_driver_volume_built_for_another_version_fails_loudly() {
        let stale = FakeRoot::new("vol-stale");
        stale.file(
            &format!("{NVIDIA_VOLUME_REL}/manifest.json"),
            r#"{"driver_version":"595.20"}"#,
        );
        let c = get(&probe(&stale.env(true, "")), "driver_volume_version").clone();
        assert_eq!(c.status, FAIL, "{c:?}");
        assert!(
            c.summary.contains("595.20") && c.summary.contains("610.57.04"),
            "{c:?}"
        );
        assert!(c.remediation.contains("docker volume rm"), "{c:?}");

        let matching = FakeRoot::new("vol-match");
        matching.file(
            &format!("{NVIDIA_VOLUME_REL}/manifest.json"),
            r#"{"driver_version":"610.57.04"}"#,
        );
        assert_eq!(
            get(&probe(&matching.env(true, "")), "driver_volume_version").status,
            PASS
        );

        // No volume at all is the common case (host driver packages) — skip.
        let none = FakeRoot::new("vol-none");
        assert_eq!(
            get(&probe(&none.env(true, "")), "driver_volume_version").status,
            SKIP
        );
    }

    /// A GPU host advertising zero codecs fails; "never probed" is a different answer and skips.
    #[test]
    fn a_gpu_host_with_no_codecs_fails() {
        let root = FakeRoot::new("codecs");
        let mut env = root.env(true, "");
        env.host_codecs = CodecProbe::Probed(vec![]);
        let c = get(&probe(&env), "encoder_codecs").clone();
        assert_eq!(c.status, FAIL, "{c:?}");
        assert!(c.remediation.contains("gst-inspect-1.0"), "{c:?}");

        env.host_codecs = CodecProbe::Failed;
        assert_eq!(get(&probe(&env), "encoder_codecs").status, FAIL);

        env.host_codecs = CodecProbe::NotProbed;
        assert_eq!(get(&probe(&env), "encoder_codecs").status, SKIP);

        env.host_codecs = CodecProbe::Probed(vec!["h264".into(), "av1".into()]);
        assert_eq!(get(&probe(&env), "encoder_codecs").status, PASS);
    }

    /// Off-vendor / no-GPU hosts: every sanity check no-ops and none contributes a failure.
    #[test]
    fn post_boot_sanity_no_ops_off_nvidia_and_without_a_gpu() {
        // AMD: a GPU is present so the vendor-neutral checks run, but the driver-volume
        // check (NVIDIA-only) must skip.
        let amd = FakeRoot::new("sanity-amd");
        amd.file("dev/dri/renderD128", "")
            .file("sys/class/drm/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n");
        let mut env = amd.env(false, "");
        env.gpu_present = true;
        let checks = probe(&env);
        assert_eq!(get(&checks, "driver_volume_version").status, SKIP);
        assert_eq!(get(&checks, "host_render_node").status, PASS);
        assert_eq!(get(&checks, "dri_node_app_access").status, PASS);
        assert_eq!(get(&checks, "encoder_codecs").status, PASS);
        assert_eq!(log_report(&checks), 0);

        // No GPU at all: all four skip.
        let none = FakeRoot::new("sanity-nogpu");
        none.file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n");
        let checks = probe(&none.env(false, ""));
        for id in SANITY_CHECK_IDS {
            assert_eq!(
                get(&checks, id).status,
                SKIP,
                "{id} must no-op without a GPU"
            );
        }
        assert_eq!(log_report(&checks), 0);
    }

    // ── (#98) boot-time gate ────────────────────────────────────────────────

    /// The container was created in the second before `nvidia_drm` made the node: the host
    /// kernel has one, `/dev/dri` in here does not.
    fn boot_race_1_root() -> FakeRoot {
        let root = FakeRoot::new("boot-race-1");
        root.file("sys/class/drm/renderD128", "")
            .file("sys/class/drm/card0", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n");
        root
    }

    fn boot_env(root: &FakeRoot) -> ProbeEnv {
        let mut env = root.env(false, "");
        env.gpu_present = true;
        env
    }

    /// A hand-built report, for the branches whose real-world trigger (a node the process
    /// cannot open) cannot be staged in a fixture the test user owns.
    fn boot_check(id: &str, status: &str) -> ReadinessCheck {
        ReadinessCheck {
            id: id.to_string(),
            status: status.to_string(),
            summary: format!("{id} is {status}"),
            remediation: format!("fix {id}"),
            observed_at: None,
            source: None,
            blocks: None,
        }
    }

    fn gate_at(root: &FakeRoot, checks: &[ReadinessCheck], gpu_present: bool) -> BootAction {
        boot_action(BootInputs {
            checks,
            gpu_present,
            container_has_render_node: container_has_render_node(&root.dir),
            provision_in_flight: false,
            prior_exits: 0,
        })
    }

    #[test]
    fn boot_gate_exits_for_retry_when_the_host_has_a_render_node_this_container_lacks() {
        let root = boot_race_1_root();
        let checks = probe(&boot_env(&root));
        assert_eq!(get(&checks, "host_render_node").status, PASS);
        assert_eq!(get(&checks, "render_node").status, FAIL);
        assert!(!container_has_render_node(&root.dir));
        match gate_at(&root, &checks, true) {
            BootAction::ExitForRetry(f) => {
                assert_eq!(f.token, BOOT_RENDER_NODE_TOKEN);
                assert_eq!(f.check, "render_node");
                assert!(
                    f.summary.contains("fixed at container creation"),
                    "the card must name the create-time device list: {}",
                    f.summary
                );
            }
            other => panic!("expected ExitForRetry, got {other:?}"),
        }
    }

    /// The permission fault: the node IS in the container, the agent cannot open it. A restart
    /// re-creates the same node with the same mode, so exiting would burn every retry on a
    /// message about a device list that is not the problem.
    #[test]
    fn boot_gate_stays_when_the_render_node_is_present_but_unopenable() {
        let checks = [
            boot_check("render_node", FAIL),
            boot_check("host_render_node", PASS),
            boot_check("dri_node_app_access", PASS),
        ];
        let action = boot_action(BootInputs {
            checks: &checks,
            gpu_present: true,
            container_has_render_node: true,
            provision_in_flight: false,
            prior_exits: 0,
        });
        match action {
            BootAction::Stay(f) => {
                assert_eq!(f.token, BOOT_RENDER_NODE_UNOPENABLE_TOKEN);
                assert_eq!(f.check, "render_node");
                assert_eq!(f.remediation, "fix render_node");
            }
            other => panic!("a mode/cgroup fault must never exit, got {other:?}"),
        }
    }

    /// Same report, node genuinely absent: the one case a fresh container start fixes.
    #[test]
    fn boot_gate_exit_hinges_on_the_container_having_no_node() {
        let checks = [
            boot_check("render_node", FAIL),
            boot_check("host_render_node", PASS),
        ];
        let action = boot_action(BootInputs {
            checks: &checks,
            gpu_present: true,
            container_has_render_node: false,
            provision_in_flight: false,
            prior_exits: 0,
        });
        assert!(matches!(action, BootAction::ExitForRetry(_)), "{action:?}");
    }

    #[test]
    fn boot_gate_never_exits_for_stale_cdi_modes_and_names_regeneration() {
        let root = FakeRoot::new("boot-race-2");
        // 0600 root-only nodes: what a CDI spec baked before udev applied group ownership
        // reproduces at every container create.
        root.file_mode("dev/dri/renderD128", "", 0o600)
            .file_mode("dev/dri/card0", "", 0o600)
            .file("sys/class/drm/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n");
        let checks = probe(&root.env(true, ""));
        assert_eq!(
            get(&checks, "render_node").status,
            PASS,
            "the node's owner can open 0600"
        );
        assert_eq!(get(&checks, "dri_node_app_access").status, FAIL);
        match gate_at(&root, &checks, true) {
            BootAction::Stay(f) => {
                assert_eq!(f.token, BOOT_DRI_MODES_TOKEN);
                assert!(
                    f.remediation.contains("nvidia-ctk cdi generate"),
                    "the CDI fix must be in the remediation: {}",
                    f.remediation
                );
            }
            other => panic!("expected Stay, got {other:?}"),
        }
    }

    #[test]
    fn boot_gate_stays_when_the_host_kernel_itself_has_no_render_node() {
        // GSP-firmware race: a GPU card exists, no render node anywhere. Restarting the
        // container cannot make one, so it must not loop.
        let root = FakeRoot::new("boot-no-host-node");
        root.file("sys/class/drm/card0", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n");
        let checks = probe(&boot_env(&root));
        match gate_at(&root, &checks, true) {
            BootAction::Stay(f) => assert_eq!(f.token, BOOT_HOST_RENDER_NODE_TOKEN),
            other => panic!("expected Stay, got {other:?}"),
        }
    }

    #[test]
    fn boot_gate_continues_on_a_host_with_no_gpu() {
        let root = FakeRoot::new("boot-nogpu");
        root.file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n");
        let checks = probe(&root.env(false, ""));
        assert_eq!(get(&checks, "render_node").status, FAIL);
        assert_eq!(gate_at(&root, &checks, false), BootAction::Continue);
    }

    #[test]
    fn boot_gate_continues_on_a_healthy_host() {
        let root = FakeRoot::new("boot-healthy");
        root.file("dev/dri/renderD128", "")
            .file("sys/class/drm/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n");
        let checks = probe(&boot_env(&root));
        assert_eq!(gate_at(&root, &checks, true), BootAction::Continue);
    }

    #[test]
    fn boot_gate_never_exits_while_a_provision_is_in_flight() {
        let root = boot_race_1_root();
        let checks = probe(&boot_env(&root));
        let action = boot_action(BootInputs {
            checks: &checks,
            gpu_present: true,
            container_has_render_node: false,
            provision_in_flight: true,
            prior_exits: 0,
        });
        match action {
            // Its own token: a provision is transient and clears on the next boot, unlike a
            // spent retry budget.
            BootAction::Stay(f) => assert_eq!(f.token, BOOT_RENDER_NODE_DEFERRED_TOKEN),
            other => panic!("a provision in flight must never exit, got {other:?}"),
        }
    }

    #[test]
    fn boot_gate_stops_exiting_once_the_retries_are_spent() {
        let root = boot_race_1_root();
        let checks = probe(&boot_env(&root));
        let at = |prior_exits| {
            boot_action(BootInputs {
                checks: &checks,
                gpu_present: true,
                container_has_render_node: false,
                provision_in_flight: false,
                prior_exits,
            })
        };
        assert!(matches!(
            at(BOOT_EXIT_MAX_ATTEMPTS - 1),
            BootAction::ExitForRetry(_)
        ));
        match at(BOOT_EXIT_MAX_ATTEMPTS) {
            BootAction::Stay(f) => assert_eq!(f.token, BOOT_RENDER_NODE_SPENT_TOKEN),
            other => panic!("expected Stay, got {other:?}"),
        }
    }

    // ── (#483) media reachability / firewall detection ──────────────────────

    #[test]
    fn parse_ip_local_port_range_reads_kernel_body() {
        assert_eq!(
            parse_ip_local_port_range("32768\t60999\n"),
            Some((32768, 60999))
        );
        // Space-separated is also seen in the wild.
        assert_eq!(
            parse_ip_local_port_range("  1024 65000  "),
            Some((1024, 65000))
        );
    }

    #[test]
    fn parse_ip_local_port_range_rejects_garbage() {
        assert_eq!(parse_ip_local_port_range(""), None);
        assert_eq!(parse_ip_local_port_range("not-a-number"), None);
        assert_eq!(parse_ip_local_port_range("60999"), None, "only one field");
        assert_eq!(
            parse_ip_local_port_range("60999 32768"),
            None,
            "lo > hi is not a valid range"
        );
        assert_eq!(
            parse_ip_local_port_range("0 65000"),
            None,
            "lo == 0 is not valid"
        );
    }

    /// A missing or failing detection binary must never crash the probe, and never surface as
    /// anything but `Unknown`.

    // ── RH-07 #403: media reachability from real traffic ─────────────────────────

    fn media_env(
        root: &FakeRoot,
        media: Option<crate::session::media_evidence::Evidence>,
    ) -> ProbeEnv {
        ProbeEnv {
            media,
            ..root.env(false, "")
        }
    }

    fn at() -> std::time::SystemTime {
        std::time::SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(1_790_000_000)
    }

    /// Before any session: `unknown`, never `warn`, never blocks (amendment 17).
    #[test]
    fn media_reachability_is_unknown_until_a_session_answers_it() {
        let root = FakeRoot::new("media-unknown");
        let c = check_media_reachability(&media_env(&root, None), Distro::Fedora);
        assert_eq!(c.status, UNKNOWN, "{c:?}");
        assert!(c.blocks.is_none() && c.remediation.is_empty(), "{c:?}");
        assert_eq!(c.source.as_deref(), Some("runtime"));
    }

    /// A remote peer reached the host: pass, stamped with when, and never claiming the
    /// whole port range is open.
    #[test]
    fn media_reachability_passes_on_a_remote_peers_traffic() {
        use crate::session::media_evidence::Evidence;
        let root = FakeRoot::new("media-reached");
        let c = check_media_reachability(
            &media_env(
                &root,
                Some(Evidence::Reached {
                    at: at(),
                    peer: "198.51.100.7".into(),
                }),
            ),
            Distro::Fedora,
        );
        assert_eq!(c.status, PASS, "{c:?}");
        assert!(c.summary.contains("198.51.100.7"), "{c:?}");
        assert!(c.summary.contains("not that every port"), "{c:?}");
        assert_eq!(c.observed_at.as_deref(), Some("2026-09-21T14:13:20Z"));
        assert!(c.blocks.is_none());
    }

    /// No traffic from a peer that offered candidates: fail with the firewall fix for the
    /// host's own media range, and still never block (amendment 17).
    #[test]
    fn media_reachability_fails_with_the_firewall_fix_when_no_traffic_arrived() {
        use crate::session::media_evidence::Evidence;
        let root = FakeRoot::new("media-blocked");
        root.file("proc/sys/net/ipv4/ip_local_port_range", "40000\t49999\n");
        let c = check_media_reachability(
            &media_env(
                &root,
                Some(Evidence::Blocked {
                    at: at(),
                    offered: 3,
                }),
            ),
            Distro::Fedora,
        );
        assert_eq!(c.status, FAIL, "{c:?}");
        assert!(c.summary.contains("offered 3"), "{c:?}");
        assert!(c.remediation.contains("40000-49999/udp"), "{c:?}");
        assert!(c.remediation.contains("mdns"), "{c:?}");
        assert!(c.blocks.is_none(), "media reachability never blocks: {c:?}");
        assert!(!c.remediation.contains("NET_ADMIN"), "{c:?}");
    }

    /// Amendment 17 (RH-07 #402): GPU fault messages are an optional diagnostic. Without
    /// the kernel log the check skips, names the host setting that would allow it, asks
    /// nothing (empty remediation) and never tells an operator to add a capability.
    #[test]
    fn xid_visibility_skip_names_the_host_setting_and_asks_nothing() {
        let root = FakeRoot::new("xid-remediation");
        let c = check_xid_visibility(&root.env(true, ""));
        assert_eq!(
            c.status, SKIP,
            "no dev/kmsg fixture, so this is the skip arm"
        );
        assert!(c.remediation.is_empty(), "a skip asks nothing: {c:?}");
        assert!(c.summary.contains("dmesg_restrict"), "{c:?}");
        assert!(c.summary.contains("--allow-kernel-log"), "{c:?}");
        assert!(c.summary.contains("optional"), "{c:?}");
        let text = format!("{} {}", c.summary, c.remediation);
        assert!(
            !text.contains("SYSLOG") && !text.contains("cap_add"),
            "{c:?}"
        );
        assert!(c.blocks.is_none());
    }

    mod host_probes;
    mod mounts;
    mod provenance;
    mod report;
    mod runtime_checks;
    mod storage_checks;
}
