//! Evidence-scoped encoder exclusions, shared by discovery, readiness and launch.
//! A factory registering successfully does not establish correct inter-frame output.
//! See docs/reports/2026-09-06-av1-vulkan-driver-comparison.md.

use std::collections::BTreeSet;
use std::path::Path;

use crate::session::Codec;

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

/// Host-wide: exclude AV1 when any accessible render GPU matches the bad combination;
/// never claim a second, untested GPU is validated because the first one is. Feeds the
/// readiness check and, through `effective_encoder`, every session and per-GPU plan —
/// so AV1 stays host-wide off even where [`classify_render_node`] clears a GPU. No
/// cached decision: a driver/agent restart reads the new identity.
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
        let name = name.to_string_lossy();
        if !name.starts_with("renderD") {
            continue;
        }
        match inspect_render_node(root, &name, &driver) {
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

/// One render node's classification against `driver` — the per-GPU half of
/// [`inspect`]'s host-wide fold, and [`classify_render_node`]'s core once the driver
/// version is known.
fn inspect_render_node(root: &Path, render_node: &str, driver: &str) -> Av1Compatibility {
    let base = render_node.rsplit('/').next().unwrap_or(render_node);
    if !base.starts_with("renderD") || !root.join("dev/dri").join(base).exists() {
        return Av1Compatibility::Unknown;
    }
    let read = |file: &str| {
        std::fs::read_to_string(
            root.join("sys/class/drm")
                .join(base)
                .join("device")
                .join(file),
        )
        .unwrap_or_default()
    };
    classify(&read("vendor"), &read("device"), driver)
}

/// Per-GPU classification (#301 layer 2) of `render_node` (`renderD*` or a full path),
/// independent of every sibling — unlike the host-wide fold in [`inspect`].
pub(crate) fn classify_render_node(root: &Path, render_node: &str) -> Av1Compatibility {
    let Some(driver) = crate::nvidia_volume::kernel_driver_version(root) else {
        return Av1Compatibility::Unknown;
    };
    inspect_render_node(root, render_node, &driver)
}

/// The codecs [`classify_render_node`]'s `KnownCorrupt` result excludes for one GPU —
/// today only AV1. The per-GPU codec plan's layer 2 (#301): applied before the codec
/// probe (layer 3) is even consulted, so a passing probe can never override it.
pub(crate) fn excluded_codecs(root: &Path, render_node: &str) -> BTreeSet<Codec> {
    if classify_render_node(root, render_node) == Av1Compatibility::KnownCorrupt {
        BTreeSet::from([Codec::Av1])
    } else {
        BTreeSet::new()
    }
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

    fn fake_render_node(root: &Path, name: &str, vendor: &str, device: &str) {
        let device_dir = root.join("sys/class/drm").join(name).join("device");
        std::fs::create_dir_all(&device_dir).unwrap();
        std::fs::write(device_dir.join("vendor"), vendor).unwrap();
        std::fs::write(device_dir.join("device"), device).unwrap();
        let dri_dir = root.join("dev/dri");
        std::fs::create_dir_all(&dri_dir).unwrap();
        std::fs::write(dri_dir.join(name), "").unwrap();
    }

    fn fake_driver_version(root: &Path, version: &str) {
        let dir = root.join("sys/module/nvidia");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("version"), version).unwrap();
    }

    /// #301 layer 2: a mixed host with one KnownCorrupt render node and one unrelated
    /// sibling — the sibling's own classification (and therefore its excluded codec
    /// set) must not inherit the corrupt GPU's exclusion, even though the host-wide
    /// fold ([`inspect`], the readiness check) still excludes AV1 host-wide.
    #[test]
    fn per_gpu_classification_does_not_leak_across_sibling_gpus() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        fake_render_node(root, "renderD128", "0x10de", "0x2b85"); // the known-corrupt combo
        fake_render_node(root, "renderD129", "0x1002", "0x73df"); // an unrelated AMD GPU
        fake_driver_version(root, "595.99.02");

        assert_eq!(
            classify_render_node(root, "renderD128"),
            Av1Compatibility::KnownCorrupt
        );
        assert_eq!(
            classify_render_node(root, "/dev/dri/renderD129"),
            Av1Compatibility::Unknown
        );
        assert_eq!(
            excluded_codecs(root, "renderD128"),
            BTreeSet::from([Codec::Av1])
        );
        assert!(excluded_codecs(root, "renderD129").is_empty());

        // The host-wide fold still excludes AV1 host-wide — the readiness check is
        // unchanged by the per-GPU classification.
        assert_eq!(inspect(root), Av1Compatibility::KnownCorrupt);
    }
}
