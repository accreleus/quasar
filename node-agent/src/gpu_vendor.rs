//! GPU-vendor detection for the `QUASAR_ENCODER` auto-default
//! (`session::settings::resolve_encoder_choice`). Detection order: the configured
//! render node's sysfs vendor, the `/dev/nvidia*` device nodes (CDI-injected
//! containers may expose no `/dev/dri` sysfs mapping, so the nvidia nodes are the
//! reliable NVIDIA signal), then a `/dev/dri/renderD*` scan.

use std::path::Path;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GpuVendor {
    Nvidia,
    Amd,
    Intel,
}

impl GpuVendor {
    pub fn as_str(self) -> &'static str {
        match self {
            GpuVendor::Nvidia => "nvidia",
            GpuVendor::Amd => "amd",
            GpuVendor::Intel => "intel",
        }
    }
}

/// Which probe produced the vendor, for the startup `encoder-autodetect` log line.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DetectSource {
    RenderNode,
    NvidiaDev,
    DriScan,
}

impl DetectSource {
    pub fn as_str(self) -> &'static str {
        match self {
            DetectSource::RenderNode => "render-node",
            DetectSource::NvidiaDev => "nvidia-dev",
            DetectSource::DriScan => "dri-scan",
        }
    }
}

/// Parse a sysfs PCI vendor id (`/sys/class/drm/<node>/device/vendor`, e.g. "0x10de").
pub fn parse_pci_vendor(raw: &str) -> Option<GpuVendor> {
    let s = raw.trim();
    let hex = s.strip_prefix("0x").or_else(|| s.strip_prefix("0X"))?;
    match u32::from_str_radix(hex, 16).ok()? {
        0x10de => Some(GpuVendor::Nvidia),
        // 0x1002 is the AMD/ATI GPU id; 0x1022 is AMD the CPU vendor, accepted defensively.
        0x1002 | 0x1022 => Some(GpuVendor::Amd),
        0x8086 => Some(GpuVendor::Intel),
        _ => None,
    }
}

/// First recognized vendor wins over the scanned `(node, raw vendor id)` list.
/// Callers pass the list sorted — read_dir order is arbitrary, so renderD numbering
/// must be imposed before this call for a deterministic pick on multi-GPU hosts.
pub fn vendor_from_scan(nodes: &[(String, String)]) -> Option<GpuVendor> {
    let recognized: Vec<(&str, GpuVendor)> = nodes
        .iter()
        .filter_map(|(node, id)| parse_pci_vendor(id).map(|v| (node.as_str(), v)))
        .collect();
    let (first_node, first) = *recognized.first()?;
    if recognized.iter().any(|&(_, v)| v != first) {
        tracing::info!(
            token = "encoder-autodetect-multi-vendor",
            nodes = ?recognized,
            "multiple GPU vendors detected; using {} ({first_node})",
            first.as_str()
        );
    }
    Some(first)
}

/// Detect the host's GPU vendor. `None` means no GPU signal was found.
pub fn detect() -> Option<(GpuVendor, DetectSource)> {
    detect_at(Path::new("/dev"), Path::new("/sys/class/drm"))
}

fn detect_at(dev: &Path, drm_sysfs: &Path) -> Option<(GpuVendor, DetectSource)> {
    if let Some(v) = vendor_for_configured_render_node(drm_sysfs) {
        return Some((v, DetectSource::RenderNode));
    }
    if has_nvidia_device_nodes(dev) {
        return Some((GpuVendor::Nvidia, DetectSource::NvidiaDev));
    }
    vendor_from_dri_scan(dev, drm_sysfs).map(|v| (v, DetectSource::DriScan))
}

fn vendor_for_configured_render_node(drm_sysfs: &Path) -> Option<GpuVendor> {
    let raw = std::env::var("QUASAR_RENDER_NODE").ok()?;
    if raw.is_empty() || !raw.starts_with("/dev/dri/") {
        return None;
    }
    let canon = crate::session::settings::canonicalize_render_node(&raw);
    let node = Path::new(&canon).file_name()?.to_str()?.to_string();
    let id = std::fs::read_to_string(drm_sysfs.join(&node).join("device/vendor")).ok()?;
    parse_pci_vendor(&id)
}

fn has_nvidia_device_nodes(dev: &Path) -> bool {
    if dev.join("nvidiactl").exists() {
        return true;
    }
    let Ok(entries) = std::fs::read_dir(dev) else {
        return false;
    };
    entries.filter_map(|e| e.ok()).any(|e| {
        e.file_name()
            .to_str()
            .and_then(|n| n.strip_prefix("nvidia"))
            .is_some_and(|rest| !rest.is_empty() && rest.bytes().all(|b| b.is_ascii_digit()))
    })
}

fn vendor_from_dri_scan(dev: &Path, drm_sysfs: &Path) -> Option<GpuVendor> {
    let Ok(entries) = std::fs::read_dir(dev.join("dri")) else {
        return None;
    };
    let mut names: Vec<String> = entries
        .filter_map(|e| e.ok())
        .filter_map(|e| e.file_name().to_str().map(String::from))
        .filter(|n| n.starts_with("renderD"))
        .collect();
    names.sort();
    let nodes: Vec<(String, String)> = names
        .into_iter()
        .filter_map(|n| {
            std::fs::read_to_string(drm_sysfs.join(&n).join("device/vendor"))
                .ok()
                .map(|id| (n, id))
        })
        .collect();
    vendor_from_scan(&nodes)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_pci_vendor_known_ids() {
        assert_eq!(parse_pci_vendor("0x10de"), Some(GpuVendor::Nvidia));
        assert_eq!(parse_pci_vendor("0x10de\n"), Some(GpuVendor::Nvidia));
        assert_eq!(parse_pci_vendor("0x1002"), Some(GpuVendor::Amd));
        assert_eq!(parse_pci_vendor("0x1022"), Some(GpuVendor::Amd));
        assert_eq!(parse_pci_vendor("0x8086"), Some(GpuVendor::Intel));
    }

    #[test]
    fn parse_pci_vendor_rejects_unknown_and_junk() {
        assert_eq!(parse_pci_vendor("0x1234"), None);
        assert_eq!(parse_pci_vendor("10de"), None);
        assert_eq!(parse_pci_vendor(""), None);
        assert_eq!(parse_pci_vendor("0x"), None);
        assert_eq!(parse_pci_vendor("nvidia"), None);
    }

    #[test]
    fn vendor_from_scan_first_recognized_wins() {
        let nodes = vec![
            ("renderD128".into(), "0x8086\n".into()),
            ("renderD129".into(), "0x10de\n".into()),
        ];
        assert_eq!(vendor_from_scan(&nodes), Some(GpuVendor::Intel));
    }

    #[test]
    fn vendor_from_scan_skips_unrecognized_ids() {
        let nodes = vec![
            ("renderD128".into(), "0xdead".into()),
            ("renderD129".into(), "0x1002\n".into()),
        ];
        assert_eq!(vendor_from_scan(&nodes), Some(GpuVendor::Amd));
    }

    #[test]
    fn vendor_from_scan_empty_is_none() {
        assert_eq!(vendor_from_scan(&[]), None);
        let junk = vec![("renderD128".to_string(), "garbage".to_string())];
        assert_eq!(vendor_from_scan(&junk), None);
    }
}
