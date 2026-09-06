//! Evidence-scoped encoder exclusions, shared by discovery, readiness and launch.
//! A factory registering successfully does not establish correct inter-frame output.
//! See docs/reports/2026-09-06-av1-vulkan-driver-comparison.md.

use std::path::Path;

pub(crate) const CHECK_ID: &str = "nvidia_vulkan_av1_compatibility";
pub(crate) const SUMMARY: &str = "AV1 is disabled on RTX 5090 with NVIDIA 595.99.02 because Vulkan AV1 produces corrupted video. Quasar negotiates HEVC or H.264 when the profile and client support them.";
pub(crate) const REMEDIATION: &str = "NVIDIA 610.57.04 has been validated for Vulkan AV1 on RTX 5090; this is not a universal minimum version. Install a compatible driver with the open kernel module required by Blackwell, then recreate the agent to refresh driver libraries and codec discovery. Quasar does not substitute NVENC AV1 because that path has a separate teardown fault.";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Av1Compatibility {
    KnownCorrupt,
    Validated,
    Unknown,
}

fn classify(vendor: &str, device: &str, driver: &str) -> Av1Compatibility {
    let pci = |raw: &str| {
        let raw = raw.trim();
        u32::from_str_radix(raw.trim_start_matches("0x").trim_start_matches("0X"), 16).ok()
    };
    if pci(vendor) != Some(0x10de) || pci(device) != Some(0x2b85) {
        return Av1Compatibility::Unknown;
    }
    match driver.trim() {
        "595.99.02" => Av1Compatibility::KnownCorrupt,
        "610.57.04" => Av1Compatibility::Validated,
        _ => Av1Compatibility::Unknown,
    }
}

/// Host codec advertisements are host-wide today. Conservatively exclude AV1 when
/// any accessible render GPU matches the bad combination; never claim that a
/// second, untested GPU is validated because the first one is. No cached decision:
/// readiness and codec discovery read the new identity after a driver/agent restart.
pub(crate) fn inspect(root: &Path) -> Av1Compatibility {
    let Some(driver) = crate::nvidia_volume::kernel_driver_version(root) else {
        return Av1Compatibility::Unknown;
    };
    let Ok(entries) = std::fs::read_dir(root.join("sys/class/drm")) else {
        return Av1Compatibility::Unknown;
    };
    let mut result = Av1Compatibility::Unknown;
    for entry in entries.flatten() {
        let name = entry.file_name();
        if !name.to_string_lossy().starts_with("renderD")
            || !root.join("dev/dri").join(&name).exists()
        {
            continue;
        }
        let read = |file| {
            std::fs::read_to_string(entry.path().join("device").join(file)).unwrap_or_default()
        };
        match classify(&read("vendor"), &read("device"), &driver) {
            Av1Compatibility::KnownCorrupt => return Av1Compatibility::KnownCorrupt,
            Av1Compatibility::Validated => result = Av1Compatibility::Validated,
            Av1Compatibility::Unknown => {}
        }
    }
    result
}

pub(crate) fn av1_blocked() -> bool {
    inspect(Path::new("/")) == Av1Compatibility::KnownCorrupt
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exclusions_are_evidence_scoped_not_a_version_floor() {
        assert_eq!(
            classify("0x10de\n", "0x2b85\n", "595.99.02\n"),
            Av1Compatibility::KnownCorrupt
        );
        assert_eq!(
            classify("0X10DE", "0X2B85", "610.57.04"),
            Av1Compatibility::Validated
        );
        for driver in [
            "595.80",
            "595.99.03",
            "610.57.03",
            "610.57.05",
            "",
            "invalid",
        ] {
            assert_eq!(
                classify("0x10de", "0x2b85", driver),
                Av1Compatibility::Unknown
            );
        }
        assert_eq!(
            classify("0x1002", "0x2b85", "595.99.02"),
            Av1Compatibility::Unknown
        );
        assert_eq!(
            classify("0x10de", "0x2684", "595.99.02"),
            Av1Compatibility::Unknown
        );
    }
}
