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

use std::io::Read as _;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

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

/// What cheapest-first firewall detection could tell about this host's inbound posture.
/// Deliberately coarse: a real "is UDP N allowed" answer would need a reachability probe or
/// full rich-rule parsing, neither of which is cheap or reliable.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub enum FirewallPosture {
    /// No detection tool answered at all. `nft` IS in the image, so this means the probe could
    /// not run it (missing CAP_NET_ADMIN, a non-container build); "no signal" must never render
    /// as a warning.
    #[default]
    Unknown,
    /// A tool answered with a permissive (default-accept) posture.
    Open,
    /// A tool answered and found NO inbound filtering at all — no input-filtering chain exists
    /// on this host (#103). Distinct from `Open` (a chain exists, its policy is accept) and from
    /// `Unknown` (nothing answered): it is a positive finding for the media path.
    Unfiltered { tool: FirewallTool, detail: String },
    /// A tool answered with a default-deny/input-filtering posture. `tool` is which one
    /// answered; [`firewall_remediation`] keys its command block on it, never on distro.
    /// `media_allow` is that SAME tool's reading of its own accept rules — a default-deny
    /// posture is only a finding when nothing lets the media through.
    Filtering {
        tool: FirewallTool,
        detail: String,
        media_allow: MediaAllow,
    },
}

/// Whether the firewall's own accept rules already admit the media path, read from the rule
/// listing the tool that produced the posture printed (#67).
///
/// Reading the chain policy alone makes every CORRECTLY configured default-deny host — the
/// posture `deploy/README.md` actually prescribes — warn forever, which is how an operator
/// learns to ignore the one check that catches "video never arrives".
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum MediaAllow {
    /// Accept rules cover both the ICE UDP window and mDNS. `evidence` is the matching rule
    /// text verbatim: whether a rule's SOURCE scope reaches the client is the one thing this
    /// cannot judge, so the operator has to be able to read the rule.
    Covered { evidence: String },
    /// One half of the media path is admitted and the other is not; `gap` names the closed half.
    Partial { evidence: String, gap: String },
    /// The rules were enumerated and none of them admits the media path.
    Absent,
}

/// Which firewall tool produced the [`FirewallPosture::Filtering`] verdict. The tool in play is
/// ground truth; distro is only a hint (a Debian box can run firewalld, a Fedora box raw nft).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FirewallTool {
    Firewalld,
    Nftables,
    Iptables,
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
    /// in-flight reports [`PROVISIONING`]; failed keeps the manual remediation plus the error.
    pub nvidia_volume: VolumeView,
    /// Does the EGL stack this container loads actually WORK, as opposed to being present on
    /// disk? A file-presence pass that is green while the compositor cannot init EGL sends the
    /// operator elsewhere, so this runtime verdict VETOES it (loop-3 guard).
    pub egl_runtime: crate::nvidia_volume::EglRuntime,
    /// Firewall detection's answer, computed once at [`ProbeEnv::live`] so every reader sees
    /// the same instant and the subprocess cost is paid once, not per check.
    pub firewall: FirewallPosture,
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
        let host_root = if Path::new(HOST_ROOT).join("etc/os-release").exists() {
            PathBuf::from(HOST_ROOT)
        } else {
            PathBuf::from("/")
        };
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
            // NVIDIA only: on AMD/Intel the EGL stack is Mesa's and none of this module's
            // remediation applies, so the subprocess (and a confusing red row) buys nothing.
            egl_runtime: if nvidia {
                crate::nvidia_volume::probe_egl_runtime(
                    crate::nvidia_volume::vendor_lib_for_selftest().as_deref(),
                )
            } else {
                crate::nvidia_volume::EglRuntime::Unknown
            },
            // Vendor/GPU-independent: a firewall problem is as real on a GPU-less box.
            firewall: detect_firewall_posture(),
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

/// Run the full check set. Pure w.r.t. `env` (no global state, network, or container launches)
/// so it is cheap to re-run on every capacity report.
pub fn probe(env: &ProbeEnv) -> Vec<ReadinessCheck> {
    let distro = detect_distro(env);
    vec![
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
        check_encoder_codecs(env, distro),
        check_xid_visibility(env),
        // Applies to every host, GPU or not — not part of the sanity family.
        check_media_reachability(env, distro),
    ]
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
        Err(e) => ReadinessCheck {
            id: ID.to_string(),
            status: SKIP.to_string(),
            summary: format!(
                "{} is not readable ({e}) — GPU faults will not appear in a session trace; \
                 an Xid can only be found by hand in the host's dmesg",
                crate::gpu_kmsg::KMSG_PATH
            ),
            remediation: format!(
                "The shipped deploy/docker-compose.yml grants this, so a stack that cannot \
                 read it predates that or has the entry removed. Under the node-agent \
                 service add `{}:{}:r` to `devices:` — `:r` is the device-cgroup permission \
                 set, a bind mount's `:ro` is rejected there — and `SYSLOG` to `cap_add:`, \
                 which the device alone does not cover while kernel.dmesg_restrict=1 (the \
                 distro default). Then recreate the agent container. Optional, and nothing \
                 else changes — the tailer is read-only, off the media path, and reports \
                 only NVRM Xid and amdgpu fault lines.",
                crate::gpu_kmsg::KMSG_PATH,
                crate::gpu_kmsg::KMSG_PATH
            ),
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

fn pass(id: &str, summary: String) -> ReadinessCheck {
    ReadinessCheck {
        id: id.to_string(),
        status: PASS.to_string(),
        summary,
        remediation: String::new(),
    }
}

fn skip(id: &str, summary: &str) -> ReadinessCheck {
    ReadinessCheck {
        id: id.to_string(),
        status: SKIP.to_string(),
        summary: summary.to_string(),
        remediation: String::new(),
    }
}

fn fail(id: &str, summary: String, remediation: String) -> ReadinessCheck {
    ReadinessCheck {
        id: id.to_string(),
        status: FAIL.to_string(),
        summary,
        remediation,
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
            format!("{fail_summary} — automatic driver-volume provisioning failed: {err}"),
            manual(extra_remediation),
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
            "app containers run APPARMOR-UNCONFINED because QUASAR_APP_APPARMOR_PROFILE is \
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
                 APPARMOR-UNCONFINED: sessions work, but they keep none of docker-default's \
                 protections (no /proc or /sys write denies, no capability or ptrace \
                 mediation)"
            ),
            format!("Load it on the HOST — the agent must not load kernel policy itself: {load}"),
        ),
        // Not "no profile": the agent cannot see the list at all, so it keeps the safe
        // fallback and says which mount is missing rather than guessing.
        S::Unknown => warn_check(
            ID,
            format!(
                "cannot tell whether the {profile} AppArmor profile is loaded — the kernel's \
                 profile list does not read from in here, so app containers take the safe \
                 fallback and run APPARMOR-UNCONFINED"
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
     (AMD/Intel), and check the agent log for the `codec support probed` line.";

// ── (#483) media reachability: host firewall vs WebRTC ICE UDP ─────────────────
//
// Mechanism: `network_mode: host` puts the agent's ICE sockets on the host's netfilter, while
// the control plane's published ports are DNAT'd through docker's own ACCEPT chain and never
// hit host input filtering. A restrictive zone therefore leaves UI/API/signaling/launch all
// healthy while ICE UDP and mDNS (5353/udp, the fallback for Chrome's `.local` candidates) are
// dropped; the only symptom is `WebRTC transport never established` ~2 min into a session.
//
// Detection is best-effort, not a reachability test: it degrades to `Unknown` (no finding,
// never a failure) when no client tool answers. `nft` is baked into the image and sees the
// HOST's real rules — netfilter state follows the network namespace, so no bind mount is
// needed, only CAP_NET_ADMIN.
//
// Severity is `warn`, never `fail`: a filtering zone with a correct allow rule is fine and this
// cannot cheaply tell the two apart.

/// Per-subprocess budget; a slower answer is discarded as `None`. Bounded because this runs
/// inline in the connect/reconnect path and must never hang it.
const FIREWALL_PROBE_TIMEOUT: Duration = Duration::from_secs(2);

/// mDNS: Chrome sends `.local` hostnames as ICE candidates and there is no fallback when this
/// is unreachable, so it is half of "the media path is open", not a nicety.
const MDNS_PORT: u32 = 5353;

/// The kernel's own default ephemeral range — what the ICE sockets draw from when this host's
/// `ip_local_port_range` cannot be read.
const DEFAULT_MEDIA_PORTS: (u32, u32) = (32768, 60999);

/// Would a host firewall silently drop the agent's ICE UDP while every other surface reports
/// fine? Mechanism in the section comment above.
fn check_media_reachability(env: &ProbeEnv, distro: Distro) -> ReadinessCheck {
    const ID: &str = "media_reachability";
    match &env.firewall {
        FirewallPosture::Unknown => skip(
            ID,
            "could not determine whether a host firewall would block WebRTC media — no \
             firewalld or nft client tool answered from inside the agent container (nft is in \
             the image and needs CAP_NET_ADMIN, which the compose file grants)",
        ),
        FirewallPosture::Unfiltered { detail, .. } => pass(
            ID,
            format!(
                "no host firewall filters inbound traffic ({detail}) — nothing here would block \
                 WebRTC media"
            ),
        ),
        FirewallPosture::Open => pass(
            ID,
            "the detected host firewall reports a default-accept posture — no evidence it would \
             block WebRTC media"
                .to_string(),
        ),
        FirewallPosture::Filtering {
            tool,
            detail,
            media_allow,
        } => filtering_check(ID, env, distro, *tool, detail, media_allow),
    }
}

/// The filtering branch, split by what the tool's own accept rules say (#67). A filtering
/// posture is the CORRECT configuration once the documented rules are in place, so `Covered`
/// passes; the warning is reserved for a media path that really is closed.
fn filtering_check(
    id: &str,
    env: &ProbeEnv,
    distro: Distro,
    tool: FirewallTool,
    detail: &str,
    media_allow: &MediaAllow,
) -> ReadinessCheck {
    let (lo, hi) = media_port_range(&env.root).unwrap_or(DEFAULT_MEDIA_PORTS);
    // The symptom, shared by both warning shapes: it is the only way an operator recognises
    // this from what they actually saw.
    const SYMPTOM: &str = "The control plane's traffic is container-DNAT'd and unaffected by \
         it, so every other readiness and health surface can look completely healthy while this \
         silently drops the node agent's own WebRTC ICE UDP (host networking, no DNAT): sessions \
         launch and appear to negotiate, then the agent reaps them about 2 minutes later logging \
         \"WebRTC transport never established\" having delivered no video at all";
    match media_allow {
        MediaAllow::Covered { evidence } => pass(
            id,
            format!(
                "a host firewall with a default-deny/input-filtering posture is active \
                 ({detail}), and its own rules already accept the WebRTC media path — UDP \
                 {lo}-{hi} and UDP/{MDNS_PORT} (mDNS) are both covered by: {evidence}. If video \
                 still never arrives, check those rules' SOURCE scope covers the client's \
                 subnet — that is the one thing this check cannot judge"
            ),
        ),
        MediaAllow::Partial { evidence, gap } => warn_check(
            id,
            format!(
                "a host firewall with a default-deny/input-filtering posture is active \
                 ({detail}) and its rules accept only part of the WebRTC media path (matched: \
                 {evidence}) — {gap}. {SYMPTOM}"
            ),
            firewall_remediation(env, tool, distro),
        ),
        MediaAllow::Absent => warn_check(
            id,
            format!(
                "a host firewall with a default-deny/input-filtering posture is active \
                 ({detail}), and no accept rule covering UDP {lo}-{hi} or UDP/{MDNS_PORT} \
                 (mDNS) was found in its active rule set. {SYMPTOM}"
            ),
            firewall_remediation(env, tool, distro),
        ),
    }
}

/// Remediation for a filtering firewall, keyed on the TOOL that answered detection, never on
/// `distro` — a distro-keyed command hands the operator a tool that is not the one filtering
/// their traffic. `distro` is a secondary hint only, for wording that is genuinely
/// distro-specific (the FedoraServer-zone sentence, shown only when firewalld and Fedora agree).
/// Full writeup: `deploy/README.md` §"Host firewall blocking WebRTC media".
fn firewall_remediation(env: &ProbeEnv, tool: FirewallTool, distro: Distro) -> String {
    let probed = media_port_range(&env.root);
    let (lo, hi) = probed.unwrap_or(DEFAULT_MEDIA_PORTS);
    let port_range = format!("{lo}-{hi}");
    let range_note = if probed.is_some() {
        String::new()
    } else {
        " (this host's own net.ipv4.ip_local_port_range was not readable — this is the Linux \
         kernel default, confirm the real range with `cat /proc/sys/net/ipv4/ip_local_port_range` \
         on the host)"
            .to_string()
    };

    let lead = format!(
        "Two things must be reachable, inbound to this host, from client devices on your \
         LAN/VPN subnet — never from `0.0.0.0/0`: UDP {port_range} (the node agent's WebRTC \
         media port range — ICE and RTP both ride on it{range_note}) and UDP/5353 (mDNS — \
         Chrome sends `.local` hostnames as ICE candidates, and without mDNS reachable there is \
         no fallback). Full writeup: deploy/README.md §\"Host firewall blocking WebRTC media\"."
    );

    let command = match tool {
        FirewallTool::Firewalld => {
            let fedora_hint = if distro == Distro::Fedora {
                " On Fedora Server specifically — the box this issue was first found on — the \
                 default FedoraServer zone allows only ssh/cockpit/dhcpv6, so this is very \
                 likely the cause if you haven't touched the firewall since install."
            } else {
                ""
            };
            format!(
                "firewalld detected. Scope the exception to the LAN rather than opening the \
                 host: `sudo firewall-cmd --permanent --zone=<zone> --add-rich-rule='rule \
                 family=ipv4 source address=<lan-subnet> port port={port_range} protocol=udp \
                 accept' && sudo firewall-cmd --permanent --zone=<zone> --add-service=mdns && \
                 sudo firewall-cmd --reload`. Replace <zone> with the zone `firewall-cmd \
                 --get-default-zone` reports and <lan-subnet> with your LAN, e.g. \
                 192.168.1.0/24.{fedora_hint}"
            )
        }
        FirewallTool::Nftables => format!(
            "nftables detected. Add an INPUT accept rule for UDP {port_range} and 5353/udp, \
             scoped to <lan-subnet>, ahead of the default-deny/reject rule in the base input \
             chain (`nft list ruleset` shows the current chain to edit)."
        ),
        FirewallTool::Iptables => format!(
            "iptables detected: `sudo iptables -I INPUT -p udp -s <lan-subnet> --dport \
             {port_range} -j ACCEPT && sudo iptables -I INPUT -p udp -s <lan-subnet> --dport \
             5353 -j ACCEPT`, inserted ahead of your existing default-deny rule, then persist it \
             however your distro expects (`iptables-save`, `netfilter-persistent`, etc.)."
        ),
    };

    format!("{lead} {command}")
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

/// Live, best-effort firewall detection: firewalld's CLI first, then nftables/iptables INPUT
/// policy. Every step degrades to "no signal", never an error — a missing binary is the
/// expected case on the stock image, and these probes must never hard-fail.
fn detect_firewall_posture() -> FirewallPosture {
    // Live probe, so the real root: a fake one is only ever passed in tests, which drive the
    // pure parsers directly.
    let media = media_port_range(Path::new("/")).unwrap_or(DEFAULT_MEDIA_PORTS);
    // Each tool's posture and its accept rules come from the SAME listing — reading one tool's
    // policy against another's rules is how a host gets told the opposite of the truth.
    let firewalld = firewalld_zone_listing().and_then(|listing| {
        parse_firewalld_zone_target(&listing)
            .map(|target| (target, firewalld_media_allow(&listing, media)))
    });
    let nft = run_with_timeout("nft", &["list", "ruleset"]).and_then(|ruleset| {
        if let Some(policy) = parse_nft_input_policy(&ruleset) {
            Some(NftSignal::Policy {
                policy,
                media_allow: nft_media_allow(&ruleset, media),
            })
        } else if !nft_has_input_base_chain(&ruleset) {
            Some(NftSignal::NoInputChain)
        } else {
            // A base chain whose policy word could not be read: no signal, not a finding.
            None
        }
    });
    let iptables = iptables_input_listing().and_then(|out| {
        parse_iptables_input_policy(&out).map(|policy| (policy, iptables_media_allow(&out, media)))
    });
    combine_firewall_signals(
        firewalld.as_ref().map(|(t, m)| (t.as_str(), m.clone())),
        nft,
        iptables.as_ref().map(|(p, m)| (p.as_str(), m.clone())),
    )
}

/// `iptables -S INPUT`, preferring whichever of the two front-ends actually answers with a
/// chain listing. The full listing, not just the policy line: the accept RULES are in it.
fn iptables_input_listing() -> Option<String> {
    run_with_timeout("iptables", &["-S", "INPUT"])
        .filter(|out| parse_iptables_input_policy(out).is_some())
        .or_else(|| run_with_timeout("iptables-nft", &["-S", "INPUT"]))
}

/// The active zone's whole `--list-all` listing — target line, ports, services and rich rules
/// in one read. `--state` first: querying zones against an absent daemon makes `firewall-cmd`
/// wait on a D-Bus reply that never comes.
fn firewalld_zone_listing() -> Option<String> {
    let state = run_with_timeout("firewall-cmd", &["--state"])?;
    if state.trim() != "running" {
        return None;
    }
    let zone = run_with_timeout("firewall-cmd", &["--get-default-zone"])?;
    let zone = zone.trim();
    if zone.is_empty() {
        return None;
    }
    let zone_arg = format!("--zone={zone}");
    run_with_timeout("firewall-cmd", &[&zone_arg, "--list-all"])
}

/// stdout as UTF-8 iff the command spawns, exits within [`FIREWALL_PROBE_TIMEOUT`], and
/// succeeds. Every other outcome is `None`. A `None` is a fact about the probe, never about the
/// host, so callers must not distinguish "absent" from "errored".
fn run_with_timeout(cmd: &str, args: &[&str]) -> Option<String> {
    use std::process::{Command, Stdio};
    let mut child = Command::new(cmd)
        .args(args)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .ok()?;
    let deadline = Instant::now() + FIREWALL_PROBE_TIMEOUT;
    loop {
        match child.try_wait() {
            Ok(Some(status)) => {
                if !status.success() {
                    return None;
                }
                break;
            }
            Ok(None) if Instant::now() < deadline => {
                std::thread::sleep(Duration::from_millis(20));
            }
            Ok(None) => {
                let _ = child.kill();
                let _ = child.wait();
                return None;
            }
            Err(_) => return None,
        }
    }
    let mut out = String::new();
    child.stdout.take()?.read_to_string(&mut out).ok()?;
    Some(out)
}

/// The zone's `target:` directive (`ACCEPT`, `DROP`, `REJECT`, `default`, ...).
fn parse_firewalld_zone_target(list_all: &str) -> Option<String> {
    list_all.lines().find_map(|line| {
        line.trim()
            .strip_prefix("target:")
            .map(|v| v.trim().to_string())
    })
}

/// Filtering unless explicitly `ACCEPT`. `default` (firewalld's own, and FedoraServer's) is
/// reject-unless-listed, not permissive.
fn firewalld_target_is_filtering(target: &str) -> bool {
    !target.eq_ignore_ascii_case("ACCEPT")
}

/// The base input chain's posture as a policy WORD (`"accept"`, or a filtering word), or `None`
/// when no `hook input` base chain exists. Two signals, in order:
///
///  1. The chain's own declared `policy` word — the hand-configured nftables case.
///  2. An unconditional catch-all verdict in the chain body. firewalld's nftables backend
///     (Fedora's default) always declares `policy accept` and enforces the zone target with a
///     trailing `reject with icmpx admin-prohibited`, so reading only the declared word
///     misreads every such host as `Open` (#527). A conditional rule like `ct state invalid
///     drop` carries a leading match and must not trigger this.
fn parse_nft_input_policy(ruleset: &str) -> Option<String> {
    let body = nft_base_chain_body(ruleset, "hook input")?;
    let declared = nft_chain_declared_policy(&body);
    if let Some(policy) = &declared {
        if policy != "accept" {
            return Some(policy.clone());
        }
    }
    if nft_has_unconditional_reject_or_drop(&body) {
        return Some("reject".to_string());
    }
    declared
}

/// Does the ruleset declare ANY base chain on the input hook? A ruleset without one filters no
/// inbound traffic at all — the common shape on a host where only Docker programs netfilter
/// (nat/forward/raw tables, no input hook) — which is a finding, not a missing signal.
fn nft_has_input_base_chain(ruleset: &str) -> bool {
    nft_base_chain_body(ruleset, "hook input").is_some()
}

/// The declared `policy` word from the chain's `hook input` header line, lowercased.
fn nft_chain_declared_policy(body: &str) -> Option<String> {
    for line in body.lines() {
        if !line.contains("hook input") {
            continue;
        }
        let idx = line.find("policy")?;
        let word = line[idx + "policy".len()..]
            .trim()
            .trim_end_matches(';')
            .split_whitespace()
            .next()?;
        return Some(word.to_ascii_lowercase());
    }
    None
}

/// A rule line whose entire trimmed text IS the verdict — firewalld's catch-all pattern. A
/// conditional drop like `ct state invalid drop` does not qualify.
fn nft_has_unconditional_reject_or_drop(body: &str) -> bool {
    body.lines().any(|line| {
        let t = line.trim();
        t.starts_with("reject") || t == "drop"
    })
}

/// The full `chain NAME { ... }` block containing the first line matching `needle`. Counting
/// every `{`/`}` character is safe: inline set literals (`ct state { established, related }
/// accept`) print on one line and net to zero depth.
fn nft_base_chain_body(ruleset: &str, needle: &str) -> Option<String> {
    let lines: Vec<&str> = ruleset.lines().collect();
    let needle_idx = lines.iter().position(|l| l.contains(needle))?;
    let mut start = needle_idx;
    while start > 0 && !lines[start].trim_start().starts_with("chain ") {
        start -= 1;
    }
    if !lines[start].trim_start().starts_with("chain ") {
        return None;
    }
    let mut depth: i32 = 0;
    let mut out = String::new();
    for line in &lines[start..] {
        depth += line.matches('{').count() as i32;
        depth -= line.matches('}').count() as i32;
        out.push_str(line);
        out.push('\n');
        if depth <= 0 {
            break;
        }
    }
    Some(out)
}

/// The chain's default policy (`-P INPUT <POLICY>`), lowercased.
fn parse_iptables_input_policy(output: &str) -> Option<String> {
    output.lines().find_map(|line| {
        line.trim()
            .strip_prefix("-P INPUT ")
            .map(|rest| rest.trim().to_ascii_lowercase())
    })
}

/// A chain policy counts as filtering unless it is explicitly `accept`.
fn policy_is_filtering(policy: &str) -> bool {
    policy != "accept"
}

// ── (#67) the accept rules, not just the chain policy ─────────────────────────
//
// A default-deny INPUT chain is what `deploy/README.md` prescribes; the rules in front of it
// are the configuration. Reading only the policy therefore warns forever at a host that is set
// up correctly. Each parser below reduces its tool's own rule listing to the UDP intervals it
// accepts inbound, and one shared verdict function judges them — so the three tools cannot
// drift apart in what "the media path is open" means.
//
// Still best-effort, and still `warn` rather than `fail`: a rule's SOURCE scope may not reach
// the client, which no rule listing can settle. That caveat rides in the pass summary.

/// A UDP accept rule reduced to the inclusive port interval it opens, plus the rule text that
/// produced it — the operator needs to read the real rule, not our summary of it.
#[derive(Debug, Clone, PartialEq, Eq)]
struct UdpAccept {
    lo: u32,
    hi: u32,
    rule: String,
}

/// Does the UNION of `accepts` cover every port in `lo..=hi`? Union, not rule-by-rule: two
/// adjacent rules cover a window neither covers alone.
fn accepts_cover(accepts: &[UdpAccept], lo: u32, hi: u32) -> bool {
    let mut intervals: Vec<(u32, u32)> = accepts.iter().map(|a| (a.lo, a.hi)).collect();
    intervals.sort_unstable();
    let mut cursor = lo;
    for (a, b) in intervals {
        if a > cursor {
            return false;
        }
        if b >= cursor {
            cursor = b.saturating_add(1);
        }
        if cursor > hi {
            return true;
        }
    }
    cursor > hi
}

/// The shared verdict: the media path needs BOTH the ICE window and mDNS, so half a
/// configuration is its own finding rather than a pass or a bare "firewall is on".
fn media_allow_from_accepts(accepts: &[UdpAccept], media: (u32, u32)) -> MediaAllow {
    let (lo, hi) = media;
    let range_ok = accepts_cover(accepts, lo, hi);
    let mdns_ok = accepts_cover(accepts, MDNS_PORT, MDNS_PORT);

    // Only the rules that bear on the media path, deduped (an inline port set yields one entry
    // per interval) and capped — this lands in an operator-facing summary line.
    let mut evidence: Vec<&str> = Vec::new();
    for a in accepts {
        let touches_media = a.lo <= hi && a.hi >= lo;
        let touches_mdns = a.lo <= MDNS_PORT && a.hi >= MDNS_PORT;
        if (touches_media || touches_mdns) && !evidence.contains(&a.rule.as_str()) {
            evidence.push(&a.rule);
        }
    }
    evidence.truncate(4);
    let evidence = evidence.join("; ");

    match (range_ok, mdns_ok) {
        (true, true) => MediaAllow::Covered { evidence },
        (false, false) => MediaAllow::Absent,
        (true, false) => MediaAllow::Partial {
            evidence,
            gap: format!(
                "UDP/{MDNS_PORT} (mDNS) is not accepted, so Chrome's `.local` ICE candidates \
                 have no fallback"
            ),
        },
        (false, true) => MediaAllow::Partial {
            evidence,
            gap: format!("UDP {lo}-{hi} (the media window) is not fully accepted"),
        },
    }
}

/// `"5353"`, or a range written with `sep` (`-` in nft/firewalld, `:` in iptables). A port
/// written as a service name does not parse, and is simply not counted as evidence.
fn parse_port_interval(spec: &str, sep: char) -> Option<(u32, u32)> {
    let spec = spec.trim();
    match spec.split_once(sep) {
        Some((a, b)) => {
            let lo: u32 = a.trim().parse().ok()?;
            let hi: u32 = b.trim().parse().ok()?;
            (lo <= hi).then_some((lo, hi))
        }
        None => spec.parse().ok().map(|p| (p, p)),
    }
}

/// The nft ruleset's reading of the media path.
fn nft_media_allow(ruleset: &str, media: (u32, u32)) -> MediaAllow {
    media_allow_from_accepts(&nft_udp_accepts(ruleset), media)
}

/// Every UDP accept on the INBOUND path. Chains declaring a non-input hook are skipped: an
/// accept on the output or forward path says nothing about inbound media. A hookless chain
/// counts — firewalld renders a zone's real allow rules into `filter_IN_<zone>_allow` and
/// jumps to it from the base input chain.
fn nft_udp_accepts(ruleset: &str) -> Vec<UdpAccept> {
    let mut out = Vec::new();
    let mut in_non_input_chain = false;
    for line in ruleset.lines() {
        let t = line.trim();
        if t.starts_with("chain ") {
            in_non_input_chain = false;
        } else if let Some(header) = t.strip_prefix("type ") {
            // `type filter hook output priority 0; policy accept;`
            in_non_input_chain = header.contains("hook ") && !header.contains("hook input");
        } else if !in_non_input_chain {
            out.extend(nft_rule_udp_accepts(t));
        }
    }
    out
}

/// One nft rule line into the UDP intervals it accepts. Empty unless the line matches
/// `udp dport <spec>` and reaches an `accept` verdict after it — a negated match (`!=`), a
/// jump, or a drop/reject is not an allow.
fn nft_rule_udp_accepts(rule: &str) -> Vec<UdpAccept> {
    let tokens: Vec<&str> = rule.split_whitespace().collect();
    let Some(dport) = tokens.iter().position(|t| *t == "dport") else {
        return Vec::new();
    };
    if dport == 0 || tokens[dport - 1] != "udp" {
        return Vec::new();
    }
    let rest = &tokens[dport + 1..];
    let Some((intervals, used)) = nft_port_spec(rest) else {
        return Vec::new();
    };
    if !rest[used..].contains(&"accept") {
        return Vec::new();
    }
    intervals
        .into_iter()
        .map(|(lo, hi)| UdpAccept {
            lo,
            hi,
            rule: rule.trim().to_string(),
        })
        .collect()
}

/// nft prints a port match as one port, an inclusive `lo-hi` range, or an inline set
/// (`{ 5353, 32768-60999 }`). Returns the intervals and how many tokens they consumed.
fn nft_port_spec(tokens: &[&str]) -> Option<(Vec<(u32, u32)>, usize)> {
    let first = *tokens.first()?;
    if !first.starts_with('{') {
        return parse_port_interval(first, '-').map(|iv| (vec![iv], 1));
    }
    let mut intervals = Vec::new();
    for (n, tok) in tokens.iter().enumerate() {
        let entry = tok
            .trim_start_matches('{')
            .trim_end_matches('}')
            .trim_end_matches(',');
        if let Some(iv) = parse_port_interval(entry, '-') {
            intervals.push(iv);
        }
        if tok.ends_with('}') {
            return Some((intervals, n + 1));
        }
    }
    None
}

/// The firewalld zone listing's reading of the media path.
fn firewalld_media_allow(list_all: &str, media: (u32, u32)) -> MediaAllow {
    media_allow_from_accepts(&firewalld_udp_accepts(list_all), media)
}

/// A zone's UDP allow surface: `ports:`, a blanket `protocols:` entry, the `mdns` service, and
/// the `rich rules:` block — the four ways `deploy/README.md`'s exception can be expressed.
fn firewalld_udp_accepts(list_all: &str) -> Vec<UdpAccept> {
    let mut out = Vec::new();
    for line in list_all.lines() {
        let t = line.trim();
        if let Some(services) = t.strip_prefix("services:") {
            if services.split_whitespace().any(|s| s == "mdns") {
                out.push(UdpAccept {
                    lo: MDNS_PORT,
                    hi: MDNS_PORT,
                    rule: format!("service mdns (udp/{MDNS_PORT})"),
                });
            }
        } else if let Some(ports) = t.strip_prefix("ports:") {
            for tok in ports.split_whitespace() {
                let Some(spec) = tok.strip_suffix("/udp") else {
                    continue;
                };
                if let Some((lo, hi)) = parse_port_interval(spec, '-') {
                    out.push(UdpAccept {
                        lo,
                        hi,
                        rule: format!("port {tok}"),
                    });
                }
            }
        } else if let Some(protocols) = t.strip_prefix("protocols:") {
            if protocols.split_whitespace().any(|p| p == "udp") {
                out.push(UdpAccept {
                    lo: 1,
                    hi: 65535,
                    rule: "protocol udp (every port)".to_string(),
                });
            }
        } else if t.starts_with("rule ") {
            out.extend(firewalld_rich_rule_udp_accept(t));
        }
    }
    out
}

/// One `rich rules:` entry. Only an `accept` action counts, and only a udp port match or the
/// mdns service opens the media path.
fn firewalld_rich_rule_udp_accept(rule: &str) -> Option<UdpAccept> {
    if !rule.split_whitespace().any(|t| t == "accept") {
        return None;
    }
    if rich_rule_attr(rule, "protocol=") == Some("udp") {
        let (lo, hi) = parse_port_interval(rich_rule_attr(rule, "port=")?, '-')?;
        return Some(UdpAccept {
            lo,
            hi,
            rule: rule.to_string(),
        });
    }
    if rich_rule_attr(rule, "name=") == Some("mdns") {
        return Some(UdpAccept {
            lo: MDNS_PORT,
            hi: MDNS_PORT,
            rule: rule.to_string(),
        });
    }
    None
}

/// The value of a `key="value"` attribute in a rich rule.
fn rich_rule_attr<'a>(rule: &'a str, key: &str) -> Option<&'a str> {
    let idx = rule.find(key)?;
    let rest = rule[idx + key.len()..].strip_prefix('"')?;
    let end = rest.find('"')?;
    Some(&rest[..end])
}

/// The iptables INPUT listing's reading of the media path.
fn iptables_media_allow(output: &str, media: (u32, u32)) -> MediaAllow {
    media_allow_from_accepts(&iptables_udp_accepts(output), media)
}

/// `iptables -S INPUT` prints the rules as well as the policy; an
/// `-A INPUT … -p udp … --dport … -j ACCEPT` line is exactly the evidence in question.
fn iptables_udp_accepts(output: &str) -> Vec<UdpAccept> {
    let mut out = Vec::new();
    for line in output.lines() {
        let t = line.trim();
        if !t.starts_with("-A INPUT") || !t.ends_with("-j ACCEPT") {
            continue;
        }
        let tokens: Vec<&str> = t.split_whitespace().collect();
        let udp = tokens
            .windows(2)
            .any(|w| (w[0] == "-p" || w[0] == "--protocol") && w[1] == "udp");
        // `!` inverts the match that follows it, which flips the rule's meaning entirely.
        if !udp || tokens.contains(&"!") {
            continue;
        }
        for (n, tok) in tokens.iter().enumerate() {
            let specs = match *tok {
                "--dport" | "--destination-port" | "--dports" => *tokens.get(n + 1).unwrap_or(&""),
                _ => continue,
            };
            for spec in specs.split(',') {
                if let Some((lo, hi)) = parse_port_interval(spec, ':') {
                    out.push(UdpAccept {
                        lo,
                        hi,
                        rule: t.to_string(),
                    });
                }
            }
        }
    }
    out
}

/// What `nft list ruleset` said. A ruleset with no input hook chain is NOT the same answer as
/// nft never running: both once collapsed to `None` and rendered as `Unknown`.
#[derive(Debug, Clone, PartialEq, Eq)]
enum NftSignal {
    Policy {
        policy: String,
        media_allow: MediaAllow,
    },
    NoInputChain,
}

/// Detail text for [`NftSignal::NoInputChain`]; reaches the operator inside the check summary.
const NFT_NO_INPUT_CHAIN_DETAIL: &str = "nft answered: no input hook chain in the ruleset";

/// The pure decision. Trust order: firewalld's zone target, then nft, then iptables INPUT
/// policy — except that nft reporting no input chain yields to an iptables reading that IS
/// filtering (both front-ends program the same netfilter). Takes already-parsed signals so
/// it needs no exec and no filesystem to test.
fn combine_firewall_signals(
    firewalld: Option<(&str, MediaAllow)>,
    nft: Option<NftSignal>,
    iptables: Option<(&str, MediaAllow)>,
) -> FirewallPosture {
    if let Some((target, media_allow)) = firewalld {
        return if firewalld_target_is_filtering(target) {
            FirewallPosture::Filtering {
                tool: FirewallTool::Firewalld,
                detail: format!("firewalld zone target={target}"),
                media_allow,
            }
        } else {
            FirewallPosture::Open
        };
    }
    let iptables_filtering = matches!(&iptables, Some((policy, _)) if policy_is_filtering(policy));
    match nft {
        Some(NftSignal::Policy {
            policy,
            media_allow,
        }) => {
            return if policy_is_filtering(&policy) {
                FirewallPosture::Filtering {
                    tool: FirewallTool::Nftables,
                    detail: format!("nftables input policy={policy}"),
                    media_allow,
                }
            } else {
                FirewallPosture::Open
            };
        }
        // Both front-ends program the same netfilter, so a filtering iptables INPUT policy is
        // the more specific reading of the same host and keeps its usual turn below.
        Some(NftSignal::NoInputChain) if !iptables_filtering => {
            return FirewallPosture::Unfiltered {
                tool: FirewallTool::Nftables,
                detail: NFT_NO_INPUT_CHAIN_DETAIL.to_string(),
            };
        }
        _ => {}
    }
    if let Some((policy, media_allow)) = iptables {
        return if policy_is_filtering(policy) {
            FirewallPosture::Filtering {
                tool: FirewallTool::Iptables,
                detail: format!("iptables INPUT policy={policy}"),
                media_allow,
            }
        } else {
            FirewallPosture::Open
        };
    }
    FirewallPosture::Unknown
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
                // `Unknown` means "not probed" and must never influence a verdict on its own.
                egl_runtime: crate::nvidia_volume::EglRuntime::Unknown,
                firewall: FirewallPosture::Unknown,
            }
        }

        fn env_vol(&self, volume: VolumeView) -> ProbeEnv {
            ProbeEnv {
                nvidia_volume: volume,
                ..self.env(true, "")
            }
        }

        fn env_firewall(&self, posture: FirewallPosture) -> ProbeEnv {
            ProbeEnv {
                firewall: posture,
                ..self.env(false, "")
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

    /// A healthy NVIDIA host: every check passes, nothing reads as a failure.
    #[test]
    fn healthy_nvidia_host_passes_every_check() {
        let root = FakeRoot::new("healthy");
        root.file("usr/share/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("usr/lib64/libnvidia-eglcore.so.570.86", "")
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("dev/kmsg", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n")
            .file("sys/class/drm/renderD128", "")
            .file(
                &format!("{NVIDIA_VOLUME_REL}/manifest.json"),
                r#"{"driver_version":"610.57.04"}"#,
            )
            // A fully set-up host now includes the app-container AppArmor profile; without
            // these two the check correctly reports `skip` and this loop rejects it.
            .file("sys/module/apparmor/parameters/enabled", "Y\n")
            .file(
                "host/sys/kernel/security/apparmor/profiles",
                "docker-default (enforce)\nquasar-app (enforce)\n",
            )
            .file("etc/os-release", "ID=fedora\nVERSION_ID=42\n");
        // Explicit: the default `Unknown` would `skip`, not `pass`.
        let checks = probe(&ProbeEnv {
            firewall: FirewallPosture::Open,
            ..root.env(true, "/usr/lib")
        });
        for c in &checks {
            assert_eq!(c.status, PASS, "check {} should pass: {:?}", c.id, c);
            assert!(
                c.remediation.is_empty(),
                "a passing check must carry no remediation: {c:?}"
            );
        }
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
        assert_eq!(
            app_apparmor_check(false, NotLoaded, None).status,
            SKIP
        );

        let loaded = app_apparmor_check(true, Loaded, None);
        assert_eq!(loaded.status, PASS);
        assert!(loaded.summary.contains("quasar-app"), "{loaded:?}");

        // Never a failure: without the profile the agent falls back to unconfined and
        // sessions run exactly as they did before #76.
        for state in [NotLoaded, Unknown] {
            let c = app_apparmor_check(true, state, None);
            assert_eq!(c.status, WARN, "{c:?}");
            assert!(
                c.remediation.contains("apparmor_parser -r -W"),
                "the load command is the whole point of the row: {c:?}"
            );
        }
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
                c.remediation.contains("dnf install"),
                "{id} must still carry the manual fallback: {c:?}"
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
                matches!(c.status.as_str(), PASS | FAIL | SKIP | PROVISIONING | WARN),
                "unknown status: {c:?}"
            );
        }
    }

    // ── GPU host post-boot sanity (#493) ─────────────────────────────────────

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

    #[test]
    fn parse_firewalld_zone_target_reads_the_target_line() {
        let list_all = "FedoraServer (active)\n  target: default\n  icmp-block-inversion: no\n  \
                         interfaces: eth0\n  sources:\n  services: cockpit dhcpv6-client ssh\n";
        assert_eq!(
            parse_firewalld_zone_target(list_all),
            Some("default".to_string())
        );

        let accept_zone = "trusted (active)\n  target: ACCEPT\n  services:\n";
        assert_eq!(
            parse_firewalld_zone_target(accept_zone),
            Some("ACCEPT".to_string())
        );
    }

    #[test]
    fn parse_firewalld_zone_target_absent_line_is_none() {
        assert_eq!(parse_firewalld_zone_target("no target line here\n"), None);
        assert_eq!(parse_firewalld_zone_target(""), None);
    }

    #[test]
    fn firewalld_target_is_filtering_only_rejects_accept() {
        assert!(!firewalld_target_is_filtering("ACCEPT"));
        assert!(!firewalld_target_is_filtering("accept"), "case-insensitive");
        assert!(firewalld_target_is_filtering("default"));
        assert!(firewalld_target_is_filtering("DROP"));
        assert!(firewalld_target_is_filtering("REJECT"));
        assert!(firewalld_target_is_filtering("%%REJECT%%"));
    }

    #[test]
    fn parse_nft_input_policy_finds_the_input_hook_line() {
        let ruleset = "table inet filter {\n\tchain input {\n\t\ttype filter hook input \
                        priority filter; policy drop;\n\t\tct state established,related accept\n\
                        \t}\n\tchain forward {\n\t\ttype filter hook forward priority filter; \
                        policy accept;\n\t}\n}\n";
        assert_eq!(parse_nft_input_policy(ruleset), Some("drop".to_string()));
    }

    #[test]
    fn parse_nft_input_policy_ignores_non_input_hooks() {
        let ruleset = "table inet filter {\n\tchain forward {\n\t\ttype filter hook forward \
                        priority filter; policy drop;\n\t}\n}\n";
        assert_eq!(
            parse_nft_input_policy(ruleset),
            None,
            "a DROP forward policy must not be read as an input policy"
        );
    }

    #[test]
    fn parse_nft_input_policy_accepts_accept_and_missing_ruleset() {
        let accept = "table inet filter {\n\tchain input {\n\t\ttype filter hook input \
                       priority filter; policy accept;\n\t}\n}\n";
        assert_eq!(parse_nft_input_policy(accept), Some("accept".to_string()));
        assert_eq!(parse_nft_input_policy(""), None);
    }

    /// The real firewalld-nftables shape (stock FedoraServer zone, target=default): declares
    /// `policy accept;` yet enforces via a trailing catch-all `reject`. `ct state invalid drop`
    /// is a decoy — conditional, and the sibling `_open` test keeps it without the catch-all.
    #[test]
    fn parse_nft_input_policy_detects_firewalld_nftables_catchall_reject() {
        let ruleset = "table inet firewalld {\n\
             \tchain filter_INPUT {\n\
             \t\ttype filter hook input priority filter + 10; policy accept;\n\
             \t\tct state { established, related } accept\n\
             \t\tct status dnat accept\n\
             \t\tiifname \"lo\" accept\n\
             \t\tct state invalid drop\n\
             \t\tjump filter_INPUT_POLICIES\n\
             \t\treject with icmpx admin-prohibited\n\
             \t}\n\
             }\n";
        assert_eq!(
            parse_nft_input_policy(ruleset),
            Some("reject".to_string()),
            "a declared-accept base chain with a trailing unconditional reject must read as \
             filtering, not Open — this is the firewalld-nftables backend shape"
        );
        assert!(policy_is_filtering("reject"));
    }

    /// Same shape, zone target=ACCEPT: no trailing catch-all. The conditional `ct state invalid
    /// drop` that every zone carries must not be mistaken for one.
    #[test]
    fn parse_nft_input_policy_firewalld_nftables_open_zone_reads_accept() {
        let ruleset = "table inet firewalld {\n\
             \tchain filter_INPUT {\n\
             \t\ttype filter hook input priority filter + 10; policy accept;\n\
             \t\tct state { established, related } accept\n\
             \t\tct status dnat accept\n\
             \t\tiifname \"lo\" accept\n\
             \t\tct state invalid drop\n\
             \t\tjump filter_INPUT_POLICIES\n\
             \t}\n\
             }\n";
        assert_eq!(parse_nft_input_policy(ruleset), Some("accept".to_string()));
    }

    /// A hand-configured chain policy of `drop` wins: the declared signal is checked first.
    #[test]
    fn parse_nft_input_policy_explicit_drop_policy_still_wins() {
        let ruleset = "table inet filter {\n\tchain input {\n\t\ttype filter hook input \
                        priority 0; policy drop;\n\t}\n}\n";
        assert_eq!(parse_nft_input_policy(ruleset), Some("drop".to_string()));
    }

    /// What `nft list ruleset` prints on a host with no host firewall of its own: only Docker's
    /// iptables-nft tables, so a `hook forward` chain with `policy drop` and no input hook.
    const DOCKER_ONLY_RULESET: &str = r#"# Warning: table ip nat is managed by iptables-nft, do not touch!
table ip nat {
	chain DOCKER {
		iifname "docker0" counter packets 0 bytes 0 return
	}

	chain POSTROUTING {
		type nat hook postrouting priority srcnat; policy accept;
		oifname != "docker0" ip saddr 192.0.2.0/24 counter packets 0 bytes 0 masquerade
	}

	chain PREROUTING {
		type nat hook prerouting priority dstnat; policy accept;
		fib daddr type local counter packets 0 bytes 0 jump DOCKER
	}
}
# Warning: table ip filter is managed by iptables-nft, do not touch!
table ip filter {
	chain DOCKER {
	}

	chain DOCKER-ISOLATION-STAGE-1 {
		counter packets 0 bytes 0 jump DOCKER-ISOLATION-STAGE-2
	}

	chain DOCKER-ISOLATION-STAGE-2 {
		counter packets 0 bytes 0 return
	}

	chain DOCKER-USER {
	}

	chain FORWARD {
		type filter hook forward priority filter; policy drop;
		counter packets 0 bytes 0 jump DOCKER-USER
		counter packets 0 bytes 0 jump DOCKER-ISOLATION-STAGE-1
		oifname "docker0" ct state related,established counter packets 0 bytes 0 accept
		iifname "docker0" oifname != "docker0" counter packets 0 bytes 0 accept
	}
}
# Warning: table ip6 nat is managed by iptables-nft, do not touch!
table ip6 nat {
	chain POSTROUTING {
		type nat hook postrouting priority srcnat; policy accept;
	}
}
# Warning: table ip6 filter is managed by iptables-nft, do not touch!
table ip6 filter {
	chain FORWARD {
		type filter hook forward priority filter; policy drop;
	}
}
# Warning: table ip raw is managed by iptables-nft, do not touch!
table ip raw {
	chain PREROUTING {
		type filter hook prerouting priority raw; policy accept;
	}
}
"#;

    /// A ruleset that answered and filters nothing inbound is a positive finding, not the
    /// `None` of a tool that never ran.
    #[test]
    fn parse_nft_input_policy_docker_only_ruleset_has_no_input_chain() {
        // An empty answer is the same finding: nothing hooks input.
        assert!(!nft_has_input_base_chain(""));
        assert!(
            !nft_has_input_base_chain(DOCKER_ONLY_RULESET),
            "Docker programs nat/filter/raw with no input hook — nothing filters inbound"
        );
        assert_eq!(
            parse_nft_input_policy(DOCKER_ONLY_RULESET),
            None,
            "a DROP forward policy must not be read as an input policy"
        );
    }

    #[test]
    fn parse_iptables_input_policy_reads_the_dash_p_line() {
        let out = "-P INPUT DROP\n-P FORWARD ACCEPT\n-P OUTPUT ACCEPT\n-A INPUT -i lo -j ACCEPT\n";
        assert_eq!(parse_iptables_input_policy(out), Some("drop".to_string()));
        let out_accept = "-P INPUT ACCEPT\n-P FORWARD ACCEPT\n-P OUTPUT ACCEPT\n";
        assert_eq!(
            parse_iptables_input_policy(out_accept),
            Some("accept".to_string())
        );
        assert_eq!(parse_iptables_input_policy(""), None);
    }

    #[test]
    fn policy_is_filtering_only_rejects_accept() {
        assert!(!policy_is_filtering("accept"));
        assert!(policy_is_filtering("drop"));
        assert!(policy_is_filtering("reject"));
    }

    /// An nft signal carrying a policy word, with no accept rules read.
    fn nft_policy(policy: &str) -> NftSignal {
        NftSignal::Policy {
            policy: policy.to_string(),
            media_allow: MediaAllow::Absent,
        }
    }

    /// Trust order: firewalld beats nft beats iptables beats no signal.
    #[test]
    fn combine_firewall_signals_trust_order_and_verdicts() {
        // No signal anywhere -> Unknown, never treated as a finding.
        assert_eq!(
            combine_firewall_signals(None, None, None),
            FirewallPosture::Unknown
        );

        // firewalld alone, filtering.
        assert_eq!(
            combine_firewall_signals(Some(("default", MediaAllow::Absent)), None, None),
            FirewallPosture::Filtering {
                tool: FirewallTool::Firewalld,
                detail: "firewalld zone target=default".to_string(),
                media_allow: MediaAllow::Absent,
            }
        );
        // firewalld alone, open.
        assert_eq!(
            combine_firewall_signals(Some(("ACCEPT", MediaAllow::Absent)), None, None),
            FirewallPosture::Open
        );

        // nft fallback used only when firewalld has no answer.
        assert_eq!(
            combine_firewall_signals(None, Some(nft_policy("drop")), None),
            FirewallPosture::Filtering {
                tool: FirewallTool::Nftables,
                detail: "nftables input policy=drop".to_string(),
                media_allow: MediaAllow::Absent,
            }
        );
        assert_eq!(
            combine_firewall_signals(None, Some(nft_policy("accept")), None),
            FirewallPosture::Open
        );

        // iptables fallback used only when neither of the above answered.
        assert_eq!(
            combine_firewall_signals(None, None, Some(("drop", MediaAllow::Absent))),
            FirewallPosture::Filtering {
                tool: FirewallTool::Iptables,
                detail: "iptables INPUT policy=drop".to_string(),
                media_allow: MediaAllow::Absent,
            }
        );
        assert_eq!(
            combine_firewall_signals(None, None, Some(("accept", MediaAllow::Absent))),
            FirewallPosture::Open
        );

        // firewalld wins over a conflicting nft/iptables signal.
        assert_eq!(
            combine_firewall_signals(
                Some(("ACCEPT", MediaAllow::Absent)),
                Some(nft_policy("drop")),
                Some(("drop", MediaAllow::Absent)),
            ),
            FirewallPosture::Open
        );
        // nft wins over iptables when firewalld is silent.
        assert_eq!(
            combine_firewall_signals(
                None,
                Some(nft_policy("accept")),
                Some(("drop", MediaAllow::Absent)),
            ),
            FirewallPosture::Open
        );
    }

    /// "nft answered, this host has no input-filtering chain" is a verdict of its own; only a
    /// tool that answered with a FILTERING policy outranks it.
    #[test]
    fn combine_firewall_signals_nft_no_input_chain_is_unfiltered() {
        let unfiltered = FirewallPosture::Unfiltered {
            tool: FirewallTool::Nftables,
            detail: NFT_NO_INPUT_CHAIN_DETAIL.to_string(),
        };
        assert_eq!(
            combine_firewall_signals(None, Some(NftSignal::NoInputChain), None),
            unfiltered
        );

        assert_eq!(
            combine_firewall_signals(
                Some(("default", MediaAllow::Absent)),
                Some(NftSignal::NoInputChain),
                None,
            ),
            FirewallPosture::Filtering {
                tool: FirewallTool::Firewalld,
                detail: "firewalld zone target=default".to_string(),
                media_allow: MediaAllow::Absent,
            },
            "firewalld's zone target still wins the trust order"
        );

        assert_eq!(
            combine_firewall_signals(
                None,
                Some(NftSignal::NoInputChain),
                Some(("drop", MediaAllow::Absent)),
            ),
            FirewallPosture::Filtering {
                tool: FirewallTool::Iptables,
                detail: "iptables INPUT policy=drop".to_string(),
                media_allow: MediaAllow::Absent,
            },
            "both front-ends program the same netfilter: a filtering INPUT policy is the more \
             specific reading"
        );

        assert_eq!(
            combine_firewall_signals(
                None,
                Some(NftSignal::NoInputChain),
                Some(("accept", MediaAllow::Absent)),
            ),
            unfiltered
        );
    }

    /// A missing or failing detection binary must never crash the probe, and never surface as
    /// anything but `Unknown`.
    #[test]
    fn run_with_timeout_absent_binary_is_none_not_a_panic() {
        let out = run_with_timeout("quasar-definitely-not-a-real-binary-xyz123", &["--state"]);
        assert_eq!(out, None);
    }

    #[test]
    fn run_with_timeout_nonzero_exit_is_none() {
        // `false` exits 1 on every POSIX system this agent ships on.
        let out = run_with_timeout("false", &[]);
        assert_eq!(out, None);
    }

    #[test]
    fn run_with_timeout_captures_stdout_on_success() {
        let out = run_with_timeout("echo", &["hello"]);
        assert_eq!(out.as_deref(), Some("hello\n"));
    }

    #[test]
    fn check_media_reachability_unknown_is_skip_never_a_warning() {
        let root = FakeRoot::new("firewall-unknown");
        let env = root.env_firewall(FirewallPosture::Unknown);
        let c = check_media_reachability(&env, Distro::Fedora);
        assert_eq!(c.status, SKIP);
        assert_eq!(
            c.summary,
            "could not determine whether a host firewall would block WebRTC media — no \
             firewalld or nft client tool answered from inside the agent container (nft is in \
             the image and needs CAP_NET_ADMIN, which the compose file grants)"
        );
        assert!(c.remediation.is_empty());
    }

    /// A host whose ruleset filters nothing inbound is the best possible answer for the media
    /// path — a pass, never the "not applicable" bucket a `skip` lands in.
    #[test]
    fn check_media_reachability_unfiltered_is_pass_with_the_positive_summary() {
        let root = FakeRoot::new("firewall-unfiltered");
        let env = root.env_firewall(FirewallPosture::Unfiltered {
            tool: FirewallTool::Nftables,
            detail: NFT_NO_INPUT_CHAIN_DETAIL.to_string(),
        });
        let c = check_media_reachability(&env, Distro::Debian);
        assert_eq!(c.status, PASS);
        assert_eq!(
            c.summary,
            "no host firewall filters inbound traffic (nft answered: no input hook chain in the \
             ruleset) — nothing here would block WebRTC media"
        );
        assert!(c.remediation.is_empty());
    }

    #[test]
    fn check_media_reachability_open_is_pass() {
        let root = FakeRoot::new("firewall-open");
        let env = root.env_firewall(FirewallPosture::Open);
        let c = check_media_reachability(&env, Distro::Fedora);
        assert_eq!(c.status, PASS);
        assert!(c.remediation.is_empty());
    }

    /// A filtering firewall is `warn` with the symptom and a real command, NEVER `fail` — a
    /// correct allow rule is fine and this check cannot tell.
    #[test]
    fn check_media_reachability_filtering_is_warn_with_remediation() {
        let root = FakeRoot::new("firewall-filtering");
        let env = root.env_firewall(FirewallPosture::Filtering {
            tool: FirewallTool::Firewalld,
            detail: "firewalld zone target=default".to_string(),
            media_allow: MediaAllow::Absent,
        });
        let c = check_media_reachability(&env, Distro::Fedora);
        assert_eq!(c.status, WARN);
        assert_ne!(
            c.status, FAIL,
            "a filtering firewall must never hard-fail (#483)"
        );
        assert!(c.summary.contains("WebRTC transport never established"));
        assert!(c.remediation.contains("firewall-cmd"));
        assert!(
            c.remediation.contains("mdns"),
            "mDNS must be part of the fix too"
        );
    }

    #[test]
    fn check_media_reachability_remediation_uses_probed_port_range() {
        let root = FakeRoot::new("firewall-portrange");
        root.file("proc/sys/net/ipv4/ip_local_port_range", "40000\t45000\n");
        let env = ProbeEnv {
            firewall: FirewallPosture::Filtering {
                tool: FirewallTool::Nftables,
                detail: "nftables input policy=drop".to_string(),
                media_allow: MediaAllow::Absent,
            },
            ..root.env(false, "")
        };
        let c = check_media_reachability(&env, Distro::Debian);
        assert_eq!(c.status, WARN);
        assert!(
            c.remediation.contains("40000-45000"),
            "remediation must use the host's real port range, not the hardcoded default: {}",
            c.remediation
        );
    }

    #[test]
    fn check_media_reachability_remediation_falls_back_when_port_range_unreadable() {
        let root = FakeRoot::new("firewall-portrange-missing");
        let env = ProbeEnv {
            firewall: FirewallPosture::Filtering {
                tool: FirewallTool::Iptables,
                detail: "iptables INPUT policy=drop".to_string(),
                media_allow: MediaAllow::Absent,
            },
            ..root.env(false, "")
        };
        let c = check_media_reachability(&env, Distro::Unknown);
        assert!(
            c.remediation.contains("32768-60999"),
            "must fall back to the documented kernel default: {}",
            c.remediation
        );
    }

    /// Remediation keys on the TOOL, not the distro: firewalld on Debian still gets the
    /// firewall-cmd/rich-rule command.
    #[test]
    fn check_media_reachability_firewalld_detected_on_debian_gets_rich_rule_text() {
        let root = FakeRoot::new("firewall-firewalld-on-debian");
        let env = root.env_firewall(FirewallPosture::Filtering {
            tool: FirewallTool::Firewalld,
            detail: "firewalld zone target=default".to_string(),
            media_allow: MediaAllow::Absent,
        });
        let c = check_media_reachability(&env, Distro::Debian);
        assert_eq!(c.status, WARN);
        assert!(
            c.remediation.contains("firewall-cmd") && c.remediation.contains("rich-rule"),
            "firewalld was detected, so the command block must be firewall-cmd/rich-rule \
             regardless of distro: {}",
            c.remediation
        );
        assert!(
            !c.remediation.contains("ufw"),
            "must not hand a Debian host the old distro-keyed ufw text when firewalld actually \
             answered: {}",
            c.remediation
        );
    }

    /// The inverse: nftables on Fedora gets the raw-nftables command, not the
    /// FedoraServer-zone prose, which is gated on firewalld having answered.
    #[test]
    fn check_media_reachability_nftables_detected_on_fedora_gets_nft_text_not_zone_prose() {
        let root = FakeRoot::new("firewall-nftables-on-fedora");
        let env = root.env_firewall(FirewallPosture::Filtering {
            tool: FirewallTool::Nftables,
            detail: "nftables input policy=drop".to_string(),
            media_allow: MediaAllow::Absent,
        });
        let c = check_media_reachability(&env, Distro::Fedora);
        assert_eq!(c.status, WARN);
        assert!(
            c.remediation.contains("nft list ruleset")
                && c.remediation.contains("base input chain"),
            "nftables was detected, so the command block must be the raw-nftables text: {}",
            c.remediation
        );
        assert!(
            !c.remediation.contains("firewall-cmd") && !c.remediation.contains("FedoraServer"),
            "must not hand a raw-nftables host the firewalld/FedoraServer-zone prose just \
             because the distro is Fedora — that sentence is gated on firewalld actually \
             answering: {}",
            c.remediation
        );
    }

    // ── (#67) media reachability: the accept rules, not just the chain policy ───
    //
    // A filtering posture WITH the documented scoped accepts in place is the correct
    // configuration; warning about it forever is how an operator learns to ignore the one
    // check that catches "video never arrives". The nft/firewalld fixtures below are the real
    // (2026-09-01) devbox output — Fedora, firewalld on the nftables backend.

    /// The media window these fixtures are judged against (the devbox's own
    /// `ip_local_port_range`, and the Linux default).
    const MEDIA: (u32, u32) = (32768, 60999);

    /// firewalld's nftables backend with `deploy/README.md`'s rules applied: the mdns service
    /// plus the LAN-scoped media rich rule, both rendered into the zone's allow chain. The
    /// base input chain still ends in the catch-all reject, so the posture stays `Filtering`.
    const NFT_FIREWALLD_MEDIA_ALLOWED: &str = concat!(
        "table inet firewalld {\n",
        "\tchain filter_INPUT {\n",
        "\t\ttype filter hook input priority filter + 10; policy accept;\n",
        "\t\tct state { established, related } accept\n",
        "\t\tct status dnat accept\n",
        "\t\tiifname \"lo\" accept\n",
        "\t\tct state invalid drop\n",
        "\t\tjump filter_INPUT_POLICIES\n",
        "\t\treject with icmpx admin-prohibited\n",
        "\t}\n",
        "\tchain filter_IN_FedoraServer_allow {\n",
        "\t\ttcp dport 22 accept\n",
        "\t\tip6 daddr fe80::/64 udp dport 546 accept\n",
        "\t\ttcp dport 9090 accept\n",
        "\t\tip daddr 224.0.0.251 udp dport 5353 accept\n",
        "\t\tip6 daddr ff02::fb udp dport 5353 accept\n",
        "\t\tip saddr 192.0.2.0/24 udp dport 32768-60999 accept\n",
        "\t}\n",
        "}\n",
    );

    /// The same host before the media rules: stock FedoraServer zone, ssh/cockpit/dhcpv6 only.
    /// This is the true positive the check exists to catch.
    const NFT_FIREWALLD_NO_MEDIA: &str = concat!(
        "table inet firewalld {\n",
        "\tchain filter_INPUT {\n",
        "\t\ttype filter hook input priority filter + 10; policy accept;\n",
        "\t\tct state { established, related } accept\n",
        "\t\tjump filter_INPUT_POLICIES\n",
        "\t\treject with icmpx admin-prohibited\n",
        "\t}\n",
        "\tchain filter_IN_FedoraServer_allow {\n",
        "\t\ttcp dport 22 accept\n",
        "\t\tip6 daddr fe80::/64 udp dport 546 accept\n",
        "\t\ttcp dport 9090 accept\n",
        "\t}\n",
        "}\n",
    );

    /// The devbox's `firewall-cmd --zone=FedoraServer --list-all`, verbatim in shape.
    const FIREWALLD_LIST_ALL_MEDIA_ALLOWED: &str = concat!(
        "FedoraServer (default, active)\n",
        "  target: default\n",
        "  interfaces: enp1s0\n",
        "  sources:\n",
        "  services: cockpit dhcpv6-client mdns ssh\n",
        "  ports:\n",
        "  protocols:\n",
        "  rich rules:\n",
        "\trule family=\"ipv4\" source address=\"192.0.2.0/24\" port port=\"32768-60999\" \
         protocol=\"udp\" accept\n",
    );

    /// #67: the exact gpu-test host configuration — filtering posture, documented rules
    /// present —
    /// must read as covered, not as a finding.
    #[test]
    fn nft_media_allow_sees_the_documented_scoped_accepts() {
        match nft_media_allow(NFT_FIREWALLD_MEDIA_ALLOWED, MEDIA) {
            MediaAllow::Covered { evidence } => {
                assert!(evidence.contains("32768-60999"), "{evidence}");
                assert!(evidence.contains("5353"), "{evidence}");
                assert!(
                    evidence.contains("192.0.2.0/24"),
                    "the rule's source scope must survive into the evidence — a rule scoped to \
                     the wrong subnet is the one thing this cannot judge, so the operator has \
                     to be able to read it: {evidence}"
                );
            }
            other => panic!("the documented rules must read as covered, got {other:?}"),
        }
    }

    #[test]
    fn nft_media_allow_stock_zone_with_no_media_rules_is_absent() {
        assert_eq!(
            nft_media_allow(NFT_FIREWALLD_NO_MEDIA, MEDIA),
            MediaAllow::Absent
        );
        assert_eq!(nft_media_allow("", MEDIA), MediaAllow::Absent);
    }

    /// Half a configuration is its own finding: mDNS open, media range still closed.
    #[test]
    fn nft_media_allow_mdns_only_is_partial_naming_the_media_range_as_the_gap() {
        let ruleset = concat!(
            "table inet firewalld {\n",
            "\tchain filter_IN_FedoraServer_allow {\n",
            "\t\tip daddr 224.0.0.251 udp dport 5353 accept\n",
            "\t}\n",
            "}\n",
        );
        match nft_media_allow(ruleset, MEDIA) {
            MediaAllow::Partial { gap, evidence } => {
                assert!(
                    gap.contains("32768-60999"),
                    "the gap must name what is missing: {gap}"
                );
                assert!(evidence.contains("5353"), "{evidence}");
            }
            other => panic!("mDNS-only must be partial, got {other:?}"),
        }
    }

    /// The mirror case: media range open, mDNS closed.
    #[test]
    fn nft_media_allow_media_range_without_mdns_is_partial() {
        let ruleset = concat!(
            "table inet filter {\n",
            "\tchain input {\n",
            "\t\ttype filter hook input priority 0; policy drop;\n",
            "\t\tudp dport 32768-60999 accept\n",
            "\t}\n",
            "}\n",
        );
        match nft_media_allow(ruleset, MEDIA) {
            MediaAllow::Partial { gap, .. } => {
                assert!(gap.contains("5353"), "the gap must name mDNS: {gap}");
            }
            other => panic!("no-mDNS must be partial, got {other:?}"),
        }
    }

    /// An accept in an output/forward base chain says nothing about inbound media.
    #[test]
    fn nft_media_allow_ignores_accepts_in_non_input_base_chains() {
        let ruleset = concat!(
            "table inet filter {\n",
            "\tchain input {\n",
            "\t\ttype filter hook input priority 0; policy drop;\n",
            "\t}\n",
            "\tchain output {\n",
            "\t\ttype filter hook output priority 0; policy accept;\n",
            "\t\tudp dport 32768-60999 accept\n",
            "\t\tudp dport 5353 accept\n",
            "\t}\n",
            "\tchain forward {\n",
            "\t\ttype filter hook forward priority 0; policy drop;\n",
            "\t\tudp dport 32768-60999 accept\n",
            "\t}\n",
            "}\n",
        );
        assert_eq!(
            nft_media_allow(ruleset, MEDIA),
            MediaAllow::Absent,
            "outbound and forwarded traffic are a different question"
        );
    }

    /// Coverage is judged on the UNION: two adjacent rules cover the range one alone does not.
    #[test]
    fn nft_media_allow_unions_adjacent_ranges() {
        let ruleset = concat!(
            "table inet filter {\n",
            "\tchain input {\n",
            "\t\ttype filter hook input priority 0; policy drop;\n",
            "\t\tudp dport 32768-45000 accept\n",
            "\t\tudp dport 45001-60999 accept\n",
            "\t\tudp dport 5353 accept\n",
            "\t}\n",
            "}\n",
        );
        assert!(matches!(
            nft_media_allow(ruleset, MEDIA),
            MediaAllow::Covered { .. }
        ));
    }

    /// A rule opening only part of the window leaves most sessions broken — partial, not covered.
    #[test]
    fn nft_media_allow_subrange_of_the_media_window_is_partial() {
        let ruleset = concat!(
            "table inet filter {\n",
            "\tchain input {\n",
            "\t\ttype filter hook input priority 0; policy drop;\n",
            "\t\tudp dport 40000-41000 accept\n",
            "\t\tudp dport 5353 accept\n",
            "\t}\n",
            "}\n",
        );
        match nft_media_allow(ruleset, MEDIA) {
            MediaAllow::Partial { gap, .. } => assert!(gap.contains("32768-60999"), "{gap}"),
            other => panic!("a subrange must not read as covered, got {other:?}"),
        }
    }

    /// nft prints multi-port rules as an inline set.
    #[test]
    fn nft_media_allow_reads_inline_port_sets() {
        let ruleset = concat!(
            "table inet filter {\n",
            "\tchain input {\n",
            "\t\ttype filter hook input priority 0; policy drop;\n",
            "\t\tudp dport { 5353, 32768-60999 } accept\n",
            "\t}\n",
            "}\n",
        );
        assert!(matches!(
            nft_media_allow(ruleset, MEDIA),
            MediaAllow::Covered { .. }
        ));
    }

    /// A negated or non-accept rule is not an allow.
    #[test]
    fn nft_media_allow_ignores_negations_and_non_accept_verdicts() {
        let ruleset = concat!(
            "table inet filter {\n",
            "\tchain input {\n",
            "\t\ttype filter hook input priority 0; policy drop;\n",
            "\t\tudp dport != 32768-60999 accept\n",
            "\t\tudp dport 5353 drop\n",
            "\t\tudp dport 32768-60999 jump some_chain\n",
            "\t}\n",
            "}\n",
        );
        assert_eq!(nft_media_allow(ruleset, MEDIA), MediaAllow::Absent);
    }

    /// #67 as the operator sees it through firewalld's own CLI, when that client IS present.
    #[test]
    fn firewalld_media_allow_reads_the_rich_rule_and_the_mdns_service() {
        match firewalld_media_allow(FIREWALLD_LIST_ALL_MEDIA_ALLOWED, MEDIA) {
            MediaAllow::Covered { evidence } => {
                assert!(evidence.contains("32768-60999"), "{evidence}");
                assert!(evidence.contains("mdns"), "{evidence}");
            }
            other => panic!("the documented zone must read as covered, got {other:?}"),
        }
    }

    #[test]
    fn firewalld_media_allow_stock_zone_is_absent() {
        let list_all = concat!(
            "FedoraServer (default, active)\n",
            "  target: default\n",
            "  services: cockpit dhcpv6-client ssh\n",
            "  ports:\n",
            "  rich rules:\n",
        );
        assert_eq!(firewalld_media_allow(list_all, MEDIA), MediaAllow::Absent);
    }

    /// The plainer configuration: `--add-port` rather than a rich rule.
    #[test]
    fn firewalld_media_allow_reads_the_ports_line() {
        let list_all = concat!(
            "public (active)\n",
            "  target: default\n",
            "  services: ssh\n",
            "  ports: 32768-60999/udp 5353/udp 8443/tcp\n",
            "  rich rules:\n",
        );
        assert!(matches!(
            firewalld_media_allow(list_all, MEDIA),
            MediaAllow::Covered { .. }
        ));
    }

    #[test]
    fn iptables_media_allow_reads_accept_rules_not_just_the_policy() {
        let out = concat!(
            "-P INPUT DROP\n",
            "-A INPUT -i lo -j ACCEPT\n",
            "-A INPUT -s 192.0.2.0/24 -p udp -m udp --dport 32768:60999 -j ACCEPT\n",
            "-A INPUT -p udp -m udp --dport 5353 -j ACCEPT\n",
        );
        match iptables_media_allow(out, MEDIA) {
            MediaAllow::Covered { evidence } => {
                assert!(evidence.contains("32768:60999"), "{evidence}")
            }
            other => panic!("iptables accept rules must count, got {other:?}"),
        }
        let policy_only = "-P INPUT DROP\n-A INPUT -i lo -j ACCEPT\n";
        assert_eq!(iptables_media_allow(policy_only, MEDIA), MediaAllow::Absent);
    }

    /// #67, the whole point: a correctly-configured firewalld host must stop warning.
    #[test]
    fn check_media_reachability_covered_rules_is_pass_not_a_permanent_warning() {
        let root = FakeRoot::new("firewall-covered");
        let env = root.env_firewall(FirewallPosture::Filtering {
            tool: FirewallTool::Nftables,
            detail: "nftables input policy=reject".to_string(),
            media_allow: MediaAllow::Covered {
                evidence: "ip saddr 192.0.2.0/24 udp dport 32768-60999 accept".to_string(),
            },
        });
        let c = check_media_reachability(&env, Distro::Fedora);
        assert_eq!(
            c.status, PASS,
            "the documented rules are active and verified working — warning anyway is #67"
        );
        assert!(
            c.summary.contains("32768-60999"),
            "the matched rule is the evidence for the pass: {}",
            c.summary
        );
        assert!(c.remediation.is_empty(), "nothing to remediate");
    }

    /// A half-open configuration keeps warning, and says which half is missing.
    #[test]
    fn check_media_reachability_partial_rules_warns_and_names_the_gap() {
        let root = FakeRoot::new("firewall-partial");
        let env = root.env_firewall(FirewallPosture::Filtering {
            tool: FirewallTool::Firewalld,
            detail: "firewalld zone target=default".to_string(),
            media_allow: MediaAllow::Partial {
                evidence: "service mdns (udp/5353)".to_string(),
                gap: "UDP 32768-60999".to_string(),
            },
        });
        let c = check_media_reachability(&env, Distro::Fedora);
        assert_eq!(c.status, WARN);
        assert!(c.summary.contains("32768-60999"), "{}", c.summary);
        assert!(
            c.remediation.contains("firewall-cmd"),
            "a partial configuration still needs the command"
        );
    }

    /// Detection carries each tool's own rule reading through to the posture it produced —
    /// never the losing tool's.
    #[test]
    fn combine_firewall_signals_carries_the_winning_tools_media_allow() {
        let covered = MediaAllow::Covered {
            evidence: "udp dport 32768-60999 accept".to_string(),
        };
        assert_eq!(
            combine_firewall_signals(
                Some(("default", covered.clone())),
                Some(nft_policy("reject")),
                None,
            ),
            FirewallPosture::Filtering {
                tool: FirewallTool::Firewalld,
                detail: "firewalld zone target=default".to_string(),
                media_allow: covered,
            },
            "firewalld won the posture, so its rule reading is the one that counts"
        );
    }

    /// Wired into `probe`, and a WARN must never be counted as a FAIL.
    #[test]
    fn media_reachability_is_wired_into_probe() {
        let root = FakeRoot::new("firewall-in-probe");
        root.file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n");
        let env = ProbeEnv {
            firewall: FirewallPosture::Filtering {
                tool: FirewallTool::Firewalld,
                detail: "firewalld zone target=default".to_string(),
                media_allow: MediaAllow::Absent,
            },
            ..root.env(false, "")
        };
        let checks = probe(&env);
        let c = get(&checks, "media_reachability");
        assert_eq!(c.status, WARN);
        assert_eq!(
            log_report(&checks),
            0,
            "a WARN-only host must not be reported as having FAILED checks: {checks:?}"
        );
    }

    /// The remediation has to name the entry the shipped compose actually uses. `devices:`
    /// takes a device-cgroup permission set (`r`/`w`/`m`), not a bind mount's `:ro` — Compose
    /// rejects `:ro` there outright, so the old text sent operators to an error (#83). And
    /// `dmesg_restrict=1` is the distro default, so the device without the capability is EPERM.
    #[test]
    fn xid_visibility_remediation_matches_the_shipped_compose_entry() {
        let root = FakeRoot::new("xid-remediation");
        let c = check_xid_visibility(&root.env(true, ""));
        assert_eq!(
            c.status, SKIP,
            "no dev/kmsg fixture, so this is the skip arm"
        );
        assert!(
            c.remediation.contains("/dev/kmsg:/dev/kmsg:r"),
            "remediation must name the device entry verbatim: {c:?}"
        );
        assert!(
            !c.remediation.contains("/dev/kmsg:/dev/kmsg:ro"),
            "must never hand out the `:ro` form — Compose rejects it under `devices:`: {c:?}"
        );
        assert!(
            c.remediation.contains("SYSLOG"),
            "the device alone is EPERM under dmesg_restrict=1: {c:?}"
        );
    }

    /// Every operator-facing string is one normalised paragraph. A `format!` literal that lost
    /// its `\` line continuations carries the source's own indentation into the summary the
    /// agent reports (#82) — HTML collapses the run so the admin UI looks fine, while the JSON,
    /// the log line and the copy-to-clipboard text all keep the gap.
    #[test]
    fn no_check_text_carries_a_run_of_spaces() {
        let visible = FakeRoot::new("text-visible");
        visible
            .file("usr/share/glvnd/egl_vendor.d/10_nvidia.json", "{}")
            .file("usr/lib64/libnvidia-eglcore.so.570.86", "")
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            // Present: the `pass` arm of xid_visibility.
            .file("dev/kmsg", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n")
            .file("sys/class/drm/renderD128", "")
            .file("etc/os-release", "ID=fedora\n");
        // Absent `dev/kmsg`: the `skip` arm, which is the one that carries a remediation.
        let hidden = FakeRoot::new("text-hidden");
        hidden
            .file("dev/dri/renderD128", "")
            .file("dev/uinput", "")
            .file("proc/sys/user/max_user_namespaces", "15000\n")
            .file("etc/os-release", "ID=fedora\n");

        let mut checks = probe(&visible.env(true, "/usr/lib"));
        checks.extend(probe(&ProbeEnv {
            firewall: FirewallPosture::Filtering {
                tool: FirewallTool::Nftables,
                detail: "nftables input policy=drop".to_string(),
                media_allow: MediaAllow::Absent,
            },
            ..hidden.env(true, "")
        }));

        for c in &checks {
            for (field, text) in [("summary", &c.summary), ("remediation", &c.remediation)] {
                assert!(
                    !text.contains("  "),
                    "{} {field} carries a run of spaces (a missing `\\` line continuation): {text:?}",
                    c.id
                );
            }
        }
    }
}
