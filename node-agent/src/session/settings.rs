//! Agent-local runtime knobs, resolved from (in priority order) a control-plane
//! `config_update` push, the process environment, then hardcoded defaults. The
//! `baseline()` constructor reproduces the historical env-only behaviour so an
//! agent that never receives a push behaves exactly as before this feature.

use crate::gpu_vendor::{DetectSource, GpuVendor};

use super::{AbrMode, EncoderChoice};

/// The agent-local realization knobs. Mirrors the per-host hostcfg catalog.
#[derive(Debug, Clone)]
pub struct RuntimeSettings {
    pub encoder: EncoderChoice,
    pub render_node: String,
    /// host-observability-2: the raw `render_node` value as configured (the
    /// `env ← overrides` overlay, verbatim — e.g. a `/dev/dri/by-path/...` value
    /// `render_node` has since canonicalized to its `renderD*` target). Kept
    /// separately because `effective_map()` must report values **as configured**
    /// (agent-api.md `effective_settings`) — canonicalizing here would make a
    /// by-path override never match the admin UI's `resolved`/`overrides` view,
    /// showing a permanent false "restart pending". `render_node` (canonical)
    /// stays what pipeline/VA element pinning uses; this field is reporting-only.
    pub render_node_configured: String,
    pub cuda_device_id: i32,
    pub gop: u32,
    pub num_slices: u32,
    pub target_usage: u32,
    pub queue_buffers: u32,
    pub zerocopy: bool,
    /// Host-stage latency probe (`QUASAR_LATENCY_PROBE`, hostcfg `latency_probe`).
    /// Default OFF. When on, the always-on pad probes additionally time
    /// compositor→encoder→payloader→send per frame and publish per-window p50/p95 on
    /// `session_metrics`. Live class — the probes attach at pipeline build, so a
    /// change applies to the next session with no agent restart. Design:
    /// `docs/superpowers/specs/2026-08-18-latency-probe-design.md`.
    pub latency_probe: bool,
    pub idle_timeout_secs: u64,
    /// SPT-02: ABR mode. The deprecated `abr_enabled` hostcfg key (bool) maps
    /// `false` to `Off`; `true` defers to the current/env-baseline mode (see
    /// `apply_json`) rather than forcing `Protective`. The 3-way `abr_mode`
    /// hostcfg key is authoritative when present. `QUASAR_ABR_MODE` overrides at
    /// the process level; the baseline reads the env.
    pub abr_mode: AbrMode,
    pub abr_floor_kbps: Option<u32>,
    pub abr_floor_ratio: f64,
    /// storage-config (#377): the host root under which the control plane's
    /// `localDriver` synthesises per-(user, app) home paths. The agent uses the
    /// **effective** value (env baseline `QUASAR_HOME_ROOT`, overlaid by a
    /// `config_update` `settings.home_root` push — live-class) for post-session
    /// home-usage measurement; an empty effective root disables measurement.
    /// Stored **verbatim as configured** (not trimmed/validated here) so
    /// `effective_map()` reports the same value space the control plane resolves
    /// against — absoluteness is validated defensively at measurement time
    /// (`session::home`), never here.
    pub home_root: String,
    /// #375: the host directory holding the **32-bit** NVIDIA driver userspace
    /// libs (e.g. `/usr/lib` on unraid), bind-mounted read-only into NVIDIA app
    /// containers at `/opt/quasar/nvidia-lib32` so native 32-bit Linux titles resolve
    /// `libGLX_nvidia.so.*` (the container ships only 64-bit driver libs).
    /// Resolved from the env baseline `QUASAR_NV_LIB32_PATH`, overlaid by a
    /// `config_update` `settings.nvidia_lib32_path` push (live-class). Empty means
    /// "no explicit override" — the agent falls back to the value it auto-detected
    /// at startup via a probe container (`agent.rs`), seeded here so
    /// `effective_map()` reports it. Stored **verbatim as configured**; the mount
    /// is NVIDIA-only (gated in `container.rs`), so a value on a non-NVIDIA host
    /// is inert.
    pub nvidia_lib32_path: String,
    /// SPT-08 ladder knobs (`abr_ladder*`), resolved `env ← config_update` like every
    /// other live-class knob. Snapshotted into `SessionConfig` at assign time.
    pub ladder: super::ladder::LadderSettings,
    /// SPT-04 ABR governor hysteresis knobs (`abr_ewma_alpha`, `abr_deadband`,
    /// `abr_max_up_step`, `abr_min_interval_ms`, `abr_max_down_step`,
    /// `abr_down_dwell_ms`, `abr_cliff_guard_frac`), resolved `env ← config_update`
    /// like every other live-class knob. Snapshotted into `SessionConfig` at
    /// assign time. See [`super::abr::AbrGovernorSettings`].
    pub abr_governor: super::abr::AbrGovernorSettings,
}

/// Explicit-value parse for hostcfg `config_update` pushes (`apply_json`), where a
/// value is always present — env resolution goes through `resolve_encoder_choice`.
pub(crate) fn parse_encoder(s: &str) -> EncoderChoice {
    parse_encoder_known(s).unwrap_or(EncoderChoice::Openh264)
}

fn parse_encoder_known(s: &str) -> Option<EncoderChoice> {
    if s.eq_ignore_ascii_case("va") || s.eq_ignore_ascii_case("vaapi") {
        Some(EncoderChoice::Va)
    } else if s.eq_ignore_ascii_case("nvenc") || s.eq_ignore_ascii_case("nvidia") {
        Some(EncoderChoice::Nvenc)
    } else if s.eq_ignore_ascii_case("vulkan") {
        Some(EncoderChoice::Vulkan)
    } else if s.eq_ignore_ascii_case("openh264") {
        Some(EncoderChoice::Openh264)
    } else {
        None
    }
}

/// Auto-detect default per vendor. `AMD_AUTO_DEFAULT` is the one-line flip point:
/// Amd→Vulkan is being live-validated on an AMD host; set it to `Va` if that fails.
const AMD_AUTO_DEFAULT: EncoderChoice = EncoderChoice::Vulkan;

fn encoder_default_for_vendor(v: Option<GpuVendor>) -> EncoderChoice {
    match v {
        Some(GpuVendor::Nvidia) => EncoderChoice::Vulkan,
        Some(GpuVendor::Amd) => AMD_AUTO_DEFAULT,
        Some(GpuVendor::Intel) => EncoderChoice::Va,
        None => EncoderChoice::Openh264,
    }
}

/// Why `resolve_encoder_from` picked its encoder, for the startup log.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum EncoderReason {
    Env,
    EnvUnrecognized,
    Detected(GpuVendor, DetectSource),
    NoGpu,
}

/// Pure resolution: explicit `QUASAR_ENCODER` wins (unrecognized non-empty values
/// keep the historical openh264 fallback); empty/unset maps the injected detection
/// result through `encoder_default_for_vendor`.
fn resolve_encoder_from(
    raw: &str,
    detected: Option<(GpuVendor, DetectSource)>,
) -> (EncoderChoice, EncoderReason) {
    let raw = raw.trim();
    if !raw.is_empty() {
        return match parse_encoder_known(raw) {
            Some(c) => (c, EncoderReason::Env),
            None => (EncoderChoice::Openh264, EncoderReason::EnvUnrecognized),
        };
    }
    match detected {
        Some((vendor, source)) => (
            encoder_default_for_vendor(Some(vendor)),
            EncoderReason::Detected(vendor, source),
        ),
        None => (encoder_default_for_vendor(None), EncoderReason::NoGpu),
    }
}

/// The one `QUASAR_ENCODER` resolution shared by `RuntimeSettings::baseline`,
/// `SessionConfig::from_env`, and `capacity::vulkan_encode_slots_override` — the
/// auto-detect must not diverge between them. Logs the choice once per process.
pub(crate) fn resolve_encoder_choice() -> EncoderChoice {
    let raw = std::env::var("QUASAR_ENCODER").unwrap_or_default();
    let detected = if raw.trim().is_empty() {
        crate::gpu_vendor::detect()
    } else {
        None
    };
    let (choice, reason) = resolve_encoder_from(&raw, detected);
    static LOGGED: std::sync::Once = std::sync::Once::new();
    LOGGED.call_once(|| {
        if reason == EncoderReason::EnvUnrecognized {
            tracing::warn!(
                token = "encoder-env-unrecognized",
                "QUASAR_ENCODER={raw:?} is not va|vaapi|nvenc|nvidia|vulkan|openh264; \
                 using openh264"
            );
        }
        let (vendor, source) = match reason {
            EncoderReason::Env | EncoderReason::EnvUnrecognized => (None, "env"),
            EncoderReason::Detected(v, s) => (Some(v.as_str()), s.as_str()),
            EncoderReason::NoGpu => (None, "none"),
        };
        tracing::info!(
            token = "encoder-autodetect",
            encoder = encoder_str(choice),
            source,
            vendor,
            "encoder resolved"
        );
    });
    choice
}

/// host-observability: canonicalize a `/dev/dri/...` render-node path to its
/// resolved target (e.g. a stable `by-path` symlink to its current `renderD*`).
/// Render-node numbering is PCI-enumeration order and can flip across reboots,
/// so a `by-path` value only stays useful if resolved once at load and pinned
/// (`va_device_element_prefix` dispatches on the *last* path component).
/// `"software"` and any non-`/dev/dri/` value pass through unchanged.
///
/// Docker does not always bind-mount the `/dev/dri/by-path/` symlink subdirectory
/// into the agent container even with `devices: - /dev/dri` (host-dependent), so
/// `fs::canonicalize` can fail because the by-path symlink itself isn't visible,
/// not because the render node is missing. On that failure, fall back to a sysfs
/// PCI-address walk (`crate::capacity::renderd_node_for_pci_addr`) keyed on the
/// address embedded in the by-path filename, since `/sys/class/drm` is always
/// present. Only when neither resolves does the raw value survive (logged at
/// warn, not fatal — the encoder build fails loudly downstream if it's bad).
pub(crate) fn canonicalize_render_node(raw: &str) -> String {
    if !raw.starts_with("/dev/dri/") {
        return raw.to_string();
    }
    match std::fs::canonicalize(raw) {
        Ok(canon) => {
            let canon = canon.to_string_lossy().into_owned();
            if canon != raw {
                tracing::info!("render_node {raw} -> {canon}");
            }
            canon
        }
        Err(e) => {
            if let Some(pci_addr) = parse_by_path_pci_addr(raw) {
                // host-observability-2: the sysfs PCI-address walk is shared with
                // capacity::detect_gpus' render_node reporting — see
                // crate::capacity::renderd_node_for_pci_addr.
                if let Some(resolved) = crate::capacity::renderd_node_for_pci_addr(&pci_addr) {
                    tracing::info!(
                        "render_node {raw} -> {resolved} (sysfs pci-address fallback; \
                         by-path canonicalize failed: {e})"
                    );
                    return resolved;
                }
            }
            tracing::warn!(
                token = "render-node-canonicalize-failed",
                "render_node {raw}: canonicalize failed ({e}); using raw value"
            );
            raw.to_string()
        }
    }
}

/// Extract the PCI address from a `/dev/dri/by-path/pci-<addr>-render` filename
/// (e.g. `pci-0000:04:00.0-render` -> `0000:04:00.0`). Only the `-render` suffix
/// matches — the sibling `-card` node is the DRM master device, not usable for
/// VA encode, so a `-card` value must NOT resolve via this path.
fn parse_by_path_pci_addr(raw: &str) -> Option<String> {
    let filename = std::path::Path::new(raw).file_name()?.to_str()?;
    let addr = filename.strip_prefix("pci-")?.strip_suffix("-render")?;
    (!addr.is_empty()).then(|| addr.to_string())
}

impl RuntimeSettings {
    /// Resolve from the environment (the historical defaults). This is the agent's
    /// starting point before any `config_update` arrives.
    pub fn baseline() -> Self {
        let render_node_configured =
            std::env::var("QUASAR_RENDER_NODE").unwrap_or_else(|_| "software".into());
        RuntimeSettings {
            encoder: resolve_encoder_choice(),
            render_node: canonicalize_render_node(&render_node_configured),
            render_node_configured,
            cuda_device_id: std::env::var("QUASAR_CUDA_DEVICE")
                .ok()
                .and_then(|s| s.parse().ok())
                .filter(|&n| n >= 0)
                .unwrap_or(0),
            gop: env_pos_u32("QUASAR_GOP", 60),
            num_slices: env_pos_u32("QUASAR_SLICES", 8),
            target_usage: env_pos_u32("QUASAR_TARGET_USAGE", 6),
            queue_buffers: env_pos_u32("QUASAR_QUEUE_BUFFERS", 3),
            zerocopy: env_bool("QUASAR_ZEROCOPY"),
            latency_probe: env_bool("QUASAR_LATENCY_PROBE"),
            idle_timeout_secs: std::env::var("QUASAR_IDLE_TIMEOUT_SECS")
                .ok()
                .and_then(|s| s.parse().ok())
                .unwrap_or(120),
            // SPT-02: ABR mode — resolved from QUASAR_ABR_MODE, legacy QUASAR_ABR /
            // QUASAR_ABR_DISABLED, then default Protective. See AbrMode::from_env().
            abr_mode: AbrMode::from_env(),
            abr_floor_kbps: std::env::var("QUASAR_ABR_FLOOR_KBPS")
                .ok()
                .and_then(|s| s.parse::<u32>().ok())
                .filter(|&n| n > 0),
            abr_floor_ratio: std::env::var("QUASAR_ABR_FLOOR_RATIO")
                .ok()
                .and_then(|s| s.parse::<f64>().ok())
                .filter(|n| n.is_finite() && *n > 0.0)
                .unwrap_or(0.3),
            // storage-config (#377): the localDriver home root. Empty default
            // (no env) ⇒ measurement disabled. Stored verbatim (see field doc).
            home_root: std::env::var("QUASAR_HOME_ROOT").unwrap_or_default(),
            // #375: 32-bit NVIDIA driver-lib dir. Empty default ⇒ no explicit
            // override; the agent's startup probe seeds the auto-detected value.
            nvidia_lib32_path: std::env::var("QUASAR_NV_LIB32_PATH").unwrap_or_default(),
            ladder: super::ladder::LadderSettings::from_env(),
            abr_governor: super::abr::AbrGovernorSettings::from_env(),
        }
    }

    /// Overlay a control-plane resolved-settings JSON object. Unknown keys and
    /// type-mismatched values are ignored (the control plane already validated;
    /// this is defensive). Each key maps to one field.
    pub fn apply_json(&mut self, v: &serde_json::Value) {
        if let Some(x) = v.get("encoder").and_then(|x| x.as_str()) {
            self.encoder = parse_encoder(x);
        }
        if let Some(x) = v.get("render_node").and_then(|x| x.as_str()) {
            self.render_node = canonicalize_render_node(x);
            self.render_node_configured = x.to_string();
        }
        if let Some(x) = v.get("cuda_device").and_then(|x| x.as_i64()) {
            self.cuda_device_id = x as i32;
        }
        if let Some(x) = v.get("gop").and_then(|x| x.as_u64()) {
            self.gop = x as u32;
        }
        if let Some(x) = v.get("slices").and_then(|x| x.as_u64()) {
            self.num_slices = x as u32;
        }
        if let Some(x) = v.get("target_usage").and_then(|x| x.as_u64()) {
            self.target_usage = x as u32;
        }
        if let Some(x) = v.get("queue_buffers").and_then(|x| x.as_u64()) {
            self.queue_buffers = x as u32;
        }
        if let Some(x) = v.get("latency_probe").and_then(|x| x.as_bool()) {
            self.latency_probe = x;
        }
        if let Some(x) = v.get("zerocopy").and_then(|x| x.as_bool()) {
            self.zerocopy = x;
        }
        if let Some(x) = v.get("idle_timeout_secs").and_then(|x| x.as_u64()) {
            self.idle_timeout_secs = x;
        }
        // `abr_enabled` (bool) is the DEPRECATED lossy projection kept for older
        // control planes: `false` forces `Off`; `true` must NOT force `Protective`
        // — a stored `abr_enabled=true` override must never silently downgrade a
        // `Smooth` host and drop the whole ladder, so from `Off` it restores the
        // env-baseline mode (or `Smooth` if that baseline was itself `Off`), else
        // it leaves the current mode untouched. `abr_mode` (3-way) is authoritative
        // and applied AFTER this, so a push carrying both lands on the explicit mode.
        if let Some(x) = v.get("abr_enabled").and_then(|x| x.as_bool()) {
            if x {
                if self.abr_mode == AbrMode::Off {
                    let baseline = AbrMode::from_env();
                    self.abr_mode = if baseline == AbrMode::Off {
                        AbrMode::Smooth
                    } else {
                        baseline
                    };
                }
            } else {
                self.abr_mode = AbrMode::Off;
            }
        }
        if let Some(x) = v.get("abr_mode").and_then(|x| x.as_str()) {
            match AbrMode::parse_env(x) {
                Some(m) => self.abr_mode = m,
                None => tracing::warn!(
                    token = "config-update-abr-mode-invalid",
                    "config_update abr_mode={x:?} is not protective|off|smooth; ignored"
                ),
            }
        }
        self.ladder.apply_json(v);
        self.abr_governor.apply_json(v);
        // abr_floor_kbps is nullable: an explicit null clears it.
        match v.get("abr_floor_kbps") {
            Some(serde_json::Value::Null) => self.abr_floor_kbps = None,
            Some(x) => {
                if let Some(n) = x.as_u64().filter(|&n| n > 0 && n <= u32::MAX as u64) {
                    self.abr_floor_kbps = Some(n as u32);
                }
            }
            None => {}
        }
        if let Some(x) = v.get("abr_floor_ratio").and_then(|x| x.as_f64()) {
            if x.is_finite() && x > 0.0 {
                self.abr_floor_ratio = x;
            }
        }
        // storage-config (#377): home_root is a sparse string override onto the
        // env baseline (live-class). Stored verbatim; validated at measurement.
        if let Some(x) = v.get("home_root").and_then(|x| x.as_str()) {
            self.home_root = x.to_string();
        }
        // #375: nvidia_lib32_path is a sparse string override onto the env
        // baseline (live-class). Stored verbatim; the mount is NVIDIA-gated.
        if let Some(x) = v.get("nvidia_lib32_path").and_then(|x| x.as_str()) {
            self.nvidia_lib32_path = x.to_string();
        }
    }

    /// host-observability: the agent's resolved settings, stringified, keyed by
    /// the `hostcfg` catalog's knob names (`control-plane/internal/hostcfg/catalog.go`)
    /// so the admin UI can join this against `resolved`/the catalog defaults.
    /// Reports `RuntimeSettings` as-is (the pre-latch view), not what a running
    /// session's pipeline actually bound: restart-class knobs
    /// (`encoder`/`render_node`/`cuda_device`) are read once at first `gst::init`
    /// (a process-wide `Once`), which `RuntimeSettings` already reflects for a
    /// process that has run a session. A `config_update` changing a restart-class
    /// knob updates this map immediately (surfacing the pending-restart
    /// discrepancy against `resolved`) even though the running process keeps
    /// encoding with the old latched value until restarted.
    ///
    /// `render_node` is reported **as configured** (`render_node_configured`),
    /// NOT canonicalized (agent-api.md `effective_settings`) — `effective` must
    /// stay directly comparable with the admin API's `resolved`/`overrides` views
    /// (same value space: whatever the env or override literally said). Reporting
    /// the canonical `renderD*` form here would make a `by-path` override never
    /// equal `resolved`, showing a permanent false "restart pending" in the UI.
    /// The canonical `render_node` field is still what pipeline/VA element pinning
    /// uses — this method just never surfaces it.
    ///
    /// `abr_enabled` is a lossy projection of the 3-way `abr_mode` (the catalog
    /// only models a bool): `true` for `Protective`/`Smooth`, `false` for `Off`.
    /// `Smooth` isn't distinguishable from `Protective` in this map — the catalog
    /// has no key for it yet.
    pub fn effective_map(&self) -> std::collections::BTreeMap<String, String> {
        let mut m = std::collections::BTreeMap::new();
        m.insert(
            "abr_enabled".to_string(),
            (self.abr_mode != AbrMode::Off).to_string(),
        );
        // SPT-08/D5: the real 3-way mode. `abr_enabled` above stays for the legacy
        // catalog row (true for Protective/Smooth) — it is no longer the only view.
        m.insert("abr_mode".to_string(), self.abr_mode.as_str().to_string());
        self.ladder.write_effective(&mut m);
        self.abr_governor.write_effective(&mut m);
        if let Some(floor) = self.abr_floor_kbps {
            m.insert("abr_floor_kbps".to_string(), floor.to_string());
        }
        m.insert(
            "abr_floor_ratio".to_string(),
            self.abr_floor_ratio.to_string(),
        );
        m.insert("gop".to_string(), self.gop.to_string());
        m.insert("slices".to_string(), self.num_slices.to_string());
        m.insert("target_usage".to_string(), self.target_usage.to_string());
        m.insert("queue_buffers".to_string(), self.queue_buffers.to_string());
        m.insert("zerocopy".to_string(), self.zerocopy.to_string());
        m.insert("latency_probe".to_string(), self.latency_probe.to_string());
        m.insert(
            "idle_timeout_secs".to_string(),
            self.idle_timeout_secs.to_string(),
        );
        m.insert("encoder".to_string(), encoder_str(self.encoder).to_string());
        m.insert(
            "render_node".to_string(),
            self.render_node_configured.clone(),
        );
        m.insert("cuda_device".to_string(), self.cuda_device_id.to_string());
        // storage-config (#377): report home_root verbatim (empty when unset) —
        // the control plane reads it back for per-host managed-home resolution.
        m.insert("home_root".to_string(), self.home_root.clone());
        // #375: report the resolved 32-bit NVIDIA lib dir (env/config_update
        // override, else the startup-probed value the agent seeds here). Surfaces
        // in host observability so an operator can see the auto-detected path.
        m.insert(
            "nvidia_lib32_path".to_string(),
            self.nvidia_lib32_path.clone(),
        );
        m
    }
}

/// The hostcfg catalog's `encoder` enum string for an `EncoderChoice`.
fn encoder_str(e: EncoderChoice) -> &'static str {
    match e {
        EncoderChoice::Openh264 => "openh264",
        EncoderChoice::Va => "va",
        EncoderChoice::Nvenc => "nvenc",
        EncoderChoice::Vulkan => "vulkan",
    }
}

fn env_pos_u32(var: &str, default: u32) -> u32 {
    std::env::var(var)
        .ok()
        .and_then(|s| s.parse::<i64>().ok())
        .filter(|&n| n > 0)
        .map(|n| n as u32)
        .unwrap_or(default)
}

fn env_bool(var: &str) -> bool {
    matches!(
        std::env::var(var).ok().as_deref(),
        Some("1") | Some("true") | Some("TRUE")
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn resolve_explicit_env_wins_over_detection() {
        let detected = Some((GpuVendor::Nvidia, DetectSource::NvidiaDev));
        for (raw, want) in [
            ("va", EncoderChoice::Va),
            ("vaapi", EncoderChoice::Va),
            ("nvenc", EncoderChoice::Nvenc),
            ("nvidia", EncoderChoice::Nvenc),
            ("VULKAN", EncoderChoice::Vulkan),
            ("openh264", EncoderChoice::Openh264),
        ] {
            assert_eq!(
                resolve_encoder_from(raw, detected),
                (want, EncoderReason::Env),
                "raw={raw}"
            );
        }
    }

    #[test]
    fn resolve_unrecognized_env_keeps_openh264_fallback() {
        assert_eq!(
            resolve_encoder_from("quicksync", None),
            (EncoderChoice::Openh264, EncoderReason::EnvUnrecognized)
        );
        // Detection must not override an explicit (if bogus) value.
        assert_eq!(
            resolve_encoder_from("bogus", Some((GpuVendor::Amd, DetectSource::DriScan))),
            (EncoderChoice::Openh264, EncoderReason::EnvUnrecognized)
        );
    }

    #[test]
    fn resolve_empty_env_maps_detected_vendor() {
        for raw in ["", "  "] {
            assert_eq!(
                resolve_encoder_from(raw, Some((GpuVendor::Nvidia, DetectSource::NvidiaDev))),
                (
                    EncoderChoice::Vulkan,
                    EncoderReason::Detected(GpuVendor::Nvidia, DetectSource::NvidiaDev)
                )
            );
        }
        assert_eq!(
            resolve_encoder_from("", Some((GpuVendor::Amd, DetectSource::DriScan))).0,
            AMD_AUTO_DEFAULT
        );
        assert_eq!(
            resolve_encoder_from("", Some((GpuVendor::Intel, DetectSource::RenderNode))).0,
            EncoderChoice::Va
        );
        assert_eq!(
            resolve_encoder_from("", None),
            (EncoderChoice::Openh264, EncoderReason::NoGpu)
        );
    }

    #[test]
    fn vendor_default_mapping() {
        assert_eq!(
            encoder_default_for_vendor(Some(GpuVendor::Nvidia)),
            EncoderChoice::Vulkan
        );
        assert_eq!(
            encoder_default_for_vendor(Some(GpuVendor::Amd)),
            AMD_AUTO_DEFAULT
        );
        assert_eq!(
            encoder_default_for_vendor(Some(GpuVendor::Intel)),
            EncoderChoice::Va
        );
        assert_eq!(encoder_default_for_vendor(None), EncoderChoice::Openh264);
    }

    #[test]
    fn default_matches_env_baseline() {
        let s = RuntimeSettings::baseline();
        assert_eq!(s.gop, 60);
        assert_eq!(s.target_usage, 6);
        assert!((s.abr_floor_ratio - 0.3).abs() < 1e-9);
        assert_eq!(s.encoder, EncoderChoice::Openh264);
        assert_eq!(s.render_node, "software");
        assert_eq!(s.render_node_configured, "software");
        assert_eq!(s.abr_mode, AbrMode::Smooth); // SPT-10: smooth is the default (QUASAR_ABR_MODE unset)
        assert_eq!(s.abr_floor_kbps, None);
    }

    #[test]
    fn apply_overlays_known_keys() {
        let mut s = RuntimeSettings::baseline();
        let json = serde_json::json!({
            "gop": 120, "abr_enabled": true, "encoder": "va",
            "abr_floor_kbps": 2500, "abr_floor_ratio": 0.5, "idle_timeout_secs": 0
        });
        s.apply_json(&json);
        assert_eq!(s.gop, 120);
        // abr_enabled:true defers to abr_mode; the baseline mode (Smooth) is
        // already non-Off, so it is left unchanged, not forced to Protective.
        assert_eq!(s.abr_mode, AbrMode::Smooth);
        assert_eq!(s.encoder, EncoderChoice::Va);
        assert_eq!(s.abr_floor_kbps, Some(2500));
        assert_eq!(s.idle_timeout_secs, 0);
    }

    // storage-config (#377): home_root overlays as a sparse string key exactly
    // like the other string knobs, verbatim, and shows up in effective_map.
    #[test]
    fn apply_overlays_home_root() {
        let mut s = RuntimeSettings::baseline();
        s.apply_json(&serde_json::json!({ "home_root": "/data/homes" }));
        assert_eq!(s.home_root, "/data/homes");
        assert_eq!(
            s.effective_map().get("home_root").map(String::as_str),
            Some("/data/homes")
        );
    }

    // A push that carries no home_root key must leave the baseline untouched
    // (sparse overlay — absent key ⇒ keep prior).
    #[test]
    fn apply_sparse_push_preserves_home_root() {
        let mut s = RuntimeSettings::baseline();
        s.home_root = "/data/homes".to_string();
        s.apply_json(&serde_json::json!({ "gop": 90 }));
        assert_eq!(s.home_root, "/data/homes");
    }

    // effective_map always reports home_root (empty string when unset) — the
    // control plane reads it back for per-host resolution, so the key is
    // load-bearing and must be present even in the default case.
    #[test]
    fn effective_map_reports_empty_home_root_by_default() {
        let s = RuntimeSettings::baseline();
        assert_eq!(
            s.effective_map().get("home_root").map(String::as_str),
            Some("")
        );
    }

    // #375: nvidia_lib32_path overlays as a sparse string key exactly like
    // home_root, verbatim, and shows up in effective_map.
    #[test]
    fn apply_overlays_nvidia_lib32_path() {
        let mut s = RuntimeSettings::baseline();
        s.apply_json(&serde_json::json!({ "nvidia_lib32_path": "/usr/lib" }));
        assert_eq!(s.nvidia_lib32_path, "/usr/lib");
        assert_eq!(
            s.effective_map()
                .get("nvidia_lib32_path")
                .map(String::as_str),
            Some("/usr/lib")
        );
    }

    // A push carrying no nvidia_lib32_path key leaves the prior value untouched
    // (sparse overlay — absent key ⇒ keep prior).
    #[test]
    fn apply_sparse_push_preserves_nvidia_lib32_path() {
        let mut s = RuntimeSettings::baseline();
        s.nvidia_lib32_path = "/usr/lib".to_string();
        s.apply_json(&serde_json::json!({ "gop": 90 }));
        assert_eq!(s.nvidia_lib32_path, "/usr/lib");
    }

    // effective_map always reports nvidia_lib32_path (empty string when unset),
    // so host observability shows the resolved value even in the default case.
    #[test]
    fn effective_map_reports_empty_nvidia_lib32_path_by_default() {
        let s = RuntimeSettings::baseline();
        assert_eq!(
            s.effective_map()
                .get("nvidia_lib32_path")
                .map(String::as_str),
            Some("")
        );
    }

    // VK-05: "vulkan" resolves to EncoderChoice::Vulkan via the same config_update/
    // RuntimeSettings overlay path as "va"/"nvenc" above.
    #[test]
    fn apply_overlay_encoder_vulkan() {
        let mut s = RuntimeSettings::baseline();
        s.apply_json(&serde_json::json!({ "encoder": "vulkan" }));
        assert_eq!(s.encoder, EncoderChoice::Vulkan);
    }

    #[test]
    fn null_clears_abr_floor_kbps() {
        let mut s = RuntimeSettings::baseline();
        s.abr_floor_kbps = Some(2500);
        s.apply_json(&serde_json::json!({ "abr_floor_kbps": null }));
        assert_eq!(s.abr_floor_kbps, None);
    }

    #[test]
    fn apply_ignores_unknown_and_keeps_prior() {
        let mut s = RuntimeSettings::baseline();
        s.gop = 90;
        s.apply_json(&serde_json::json!({ "bogus": 1 }));
        assert_eq!(s.gop, 90);
    }

    #[test]
    fn parse_by_path_pci_addr_matches_render_only() {
        assert_eq!(
            parse_by_path_pci_addr("/dev/dri/by-path/pci-0000:04:00.0-render"),
            Some("0000:04:00.0".to_string())
        );
        // The `-card` sibling is the DRM master node, not usable for VA encode —
        // must NOT match.
        assert_eq!(
            parse_by_path_pci_addr("/dev/dri/by-path/pci-0000:04:00.0-card"),
            None
        );
        assert_eq!(parse_by_path_pci_addr("/dev/dri/renderD128"), None);
    }

    #[test]
    fn canonicalize_render_node_passes_through_non_dri() {
        assert_eq!(canonicalize_render_node("software"), "software");
        assert_eq!(
            canonicalize_render_node("/some/other/path"),
            "/some/other/path"
        );
    }

    #[test]
    fn canonicalize_render_node_gates_on_dev_dri_prefix() {
        // Nonexistent /dev/dri path with a name that doesn't match the by-path
        // pci-<addr>-render pattern: canonicalize fails, no sysfs fallback
        // candidate, raw value kept.
        assert_eq!(
            canonicalize_render_node("/dev/dri/renderD9999-does-not-exist"),
            "/dev/dri/renderD9999-does-not-exist"
        );
    }

    // ── D5: abr_mode is now a first-class hostcfg key ────────────────────────
    // The lossy `abr_enabled` bool could never select `smooth`; `abr_mode` can, and
    // it WINS when both are present in one push.
    #[test]
    fn apply_json_abr_mode_selects_smooth() {
        let mut s = RuntimeSettings::baseline();
        s.apply_json(&serde_json::json!({ "abr_mode": "smooth" }));
        assert_eq!(s.abr_mode, AbrMode::Smooth);
        s.apply_json(&serde_json::json!({ "abr_mode": "protective" }));
        assert_eq!(s.abr_mode, AbrMode::Protective);
        s.apply_json(&serde_json::json!({ "abr_mode": "off" }));
        assert_eq!(s.abr_mode, AbrMode::Off);
    }

    #[test]
    fn abr_mode_wins_over_the_deprecated_abr_enabled() {
        let mut s = RuntimeSettings::baseline();
        s.apply_json(&serde_json::json!({ "abr_enabled": true, "abr_mode": "smooth" }));
        assert_eq!(s.abr_mode, AbrMode::Smooth, "abr_mode is authoritative");
        // abr_enabled=false with no abr_mode still means Off (back-compat).
        let mut s2 = RuntimeSettings::baseline();
        s2.apply_json(&serde_json::json!({ "abr_enabled": false }));
        assert_eq!(s2.abr_mode, AbrMode::Off);
    }

    // `abr_enabled:true` must defer to the current/env-baseline mode, never force
    // Protective — must not silently downgrade a Smooth host and drop the ladder.
    #[test]
    fn abr_enabled_true_on_a_smooth_baseline_stays_smooth() {
        let mut s = RuntimeSettings::baseline(); // QUASAR_ABR_MODE unset ⇒ Smooth
        assert_eq!(s.abr_mode, AbrMode::Smooth);
        s.apply_json(&serde_json::json!({ "abr_enabled": true }));
        assert_eq!(
            s.abr_mode,
            AbrMode::Smooth,
            "true must not force Protective"
        );
    }

    #[test]
    fn abr_enabled_false_then_true_returns_to_the_baseline_mode() {
        let mut s = RuntimeSettings::baseline(); // baseline = Smooth
        s.apply_json(&serde_json::json!({ "abr_enabled": false }));
        assert_eq!(s.abr_mode, AbrMode::Off);
        s.apply_json(&serde_json::json!({ "abr_enabled": true }));
        assert_eq!(
            s.abr_mode,
            AbrMode::Smooth,
            "re-enabling from Off must restore the env-baseline mode"
        );
    }

    #[test]
    fn abr_enabled_true_with_explicit_abr_mode_uses_the_explicit_mode() {
        let mut s = RuntimeSettings::baseline();
        s.apply_json(&serde_json::json!({ "abr_enabled": true, "abr_mode": "protective" }));
        assert_eq!(s.abr_mode, AbrMode::Protective);
    }

    #[test]
    fn apply_json_ignores_a_junk_abr_mode() {
        let mut s = RuntimeSettings::baseline();
        s.abr_mode = AbrMode::Smooth;
        s.apply_json(&serde_json::json!({ "abr_mode": "bogus" }));
        assert_eq!(s.abr_mode, AbrMode::Smooth, "junk must not change the mode");
    }

    // effective_map reports the real 3-way mode AND keeps the legacy bool so the
    // admin UI's existing abr_enabled row does not go blank.
    #[test]
    fn effective_map_reports_abr_mode_and_the_legacy_bool() {
        let mut s = RuntimeSettings::baseline();
        s.abr_mode = AbrMode::Smooth;
        let m = s.effective_map();
        assert_eq!(m.get("abr_mode").map(String::as_str), Some("smooth"));
        assert_eq!(m.get("abr_enabled").map(String::as_str), Some("true"));
    }

    // ── D5: every ladder knob round-trips through a sparse push ──────────────
    #[test]
    fn apply_json_overlays_every_ladder_knob() {
        let mut s = RuntimeSettings::baseline();
        s.apply_json(&serde_json::json!({
            "abr_ladder": false,
            "abr_ladder_max_bias": 3,
            "abr_ladder_engage_dwell": 5,
            "abr_ladder_recover_dwell": 7,
            "abr_ladder_resolution": true,
            "abr_ladder_res_exponent": 0.8,
            "abr_ladder_res_engage_frac": 0.55,
            "abr_ladder_res_recover_frac": 0.85,
            "abr_ladder_res_engage_dwell": 3,
            "abr_ladder_res_recover_dwell": 6,
            "abr_ladder_res_min_step_s": 20,
            "abr_ladder_res_min_height": 1080,
            "abr_ladder_fps": true,
            "abr_ladder_order": "res_first"
        }));
        let l = s.ladder;
        assert!(!l.enabled);
        assert_eq!(l.max_bias, 3);
        assert_eq!(l.engage_dwell, 5);
        assert_eq!(l.recover_dwell, 7);
        assert!(l.resolution_enabled);
        assert!((l.res.exponent - 0.8).abs() < 1e-9);
        assert!((l.res.engage_frac - 0.55).abs() < 1e-9);
        assert!((l.res.recover_frac - 0.85).abs() < 1e-9);
        assert_eq!(l.res.engage_dwell, 3);
        assert_eq!(l.res.recover_dwell, 6);
        assert_eq!(l.res.min_step_s, 20);
        assert_eq!(l.res.min_height, 1080);
        assert!(l.fps_enabled);
        assert_eq!(l.order, crate::session::ladder::LadderOrder::ResFirst);
    }

    #[test]
    fn ladder_defaults_are_the_ship_dark_posture() {
        let l = RuntimeSettings::baseline().ladder;
        assert!(l.enabled, "the speed-bias ladder is on by default");
        assert!(!l.resolution_enabled, "the resolution rung SHIPS DARK");
        assert!(!l.fps_enabled, "the fps rung SHIPS DARK");
        assert_eq!(l.order, crate::session::ladder::LadderOrder::Hybrid);
        assert_eq!(l.max_bias, 2);
        assert!((l.res.exponent - 0.75).abs() < 1e-9);
        assert_eq!(l.res.min_height, 720);
    }

    #[test]
    fn a_sparse_push_preserves_untouched_ladder_knobs() {
        let mut s = RuntimeSettings::baseline();
        s.ladder.resolution_enabled = true;
        s.apply_json(&serde_json::json!({ "gop": 90 }));
        assert!(s.ladder.resolution_enabled);
    }

    #[test]
    fn effective_map_reports_every_ladder_knob() {
        let s = RuntimeSettings::baseline();
        let m = s.effective_map();
        for key in [
            "abr_ladder",
            "abr_ladder_max_bias",
            "abr_ladder_engage_dwell",
            "abr_ladder_recover_dwell",
            "abr_ladder_resolution",
            "abr_ladder_res_exponent",
            "abr_ladder_res_engage_frac",
            "abr_ladder_res_recover_frac",
            "abr_ladder_res_engage_dwell",
            "abr_ladder_res_recover_dwell",
            "abr_ladder_res_min_step_s",
            "abr_ladder_res_min_height",
            "abr_ladder_fps",
            "abr_ladder_order",
        ] {
            assert!(m.contains_key(key), "effective_map is missing {key}");
        }
        assert_eq!(
            m.get("abr_ladder_resolution").map(String::as_str),
            Some("false")
        );
        assert_eq!(
            m.get("abr_ladder_order").map(String::as_str),
            Some("hybrid")
        );
    }

    #[test]
    fn effective_map_contains_encoder_and_render_node() {
        let s = RuntimeSettings::baseline();
        let m = s.effective_map();
        assert_eq!(m.get("encoder").map(String::as_str), Some("openh264"));
        assert_eq!(m.get("render_node").map(String::as_str), Some("software"));
        assert_eq!(m.get("abr_enabled").map(String::as_str), Some("true")); // Smooth != Off
        assert!(m.contains_key("gop"));
        assert!(m.contains_key("cuda_device"));
    }

    #[test]
    fn apply_json_tracks_render_node_configured_verbatim() {
        // A path that won't canonicalize on this machine (no /dev/dri, no
        // matching sysfs render entry) — render_node (canonical) falls back to
        // the raw value too in that failure case, but render_node_configured
        // must ALWAYS be the verbatim override regardless of what canonicalize
        // did or didn't do.
        let mut s = RuntimeSettings::baseline();
        s.apply_json(&serde_json::json!({
            "render_node": "/dev/dri/by-path/pci-0000:04:00.0-render"
        }));
        assert_eq!(
            s.render_node_configured,
            "/dev/dri/by-path/pci-0000:04:00.0-render"
        );
    }

    #[test]
    fn effective_map_reports_configured_render_node_not_canonical() {
        // host-observability-2: simulate the case that matters — a by-path
        // override whose canonicalization actually resolved to a renderD*
        // target (the normal Tower-side outcome). effective_map() must report
        // the CONFIGURED (raw) value, not the canonical one, so it stays
        // comparable to the admin UI's resolved/overrides view.
        let mut s = RuntimeSettings::baseline();
        s.render_node = "/dev/dri/renderD128".to_string(); // what pipeline/VA pinning uses
        s.render_node_configured = "/dev/dri/by-path/pci-0000:04:00.0-render".to_string();

        let m = s.effective_map();
        assert_eq!(
            m.get("render_node").map(String::as_str),
            Some("/dev/dri/by-path/pci-0000:04:00.0-render")
        );
        // The canonical form must NOT leak into effective_map.
        assert_ne!(
            m.get("render_node").map(String::as_str),
            Some(s.render_node.as_str())
        );
    }
}
