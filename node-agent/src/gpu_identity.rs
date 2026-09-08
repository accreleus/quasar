//! Per-GPU driver identity — the opaque fingerprint of the encode stack a GPU is
//! running, reported as `capacity.gpus[].driver_identity` (agent-api.md, #144). A
//! certification measures one silicon + driver + encode-stack combination, so the
//! control plane refuses a stored measurement whose identity is not this one.
//!
//! Resolved by fallback, because vendors expose driver versions differently:
//!
//! 1. NVIDIA — the loaded kernel module's version, the same source
//!    `encoder_compatibility` reads. A file read: no subprocess, nothing that can hang.
//! 2. Any vendor — Vulkan `VK_KHR_driver_properties` (`driverName` + `driverInfo`) out
//!    of `vulkaninfo --summary`, which covers RADV/AMDVLK/ANV uniformly and names the
//!    userspace stack (`Mesa 25.3.6`) a VA/Vulkan encoder's timings depend on. The agent
//!    image carries `vulkan-tools`.
//! 3. Neither — no identity. Matching then fails open: every stored row stays eligible.
//!
//! No sysfs/DRM-module fallback below those: a module name does not change when Mesa is
//! upgraded, so it would report "unchanged" across the change this exists to notice.
//!
//! `capacity::detect` runs on the agent's select loop, so the `vulkaninfo` child runs at
//! most once per process and only when rung 1 answered for no GPU. A driver change means
//! recreating the agent container (the driver volume binds at container start), so the
//! memo cannot outlive the stack it describes.

use std::path::Path;
use std::sync::OnceLock;
use std::time::Duration;

/// Tracing target for this module.
const T: &str = "quasar.gpu_identity";

/// `vulkaninfo` enumerates every physical device and exits; it is not a long-running
/// probe. Bounded anyway, because a wedged ICD would otherwise stall capacity detection
/// on the agent's select loop.
const VULKANINFO_TIMEOUT: Duration = Duration::from_secs(10);

/// Identities are compared for equality by the control plane and stored on every
/// certification row, so they are kept short. Longer than any real
/// `driverName`/`driverInfo` pair, short enough that a malformed one cannot bloat a row.
const MAX_LEN: usize = 96;

/// One physical device's Vulkan driver properties, as reported by `vulkaninfo --summary`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VulkanDriver {
    pub vendor_id: u32,
    pub device_id: u32,
    /// `VkPhysicalDeviceDriverProperties::driverName`, e.g. `radv`, `NVIDIA`.
    pub driver_name: String,
    /// `VkPhysicalDeviceDriverProperties::driverInfo`, e.g. `Mesa 25.3.6`, `595.99.02`.
    pub driver_info: String,
}

impl VulkanDriver {
    fn identity(&self) -> Option<String> {
        let name = sanitize(&self.driver_name);
        let info = sanitize(&self.driver_info);
        match (name.is_empty(), info.is_empty()) {
            (true, true) => None,
            (true, false) => Some(truncate(format!("vk:{info}"))),
            (false, true) => Some(truncate(format!("vk:{name}"))),
            (false, false) => Some(truncate(format!("vk:{name}:{info}"))),
        }
    }
}

/// The resolved sources for one capacity detection. Built once per `detect()` and asked
/// per GPU, so a multi-GPU host reads `/sys/module/nvidia/version` once, not per card.
pub struct DriverIdentities {
    nvidia_driver: Option<String>,
    /// Injected by tests; production reads the memoised `vulkaninfo` probe lazily, so a
    /// host whose GPUs all resolve at rung 1 never spawns the child.
    vulkan: Option<Vec<VulkanDriver>>,
}

impl DriverIdentities {
    /// Production constructor. `root` is the filesystem root the NVIDIA module version is
    /// read under (`/`), matching [`crate::encoder_compatibility::inspect`].
    pub fn detect(root: &Path) -> Self {
        Self {
            nvidia_driver: crate::nvidia_volume::kernel_driver_version(root),
            vulkan: None,
        }
    }

    /// Test constructor: both rungs supplied, nothing read and nothing spawned.
    #[cfg(test)]
    pub(crate) fn with(nvidia_driver: Option<&str>, vulkan: Vec<VulkanDriver>) -> Self {
        Self {
            nvidia_driver: nvidia_driver.map(str::to_string),
            vulkan: Some(vulkan),
        }
    }

    /// This GPU's identity, or `None` when no rung of the ladder can name one — in which
    /// case the field is omitted from the report and matching fails open.
    ///
    /// `vendor` is the agent's own vocabulary (`nvidia`/`amd`/`intel`); `vendor_id` and
    /// `device_id` are the sysfs PCI ids, which are what a Vulkan device is matched on.
    /// Two identical GPUs share a PCI device id and therefore an identity, which is
    /// correct: they run the same driver.
    pub fn for_gpu(
        &self,
        vendor: &str,
        vendor_id: Option<u32>,
        device_id: Option<u32>,
    ) -> Option<String> {
        if vendor == "nvidia" {
            if let Some(v) = &self.nvidia_driver {
                return Some(truncate(format!("nvidia:{}", sanitize(v))));
            }
        }
        let (vendor_id, device_id) = (vendor_id?, device_id?);
        self.vulkan_drivers()
            .iter()
            .find(|d| d.vendor_id == vendor_id && d.device_id == device_id)
            .and_then(VulkanDriver::identity)
    }

    fn vulkan_drivers(&self) -> &[VulkanDriver] {
        match &self.vulkan {
            Some(v) => v,
            None => vulkan_drivers_memoised(),
        }
    }
}

/// The `vulkaninfo --summary` probe, run at most once per process (see the module doc).
fn vulkan_drivers_memoised() -> &'static [VulkanDriver] {
    static CACHE: OnceLock<Vec<VulkanDriver>> = OnceLock::new();
    CACHE.get_or_init(|| {
        let Some((stdout, status)) =
            crate::vram::run_bounded("vulkaninfo", &["--summary"], VULKANINFO_TIMEOUT)
        else {
            tracing::debug!(
                target: T,
                "vulkaninfo did not run (absent, or exceeded its deadline); GPUs with no other \
                 identity source report none"
            );
            return Vec::new();
        };
        if !status.success() {
            tracing::debug!(target: T, code = ?status.code(), "vulkaninfo exited non-zero; no Vulkan driver identities");
            return Vec::new();
        }
        let drivers = parse_vulkan_summary(&stdout);
        tracing::info!(
            target: T,
            token = "gpu-driver-identity-vulkan",
            devices = drivers.len(),
            "read Vulkan driver properties for driver identity"
        );
        drivers
    })
}

/// Parse the `Devices:` block of `vulkaninfo --summary`.
///
/// The block is one `GPUn:` header per physical device followed by indented `key = value`
/// lines, of which four matter: `vendorID`, `deviceID`, `driverName`, `driverInfo`. Keys
/// this does not know are skipped, and a device missing an id is dropped rather than
/// guessed at — an unmatched device simply has no identity.
pub fn parse_vulkan_summary(out: &str) -> Vec<VulkanDriver> {
    #[derive(Default)]
    struct Partial {
        vendor_id: Option<u32>,
        device_id: Option<u32>,
        driver_name: String,
        driver_info: String,
    }
    impl Partial {
        fn finish(self) -> Option<VulkanDriver> {
            Some(VulkanDriver {
                vendor_id: self.vendor_id?,
                device_id: self.device_id?,
                driver_name: self.driver_name,
                driver_info: self.driver_info,
            })
        }
    }

    let mut devices = Vec::new();
    let mut cur: Option<Partial> = None;

    for line in out.lines() {
        let trimmed = line.trim();
        let is_header =
            trimmed.starts_with("GPU") && trimmed.ends_with(':') && !trimmed.contains(' ');
        // A device block ends at the next header, or at the first non-indented line that
        // is not a property — the summary continues with sections that are not device
        // properties.
        if is_header
            || (!trimmed.contains('=') && !trimmed.is_empty() && !line.starts_with([' ', '\t']))
        {
            if let Some(d) = cur.take().and_then(Partial::finish) {
                devices.push(d);
            }
            if is_header {
                cur = Some(Partial::default());
            }
            continue;
        }
        let Some((key, value)) = trimmed.split_once('=') else {
            continue;
        };
        let Some(dev) = cur.as_mut() else { continue };
        let value = value.trim();
        match key.trim() {
            "vendorID" => dev.vendor_id = parse_hex_id(value),
            "deviceID" => dev.device_id = parse_hex_id(value),
            "driverName" => dev.driver_name = value.to_string(),
            "driverInfo" => dev.driver_info = value.to_string(),
            _ => {}
        }
    }
    if let Some(d) = cur.take().and_then(Partial::finish) {
        devices.push(d);
    }
    devices
}

/// Parse a PCI id in either wire form: sysfs writes `0x1002`, `vulkaninfo` writes
/// `0x1002` too, but a decimal form is accepted so a future formatting change is not a
/// silent mismatch.
pub fn parse_hex_id(raw: &str) -> Option<u32> {
    let s = raw.trim();
    if let Some(hex) = s.strip_prefix("0x").or_else(|| s.strip_prefix("0X")) {
        return u32::from_str_radix(hex, 16).ok();
    }
    s.parse::<u32>().ok()
}

/// Collapse whitespace and drop control characters: an identity is compared for equality
/// and stored, so it must not carry a newline or a NUL from a driver string.
fn sanitize(raw: &str) -> String {
    let mut out = String::with_capacity(raw.len());
    let mut pending_space = false;
    for ch in raw.trim().chars() {
        if ch.is_whitespace() {
            pending_space = !out.is_empty();
            continue;
        }
        if ch.is_control() {
            continue;
        }
        if pending_space {
            out.push(' ');
            pending_space = false;
        }
        out.push(ch);
    }
    out
}

/// Bound the stored length on a char boundary.
fn truncate(mut s: String) -> String {
    if s.len() <= MAX_LEN {
        return s;
    }
    let mut end = MAX_LEN;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    s.truncate(end);
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Verbatim `vulkaninfo --summary` output from a Quasar agent container (RADV on an
    /// integrated AMD GPU), loader warnings and all — the parser must survive the noise
    /// that precedes the device block.
    const RADV_SUMMARY: &str = r#"WARNING: [Loader Message] Code 0 : Driver "lvp_icd.x86_64.json" ignored because it was disabled by env var 'VK_LOADER_DRIVERS_DISABLE'
'DISPLAY' environment variable not set... skipping surface info
==========
VULKANINFO
==========

Vulkan Instance Version: 1.4.341

Instance Extensions: count = 25
-------------------------------
VK_EXT_acquire_drm_display             : extension revision 1

Instance Layers: count = 1
--------------------------
VK_LAYER_MESA_device_select Linux device selection layer 1.4.303  version 1

Devices:
========
GPU0:
	apiVersion         = 1.4.328
	driverVersion      = 25.3.6
	vendorID           = 0x1002
	deviceID           = 0x1636
	deviceType         = PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU
	deviceName         = AMD Radeon Graphics (RADV RENOIR)
	driverID           = DRIVER_ID_MESA_RADV
	driverName         = radv
	driverInfo         = Mesa 25.3.6
	conformanceVersion = 1.4.0.0
	deviceUUID         = 00000000-0400-0000-0000-000000000000
	driverUUID         = 414d442d-4d45-5341-2d44-525600000000
"#;

    #[test]
    fn parses_a_real_vulkaninfo_summary() {
        let drivers = parse_vulkan_summary(RADV_SUMMARY);
        assert_eq!(
            drivers,
            vec![VulkanDriver {
                vendor_id: 0x1002,
                device_id: 0x1636,
                driver_name: "radv".to_string(),
                driver_info: "Mesa 25.3.6".to_string(),
            }]
        );
    }

    #[test]
    fn parses_multiple_devices_and_drops_one_missing_its_ids() {
        let out = "Devices:\n========\nGPU0:\n\tvendorID = 0x10de\n\tdeviceID = 0x2b85\n\tdriverName = NVIDIA\n\tdriverInfo = 610.57.04\nGPU1:\n\tdriverName = lavapipe\n\tdriverInfo = Mesa 25.3.6 (LLVM 20)\nGPU2:\n\tvendorID = 0x8086\n\tdeviceID = 0x9a49\n\tdriverName = Intel open-source Mesa driver\n\tdriverInfo = Mesa 25.3.6\n";
        let drivers = parse_vulkan_summary(out);
        assert_eq!(
            drivers.iter().map(|d| d.device_id).collect::<Vec<_>>(),
            vec![0x2b85, 0x9a49],
            "a device with no vendorID/deviceID cannot be matched to a GPU, so it is dropped"
        );
    }

    #[test]
    fn nvidia_takes_the_kernel_module_version_before_vulkan() {
        let id = DriverIdentities::with(
            Some("595.99.02"),
            vec![VulkanDriver {
                vendor_id: 0x10de,
                device_id: 0x2b85,
                driver_name: "NVIDIA".into(),
                driver_info: "595.99.02".into(),
            }],
        );
        assert_eq!(
            id.for_gpu("nvidia", Some(0x10de), Some(0x2b85)),
            Some("nvidia:595.99.02".to_string())
        );
    }

    #[test]
    fn nvidia_falls_through_to_vulkan_when_the_module_version_is_unreadable() {
        let id = DriverIdentities::with(
            None,
            vec![VulkanDriver {
                vendor_id: 0x10de,
                device_id: 0x2b85,
                driver_name: "NVIDIA".into(),
                driver_info: "610.57.04".into(),
            }],
        );
        assert_eq!(
            id.for_gpu("nvidia", Some(0x10de), Some(0x2b85)),
            Some("vk:NVIDIA:610.57.04".to_string())
        );
    }

    #[test]
    fn a_mesa_gpu_is_identified_by_its_vulkan_driver_properties() {
        let id = DriverIdentities::with(Some("595.99.02"), parse_vulkan_summary(RADV_SUMMARY));
        assert_eq!(
            id.for_gpu("amd", Some(0x1002), Some(0x1636)),
            Some("vk:radv:Mesa 25.3.6".to_string()),
            "the NVIDIA rung must not answer for an AMD GPU on a mixed host"
        );
    }

    #[test]
    fn an_unmatched_gpu_reports_nothing_rather_than_a_placeholder() {
        let id = DriverIdentities::with(None, parse_vulkan_summary(RADV_SUMMARY));
        assert_eq!(id.for_gpu("intel", Some(0x8086), Some(0x9a49)), None);
        assert_eq!(id.for_gpu("amd", None, None), None);
        assert_eq!(
            DriverIdentities::with(None, Vec::new()).for_gpu("nvidia", Some(0x10de), Some(0x2b85)),
            None
        );
    }

    #[test]
    fn identities_are_single_line_and_bounded() {
        let d = VulkanDriver {
            vendor_id: 1,
            device_id: 2,
            driver_name: "  weird\u{0}\nname ".into(),
            driver_info: "x".repeat(200),
        };
        let id = d.identity().unwrap();
        assert!(id.starts_with("vk:weird name:"), "got {id}");
        assert!(id.len() <= MAX_LEN);
        assert!(!id.contains('\n') && !id.contains('\u{0}'));
    }

    #[test]
    fn a_driver_with_no_usable_strings_has_no_identity() {
        assert_eq!(
            VulkanDriver {
                vendor_id: 1,
                device_id: 2,
                driver_name: " ".into(),
                driver_info: "\n".into(),
            }
            .identity(),
            None
        );
    }

    #[test]
    fn pci_ids_parse_in_either_case_and_base() {
        assert_eq!(parse_hex_id("0x10de"), Some(0x10de));
        assert_eq!(parse_hex_id("0X10DE\n"), Some(0x10de));
        assert_eq!(parse_hex_id(" 4318 "), Some(4318));
        assert_eq!(parse_hex_id("nvidia"), None);
    }
}
