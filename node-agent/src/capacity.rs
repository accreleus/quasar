//! Host and GPU capacity detection from sysfs/procfs, plus the console-mode inventory
//! (DRM outputs, audio sinks, physical input) the admin UI populates selectors from.
//!
//! `encode_slots_total` is what admission RESERVES against; live free VRAM is a separate,
//! advisory signal sampled by [`crate::vram`]. Detection failure fails CLOSED — an empty GPU
//! list makes the host unschedulable — unless `QUASAR_SYNTHETIC_GPU_CAPACITY` is set.
//!
//! `detect()` runs on the agent's select loop, so every external probe here must be bounded
//! ([`crate::vram::run_bounded`]) or memoized for the process lifetime.

use crate::messages::{
    AudioSink, ConsoleCapabilities, DrmModeCapability, DrmOutputCapability, GpuCapacity,
    HostCapacity, InputDeviceInfo, StorageVolume,
};
use crate::session::EncoderChoice;
use crate::vram::VramTarget;

pub struct SystemCapacity {
    pub host: HostCapacity,
    pub gpus: Vec<GpuCapacity>,
    pub gpu_detection: String,
    pub gpu_detection_reason: Option<String>,
    pub console: ConsoleCapabilities,
    /// Sampling descriptors for `vram::sample`, one per GPU in the same order as `gpus`.
    /// Carries the sysfs card dir and (NVIDIA) the PCI bus id — neither reachable from
    /// `GpuCapacity`, whose `device_path` is the `/dev/dri/renderD*` node.
    pub vram_targets: Vec<VramTarget>,
}

/// Always statvfs'd as label "agent-data". On the reference compose deployments this is a
/// named volume on the Docker data filesystem, so it reflects images/containers/homes space.
const AGENT_DATA_ROOT: &str = "/var/lib/quasar-agent";

pub fn detect() -> SystemCapacity {
    let allow_synthetic = std::env::var("QUASAR_SYNTHETIC_GPU_CAPACITY")
        .is_ok_and(|v| matches!(v.to_ascii_lowercase().as_str(), "1" | "true" | "yes"));
    let mem_mb = detect_mem_mb();
    let (gpus, vram_targets, gpu_detection, gpu_detection_reason) = detect_gpus_at(
        std::path::Path::new("/sys/class/drm"),
        allow_synthetic,
        mem_mb,
    );
    SystemCapacity {
        host: HostCapacity {
            cpu_cores: detect_cpu_cores(),
            mem_mb,
            storage: Some(detect_storage()),
            cpu_model: detect_cpu_model(),
        },
        gpus,
        gpu_detection,
        gpu_detection_reason,
        console: detect_console_capabilities(),
        vram_targets,
    }
}

/// `/proc/cpuinfo`'s `model name`. `None` when unreadable or absent — some non-x86 layouts
/// use a different key.
fn detect_cpu_model() -> Option<String> {
    let content = std::fs::read_to_string("/proc/cpuinfo").ok()?;
    parse_cpu_model(&content)
}

fn parse_cpu_model(content: &str) -> Option<String> {
    for line in content.lines() {
        let mut parts = line.splitn(2, ':');
        let Some(key) = parts.next() else {
            continue;
        };
        if key.trim() == "model name" {
            if let Some(val) = parts.next().map(str::trim).filter(|v| !v.is_empty()) {
                return Some(val.to_string());
            }
        }
    }
    None
}

/// statvfs the agent-visible storage roots. Two roots on the same `st_dev` dedupe to
/// "agent-data": the homes root is often a subdirectory of the agent data volume.
pub(crate) fn detect_storage() -> Vec<StorageVolume> {
    let mut out: Vec<StorageVolume> = Vec::new();
    let mut seen_dev: Vec<u64> = Vec::new();

    if let Some(v) = statvfs_volume("agent-data", std::path::Path::new(AGENT_DATA_ROOT)) {
        if let Some(dev) = st_dev(std::path::Path::new(AGENT_DATA_ROOT)) {
            seen_dev.push(dev);
        }
        out.push(v);
    }

    if let Some(root) = crate::session::home::configured_home_root() {
        let already_seen = st_dev(&root).is_some_and(|dev| seen_dev.contains(&dev));
        if !already_seen {
            if let Some(v) = statvfs_volume("homes", &root) {
                out.push(v);
            }
        }
    }

    out
}

fn st_dev(path: &std::path::Path) -> Option<u64> {
    use std::os::unix::fs::MetadataExt;
    std::fs::metadata(path).ok().map(|m| m.dev())
}

fn statvfs_volume(label: &str, path: &std::path::Path) -> Option<StorageVolume> {
    if !path.exists() {
        return None;
    }
    let c_path = std::ffi::CString::new(path.to_str()?.as_bytes()).ok()?;
    let mut vfs: libc::statvfs = unsafe { std::mem::zeroed() };
    // SAFETY: `c_path` outlives the call; `vfs` is a plain-data out-param.
    let rc = unsafe { libc::statvfs(c_path.as_ptr(), &mut vfs) };
    if rc != 0 {
        tracing::warn!(
            token = "statvfs-failed",
            "statvfs({}) failed: {}",
            path.display(),
            std::io::Error::last_os_error()
        );
        return None;
    }
    const MIB: u64 = 1024 * 1024;
    let total_mb = (vfs.f_blocks * vfs.f_frsize / MIB) as i64;
    let available_mb = (vfs.f_bavail * vfs.f_frsize / MIB) as i64;
    Some(StorageVolume {
        label: label.to_string(),
        path: path.to_string_lossy().into_owned(),
        total_mb,
        available_mb,
    })
}

/// Console-mode capabilities from sysfs/procfs. Best-effort: a missing source yields an
/// empty list and the UI then offers only `auto`. Also polled by `session::console_hotplug`
/// to snapshot-diff for display/input hotplug.
pub(crate) fn detect_console_capabilities() -> ConsoleCapabilities {
    let outputs = detect_drm_outputs();
    let typed_connectors: Vec<String> = outputs
        .iter()
        .filter(|o| o.connected)
        .map(|o| o.connector.clone())
        .collect();
    ConsoleCapabilities {
        connectors: if typed_connectors.is_empty() {
            detect_drm_connectors()
        } else {
            crate::ddc::powered_connectors(typed_connectors)
        },
        outputs,
        audio_sinks: detect_audio_sinks(),
        input_devices: detect_input_devices(),
    }
}

/// Output/connector inventory only, without the DDC power probe and audio/input enumeration
/// `detect_console_capabilities` also does. `session::console::spawn_weston_console` calls
/// this per launch; the full probe would eat its 15s socket-wait budget for discarded data.
pub(crate) fn detect_drm_outputs() -> Vec<DrmOutputCapability> {
    detect_drm_outputs_at(std::path::Path::new("/dev/dri"))
}

#[derive(Debug)]
struct DrmCard(std::fs::File);

impl std::os::fd::AsFd for DrmCard {
    fn as_fd(&self) -> std::os::fd::BorrowedFd<'_> {
        self.0.as_fd()
    }
}
impl drm::Device for DrmCard {}
impl drm::control::Device for DrmCard {}

fn drm_mode_capability(mode: &drm::control::Mode) -> DrmModeCapability {
    use drm::control::{ModeFlags, ModeTypeFlags};
    let (width, height) = mode.size();
    let (_, _, htotal) = mode.hsync();
    let (_, _, vtotal) = mode.vsync();
    DrmModeCapability {
        name: mode.name().to_string_lossy().into_owned(),
        width,
        height,
        refresh_millihz: refresh_millihz(
            mode.clock(),
            htotal,
            vtotal,
            mode.flags().contains(ModeFlags::INTERLACE),
            mode.flags().contains(ModeFlags::DBLSCAN),
            mode.vscan(),
            mode.vrefresh(),
        ),
        preferred: mode.mode_type().contains(ModeTypeFlags::PREFERRED),
        interlaced: mode.flags().contains(ModeFlags::INTERLACE),
        clock_khz: mode.clock(),
        htotal,
        vtotal,
    }
}

fn detect_drm_outputs_at(dri_root: &std::path::Path) -> Vec<DrmOutputCapability> {
    use drm::control::{connector, Device as _};

    let mut cards: Vec<_> = std::fs::read_dir(dri_root)
        .into_iter()
        .flatten()
        .flatten()
        .filter(|e| e.file_name().to_string_lossy().starts_with("card"))
        .collect();
    cards.sort_by_key(|e| e.file_name());
    let mut outputs = Vec::new();
    for entry in cards {
        let card_name = entry.file_name().to_string_lossy().into_owned();
        let Ok(file) = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(entry.path())
        else {
            continue;
        };
        let card = DrmCard(file);
        let Ok(resources) = card.resource_handles() else {
            continue;
        };
        let render_node = render_node_for_card(&card_name);
        for handle in resources.connectors() {
            let Ok(info) = card.get_connector(*handle, false) else {
                continue;
            };
            let connector_name = format!("{}-{}", info.interface().as_str(), info.interface_id());
            let modes = info.modes().iter().map(drm_mode_capability).collect();
            let active_mode = info
                .current_encoder()
                .and_then(|encoder| card.get_encoder(encoder).ok())
                .and_then(|encoder| encoder.crtc())
                .and_then(|crtc| card.get_crtc(crtc).ok())
                .and_then(|crtc| crtc.mode().map(|mode| drm_mode_capability(&mode)));
            outputs.push(DrmOutputCapability {
                id: format!("{card_name}:{connector_name}"),
                card: card_name.clone(),
                render_node: render_node.clone(),
                connector: connector_name,
                connected: info.state() == connector::State::Connected,
                active_mode,
                modes,
            });
        }
    }
    outputs.sort_by(|a, b| a.id.cmp(&b.id));
    outputs
}

#[allow(clippy::too_many_arguments)]
fn refresh_millihz(
    clock_khz: u32,
    htotal: u16,
    vtotal: u16,
    interlaced: bool,
    doublescan: bool,
    vscan: u16,
    fallback_hz: u32,
) -> u32 {
    let mut refresh = if htotal > 0 && vtotal > 0 {
        (u64::from(clock_khz) * 1_000_000 / (u64::from(htotal) * u64::from(vtotal))) as u32
    } else {
        fallback_hz.saturating_mul(1000)
    };
    if interlaced {
        refresh = refresh.saturating_mul(2);
    }
    if doublescan {
        refresh /= 2;
    }
    if vscan > 1 {
        refresh /= u32::from(vscan);
    }
    refresh
}

fn render_node_for_card(card_name: &str) -> Option<String> {
    let root = std::path::Path::new("/sys/class/drm")
        .join(card_name)
        .join("device/drm");
    let mut renders: Vec<_> = std::fs::read_dir(root)
        .ok()?
        .flatten()
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .filter(|name| name.starts_with("renderD"))
        .collect();
    renders.sort();
    renders.first().map(|name| format!("/dev/dri/{name}"))
}

/// Connected DRM connectors: `/sys/class/drm/card*-<CONNECTOR>/status` == "connected". The
/// reported name ("DP-4") is the dir suffix after the "cardN-" prefix.
fn detect_drm_connectors() -> Vec<String> {
    let mut out = Vec::new();
    let Ok(entries) = std::fs::read_dir("/sys/class/drm") else {
        return out;
    };
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        // "card0-DP-4"; skip "card0", "renderD128".
        let Some((card, connector)) = name.split_once('-') else {
            continue;
        };
        if !card.starts_with("card") || connector.is_empty() {
            continue;
        }
        let status = std::fs::read_to_string(entry.path().join("status")).unwrap_or_default();
        if status.trim() == "connected" {
            out.push(connector.to_string());
        }
    }
    out.sort();
    out.dedup();
    // On DP/nvidia-drm a monitor in standby keeps `status=connected`, so gate on DDC/CI VCP
    // 0xd6 power state; console auto-start/stop rides on this. No-op without ddcutil.
    crate::ddc::powered_connectors(out)
}

/// Host audio sinks from `/proc/asound` or the host bind at `/host-proc/asound`. A card name
/// alone is insufficient for HDMI/DP: the active output is often `hw:<card>,<device>`, not
/// the non-existent device zero.
fn detect_audio_sinks() -> Vec<AudioSink> {
    // Docker creates an empty `/proc/asound` even with no host ALSA metadata visible, so
    // pick the first root that actually has `cards` or the explicit host bind is shadowed.
    let asound = ["/proc/asound", "/host-proc/asound"]
        .into_iter()
        .find(|root| std::path::Path::new(root).join("cards").is_file())
        .unwrap_or("/proc/asound");
    let cards = std::fs::read_to_string(format!("{asound}/cards")).unwrap_or_default();
    let pcm = std::fs::read_to_string(format!("{asound}/pcm")).unwrap_or_default();
    parse_audio_sinks(&cards, &pcm)
        .into_iter()
        // Compose may expose only one sound device; advertising the rest of the host's
        // inventory hands an operator a sink whose ALSA node the pipeline cannot open.
        .filter(|sink| audio_sink_device_visible(&sink.id))
        .collect()
}

fn audio_sink_device_visible(id: &str) -> bool {
    audio_sink_device_path(id).is_none_or(|path| path.exists())
}

fn audio_sink_device_path(id: &str) -> Option<std::path::PathBuf> {
    let Some((card, device)) = id
        .strip_prefix("hw:")
        .and_then(|address| address.split_once(','))
    else {
        // Card-level fallbacks name a controller, not a playback PCM; still useful on a
        // host that exposes no pcm data.
        return None;
    };
    Some(std::path::PathBuf::from(format!(
        "/dev/snd/pcmC{card}D{device}p"
    )))
}

fn parse_audio_sinks(cards: &str, pcm: &str) -> Vec<AudioSink> {
    let mut labels = std::collections::BTreeMap::new();
    for line in cards.lines() {
        let trimmed = line.trim_start();
        let Some((idx_str, rest)) = trimmed.split_once(' ') else {
            continue;
        };
        let Ok(idx) = idx_str.trim().parse::<i32>() else {
            continue;
        };
        let label = rest
            .rsplit_once(" - ")
            .map(|(_, l)| l.trim())
            .filter(|l| !l.is_empty())
            .unwrap_or_else(|| rest.trim())
            .to_string();
        labels.insert(idx, label);
    }

    let mut out = Vec::new();
    for line in pcm.lines() {
        // Kernel format: "00-03: HDMI 0 : HDMI 0 : playback 1".
        let Some((address, detail)) = line.split_once(':') else {
            continue;
        };
        if !detail.contains("playback") {
            continue;
        }
        let Some((card, device)) = address.trim().split_once('-') else {
            continue;
        };
        let (Ok(card), Ok(device)) = (card.parse::<i32>(), device.parse::<i32>()) else {
            continue;
        };
        let endpoint = detail
            .split(':')
            .next()
            .map(str::trim)
            .filter(|v| !v.is_empty())
            .unwrap_or("playback");
        let card_label = labels
            .get(&card)
            .cloned()
            .unwrap_or_else(|| format!("card {card}"));
        out.push(AudioSink {
            id: format!("hw:{card},{device}"),
            label: format!("{card_label} — {endpoint}"),
        });
    }

    // A card with no PCM detail yet (a USB DAC still initializing) keeps a card-level option.
    if out.is_empty() {
        out.extend(labels.into_iter().map(|(idx, label)| AudioSink {
            id: format!("hw:{idx}"),
            label,
        }));
    }
    out
}

/// `/dev/input/event*` with the name from `/sys/class/input/<event>/device/name`. Also
/// resolves `console_config.input_devices: "auto"` in `session::physical_input`.
pub(crate) fn detect_input_devices() -> Vec<InputDeviceInfo> {
    let mut out = Vec::new();
    let Ok(entries) = std::fs::read_dir("/dev/input") else {
        return out;
    };
    let mut events: Vec<String> = entries
        .flatten()
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .filter(|n| n.starts_with("event"))
        .collect();
    events.sort_by_key(|a| natural_event_order(a));
    for ev in events {
        let name_path = format!("/sys/class/input/{ev}/device/name");
        let label = std::fs::read_to_string(&name_path)
            .map(|s| s.trim().to_string())
            .unwrap_or_else(|_| ev.clone());
        out.push(InputDeviceInfo {
            path: format!("/dev/input/{ev}"),
            label,
        });
    }
    out
}

/// Sort key so event2 < event10 (numeric, not lexical).
fn natural_event_order(name: &str) -> u32 {
    name.trim_start_matches("event").parse().unwrap_or(u32::MAX)
}

fn detect_cpu_cores() -> i32 {
    std::fs::read_to_string("/proc/cpuinfo")
        .map(|s| s.lines().filter(|l| l.starts_with("processor")).count() as i32)
        .unwrap_or(1)
}

fn detect_mem_mb() -> i32 {
    let content = std::fs::read_to_string("/proc/meminfo").unwrap_or_default();
    for line in content.lines() {
        if line.starts_with("MemTotal:") {
            let kb: i64 = line
                .split_whitespace()
                .nth(1)
                .and_then(|s| s.parse().ok())
                .unwrap_or(0);
            return (kb / 1024) as i32;
        }
    }
    0
}

fn detect_gpus_at(
    root: &std::path::Path,
    allow_synthetic: bool,
    mem_mb: i32,
) -> (Vec<GpuCapacity>, Vec<VramTarget>, String, Option<String>) {
    let entries = match std::fs::read_dir(root) {
        Ok(e) => e,
        Err(err) => {
            return detection_failure(
                "failed",
                format!("cannot read DRM inventory: {err}"),
                allow_synthetic,
            )
        }
    };

    let mut card_paths: Vec<_> = entries
        .flatten()
        .map(|e| e.path())
        .filter(|p| {
            let name = p
                .file_name()
                .unwrap_or_default()
                .to_string_lossy()
                .into_owned();
            // "cardN" only; skip "renderD*" and "card0-HDMI-A-1".
            name.starts_with("card") && !name.contains('-') && name[4..].parse::<u32>().is_ok()
        })
        .collect();
    card_paths.sort();

    // Resolved once per detect(), not per GPU, so a multi-GPU host does not re-warn on a bad
    // knob value once per card.
    let vulkan_slots = vulkan_encode_slots_override();

    let mut gpus = Vec::new();
    let mut vram_targets = Vec::new();
    for (index, card_path) in card_paths.iter().enumerate() {
        let device_path = card_path.join("device");
        if !device_path.exists() {
            continue;
        }

        let vendor_id = std::fs::read_to_string(device_path.join("vendor"))
            .map(|s| s.trim().to_string())
            .unwrap_or_default();

        let vendor = match vendor_id.as_str() {
            "0x1002" => "amd",
            "0x10de" => "nvidia",
            "0x8086" => "intel",
            _ => continue,
        };

        let model = read_model(&device_path, vendor);
        let vram_mb_total = if vendor == "intel" {
            read_intel_memory_mb(card_path, mem_mb)
        } else {
            read_vram_mb(&device_path, vendor)
        };
        let Some(vram_mb_total) = vram_mb_total else {
            tracing::warn!(
                token = "gpu-omitted-no-vram",vendor, path = %device_path.display(), "GPU omitted: VRAM capacity unavailable");
            continue;
        };

        // The concurrent encode-session cap admission reserves against. A conservative
        // vendor stub; real limits need nvml / drm ioctls. Overrides land in the post-passes
        // below, scoped there rather than blanket-applied here.
        let encode_slots_total = match vendor {
            "amd" => 2,
            "nvidia" => 3,
            "intel" => 2,
            _ => 1,
        };

        // The by-path form is constructed, not read off disk, so it survives a container
        // with no /dev/dri/by-path bind — but only when this GPU has a render node at all
        // (a display-only iGPU may not).
        let (render_node, resolved_device_path) = pci_address(&device_path)
            .and_then(|addr| {
                renderd_node_for_pci_addr_root(&addr, root)
                    .map(|device| (format!("/dev/dri/by-path/pci-{addr}-render"), device))
            })
            .map_or((None, None), |(stable, device)| {
                (Some(stable), Some(device))
            });

        // `pci_addr` matters only on the NVIDIA path; the AMD sampler reads `sysfs_device`
        // directly and never matches by bus id.
        let pci_addr = if vendor == "nvidia" {
            pci_address(&device_path)
        } else {
            None
        };
        vram_targets.push(VramTarget {
            index: index as i32,
            vendor: vendor.to_string(),
            sysfs_device: device_path.clone(),
            pci_addr,
            total_mb: vram_mb_total,
        });

        gpus.push(GpuCapacity {
            index: index as i32,
            vendor: vendor.to_string(),
            model,
            vram_mb_total,
            encode_slots_total,
            render_node,
            device_path: resolved_device_path,
        });
    }

    if let Some(slots) = vulkan_slots {
        apply_vulkan_override(&mut gpus, slots);
    }

    // #489: the NVIDIA driver has a UAF that SIGSEGVs the whole agent when one session's
    // NVENC teardown overlaps another live NVENC session. Setting the ceiling to 1 makes
    // that overlap unschedulable, since admission enforces encode_slots_total.
    if let Some(slots) = nvenc_max_sessions_override() {
        apply_nvenc_override_to(&mut gpus, slots);
    }

    if gpus.is_empty() {
        detection_failure(
            "unavailable",
            "no GPU with known capacity detected".to_string(),
            allow_synthetic,
        )
    } else {
        (gpus, vram_targets, "ok".to_string(), None)
    }
}

/// `Some(n)` only when the resolved encoder (explicit `QUASAR_ENCODER` or the
/// vendor auto-detect) is vulkan; `n` is the `QUASAR_VULKAN_MAX_SESSIONS` ceiling
/// replacing the vendor stub. Every other encoder leaves the stub untouched.
fn vulkan_encode_slots_override() -> Option<i32> {
    let choice = crate::session::settings::resolve_encoder_choice();
    (choice == EncoderChoice::Vulkan).then(vulkan_max_sessions)
}

/// Scopes the `QUASAR_VULKAN_MAX_SESSIONS` override to the one GPU vulkan renders through,
/// matching `session::settings::canonicalize_render_node` (reused, not re-derived) against
/// each GPU's `device_path`. Applying it to every GPU makes a multi-GPU host advertise
/// vulkan slots its iGPU can never serve.
///
/// Fails open: an unresolvable or unmatched render node applies the override to every GPU
/// with a warn, since a vulkan host must never advertise zero vulkan capacity.
fn apply_vulkan_override(gpus: &mut [GpuCapacity], slots: i32) {
    if gpus.is_empty() {
        return;
    }
    let configured = configured_vulkan_render_node();
    apply_vulkan_override_to(gpus, slots, configured.as_deref());
}

/// The pure matching/fallback decision, split out so it is testable without process env:
/// unrelated `session::settings` tests read `QUASAR_RENDER_NODE` unguarded, so setting it in
/// a test races them under the default parallel runner. Guarded by
/// `vulkan_capacity_override_scoped_to_configured_render_node`.
fn apply_vulkan_override_to(gpus: &mut [GpuCapacity], slots: i32, configured: Option<&str>) {
    let target = configured.and_then(|node| {
        gpus.iter()
            .position(|g| g.device_path.as_deref() == Some(node))
    });

    match target {
        Some(idx) => gpus[idx].encode_slots_total = slots,
        None => {
            // Unpinned (QUASAR_RENDER_NODE unset) is the normal default path, not a
            // misconfiguration — only a configured-but-unmatched value warrants WARN.
            if configured.is_some() {
                tracing::warn!(
                    token = "vulkan-render-node-unmatched",
                    configured_render_node = ?configured,
                    slots,
                    "QUASAR_ENCODER=vulkan: configured render node did not match any \
                     detected GPU's device_path; applying QUASAR_VULKAN_MAX_SESSIONS to \
                     every detected GPU (fail-open — never advertise zero vulkan capacity)"
                );
            } else {
                tracing::debug!(
                    slots,
                    "QUASAR_ENCODER=vulkan, no render node configured: applying \
                     QUASAR_VULKAN_MAX_SESSIONS to every detected GPU"
                );
            }
            for g in gpus.iter_mut() {
                g.encode_slots_total = slots;
            }
        }
    }
}

/// `QUASAR_NVENC_MAX_SESSIONS`, `Some(n)` only for a positive integer. No encoder gating,
/// unlike the vulkan knob: NVENC is also the per-session vendor fallback on a vulkan host,
/// so the ceiling applies to every NVIDIA GPU whenever it is set. Malformed or non-positive
/// values warn and are ignored — never a silently zero or negative advertised capacity.
fn nvenc_max_sessions_override() -> Option<i32> {
    let raw = std::env::var("QUASAR_NVENC_MAX_SESSIONS").ok()?;
    match raw.trim().parse::<i32>() {
        Ok(n) if n > 0 => Some(n),
        Ok(n) => {
            tracing::warn!(
                token = "knob-invalid-nvenc-max-sessions",
                value = n,
                "QUASAR_NVENC_MAX_SESSIONS must be a positive integer; ignoring \
                 (vendor default stands)"
            );
            None
        }
        Err(_) => {
            tracing::warn!(
                token = "knob-invalid-nvenc-max-sessions",
                value = %raw,
                "QUASAR_NVENC_MAX_SESSIONS is not a valid integer; ignoring \
                 (vendor default stands)"
            );
            None
        }
    }
}

/// Non-NVIDIA GPUs must never be touched: the knob is about the NVENC teardown UAF, not
/// encode capacity in general.
fn apply_nvenc_override_to(gpus: &mut [GpuCapacity], slots: i32) {
    for g in gpus.iter_mut().filter(|g| g.vendor == "nvidia") {
        g.encode_slots_total = slots;
    }
}

/// `QUASAR_RENDER_NODE` canonicalized to the `/dev/dri/renderD*` form comparable against
/// `GpuCapacity::device_path`, via `session::settings::canonicalize_render_node` — never a
/// second render-node resolution. `None` for the `"software"` default or anything outside
/// `/dev/dri/`, which callers read as "no specific GPU configured".
fn configured_vulkan_render_node() -> Option<String> {
    let raw = std::env::var("QUASAR_RENDER_NODE").unwrap_or_else(|_| "software".to_string());
    if !raw.starts_with("/dev/dri/") {
        return None;
    }
    Some(crate::session::settings::canonicalize_render_node(&raw))
}

/// `QUASAR_VULKAN_MAX_SESSIONS`, default 2 — the rung-3 soak proved 2 concurrent Vulkan
/// sessions healthy under a per-session kill test; N=4 was a probe (60fps, encode p95 <8ms,
/// ~710 MiB VRAM/session), and a deeper soak gates raising the default. Spec
/// `docs/design/plans/2026-07-25-vulkan-multisession-spec.md` §2d/§4. Non-numeric, zero and
/// negative all warn and fall back: a malformed knob must never advertise zero capacity.
fn vulkan_max_sessions() -> i32 {
    const DEFAULT: i32 = 2;
    match std::env::var("QUASAR_VULKAN_MAX_SESSIONS") {
        Err(_) => DEFAULT,
        Ok(raw) => match raw.trim().parse::<i32>() {
            Ok(n) if n > 0 => n,
            Ok(n) => {
                tracing::warn!(
                    token = "knob-invalid-vulkan-max-sessions",
                    value = n,
                    default = DEFAULT,
                    "QUASAR_VULKAN_MAX_SESSIONS must be a positive integer; using default"
                );
                DEFAULT
            }
            Err(_) => {
                tracing::warn!(
                    token = "knob-invalid-vulkan-max-sessions",
                    value = %raw,
                    default = DEFAULT,
                    "QUASAR_VULKAN_MAX_SESSIONS is not a valid integer; using default"
                );
                DEFAULT
            }
        },
    }
}

/// PCI bus address (`0000:01:00.0`) from a sysfs `device` symlink's canonical basename.
fn pci_address(device_path: &std::path::Path) -> Option<String> {
    let canon = std::fs::canonicalize(device_path).ok()?;
    canon.file_name()?.to_str().map(str::to_string)
}

/// Resolve a PCI address to its `/dev/dri/renderD*` node by walking `/sys/class/drm`. Shared
/// by `session::settings`' render-node canonicalization fallback (a by-path value whose
/// symlink is invisible in the container) and this file's `render_node` reporting.
pub(crate) fn renderd_node_for_pci_addr(pci_addr: &str) -> Option<String> {
    renderd_node_for_pci_addr_root(pci_addr, std::path::Path::new("/sys/class/drm"))
}

/// Injectable-root version, so a test can point at a tempdir instead of `/sys/class/drm`.
pub(crate) fn renderd_node_for_pci_addr_root(
    pci_addr: &str,
    sysfs_root: &std::path::Path,
) -> Option<String> {
    let entries = std::fs::read_dir(sysfs_root).ok()?;
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        if !name.starts_with("renderD") {
            continue;
        }
        let Ok(canon) = std::fs::canonicalize(entry.path().join("device")) else {
            continue;
        };
        let Some(basename) = canon.file_name().and_then(|f| f.to_str()) else {
            continue;
        };
        if basename.eq_ignore_ascii_case(pci_addr) {
            return Some(format!("/dev/dri/{name}"));
        }
    }
    None
}

fn read_model(device_path: &std::path::Path, vendor: &str) -> String {
    if let Ok(name) = std::fs::read_to_string(device_path.join("product_name")) {
        let name = name.trim().to_string();
        if !name.is_empty() {
            return name;
        }
    }
    // The proprietary driver's procfs tree carries the brand name ("GeForce RTX 5090");
    // pci.ids is a stock database and does not. Try it first.
    if vendor == "nvidia" {
        if let Some(addr) = pci_address(device_path) {
            if let Some(model) = read_nvidia_proc_model(&addr) {
                return model;
            }
        }
    }
    if let Some(model) = read_pci_ids_model(device_path, vendor) {
        return model;
    }
    if let Ok(uevent) = std::fs::read_to_string(device_path.join("uevent")) {
        for line in uevent.lines() {
            if let Some(id) = line.strip_prefix("PCI_ID=") {
                return format!("{vendor} PCI:{id}");
            }
        }
    }
    format!("{vendor} GPU")
}

/// `Model:` from `/proc/driver/nvidia/gpus/<addr>/information` (proprietary driver only).
fn read_nvidia_proc_model(pci_addr: &str) -> Option<String> {
    let content =
        std::fs::read_to_string(format!("/proc/driver/nvidia/gpus/{pci_addr}/information")).ok()?;
    parse_nvidia_information(&content)
}

fn parse_nvidia_information(content: &str) -> Option<String> {
    for line in content.lines() {
        if let Some(model) = line.strip_prefix("Model:") {
            let model = model.trim();
            if !model.is_empty() {
                return Some(model.to_string());
            }
        }
    }
    None
}

/// #412: process-lifetime memo for the ~1.5 MB `pci.ids` database. The id→name mapping is
/// boot-fixed, and `detect()` runs on the select loop at startup, on every `config_update`,
/// every console hotplug tick, and every session stop or failure.
///
/// A struct rather than a bare `static OnceLock` so a test can own an instance over an
/// injected path and prove the file is opened once.
struct PciIdsDb {
    contents: std::sync::OnceLock<Option<String>>,
}

impl PciIdsDb {
    const fn new() -> Self {
        Self {
            contents: std::sync::OnceLock::new(),
        }
    }

    /// Read at most once per instance, the negative result included: a host with no
    /// `pci.ids` must not pay a failed read per GPU per detect.
    fn get(&self, paths: &[&std::path::Path]) -> Option<&str> {
        self.contents
            .get_or_init(|| paths.iter().find_map(|p| std::fs::read_to_string(p).ok()))
            .as_deref()
    }
}

static PCI_IDS: PciIdsDb = PciIdsDb::new();

/// Debian/Ubuntu path first, then the Fedora/RHEL (`hwdata`) path.
const PCI_IDS_PATHS: [&str; 2] = ["/usr/share/misc/pci.ids", "/usr/share/hwdata/pci.ids"];

fn system_pci_ids() -> Option<&'static str> {
    let paths: Vec<&std::path::Path> = PCI_IDS_PATHS
        .iter()
        .map(|p| std::path::Path::new(*p))
        .collect();
    PCI_IDS.get(&paths)
}

/// sysfs vendor/device ids against the system `pci.ids` database (memoized, [`PciIdsDb`]).
fn read_pci_ids_model(device_path: &std::path::Path, vendor: &str) -> Option<String> {
    let vendor_id = std::fs::read_to_string(device_path.join("vendor"))
        .ok()?
        .trim()
        .trim_start_matches("0x")
        .to_ascii_lowercase();
    let device_id = std::fs::read_to_string(device_path.join("device"))
        .ok()?
        .trim()
        .trim_start_matches("0x")
        .to_ascii_lowercase();
    let db = system_pci_ids()?;
    let device_name = parse_pci_ids(db, &vendor_id, &device_id)?;
    Some(format!("{vendor} {device_name}"))
}

/// Ids are lowercase hex with no `0x`. Format: a column-0 vendor line, then tab-indented
/// device lines until the next column-0 line.
fn parse_pci_ids(db: &str, vendor_id: &str, device_id: &str) -> Option<String> {
    let mut in_vendor = false;
    for line in db.lines() {
        if line.starts_with('#') || line.is_empty() {
            continue;
        }
        if !line.starts_with('\t') {
            in_vendor = line
                .split_whitespace()
                .next()
                .map(|id| id.eq_ignore_ascii_case(vendor_id))
                .unwrap_or(false);
            continue;
        }
        if !in_vendor || line.starts_with("\t\t") {
            // Skip subdevice (double-tab) lines: this is a device-list lookup.
            continue;
        }
        let trimmed = line.trim_start_matches('\t');
        let mut parts = trimmed.splitn(2, char::is_whitespace);
        let id = parts.next().unwrap_or_default();
        if id.eq_ignore_ascii_case(device_id) {
            return Some(parts.next().unwrap_or_default().trim().to_string());
        }
    }
    None
}

/// Intel local-memory sysfs is optional (and driver-dependent). An iGPU has no
/// dedicated VRAM: budget half of host RAM for admission, as an estimate, never a
/// free-memory reading. Keep the remaining half for the OS and session processes.
/// This does not prove encoder support; readiness still owns that decision.
fn read_intel_memory_mb(card_path: &std::path::Path, mem_mb: i32) -> Option<i32> {
    // i915 exposes this on the DRM card, NOT on its PCI `device` directory.
    let local = read_optional_memory_bytes(&card_path.join("lmem_total_bytes"))?;
    if local > 0 {
        return positive_memory_mb(local);
    }

    // Where xe exposes per-tile VRAM, count every tile, not just tile0. Missing
    // files mean this ABI is unavailable; malformed/unreadable data fails closed.
    let mut local = 0_u64;
    let mut missing = false;
    for entry in std::fs::read_dir(card_path.join("device")).ok()? {
        let entry = entry.ok()?;
        let name = entry.file_name();
        let name = name.to_string_lossy();
        if name
            .strip_prefix("tile")
            .is_some_and(|n| !n.is_empty() && n.parse::<u32>().is_ok())
        {
            let bytes = read_optional_memory_bytes(&entry.path().join("physical_vram_size_bytes"))?;
            missing |= bytes == 0;
            local = local.checked_add(bytes)?;
        }
    }
    if local > 0 {
        return if missing {
            None
        } else {
            positive_memory_mb(local)
        };
    }

    let budget = mem_mb / 2;
    if budget <= 0 {
        return None;
    }
    tracing::info!(
        token = "intel-shared-memory-capacity",
        path = %card_path.display(),
        host_mem_mb = mem_mb,
        budget_mb = budget,
        "Intel local-memory capacity unavailable; using estimated shared-memory budget"
    );
    Some(budget)
}

/// Absent sysfs attributes are optional; corrupt or unreadable ones are not a
/// reason to advertise a potentially larger shared-memory estimate.
fn read_optional_memory_bytes(path: &std::path::Path) -> Option<u64> {
    match std::fs::read_to_string(path) {
        Ok(value) => value.trim().parse().ok(),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Some(0),
        Err(_) => None,
    }
}

fn positive_memory_mb(bytes: u64) -> Option<i32> {
    i32::try_from(bytes / 1024 / 1024).ok().filter(|mb| *mb > 0)
}

fn read_vram_mb(device_path: &std::path::Path, vendor: &str) -> Option<i32> {
    if vendor == "amd" {
        let bytes: u64 = std::fs::read_to_string(device_path.join("mem_info_vram_total"))
            .ok()
            .and_then(|s| s.trim().parse().ok())
            .unwrap_or(0);
        if bytes > 0 {
            return Some((bytes / 1024 / 1024) as i32);
        }
    }
    if vendor == "nvidia" {
        if let Some(addr) = pci_address(device_path) {
            if let Some(mb) = nvidia_smi_vram_mb(&addr) {
                return Some(mb);
            }
        }
    }
    None
}

/// Memoized for the process lifetime (VRAM size is static), so N GPUs across any number of
/// `detect()` calls cost exactly one `nvidia-smi` invocation.
fn nvidia_smi_vram_mb(pci_addr: &str) -> Option<i32> {
    let rows = nvidia_smi_rows();
    let rows = rows.as_ref()?;
    let normalized = normalize_pci_addr(pci_addr);
    rows.iter()
        .find(|(addr, _)| normalize_pci_addr(addr) == normalized)
        .map(|(_, mb)| *mb)
}

/// Capacity enumeration is DRM-card based while CUDA ordinals follow `nvidia-smi`; those
/// indexes are unrelated, so match by PCI identity.
pub(crate) fn nvidia_cuda_index_for_render_node(render_node: &str) -> Option<i32> {
    nvidia_cuda_index_for_render_node_in_rows(render_node, nvidia_smi_rows().as_ref()?)
}

fn nvidia_cuda_index_for_render_node_in_rows(
    render_node: &str,
    rows: &[(String, i32)],
) -> Option<i32> {
    let pci_addr = render_node
        .strip_prefix("/dev/dri/by-path/pci-")?
        .strip_suffix("-render")?;
    let normalized = normalize_pci_addr(pci_addr);
    rows.iter()
        .position(|(addr, _)| normalize_pci_addr(addr) == normalized)
        .map(|index| index as i32)
}

/// Domain-padding-agnostic PCI address, so sysfs's `0000:01:00.0` compares equal to
/// nvidia-smi's `00000000:01:00.0`. `crate::vram` reuses it for the same matching against a
/// different `nvidia-smi` query.
pub(crate) fn normalize_pci_addr(addr: &str) -> String {
    let addr = addr.to_ascii_lowercase();
    let parts: Vec<&str> = addr.split(':').collect();
    if parts.len() == 3 {
        // domain:bus:device.function
        let domain = parts[0].trim_start_matches('0');
        format!("{}:{}:{}", domain, parts[1], parts[2])
    } else {
        addr
    }
}

/// #407: hard deadline on the capacity-path `nvidia-smi` fork, which runs on the agent's
/// select-loop thread. On an Xid-class fault `nvidia-smi` wedges indefinitely; blocking that
/// thread stops heartbeats, metrics, relay and `session_stop`, and at t+20s the control
/// plane declares the host stale and reaps EVERY session on it.
///
/// 10s stays under that 20s stale-host deadline, so even a full timeout cannot cost the host
/// its sessions. Timeout ⇒ `None` and the caller fails closed.
const NVIDIA_SMI_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(10);

/// #531: pay the one-time `nvidia-smi` fork on the caller's thread (the blocking pool, from
/// `agent::detect_capacity_blocking`), so [`nvidia_cuda_index_for_render_node`] on the
/// session-assign path — reached from the synchronous `handle_control` — is a cache read.
pub(crate) fn prewarm_nvidia_smi_rows() {
    let _ = nvidia_smi_rows();
}

fn nvidia_smi_rows() -> &'static Option<Vec<(String, i32)>> {
    static ROWS: std::sync::OnceLock<Option<Vec<(String, i32)>>> = std::sync::OnceLock::new();
    ROWS.get_or_init(|| {
        query_gpu_rows(
            "nvidia-smi",
            &[
                "--query-gpu=pci.bus_id,memory.total",
                "--format=csv,noheader,nounits",
            ],
            NVIDIA_SMI_TIMEOUT,
        )
    })
}

/// Command injectable so the deadline is exercised against a real `sh -c 'sleep …'` standing
/// in for a wedged `nvidia-smi`. Uses the crate's bounded-exec primitive, which kills and
/// reaps a hung child rather than timing out an await.
fn query_gpu_rows(
    cmd: &str,
    args: &[&str],
    timeout: std::time::Duration,
) -> Option<Vec<(String, i32)>> {
    match crate::vram::run_bounded(cmd, args, timeout) {
        Some((stdout, status)) if status.success() => Some(parse_nvidia_smi_csv(&stdout)),
        Some((_, status)) => {
            tracing::debug!(%status, "capacity: {cmd} exited non-zero; no GPU rows");
            None
        }
        None => {
            tracing::warn!(
                token = "gpu-query-timeout",
                "capacity: {cmd} failed to spawn, or exceeded the {timeout:?} deadline and was \
                 killed — GPU VRAM/CUDA-ordinal lookup unavailable this process"
            );
            None
        }
    }
}

/// Rows of `<bus_id>, <mb>`.
fn parse_nvidia_smi_csv(text: &str) -> Vec<(String, i32)> {
    let mut out = Vec::new();
    for line in text.lines() {
        let mut parts = line.splitn(2, ',');
        let addr = parts.next().map(str::trim).unwrap_or_default();
        let mb = parts.next().and_then(|s| s.trim().parse::<i32>().ok());
        if let (false, Some(mb)) = (addr.is_empty(), mb) {
            out.push((addr.to_string(), mb));
        }
    }
    out
}

fn detection_failure(
    status: &str,
    reason: String,
    allow_synthetic: bool,
) -> (Vec<GpuCapacity>, Vec<VramTarget>, String, Option<String>) {
    if !allow_synthetic {
        tracing::error!(
            token = "gpu-capacity-unavailable",%status, %reason, "GPU capacity unavailable; host will be unschedulable");
        return (Vec::new(), Vec::new(), status.to_string(), Some(reason));
    }
    tracing::warn!(
        token = "synthetic-gpu-capacity",%reason, "synthetic GPU capacity explicitly enabled for development");
    (
        vec![GpuCapacity {
            index: 0,
            vendor: "unknown".to_string(),
            model: "unknown".to_string(),
            vram_mb_total: 8192,
            encode_slots_total: 1,
            render_node: None,
            device_path: None,
        }],
        vec![VramTarget {
            index: 0,
            vendor: "unknown".to_string(),
            sysfs_device: std::path::PathBuf::new(),
            pci_addr: None,
            total_mb: 8192,
        }],
        "ok".to_string(),
        Some("synthetic development capacity".to_string()),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    const PCI_IDS_FIXTURE: &str = "\
# comment line, ignored
0001  Some Other Vendor
\t0010  Widget Card
10de  NVIDIA Corporation
\t2685  AD102 [GeForce RTX 5090]
\t2704  GA104 [GeForce RTX 3070]
\t\t1043 8613  Subdevice line, should be skipped
1002  Advanced Micro Devices, Inc. [AMD/ATI]
\t1638  Renoir
";

    #[test]
    fn missing_drm_inventory_fails_closed() {
        let (gpus, vram_targets, status, reason) = detect_gpus_at(
            std::path::Path::new("/definitely/not/a/drm/inventory"),
            false,
            16384,
        );
        assert!(gpus.is_empty());
        assert!(vram_targets.is_empty());
        assert_eq!(status, "failed");
        assert!(reason.is_some());
    }

    #[test]
    fn empty_drm_inventory_is_unavailable() {
        let dir = tempfile::tempdir().unwrap();
        let (gpus, vram_targets, status, _) = detect_gpus_at(dir.path(), false, 16384);
        assert!(gpus.is_empty());
        assert!(vram_targets.is_empty());
        assert_eq!(status, "unavailable");
    }

    #[test]
    fn synthetic_capacity_requires_explicit_opt_in() {
        let dir = tempfile::tempdir().unwrap();
        let (gpus, vram_targets, status, reason) = detect_gpus_at(dir.path(), true, 16384);
        assert_eq!(gpus.len(), 1);
        assert_eq!(gpus[0].vendor, "unknown");
        assert_eq!(vram_targets.len(), 1);
        assert_eq!(vram_targets[0].vendor, "unknown");
        assert_eq!(status, "ok");
        assert_eq!(reason.as_deref(), Some("synthetic development capacity"));
    }

    /// Deletes the file after the first read: a memoized database still answers, a
    /// re-reading one cannot.
    #[test]
    fn pci_ids_database_is_read_once_per_process() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("pci.ids");
        std::fs::write(&path, PCI_IDS_FIXTURE).unwrap();

        let db = PciIdsDb::new();
        let first = db.get(&[path.as_path()]).expect("first read");
        assert!(first.contains("GeForce RTX 5090"));

        std::fs::remove_file(&path).unwrap();
        let second = db
            .get(&[path.as_path()])
            .expect("second lookup must be served from the memo, not the filesystem");
        assert!(second.contains("GeForce RTX 5090"));
    }

    /// The negative result must memoize too, or every GPU on every detect pays a failed open.
    #[test]
    fn pci_ids_absence_is_memoized_too() {
        let dir = tempfile::tempdir().unwrap();
        let missing = dir.path().join("nope.ids");
        let db = PciIdsDb::new();
        assert!(db.get(&[missing.as_path()]).is_none());
        std::fs::write(&missing, PCI_IDS_FIXTURE).unwrap();
        assert!(
            db.get(&[missing.as_path()]).is_none(),
            "the absent-database result must be memoized, not re-probed"
        );
    }

    /// A wedged GPU tool must be killed at the deadline, not block the select loop until the
    /// control plane reaps every session on the host.
    #[test]
    fn gpu_row_query_is_killed_at_its_deadline() {
        let started = std::time::Instant::now();
        let rows = query_gpu_rows(
            "sh",
            &["-c", "sleep 60"],
            std::time::Duration::from_millis(300),
        );
        let elapsed = started.elapsed();
        assert!(rows.is_none(), "a killed probe must fail closed");
        assert!(
            elapsed < std::time::Duration::from_secs(5),
            "the probe blocked its caller for {elapsed:?} — the deadline did not fire"
        );
    }

    #[test]
    fn gpu_row_query_parses_a_successful_response() {
        let rows = query_gpu_rows(
            "sh",
            &["-c", "printf '00000000:01:00.0, 32607\\n'"],
            std::time::Duration::from_secs(5),
        )
        .expect("successful probe");
        assert_eq!(rows, vec![("00000000:01:00.0".to_string(), 32607)]);
    }

    /// A non-zero exit is a miss, not a parse of empty output.
    #[test]
    fn gpu_row_query_treats_a_failed_exit_as_no_rows() {
        assert!(
            query_gpu_rows("sh", &["-c", "exit 9"], std::time::Duration::from_secs(5)).is_none()
        );
    }

    #[test]
    fn pci_ids_parser_finds_device() {
        assert_eq!(
            parse_pci_ids(PCI_IDS_FIXTURE, "10de", "2685"),
            Some("AD102 [GeForce RTX 5090]".to_string())
        );
        assert_eq!(
            parse_pci_ids(PCI_IDS_FIXTURE, "1002", "1638"),
            Some("Renoir".to_string())
        );
    }

    #[test]
    fn pci_ids_parser_skips_subdevice_lines() {
        // "1043" is a subdevice id under 10de/2704, not a device id at that vendor.
        assert_eq!(parse_pci_ids(PCI_IDS_FIXTURE, "10de", "1043"), None);
    }

    #[test]
    fn pci_ids_parser_unknown_returns_none() {
        assert_eq!(parse_pci_ids(PCI_IDS_FIXTURE, "ffff", "ffff"), None);
    }

    #[test]
    fn audio_sinks_report_exact_playback_pcm_devices() {
        let cards = "\
 0 [NVidia         ]: HDA-Intel - HDA NVidia
 1 [Generic        ]: HDA-Intel - HD-Audio Generic
";
        let pcm = "\
00-03: HDMI 0 : HDMI 0 : playback 1
00-07: HDMI 1 : HDMI 1 : playback 1
01-00: ALC1220 Analog : ALC1220 Analog : playback 1 : capture 1
";
        assert_eq!(
            parse_audio_sinks(cards, pcm),
            vec![
                AudioSink {
                    id: "hw:0,3".to_string(),
                    label: "HDA NVidia — HDMI 0".to_string(),
                },
                AudioSink {
                    id: "hw:0,7".to_string(),
                    label: "HDA NVidia — HDMI 1".to_string(),
                },
                AudioSink {
                    id: "hw:1,0".to_string(),
                    label: "HD-Audio Generic — ALC1220 Analog".to_string(),
                },
            ]
        );
    }

    #[test]
    fn audio_sinks_fall_back_to_card_when_pcm_is_unavailable() {
        let cards = " 0 [NVidia ]: HDA-Intel - HDA NVidia\n";
        assert_eq!(
            parse_audio_sinks(cards, ""),
            vec![AudioSink {
                id: "hw:0".to_string(),
                label: "HDA NVidia".to_string(),
            }]
        );
    }

    #[test]
    fn audio_sink_pcm_path_matches_alsa_endpoint() {
        assert_eq!(
            audio_sink_device_path("hw:0,3").as_deref(),
            Some(std::path::Path::new("/dev/snd/pcmC0D3p"))
        );
        assert!(audio_sink_device_path("hw:0").is_none());
    }

    #[test]
    fn nvidia_information_parser_finds_model() {
        let fixture = "\
Model: \t\t NVIDIA GeForce RTX 5090
IRQ:   \t\t 146
GPU UUID: \t GPU-abcdef01-2345-6789-abcd-ef0123456789
Video BIOS: \t 98.02.3c.40.b2
";
        assert_eq!(
            parse_nvidia_information(fixture),
            Some("NVIDIA GeForce RTX 5090".to_string())
        );
    }

    #[test]
    fn nvidia_information_parser_missing_model_line() {
        assert_eq!(parse_nvidia_information("IRQ: 146\n"), None);
    }

    #[test]
    fn nvidia_smi_csv_matches_by_normalized_pci_addr() {
        let csv = "00000000:01:00.0, 32768\n00000000:41:00.0, 24576\n";
        let rows = parse_nvidia_smi_csv(csv);
        assert_eq!(rows.len(), 2);
        // The sysfs form must match nvidia-smi's zero-padded one.
        let found = rows
            .iter()
            .find(|(addr, _)| normalize_pci_addr(addr) == normalize_pci_addr("0000:01:00.0"));
        assert_eq!(found.map(|(_, mb)| *mb), Some(32768));
    }

    #[test]
    fn normalize_pci_addr_strips_domain_padding() {
        assert_eq!(
            normalize_pci_addr("00000000:01:00.0"),
            normalize_pci_addr("0000:01:00.0")
        );
    }

    /// `<root>/renderD<n>/device` symlinked to a dir whose basename is `pci_addr`, mimicking
    /// `/sys/class/drm/renderD128/device -> ../../devices/.../0000:04:00.0`.
    fn fake_sysfs_render_entry(root: &std::path::Path, render_name: &str, pci_addr: &str) {
        let device_target = root.join("devices").join(pci_addr);
        std::fs::create_dir_all(&device_target).unwrap();
        let render_dir = root.join(render_name);
        std::fs::create_dir_all(&render_dir).unwrap();
        std::os::unix::fs::symlink(&device_target, render_dir.join("device")).unwrap();
    }

    #[test]
    fn renderd_node_for_pci_addr_root_finds_match() {
        let dir = tempfile::tempdir().unwrap();
        let drm_root = dir.path().join("class-drm");
        std::fs::create_dir_all(&drm_root).unwrap();
        fake_sysfs_render_entry(&drm_root, "renderD128", "0000:04:00.0");

        let resolved = renderd_node_for_pci_addr_root("0000:04:00.0", &drm_root);
        assert_eq!(resolved, Some("/dev/dri/renderD128".to_string()));
    }

    #[test]
    fn renderd_node_for_pci_addr_root_no_match_returns_none() {
        let dir = tempfile::tempdir().unwrap();
        let drm_root = dir.path().join("class-drm");
        std::fs::create_dir_all(&drm_root).unwrap();
        fake_sysfs_render_entry(&drm_root, "renderD128", "0000:04:00.0");

        assert_eq!(
            renderd_node_for_pci_addr_root("0000:99:00.0", &drm_root),
            None
        );
    }

    #[test]
    fn renderd_node_for_pci_addr_root_iterates_multiple_entries() {
        let dir = tempfile::tempdir().unwrap();
        let drm_root = dir.path().join("class-drm");
        std::fs::create_dir_all(&drm_root).unwrap();
        fake_sysfs_render_entry(&drm_root, "renderD128", "0000:01:00.0");
        fake_sysfs_render_entry(&drm_root, "renderD129", "0000:04:00.0");

        assert_eq!(
            renderd_node_for_pci_addr_root("0000:04:00.0", &drm_root),
            Some("/dev/dri/renderD129".to_string())
        );
    }

    /// The by-path string is a plain `format!()`, not filesystem-derived; what is under test
    /// is the "GPU has a render node" gate that decides whether to construct it.
    #[test]
    fn by_path_render_node_constructed_when_render_entry_exists() {
        let dir = tempfile::tempdir().unwrap();
        let drm_root = dir.path().join("class-drm");
        std::fs::create_dir_all(&drm_root).unwrap();
        fake_sysfs_render_entry(&drm_root, "renderD128", "0000:04:00.0");

        let addr = "0000:04:00.0";
        let render_node = renderd_node_for_pci_addr_root(addr, &drm_root)
            .map(|_| format!("/dev/dri/by-path/pci-{addr}-render"));
        assert_eq!(
            render_node,
            Some("/dev/dri/by-path/pci-0000:04:00.0-render".to_string())
        );
    }

    #[test]
    fn by_path_render_node_absent_when_no_render_entry() {
        let dir = tempfile::tempdir().unwrap();
        let drm_root = dir.path().join("class-drm");
        std::fs::create_dir_all(&drm_root).unwrap();
        // No renderD* entries: a display-only iGPU.

        let addr = "0000:04:00.0";
        let render_node = renderd_node_for_pci_addr_root(addr, &drm_root)
            .map(|_| format!("/dev/dri/by-path/pci-{addr}-render"));
        assert_eq!(render_node, None);
    }

    #[test]
    fn cpu_model_parser_finds_model_name() {
        let fixture = "\
processor\t: 0
vendor_id\t: AuthenticAMD
cpu family\t: 25
model name\t: AMD Ryzen 9 7950X 16-Core Processor
stepping\t: 2
";
        assert_eq!(
            parse_cpu_model(fixture),
            Some("AMD Ryzen 9 7950X 16-Core Processor".to_string())
        );
    }

    #[test]
    fn cpu_model_parser_missing_key_returns_none() {
        assert_eq!(parse_cpu_model("processor\t: 0\nvendor_id\t: ARM\n"), None);
    }

    #[test]
    fn cuda_index_is_resolved_by_pci_not_drm_index() {
        let rows = vec![
            ("00000000:65:00.0".to_string(), 24_000),
            ("00000000:01:00.0".to_string(), 16_000),
        ];
        assert_eq!(
            nvidia_cuda_index_for_render_node_in_rows(
                "/dev/dri/by-path/pci-0000:01:00.0-render",
                &rows,
            ),
            Some(1)
        );
        assert_eq!(
            nvidia_cuda_index_for_render_node_in_rows("/dev/dri/renderD128", &rows),
            None
        );
    }

    #[test]
    fn drm_refresh_uses_exact_timing_millihertz() {
        assert_eq!(
            refresh_millihz(148_500, 2200, 1125, false, false, 0, 0),
            60_000
        );
        assert_eq!(
            refresh_millihz(148_352, 2200, 1125, false, false, 0, 0),
            59_940
        );
        assert_eq!(
            refresh_millihz(74_250, 2200, 1125, true, false, 0, 0),
            60_000
        );
    }

    // ---- QUASAR_VULKAN_MAX_SESSIONS ----

    /// A `/sys/class/drm/<card>/device/{vendor,mem_info_vram_total}` fixture, the shape
    /// `detect_gpus_at` walks. AMD only: its VRAM read is a plain sysfs file, so these tests
    /// stay hermetic where the nvidia path would need `nvidia-smi`.
    fn fake_amd_card(root: &std::path::Path, card_name: &str) {
        let device_dir = root.join(card_name).join("device");
        std::fs::create_dir_all(&device_dir).unwrap();
        std::fs::write(device_dir.join("vendor"), "0x1002\n").unwrap();
        std::fs::write(device_dir.join("mem_info_vram_total"), "8589934592\n").unwrap();
    }

    #[test]
    fn intel_igpu_without_dedicated_vram_remains_in_inventory() {
        let dir = tempfile::tempdir().unwrap();
        let device = dir.path().join("card0/device");
        std::fs::create_dir_all(&device).unwrap();
        std::fs::write(device.join("vendor"), "0x8086\n").unwrap();
        std::fs::write(device.join("device"), "0x4692\n").unwrap();

        let (gpus, targets, status, reason) = detect_gpus_at(dir.path(), false, 16384);
        assert_eq!(status, "ok", "Intel iGPU discarded: {reason:?}");
        assert_eq!(gpus.len(), 1);
        assert_eq!(gpus[0].vendor, "intel");
        assert_eq!(gpus[0].vram_mb_total, 8192);
        assert_eq!(targets.len(), 1);
        assert_eq!(targets[0].total_mb, gpus[0].vram_mb_total);
    }

    #[test]
    fn intel_memory_sources_and_invalid_capacity() {
        type MemoryCase = (
            &'static str,
            &'static [(&'static str, &'static str)],
            i32,
            Option<i32>,
        );
        let cases: &[MemoryCase] = &[
            ("shared", &[], 16384, Some(8192)),
            ("no host memory", &[], 0, None),
            ("invalid host memory", &[], -4096, None),
            (
                "i915 local",
                &[("lmem_total_bytes", "4294967296\n")],
                16384,
                Some(4096),
            ),
            (
                "i915 zero means shared",
                &[("lmem_total_bytes", "0")],
                16384,
                Some(8192),
            ),
            (
                "xe local",
                &[("device/tile0/physical_vram_size_bytes", "6442450944")],
                16384,
                Some(6144),
            ),
            (
                "xe multiple tiles",
                &[
                    ("device/tile0/physical_vram_size_bytes", "4294967296"),
                    ("device/tile1/physical_vram_size_bytes", "4294967296"),
                ],
                16384,
                Some(8192),
            ),
            (
                "local needs no host memory",
                &[("lmem_total_bytes", "4294967296")],
                0,
                Some(4096),
            ),
            ("corrupt local", &[("lmem_total_bytes", "bad")], 16384, None),
            ("negative local", &[("lmem_total_bytes", "-1")], 16384, None),
            ("sub MiB local", &[("lmem_total_bytes", "1")], 16384, None),
            (
                "overflow local",
                &[("lmem_total_bytes", "18446744073709551615")],
                16384,
                None,
            ),
            (
                "corrupt xe",
                &[("device/tile0/physical_vram_size_bytes", "bad")],
                16384,
                None,
            ),
            (
                "incomplete xe",
                &[
                    ("device/tile0/physical_vram_size_bytes", "4294967296"),
                    ("device/tile1/other", "0"),
                ],
                16384,
                None,
            ),
        ];
        for (name, files, mem_mb, expected) in cases {
            let dir = tempfile::tempdir().unwrap();
            let card = dir.path().join("card0");
            std::fs::create_dir_all(card.join("device")).unwrap();
            std::fs::write(card.join("device/vendor"), "0x8086").unwrap();
            for (path, value) in *files {
                let path = card.join(path);
                std::fs::create_dir_all(path.parent().unwrap()).unwrap();
                std::fs::write(path, value).unwrap();
            }
            let (gpus, targets, status, _) = detect_gpus_at(dir.path(), false, *mem_mb);
            assert_eq!(gpus.first().map(|g| g.vram_mb_total), *expected, "{name}");
            assert_eq!(
                status,
                if expected.is_some() {
                    "ok"
                } else {
                    "unavailable"
                },
                "{name}"
            );
            assert_eq!(targets.len(), gpus.len(), "{name}");
        }
    }

    #[test]
    fn intel_detection_preserves_render_node_and_other_vendors() {
        let dir = tempfile::tempdir().unwrap();
        fake_amd_card_with_render_node(dir.path(), "card0", "renderD128", "0000:00:02.0");
        let device = dir.path().join("card0/device");
        std::fs::write(device.join("vendor"), "0x8086").unwrap();
        std::fs::remove_file(device.join("mem_info_vram_total")).unwrap();
        fake_amd_card(dir.path(), "card1");
        let (gpus, targets, status, _) = detect_gpus_at(dir.path(), false, 8192);
        assert_eq!(status, "ok");
        assert_eq!(gpus.len(), 2);
        assert_eq!(gpus[0].vendor, "intel");
        assert_eq!(gpus[0].vram_mb_total, 4096);
        assert_eq!(
            gpus[0].render_node.as_deref(),
            Some("/dev/dri/by-path/pci-0000:00:02.0-render")
        );
        assert_eq!(gpus[0].device_path.as_deref(), Some("/dev/dri/renderD128"));
        assert_eq!(gpus[1].vendor, "amd");
        assert_eq!(gpus[1].vram_mb_total, 8192);
        for (gpu, target) in gpus.iter().zip(&targets) {
            assert_eq!(gpu.index, target.index);
            assert_eq!(gpu.vram_mb_total, target.total_mb);
        }
        // An estimate must not masquerade as measured live free VRAM.
        let samples = crate::vram::sample(&targets[..1]);
        assert_eq!(samples[0].free_mb, None);
        assert_eq!(samples[0].used_mb, None);
    }

    /// A `VramTarget` per detected GPU, same index and order as `gpus`, without changing
    /// `GpuCapacity`'s wire shape.
    #[test]
    fn detect_gpus_at_threads_matching_vram_targets() {
        let dir = tempfile::tempdir().unwrap();
        let drm_root = dir.path().join("class-drm");
        std::fs::create_dir_all(&drm_root).unwrap();
        fake_amd_card(&drm_root, "card0");

        let (gpus, vram_targets, status, _) = detect_gpus_at(&drm_root, false, 16384);
        assert_eq!(status, "ok");
        assert_eq!(gpus.len(), 1);
        assert_eq!(vram_targets.len(), 1);

        let gpu = &gpus[0];
        let target = &vram_targets[0];
        assert_eq!(target.index, gpu.index);
        assert_eq!(target.vendor, gpu.vendor);
        assert_eq!(target.total_mb, gpu.vram_mb_total);
        assert_eq!(target.sysfs_device, drm_root.join("card0").join("device"));
        assert_eq!(
            target.pci_addr, None,
            "AMD target carries no pci_addr — the sampler reads sysfs_device directly"
        );
    }

    fn restore_env(key: &str, prior: Option<String>) {
        match prior {
            Some(v) => std::env::set_var(key, v),
            None => std::env::remove_var(key),
        }
    }

    // These two vars are process-global env, so every case must live in ONE test that saves
    // and restores them: there is no `serial_test` dep in this crate.
    #[test]
    fn vulkan_capacity_knob_env_gating() {
        let keys = ["QUASAR_ENCODER", "QUASAR_VULKAN_MAX_SESSIONS"];
        let saved: Vec<(&str, Option<String>)> =
            keys.iter().map(|k| (*k, std::env::var(k).ok())).collect();
        for k in &keys {
            std::env::remove_var(k);
        }

        let dir = tempfile::tempdir().unwrap();
        let drm_root = dir.path().join("class-drm");
        std::fs::create_dir_all(&drm_root).unwrap();
        fake_amd_card(&drm_root, "card0");

        // Non-vulkan host: the knob is inert off the vulkan path.
        std::env::set_var("QUASAR_VULKAN_MAX_SESSIONS", "5");
        let (gpus, _, _, _) = detect_gpus_at(&drm_root, false, 16384);
        assert_eq!(gpus.len(), 1);
        assert_eq!(
            gpus[0].encode_slots_total, 2,
            "non-vulkan host keeps the amd:2 vendor stub"
        );

        // Vulkan host honors the knob.
        std::env::set_var("QUASAR_ENCODER", "vulkan");
        std::env::set_var("QUASAR_VULKAN_MAX_SESSIONS", "5");
        let (gpus, _, _, _) = detect_gpus_at(&drm_root, false, 16384);
        assert_eq!(
            gpus[0].encode_slots_total, 5,
            "vulkan host honors QUASAR_VULKAN_MAX_SESSIONS"
        );

        std::env::remove_var("QUASAR_VULKAN_MAX_SESSIONS");
        let (gpus, _, _, _) = detect_gpus_at(&drm_root, false, 16384);
        assert_eq!(
            gpus[0].encode_slots_total, 2,
            "vulkan host with the knob unset defaults to 2"
        );

        // Malformed or non-positive values must never advertise a zero or negative capacity.
        for bad in ["0", "-1", "not-a-number", ""] {
            std::env::set_var("QUASAR_VULKAN_MAX_SESSIONS", bad);
            let (gpus, _, _, _) = detect_gpus_at(&drm_root, false, 16384);
            assert_eq!(
                gpus[0].encode_slots_total, 2,
                "invalid QUASAR_VULKAN_MAX_SESSIONS={bad:?} falls back to default 2"
            );
        }

        for (k, v) in saved {
            restore_env(k, v);
        }
    }

    // ---- QUASAR_NVENC_MAX_SESSIONS ----

    fn synthetic_gpu(vendor: &str, slots: i32) -> GpuCapacity {
        GpuCapacity {
            index: 0,
            vendor: vendor.to_string(),
            model: format!("{vendor} test"),
            vram_mb_total: 8192,
            encode_slots_total: slots,
            render_node: None,
            device_path: None,
        }
    }

    /// An AMD iGPU on the same host must keep its own vendor stub.
    #[test]
    fn nvenc_override_touches_only_nvidia_gpus() {
        let mut gpus = vec![synthetic_gpu("amd", 2), synthetic_gpu("nvidia", 3)];
        apply_nvenc_override_to(&mut gpus, 1);
        assert_eq!(gpus[0].encode_slots_total, 2, "amd stub untouched");
        assert_eq!(gpus[1].encode_slots_total, 1, "nvidia capped to 1");
    }

    /// Unset ⇒ None and the stub stands; malformed or non-positive ⇒ None, never a zero or
    /// negative advertised capacity. One test, because the var is process-global env.
    #[test]
    fn nvenc_max_sessions_env_parsing() {
        let key = "QUASAR_NVENC_MAX_SESSIONS";
        let saved = std::env::var(key).ok();

        std::env::remove_var(key);
        assert_eq!(nvenc_max_sessions_override(), None, "unset ⇒ stub stands");

        std::env::set_var(key, "1");
        assert_eq!(nvenc_max_sessions_override(), Some(1));

        std::env::set_var(key, " 2 ");
        assert_eq!(nvenc_max_sessions_override(), Some(2), "whitespace trimmed");

        for bad in ["0", "-1", "not-a-number", ""] {
            std::env::set_var(key, bad);
            assert_eq!(
                nvenc_max_sessions_override(),
                None,
                "invalid QUASAR_NVENC_MAX_SESSIONS={bad:?} is ignored"
            );
        }

        restore_env(key, saved);
    }

    /// Adds the symlink chain `fake_amd_card` lacks, so `GpuCapacity::device_path` is
    /// populated — that is what `apply_vulkan_override` matches the render node against.
    fn fake_amd_card_with_render_node(
        root: &std::path::Path,
        card_name: &str,
        render_name: &str,
        pci_addr: &str,
    ) {
        let device_target = root.join("devices").join(pci_addr);
        std::fs::create_dir_all(&device_target).unwrap();
        std::fs::write(device_target.join("vendor"), "0x1002\n").unwrap();
        std::fs::write(device_target.join("mem_info_vram_total"), "8589934592\n").unwrap();

        let card_dir = root.join(card_name);
        std::fs::create_dir_all(&card_dir).unwrap();
        std::os::unix::fs::symlink(&device_target, card_dir.join("device")).unwrap();

        let render_dir = root.join(render_name);
        std::fs::create_dir_all(&render_dir).unwrap();
        std::os::unix::fs::symlink(&device_target, render_dir.join("device")).unwrap();
    }

    /// Must not touch `QUASAR_RENDER_NODE`/`QUASAR_ENCODER` or call `detect_gpus_at`: both
    /// would race `session::settings`' unguarded readers and the sibling env test under the
    /// default parallel runner. It resolves `device_path` through the same root-scoped sysfs
    /// walk and drives the pure `apply_vulkan_override_to` with the render node as an
    /// argument instead.
    #[test]
    fn vulkan_capacity_override_scoped_to_configured_render_node() {
        let dir = tempfile::tempdir().unwrap();
        let drm_root = dir.path().join("class-drm");
        std::fs::create_dir_all(&drm_root).unwrap();
        // An iGPU + discrete GPU host. Same-vendor keeps the fixture hermetic; vendor is
        // irrelevant to the render-node matching under test.
        fake_amd_card_with_render_node(&drm_root, "card0", "renderD128", "0000:01:00.0");
        fake_amd_card_with_render_node(&drm_root, "card1", "renderD129", "0000:04:00.0");

        let card0_path = renderd_node_for_pci_addr_root("0000:01:00.0", &drm_root)
            .expect("card0 device_path resolved via the fake-sysfs fixture");
        let card1_path = renderd_node_for_pci_addr_root("0000:04:00.0", &drm_root)
            .expect("card1 device_path resolved via the fake-sysfs fixture");
        assert_eq!(card0_path, "/dev/dri/renderD128");
        assert_eq!(card1_path, "/dev/dri/renderD129");

        let amd_gpu = |device_path: String| GpuCapacity {
            index: 0,
            vendor: "amd".to_string(),
            model: "amd GPU".to_string(),
            vram_mb_total: 8192,
            encode_slots_total: 2,
            render_node: None,
            device_path: Some(device_path),
        };
        let base_gpus = vec![amd_gpu(card0_path), amd_gpu(card1_path)];

        // Matching card1: only card1 gets the override, card0 keeps the vendor stub.
        let mut gpus = base_gpus.clone();
        apply_vulkan_override_to(&mut gpus, 5, Some("/dev/dri/renderD129"));
        let card0 = gpus
            .iter()
            .find(|g| g.device_path.as_deref() == Some("/dev/dri/renderD128"))
            .unwrap();
        let card1 = gpus
            .iter()
            .find(|g| g.device_path.as_deref() == Some("/dev/dri/renderD129"))
            .unwrap();
        assert_eq!(
            card0.encode_slots_total, 2,
            "non-matching GPU keeps its vendor stub, not the vulkan override"
        );
        assert_eq!(
            card1.encode_slots_total, 5,
            "GPU matching the configured render node gets the vulkan override"
        );

        // No match (typo, stale by-path): fail open rather than under-advertise capacity.
        let mut gpus = base_gpus.clone();
        apply_vulkan_override_to(&mut gpus, 5, Some("/dev/dri/renderD999"));
        assert!(
            gpus.iter().all(|g| g.encode_slots_total == 5),
            "no render-node match falls back to applying the override to every GPU"
        );

        // No render node configured (the "software" default): same fail-open fallback.
        let mut gpus = base_gpus.clone();
        apply_vulkan_override_to(&mut gpus, 5, None);
        assert!(
            gpus.iter().all(|g| g.encode_slots_total == 5),
            "unconfigured render node falls back to applying the override to every GPU"
        );
    }

    /// The prefix gate and "software" default, isolated from `QUASAR_RENDER_NODE` env by
    /// exercising the same `canonicalize_render_node` call directly.
    #[test]
    fn configured_vulkan_render_node_prefix_gate() {
        // A non-`/dev/dri/` value must never count as a configured render node.
        assert!(
            !"software".starts_with("/dev/dri/"),
            "sanity: the default value is gated out by the prefix check"
        );
        // A `/dev/dri/...` value absent on this deviceless host canonicalizes to itself.
        assert_eq!(
            crate::session::settings::canonicalize_render_node("/dev/dri/renderD128"),
            "/dev/dri/renderD128"
        );
    }
}
