//! GPU and device detection by a disposable probe container.
//!
//! The actor cannot look for itself: a container sees only the devices it was given, and
//! the actor is given none. So the actor runs a short-lived probe from the agent image
//! with the host's `/dev` bound read-only at `/host/dev` (and `/run` at `/host/run`, for
//! logind's state and the console-audio socket directory, RH-07 #407), reads what it printed, and removes it whatever happened.
//!
//! Presence is read by listing `/host/dev` (a glob, which is a directory read), never by
//! `stat`ing a node: under SELinux a confined container may list the host's `/dev` but
//! not `stat` most of its nodes, and a `[ -e ]` there reads "absent" (found on the first
//! rootless Podman install, RH-07).
//!
//! Which evidence wins: **device nodes**. `/sys/class/drm` is not namespaced, so inside a
//! system container (an LXC guest) it lists every GPU of the physical host, including
//! ones whose nodes this machine does not have. The probe therefore starts from the
//! render nodes present under `/dev/dri` and reads each node's vendor through its own
//! `major:minor` (`/sys/dev/char/<maj>:<min>/device/vendor`); a GPU visible only in sysfs
//! is never chosen.

use std::collections::BTreeMap;
use std::time::Duration;

use tracing::warn;

use crate::engine::{ContainerSpec, EngineError, PlatformEngine, RestartPolicy};
use crate::recipe::{
    labels, names, Bind, GpuFacts, GpuNode, GpuRequest, GpuVendor, HostDevices, ImageRef,
};
use quasar_runtime::GpuInjection;

pub const PROBE_HELPER: &str = "gpu-probe";
pub const GPUS_PROBE_HELPER: &str = "gpus-probe";
const PROBE_TIMEOUT: Duration = Duration::from_secs(60);
/// How many times the `--gpus` probe is asked before a transient failure fails the start.
pub const GPUS_PROBE_ATTEMPTS: u32 = 3;

/// POSIX sh, so it runs in any image with coreutils or busybox.
pub const SCRIPT: &str = r#"echo "quasar-probe 1"
for f in /host/dev/*; do case "${f##*/}" in uinput|kmsg|nvidiactl|fuse|snd) echo "dev ${f##*/}";; esac; done
for f in /host/dev/i2c-*; do n=${f##*/i2c-}; case "$n" in ''|*[!0-9]*) ;; *) echo "i2c $n";; esac; done
for f in /host/run/systemd/*; do case "${f##*/}" in seats|sessions) echo "logind ${f##*/}";; esac; done
for f in /host/run/quasar-console-audi[o]; do [ "$f" = /host/run/quasar-console-audio ] && echo "console_audio dir"; done
[ "$(cat /proc/sys/kernel/dmesg_restrict 2>/dev/null)" = 0 ] && echo "kernel_log open"
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
    /// The host lets unprivileged processes read the kernel log (`dmesg_restrict=0`).
    pub kernel_log: bool,
    /// The host has `/dev/fuse` (sessions may be given it).
    pub fuse: bool,
    /// The host has `/dev/snd` (console mode may be given it, RH-07 #395).
    pub sound: bool,
    /// The host's `/dev/i2c-<n>` bus numbers, sorted (console mode's DDC, RH-07 #407).
    pub i2c: Vec<u32>,
    /// Which of logind's `seats` and `sessions` directories the host's `/run/systemd` has.
    pub logind: Vec<String>,
    /// The host's `/run` has `quasar-console-audio`, the desktop user's Quasar-only
    /// PipeWire socket directory (console audio, RH-07 #407).
    pub console_audio: bool,
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
            (Some("dev"), Some("fuse")) => report.fuse = true,
            (Some("dev"), Some("snd")) => report.sound = true,
            (Some("kernel_log"), Some("open")) => report.kernel_log = true,
            (Some("console_audio"), Some("dir")) => report.console_audio = true,
            (Some("i2c"), Some(n)) => {
                let bus = n
                    .parse::<u32>()
                    .map_err(|_| ProbeError::Unreadable(format!("unexpected line {line:?}")))?;
                if !report.i2c.contains(&bus) {
                    report.i2c.push(bus);
                }
            }
            (Some("logind"), Some(dir @ ("seats" | "sessions"))) => {
                report.logind.push(dir.to_owned())
            }
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
    report.i2c.sort_unstable();
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
    /// Both of logind's state directories are there to bind.
    pub fn logind(&self) -> bool {
        ["seats", "sessions"]
            .iter()
            .all(|d| self.logind.iter().any(|l| l == d))
    }

    /// An NVIDIA device node, the only case worth asking the engine for `--gpus`.
    pub fn has_nvidia(&self) -> bool {
        self.nvidia_nodes
            || self
                .nodes
                .iter()
                .any(|(_, id)| id.as_deref().and_then(vendor_of) == Some(GpuVendor::Nvidia))
    }
}

/// The machine's GPU facts from one report: an NVIDIA render node is preferred (the
/// discrete card of a mixed host, the Compose NVIDIA overlay's case), with the lowest other
/// recognised node as its fallback for when the engine does not serve `--gpus`; otherwise
/// the lowest recognised render node. Whether `--gpus` is served is decided separately.
pub fn select(report: &ProbeReport) -> (GpuFacts, HostDevices) {
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
    let nvidia = renders.iter().find(|(_, _, v)| *v == GpuVendor::Nvidia);
    let other = renders.iter().find(|(_, _, v)| *v != GpuVendor::Nvidia);
    let chosen = nvidia.or(other);
    let gpu = GpuFacts {
        unknown: Default::default(),
        vendor: chosen.map(|(_, _, v)| *v),
        render_node: chosen.map(|(_, n, _)| (*n).to_owned()),
        gpus_served: false,
        cdi: false,
        fallback: nvidia.and(other).map(|(_, n, v)| GpuNode {
            unknown: Default::default(),
            vendor: *v,
            render_node: (*n).to_owned(),
        }),
    };
    let devices = HostDevices {
        unknown: Default::default(),
        dri: !report.nodes.is_empty(),
        uinput: report.uinput,
        kmsg: report.kmsg,
        kernel_log: report.kmsg && report.kernel_log,
        fuse: report.fuse,
        sound: report.sound,
        i2c: report.i2c.clone(),
        logind: report.logind(),
        console_audio: report.console_audio,
        engine_rootless: false,
        host_sysfs: false,
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
        binds: vec![
            Bind {
                source: "/dev".into(),
                target: "/host/dev".into(),
                read_only: true,
            },
            // For logind's state directories (RH-07 #407), listed and never read. `/run`
            // rather than `/run/systemd`: a host without systemd has no such directory, and
            // Podman refuses a bind of a missing source (Docker would create it on the
            // host). It holds the engine socket on a rootful host; the probe runs a fixed
            // script from the agent's own image, which is given that socket anyway, with
            // no network. Under SELinux a confined listing may be denied, which reads as no
            // logind: the agent then names no display holder, and nothing else changes.
            Bind {
                source: "/run".into(),
                target: "/host/run".into(),
                read_only: true,
            },
        ],
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: Vec::new(),
        cap_add: Vec::new(),
        security_opt: Vec::new(),
        init: false,
        restart: RestartPolicy::No,
        ports: Vec::new(),
        healthcheck: Some(no_healthcheck()),
    }
}

/// Exits 0 only when the NVIDIA control node is inside: listing /dev, since a confined
/// container may be denied `stat` there.
const GPUS_PROBE_TEST: &str = "set -- /dev/nvidiactl*; [ \"$1\" = /dev/nvidiactl ]";

/// Probes run the agent image; its healthcheck means nothing for a one-shot container.
fn no_healthcheck() -> quasar_runtime::platform::Healthcheck {
    quasar_runtime::platform::Healthcheck {
        test: vec!["NONE".into()],
        interval_s: 0,
        timeout_s: 0,
        retries: 0,
        start_period_s: 0,
    }
}

/// The GPU probe: a container requesting every NVIDIA GPU the way the agent will (CDI or
/// `--gpus`), which must start and find the control node inside; an engine may accept a
/// request and inject nothing (rootless Podman with `--gpus`). Removed on every path that
/// created it.
pub fn gpus_spec(image: &ImageRef, injection: GpuInjection) -> ContainerSpec {
    ContainerSpec {
        name: names::GPU_PROBE.into(),
        image: image.reference(),
        // Served means the NVIDIA control node is inside: an engine may accept a request it
        // ignores (rootless Podman does with `--gpus`). Matched by listing /dev, since a
        // confined container may be denied `stat` there.
        entrypoint: Some(vec!["/bin/sh".into(), "-c".into()]),
        cmd: Some(vec![GPUS_PROBE_TEST.into()]),
        env: BTreeMap::new(),
        labels: BTreeMap::from([(labels::HELPER.to_string(), GPUS_PROBE_HELPER.to_string())]),
        network_mode: Some("none".into()),
        binds: Vec::new(),
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: vec![GpuRequest::nvidia_all(injection)],
        cap_add: Vec::new(),
        security_opt: Vec::new(),
        init: false,
        restart: RestartPolicy::No,
        ports: Vec::new(),
        healthcheck: Some(no_healthcheck()),
    }
}

/// The answer of a `--gpus all` probe.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum GpusAnswer {
    Served,
    /// A definite no, and why: the engine refused the device request ("could not select
    /// device driver"), or the probe ran and exited non-zero.
    Refused(String),
}

/// Whether the engine serves `--gpus all`. A transient failure (the engine did not answer,
/// or failed on its side for another reason than refusing the device request) is asked
/// again, [`GPUS_PROBE_ATTEMPTS`] times in all with `backoff` between; after that, and for
/// any other failure (a missing image, a name conflict, a crash), `Err`: no answer, so the
/// caller must not install on a guess.
pub fn serves_gpus(
    engine: &dyn PlatformEngine,
    image: &ImageRef,
    injection: GpuInjection,
    backoff: Duration,
) -> Result<GpusAnswer, EngineError> {
    let mut attempt = 1;
    loop {
        match gpus_attempt(engine, image, injection) {
            Err(e) if e.is_transient() && attempt < GPUS_PROBE_ATTEMPTS => {
                warn!(
                    token = "actor-gpus-probe-retry",
                    attempt, "the --gpus all probe got no answer ({e}); asking again"
                );
                std::thread::sleep(backoff * attempt);
                attempt += 1;
            }
            outcome => return outcome,
        }
    }
}

fn gpus_attempt(
    engine: &dyn PlatformEngine,
    image: &ImageRef,
    injection: GpuInjection,
) -> Result<GpusAnswer, EngineError> {
    let id = match engine.create_container(&gpus_spec(image, injection)) {
        Ok(id) => id,
        Err(e) if e.is_device_request_refusal() => return Ok(GpusAnswer::Refused(e.to_string())),
        Err(e) => return Err(e),
    };
    let outcome = match engine.start_container(&id) {
        Err(e) if e.is_device_request_refusal() => Ok(GpusAnswer::Refused(e.to_string())),
        Err(e) => Err(e),
        Ok(()) => match engine.wait_container(&id, PROBE_TIMEOUT) {
            Ok(0) => Ok(GpusAnswer::Served),
            Ok(code) => Ok(GpusAnswer::Refused(format!(
                "the probe started but saw no NVIDIA device inside (exit {code})"
            ))),
            Err(e) => Err(e),
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
    /// RH-07 #402: kernel-log access is reported only when the host allows it, and the
    /// agent is offered it only with /dev/kmsg present.
    #[test]
    fn kernel_log_access_follows_the_host_setting() {
        let open = parse("quasar-probe 1\ndev kmsg\nkernel_log open\nend").unwrap();
        assert!(open.kernel_log);
        assert!(select(&open).1.kernel_log);
        let restricted = parse("quasar-probe 1\ndev kmsg\nend").unwrap();
        assert!(!select(&restricted).1.kernel_log);
        let no_node = parse("quasar-probe 1\nkernel_log open\nend").unwrap();
        assert!(!select(&no_node).1.kernel_log);
        // #402 review: the agent no longer sees the host's /dev, so FUSE is probed here.
        assert!(
            select(&parse("quasar-probe 1\ndev fuse\nend").unwrap())
                .1
                .fuse
        );
        assert!(!select(&restricted).1.fuse);
    }

    use super::*;

    const LXC_AMD: &str = "quasar-probe 1\ndev uinput\nnode /dev/dri/renderD129 226:129 0x1002\nnode /dev/dri/card1 226:1 0x1002\nend\n";

    #[test]
    fn a_system_container_with_one_passed_through_gpu_selects_that_node() {
        let report = parse(LXC_AMD).unwrap();
        let (gpu, devices) = select(&report);
        assert_eq!(gpu.vendor, Some(GpuVendor::Amd));
        assert_eq!(gpu.render_node.as_deref(), Some("/dev/dri/renderD129"));
        assert!(devices.dri && devices.uinput && !devices.kmsg);
    }

    #[test]
    fn an_nvidia_node_wins_only_when_the_engine_serves_it() {
        let out = "quasar-probe 1\ndev nvidiactl\nnode /dev/dri/renderD128 226:128 0x1002\nnode /dev/dri/renderD129 226:129 0x10de\nend";
        let report = parse(out).unwrap();
        assert!(report.has_nvidia());
        let (mut gpu, _) = select(&report);
        assert_eq!(gpu.vendor, Some(GpuVendor::Nvidia));
        assert!(!gpu.nvidia_shape());
        assert_eq!(gpu.effective_render_node(), Some("/dev/dri/renderD128"));
        gpu.gpus_served = true;
        assert!(gpu.nvidia_shape());
        assert_eq!(gpu.effective_render_node(), Some("/dev/dri/renderD129"));
    }

    #[test]
    fn no_render_node_is_no_gpu_and_no_dri_device() {
        let (gpu, devices) = select(&parse("quasar-probe 1\ndev kmsg\nend").unwrap());
        assert_eq!(gpu.vendor, None);
        assert_eq!(gpu.render_node, None);
        assert!(!devices.dri && devices.kmsg && !devices.uinput);
    }

    #[test]
    fn a_node_whose_vendor_sysfs_cannot_name_is_present_but_never_chosen() {
        let (gpu, devices) =
            select(&parse("quasar-probe 1\nnode /dev/dri/renderD128 226:128 -\nend").unwrap());
        assert_eq!(gpu.vendor, None);
        assert!(devices.dri);
    }

    #[test]
    fn truncated_or_foreign_output_is_refused() {
        assert!(parse("").is_err());
        assert!(parse("quasar-probe 1\ndev uinput\n").is_err());
        assert!(parse("quasar-probe 1\nsomething else\nend").is_err());
    }

    /// RH-07 #407: i2c buses and logind's directories, read by listing; a bus number that
    /// is not one is refused, and logind counts only with both directories.
    #[test]
    fn i2c_buses_and_logind_are_reported_for_console_mode() {
        let report =
            parse("quasar-probe 1\ni2c 7\ni2c 3\ni2c 7\nlogind seats\nlogind sessions\nend")
                .unwrap();
        assert_eq!(report.i2c, vec![3, 7]);
        let (_, devices) = select(&report);
        assert_eq!(devices.i2c, vec![3, 7]);
        assert!(devices.logind);
        let half = parse("quasar-probe 1\nlogind seats\nend").unwrap();
        assert!(!select(&half).1.logind);
        assert!(select(&half).1.i2c.is_empty());
        assert!(parse("quasar-probe 1\ni2c x\nend").is_err());
        assert!(parse("quasar-probe 1\nlogind other\nend").is_err());
    }

    /// RH-07 #407 (D13): the console-audio socket directory is read by listing `/host/run`,
    /// and becomes the recipe input.
    #[test]
    fn the_console_audio_directory_is_reported_from_a_listing() {
        let root = tempfile::tempdir().unwrap();
        let host = root.path().join("host");
        std::fs::create_dir_all(host.join("dev")).unwrap();
        std::fs::create_dir_all(host.join("run")).unwrap();
        let run = |host: &std::path::Path| {
            let script = SCRIPT.replace("/host/", &format!("{}/", host.display()));
            let out = std::process::Command::new("sh")
                .arg("-c")
                .arg(script)
                .output()
                .expect("sh");
            parse(&String::from_utf8_lossy(&out.stdout)).unwrap()
        };
        assert!(!run(&host).console_audio);
        assert!(!select(&run(&host)).1.console_audio);
        std::fs::create_dir_all(host.join("run/quasar-console-audio")).unwrap();
        let report = run(&host);
        assert!(report.console_audio);
        assert!(select(&report).1.console_audio);
        assert!(parse("quasar-probe 1\nconsole_audio other\nend").is_err());
    }

    /// The script lists i2c nodes and logind's directories by glob: a real shell against a
    /// fake `/host` tree (regular files stand in for nodes; nothing is `stat`ed).
    #[test]
    fn the_script_reports_i2c_and_logind_from_a_listing() {
        let root = tempfile::tempdir().unwrap();
        let host = root.path().join("host");
        std::fs::create_dir_all(host.join("dev")).unwrap();
        std::fs::create_dir_all(host.join("run/systemd/seats")).unwrap();
        std::fs::create_dir_all(host.join("run/systemd/sessions")).unwrap();
        for n in ["i2c-4", "i2c-12", "i2c-dev", "i2c-"] {
            std::fs::write(host.join("dev").join(n), "").unwrap();
        }
        let script = SCRIPT.replace("/host/", &format!("{}/", host.display()));
        let out = std::process::Command::new("sh")
            .arg("-c")
            .arg(script)
            .output()
            .expect("sh");
        let report = parse(&String::from_utf8_lossy(&out.stdout)).unwrap();
        assert_eq!(report.i2c, vec![4, 12]);
        assert!(report.logind());
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
