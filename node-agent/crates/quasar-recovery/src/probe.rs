//! GPU and device detection by a disposable probe container.
//!
//! The actor cannot look for itself: a container sees only the devices it was given, and
//! the actor is given none. So the actor runs a short-lived probe from the agent image
//! with the host's `/dev` bound read-only at `/host/dev`, reads what it printed, and
//! removes it whatever happened.
//!
//! Which evidence wins: **device nodes**. `/sys/class/drm` is not namespaced, so inside a
//! system container (an LXC guest) it lists every GPU of the physical host, including
//! ones whose nodes this machine does not have. The probe therefore starts from the
//! render nodes present under `/dev/dri` and reads each node's vendor through its own
//! `major:minor` (`/sys/dev/char/<maj>:<min>/device/vendor`); a GPU visible only in sysfs
//! is never chosen.

use std::collections::BTreeMap;
use std::time::Duration;

use crate::engine::{ContainerSpec, EngineError, ErrorKind, PlatformEngine, RestartPolicy};
use crate::recipe::{labels, names, Bind, GpuFacts, GpuRequest, GpuVendor, HostDevices, ImageRef};

pub const PROBE_HELPER: &str = "gpu-probe";
pub const GPUS_PROBE_HELPER: &str = "gpus-probe";
const PROBE_TIMEOUT: Duration = Duration::from_secs(60);

/// POSIX sh, so it runs in any image with coreutils or busybox.
pub const SCRIPT: &str = r#"echo "quasar-probe 1"
for d in uinput kmsg nvidiactl; do [ -e "/host/dev/$d" ] && echo "dev $d"; done
for n in /host/dev/dri/renderD* /host/dev/dri/card*; do
  [ -c "$n" ] || continue
  mm=$(stat -c '%t:%T' "$n") || continue
  maj=$((0x${mm%%:*})); min=$((0x${mm##*:}))
  v=$(cat "/sys/dev/char/$maj:$min/device/vendor" 2>/dev/null) || v=-
  echo "node /dev/dri/${n##*/} $maj:$min $v"
done
echo end"#;

/// What one probe run reported.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ProbeReport {
    pub uinput: bool,
    pub kmsg: bool,
    pub nvidia_nodes: bool,
    /// `(node, pci vendor id)`, render and card nodes, in the order printed.
    pub nodes: Vec<(String, Option<String>)>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ProbeError {
    Engine(EngineError),
    /// The probe ran but its output is not a report (a truncated run, a wrong image).
    Unreadable(String),
}

impl std::fmt::Display for ProbeError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ProbeError::Engine(e) => write!(f, "{e}"),
            ProbeError::Unreadable(why) => write!(f, "unreadable probe output: {why}"),
        }
    }
}

pub fn parse(output: &str) -> Result<ProbeReport, ProbeError> {
    let mut lines = output.lines().map(str::trim).filter(|l| !l.is_empty());
    if lines.next() != Some("quasar-probe 1") {
        return Err(ProbeError::Unreadable("no `quasar-probe 1` header".into()));
    }
    let mut report = ProbeReport::default();
    let mut ended = false;
    for line in lines {
        let mut words = line.split_whitespace();
        match (words.next(), words.next()) {
            (Some("dev"), Some("uinput")) => report.uinput = true,
            (Some("dev"), Some("kmsg")) => report.kmsg = true,
            (Some("dev"), Some("nvidiactl")) => report.nvidia_nodes = true,
            (Some("node"), Some(node)) if node.starts_with("/dev/dri/") => {
                let _majmin = words.next();
                let vendor = words.next().filter(|v| *v != "-").map(str::to_owned);
                report.nodes.push((node.to_owned(), vendor));
            }
            (Some("end"), None) => {
                ended = true;
                break;
            }
            _ => return Err(ProbeError::Unreadable(format!("unexpected line {line:?}"))),
        }
    }
    if !ended {
        return Err(ProbeError::Unreadable("no `end` line".into()));
    }
    Ok(report)
}

pub fn vendor_of(pci_id: &str) -> Option<GpuVendor> {
    let hex = pci_id.trim().strip_prefix("0x")?;
    match u32::from_str_radix(hex, 16).ok()? {
        0x10de => Some(GpuVendor::Nvidia),
        0x1002 | 0x1022 => Some(GpuVendor::Amd),
        0x8086 => Some(GpuVendor::Intel),
        _ => None,
    }
}

fn render_number(node: &str) -> Option<u32> {
    node.strip_prefix("/dev/dri/renderD")?.parse().ok()
}

impl ProbeReport {
    /// An NVIDIA device node, the only case worth asking the engine for `--gpus`.
    pub fn has_nvidia(&self) -> bool {
        self.nvidia_nodes
            || self
                .nodes
                .iter()
                .any(|(_, id)| id.as_deref().and_then(vendor_of) == Some(GpuVendor::Nvidia))
    }
}

/// The machine's GPU facts from one report: an NVIDIA node the engine can serve wins (the
/// discrete card of a mixed host, the Compose NVIDIA overlay's case); otherwise the lowest
/// recognised render node.
pub fn select(report: &ProbeReport, gpus_served: bool) -> (GpuFacts, HostDevices) {
    let mut renders: Vec<(u32, &str, GpuVendor)> = report
        .nodes
        .iter()
        .filter_map(|(node, id)| {
            Some((
                render_number(node)?,
                node.as_str(),
                vendor_of(id.as_deref()?)?,
            ))
        })
        .collect();
    renders.sort_by_key(|(n, node, _)| (*n, *node));
    let nvidia = renders
        .iter()
        .find(|(_, _, v)| *v == GpuVendor::Nvidia && gpus_served);
    let chosen = nvidia.or_else(|| renders.first());
    let gpu = GpuFacts {
        vendor: chosen.map(|(_, _, v)| *v),
        render_node: chosen.map(|(_, n, _)| (*n).to_owned()),
        gpus_served,
    };
    let devices = HostDevices {
        dri: !report.nodes.is_empty(),
        uinput: report.uinput,
        kmsg: report.kmsg,
    };
    (gpu, devices)
}

pub fn probe_spec(image: &ImageRef) -> ContainerSpec {
    ContainerSpec {
        name: names::GPU_PROBE.into(),
        image: image.reference(),
        entrypoint: Some(vec!["/bin/sh".into(), "-c".into()]),
        cmd: Some(vec![SCRIPT.into()]),
        env: BTreeMap::new(),
        labels: BTreeMap::from([(labels::HELPER.to_string(), PROBE_HELPER.to_string())]),
        network_mode: Some("none".into()),
        binds: vec![Bind {
            source: "/dev".into(),
            target: "/host/dev".into(),
            read_only: true,
        }],
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: Vec::new(),
        cap_add: Vec::new(),
        security_opt: Vec::new(),
        init: false,
        restart: RestartPolicy::No,
    }
}

/// The `--gpus all` probe: a container requesting every GPU, running `true`. The engine
/// serves `--gpus` through an `nvidia` runtime, CDI, or the container toolkit's hook, and
/// only the last is invisible in `/info`, so the evidence is whether this starts and exits
/// 0. Removed on every path that created it.
pub fn gpus_spec(image: &ImageRef) -> ContainerSpec {
    ContainerSpec {
        name: names::GPU_PROBE.into(),
        image: image.reference(),
        entrypoint: Some(vec!["/bin/sh".into(), "-c".into()]),
        cmd: Some(vec!["true".into()]),
        env: BTreeMap::new(),
        labels: BTreeMap::from([(labels::HELPER.to_string(), GPUS_PROBE_HELPER.to_string())]),
        network_mode: Some("none".into()),
        binds: Vec::new(),
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: vec![GpuRequest {
            driver: None,
            count: -1,
            capabilities: vec![vec!["gpu".into()]],
        }],
        cap_add: Vec::new(),
        security_opt: Vec::new(),
        init: false,
        restart: RestartPolicy::No,
    }
}

/// Whether the engine serves `--gpus all`, and why not when it does not.
///
/// Only a definitive answer is `Ok`: the engine answered the create or the start with a
/// refusal (`ErrorKind::Engine`, which is how it rejects a device request it cannot
/// satisfy), or the probe ran and exited non-zero. Anything else (an unreachable engine, a
/// timeout, an unknown outcome, a crash) is `Err`, so the caller records nothing and the
/// next start asks again: machine inputs are never written from a non-answer.
pub fn serves_gpus(
    engine: &dyn PlatformEngine,
    image: &ImageRef,
) -> Result<Result<(), String>, EngineError> {
    let refused = |e: &EngineError| matches!(e, EngineError::Runtime(ErrorKind::Engine));
    let id = match engine.create_container(&gpus_spec(image)) {
        Ok(id) => id,
        Err(e) if refused(&e) => return Ok(Err(format!("the engine refused to create it ({e})"))),
        Err(e) => return Err(e),
    };
    let outcome = match engine.start_container(&id) {
        Err(e) if refused(&e) => Ok(Err(format!("the engine refused to start it ({e})"))),
        Err(e) => Err(e),
        Ok(()) => match engine.wait_container(&id, PROBE_TIMEOUT) {
            Err(e) => Err(e),
            Ok(0) => Ok(Ok(())),
            Ok(code) => Ok(Err(format!("it exited {code}"))),
        },
    };
    match (engine.remove_container(&id), outcome) {
        (Err(EngineError::Crashed), _) => Err(EngineError::Crashed),
        (_, Err(e)) => Err(e),
        (Err(e), Ok(_)) => Err(e),
        (Ok(()), Ok(answer)) => Ok(answer),
    }
}

/// Create, run and remove the probe. The container is removed on every path that
/// created it, including a failed run.
pub fn run(engine: &dyn PlatformEngine, image: &ImageRef) -> Result<ProbeReport, ProbeError> {
    let id = engine
        .create_container(&probe_spec(image))
        .map_err(ProbeError::Engine)?;
    let outcome = (|| {
        engine.start_container(&id).map_err(ProbeError::Engine)?;
        let code = engine
            .wait_container(&id, PROBE_TIMEOUT)
            .map_err(ProbeError::Engine)?;
        let logs = engine.logs_tail(&id, 200).map_err(ProbeError::Engine)?;
        if code != 0 {
            return Err(ProbeError::Unreadable(format!("the probe exited {code}")));
        }
        parse(&logs)
    })();
    let removed = engine.remove_container(&id);
    match (outcome, removed) {
        (_, Err(EngineError::Crashed)) => Err(ProbeError::Engine(EngineError::Crashed)),
        (Err(e), _) => Err(e),
        (Ok(_), Err(e)) => Err(ProbeError::Engine(e)),
        (Ok(report), Ok(())) => Ok(report),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const LXC_AMD: &str = "quasar-probe 1\ndev uinput\nnode /dev/dri/renderD129 226:129 0x1002\nnode /dev/dri/card1 226:1 0x1002\nend\n";

    #[test]
    fn a_system_container_with_one_passed_through_gpu_selects_that_node() {
        let report = parse(LXC_AMD).unwrap();
        let (gpu, devices) = select(&report, false);
        assert_eq!(gpu.vendor, Some(GpuVendor::Amd));
        assert_eq!(gpu.render_node.as_deref(), Some("/dev/dri/renderD129"));
        assert!(devices.dri && devices.uinput && !devices.kmsg);
    }

    #[test]
    fn an_nvidia_node_wins_only_when_the_engine_can_serve_it() {
        let out = "quasar-probe 1\ndev nvidiactl\nnode /dev/dri/renderD128 226:128 0x1002\nnode /dev/dri/renderD129 226:129 0x10de\nend";
        let report = parse(out).unwrap();
        let (with, _) = select(&report, true);
        assert_eq!(with.vendor, Some(GpuVendor::Nvidia));
        assert_eq!(with.render_node.as_deref(), Some("/dev/dri/renderD129"));
        assert!(with.nvidia_shape());
        let (without, _) = select(&report, false);
        assert_eq!(without.vendor, Some(GpuVendor::Amd));
        assert!(!without.nvidia_shape());
    }

    #[test]
    fn no_render_node_is_no_gpu_and_no_dri_device() {
        let (gpu, devices) = select(&parse("quasar-probe 1\ndev kmsg\nend").unwrap(), true);
        assert_eq!(gpu.vendor, None);
        assert_eq!(gpu.render_node, None);
        assert!(!devices.dri && devices.kmsg && !devices.uinput);
    }

    #[test]
    fn a_node_whose_vendor_sysfs_cannot_name_is_present_but_never_chosen() {
        let (gpu, devices) = select(
            &parse("quasar-probe 1\nnode /dev/dri/renderD128 226:128 -\nend").unwrap(),
            false,
        );
        assert_eq!(gpu.vendor, None);
        assert!(devices.dri);
    }

    #[test]
    fn truncated_or_foreign_output_is_refused() {
        assert!(parse("").is_err());
        assert!(parse("quasar-probe 1\ndev uinput\n").is_err());
        assert!(parse("quasar-probe 1\nsomething else\nend").is_err());
    }

    /// The script prints exactly what `parse` reads, run by a real POSIX shell against a
    /// fake `/host/dev` with no device nodes (creating char devices needs root).
    #[test]
    fn the_script_output_parses_on_a_host_with_no_devices() {
        let out = std::process::Command::new("sh")
            .arg("-c")
            .arg(SCRIPT)
            .output()
            .expect("sh");
        let report = parse(&String::from_utf8_lossy(&out.stdout)).unwrap();
        assert!(report.nodes.is_empty());
    }
}
