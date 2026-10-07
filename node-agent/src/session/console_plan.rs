//! What a console session's container is granted so its desktop can drive the display
//! itself (#453, ADR 0009). Pure: the host facts come in, the `docker run` fragment goes
//! out, and `ContainerRuntime::run` appends it in place of the nested-display grants.

/// Set on a console container; the image's launcher starts its desktop on the DRM backend
/// instead of nested. Contract with quasar-images' KDE and Steam launchers.
pub const DIRECT_DISPLAY_ENV: &str = "QUASAR_DIRECT_DISPLAY";

/// Where libudev reads device properties, on the host and in the console container.
pub const UDEV_DATA: &str = "/run/udev/data";

/// `1` or `0`: whether the host has a sound device (`/dev/snd`) to hand a console desktop.
/// The agent holds none itself, so the recovery actor reads it each time console
/// mode is turned on and sets it on the agent. The Compose console overlay does not set it:
/// a Compose agent sees the host's `/dev/snd` through the base file's `/dev:/host/dev` bind.
pub const HOST_SOUND_ENV: &str = "QUASAR_HOST_SOUND";

/// The host's sound answer: the recovery actor's when it gave one, else whether the host's
/// `/dev/snd` is visible to the agent (`/host/dev/snd` on a Compose install).
pub fn host_sound(told: Option<&str>, own_dev_snd: bool) -> bool {
    match told.map(str::trim) {
        Some("1") => true,
        Some("0") => false,
        _ => own_dev_snd,
    }
}

/// evdev's character major. Bind-mounting `/dev/input` alone is not enough: a device
/// plugged in after start is a new node the device cgroup has never allowed.
const INPUT_CGROUP_RULE: &str = "c 13:* rwm";

/// ALSA's character major, for a sound card plugged in after start (rootful engines; a
/// rootless engine drops every cgroup rule in the runtime and relies on the host's ACL).
const SOUND_CGROUP_RULE: &str = "c 116:* rwm";

/// Where a hidraw device's sysfs sibling lives relative to its evdev `device` symlink:
/// `event<N>/device` resolves to the HID device's `input/input<M>` directory, so its
/// parent's parent is the HID device directory that also owns `hidraw/hidraw<K>`.
fn hidraw_dir_for_input_device(input_device_dir: &std::path::Path) -> Option<std::path::PathBuf> {
    Some(input_device_dir.parent()?.parent()?.join("hidraw"))
}

/// Which input devices the desktop gets.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum InputGrant {
    /// The host's whole `/dev/input`, including devices plugged in later.
    All,
    /// Only these nodes, as they exist at launch.
    Nodes(Vec<String>),
}

/// What `ContainerRuntime::run` grants a console container in place of the nested
/// display: the host facts plus the input grant.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DirectDisplay {
    pub host: ConsoleHost,
    pub input: InputGrant,
}

/// The host facts a console plan is built from.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConsoleHost {
    /// The card node of the GPU driving the console connector, e.g. `/dev/dri/card0`.
    pub card_node: String,
    /// That GPU's render node, when it has one.
    pub render_node: Option<String>,
    /// The host has `/dev/snd`.
    pub sound: bool,
    /// The `/dev/hidrawN` nodes to grant: the caller resolves these for the chosen
    /// [`InputGrant`] before building the plan — every existing node for `All`, or just
    /// the allowlisted events' resolved siblings for `Nodes` (#462). Steam Input reads
    /// controllers over hidraw; with no hidraw node it falls back to an evdev GUID match
    /// that misnames the pad.
    pub hidraw_nodes: Vec<String>,
    /// hidraw's character major, so a controller plugged in later (an `All` grant only —
    /// `Nodes` grants no hotplug rule) can still open its node once it exists. Dynamic,
    /// read from `/proc/devices` at launch; `None` when the host has no hidraw major
    /// registered at all.
    pub hidraw_major: Option<u32>,
}

/// `console_config.input_devices`: absent or `"auto"` is every device, an array of node
/// paths is exactly those. Anything else refuses the launch rather than guess.
pub fn input_grant(input_devices: &serde_json::Value) -> Result<InputGrant, String> {
    match input_devices {
        serde_json::Value::Null => Ok(InputGrant::All),
        serde_json::Value::String(s) if s == "auto" => Ok(InputGrant::All),
        serde_json::Value::Array(items) => {
            let mut nodes: Vec<String> = Vec::new();
            for item in items {
                let node = item
                    .as_str()
                    .filter(|path| is_event_node(path))
                    .ok_or_else(|| {
                        format!("input_devices entry {item} is not a /dev/input/event node")
                    })?;
                if !nodes.iter().any(|n| n == node) {
                    nodes.push(node.to_string());
                }
            }
            Ok(InputGrant::Nodes(nodes))
        }
        other => Err(format!(
            "input_devices must be \"auto\" or a list of /dev/input/event nodes, not {other}"
        )),
    }
}

/// Why an assignment's topology cannot run here (agent-api.md amendment 19): `dual_output`
/// is retired (it arrives as [`VideoTopology::Unsupported`]), and a console session's app
/// must declare `runtime_spec.direct_display`.
///
/// [`VideoTopology::Unsupported`]: crate::messages::VideoTopology::Unsupported
pub fn topology_refusal(
    topology: crate::messages::VideoTopology,
    app_direct: bool,
) -> Option<String> {
    use crate::messages::VideoTopology;
    match topology {
        VideoTopology::StreamOnly => None,
        VideoTopology::LocalOnly if app_direct => None,
        VideoTopology::LocalOnly => Some(
            "a console session's app must declare runtime_spec.direct_display: this one \
             cannot drive the display directly"
                .into(),
        ),
        VideoTopology::Unsupported => Some(
            "this agent runs stream_only and local_only sessions only (dual_output is \
             retired: a console session is never streamed)"
                .into(),
        ),
    }
}

/// `/dev/input/eventN` exactly: a directory handed to `--device` expands to every node in it.
fn is_event_node(path: &str) -> bool {
    path.strip_prefix("/dev/input/event")
        .is_some_and(|n| !n.is_empty() && n.len() <= 4 && n.bytes().all(|b| b.is_ascii_digit()))
}

/// The output a console desktop drives: `cardN:CONNECTOR` names one, absent or `auto`
/// takes the first connected output. The connector must have a monitor.
pub fn console_output<'a>(
    output_id: Option<&str>,
    outputs: &'a [crate::messages::DrmOutputCapability],
) -> Result<&'a crate::messages::DrmOutputCapability, String> {
    let output = match output_id.filter(|id| *id != "auto") {
        Some(id) => outputs
            .iter()
            .find(|o| o.id == id)
            .ok_or_else(|| format!("console output {id} not found on this host"))?,
        None => outputs
            .iter()
            .find(|o| o.connected)
            .ok_or_else(|| "no console output has a monitor (no monitor connected)".to_string())?,
    };
    if !output.connected {
        return Err(format!(
            "console output {} has no monitor connected",
            output.id
        ));
    }
    Ok(output)
}

/// The grants, as `docker run` arguments `application_request_from_args` understands.
pub fn console_args(host: &ConsoleHost, input: &InputGrant) -> Vec<String> {
    let mut args = Vec::new();
    let mut device = |node: &str| {
        args.push("--device".to_string());
        args.push(node.to_string());
    };
    device(&host.card_node);
    if let Some(render) = &host.render_node {
        device(render);
    }
    // `/dev/hidrawN`'s parent is `/dev` itself, so it is never bind-mounted (that would
    // hand over the whole device directory); each node the caller resolved is passed
    // individually instead, same as an input allowlist's nodes below.
    for node in &host.hidraw_nodes {
        device(node);
    }
    let mut bind = |src: &str| {
        args.push("--mount".to_string());
        args.push(format!("type=bind,src={src},dst={src},readonly"));
    };
    // Read-only: device nodes still open read-write, but container root cannot chmod,
    // chown or unlink the host's own nodes.
    if input == &InputGrant::All {
        bind("/dev/input");
    }
    // Sound is a directory bind, not `--device /dev/snd`: rootless Podman cannot mknod, so
    // it realizes each `--device` node as a bind over an empty regular file, which `readdir`
    // lists as DT_REG. PipeWire's ALSA monitor counts PCM devices from `/dev/snd` entries
    // that are DT_CHR, found none, and ignored the card (#460).
    if host.sound {
        bind("/dev/snd");
    }
    // libudev reads device properties here. Hotplug events arrive over netlink, which is
    // why the container shares the host's network namespace. The host's udev control
    // socket is never handed in (root inside could steer the host's udevd through it);
    // the image makes the placeholder libudev looks for.
    bind(UDEV_DATA);
    match input {
        InputGrant::All => {
            args.extend(["--device-cgroup-rule".into(), INPUT_CGROUP_RULE.into()]);
            // A controller plugged in after launch is a hidraw node the device cgroup
            // has never allowed, same reasoning as the evdev rule above. An allowlisted
            // (`Nodes`) grant gets no such rule: the nodes it was given are exactly what
            // was resolved at launch, nothing more.
            if let Some(major) = host.hidraw_major {
                args.extend(["--device-cgroup-rule".into(), format!("c {major}:* rwm")]);
            }
        }
        InputGrant::Nodes(nodes) => {
            for node in nodes {
                args.extend(["--device".into(), node.clone()]);
            }
        }
    }
    if host.sound {
        args.extend(["--device-cgroup-rule".into(), SOUND_CGROUP_RULE.into()]);
    }
    args.extend([
        "--network".into(),
        "host".into(),
        "-e".into(),
        format!("{DIRECT_DISPLAY_ENV}=1"),
    ]);
    args
}

/// hidraw's character major from a `/proc/devices` listing (its own format: a `Character
/// devices:` section, then `<major> <name>` lines, blank line, `Block devices:`). Dynamic
/// on most kernels (allocated from the misc range), so this cannot be a constant like
/// [`INPUT_CGROUP_RULE`]'s evdev major.
pub fn hidraw_major_from(proc_devices: &str) -> Option<u32> {
    proc_devices.lines().find_map(|line| {
        let (major, name) = line.trim().split_once(char::is_whitespace)?;
        if name.trim() != "hidraw" {
            return None;
        }
        major.trim().parse::<u32>().ok()
    })
}

/// The production root: sysfs is not namespaced (same as `/sys/class/drm`, read without a
/// `/host` mount elsewhere in this agent), so the bare path already answers for the host.
fn sysfs_root() -> &'static std::path::Path {
    std::path::Path::new("/sys")
}

/// An allowlisted `/dev/input/eventN`'s hidraw sibling, if the same HID device also
/// exposes one, read from the real sysfs root.
pub fn hidraw_sibling(event_node: &str) -> Option<String> {
    hidraw_sibling_at(sysfs_root(), event_node)
}

/// Every hidraw device the host has right now, as the host path of its node
/// (`/dev/hidrawN`), for an `InputGrant::All` console. Listed from sysfs, which is not
/// namespaced: an owned agent has neither the host's `/dev/hidraw*` nor `/host/dev`, and
/// needs neither, since the engine resolves a `--device` path on the host.
pub fn host_hidraw_nodes() -> Vec<String> {
    host_hidraw_nodes_at(sysfs_root())
}

/// Injectable-root version of [`host_hidraw_nodes`]: the `class/hidraw/hidrawN` entries
/// under `sysfs_root`, in device-number order.
pub fn host_hidraw_nodes_at(sysfs_root: &std::path::Path) -> Vec<String> {
    let Ok(entries) = std::fs::read_dir(sysfs_root.join("class/hidraw")) else {
        return Vec::new();
    };
    let mut numbers: Vec<u32> = entries
        .flatten()
        .filter_map(|entry| {
            let name = entry.file_name();
            let digits = name.to_str()?.strip_prefix("hidraw")?;
            (!digits.is_empty() && digits.bytes().all(|b| b.is_ascii_digit()))
                .then(|| digits.parse().ok())
                .flatten()
        })
        .collect();
    numbers.sort_unstable();
    numbers.dedup();
    numbers
        .into_iter()
        .map(|n| format!("/dev/hidraw{n}"))
        .collect()
}

/// Injectable-root version of [`hidraw_sibling`], so a test can point at a tempdir built
/// to mimic `/sys/class/input/event<N>/device/../../hidraw/hidraw<M>` instead of the real
/// sysfs tree.
pub fn hidraw_sibling_at(sysfs_root: &std::path::Path, event_node: &str) -> Option<String> {
    let event_name = std::path::Path::new(event_node).file_name()?.to_str()?;
    let device_link = sysfs_root
        .join("class/input")
        .join(event_name)
        .join("device");
    let input_device_dir = std::fs::canonicalize(device_link).ok()?;
    let hidraw_dir = hidraw_dir_for_input_device(&input_device_dir)?;
    std::fs::read_dir(hidraw_dir)
        .ok()?
        .flatten()
        .find_map(|entry| {
            let name = entry.file_name().to_str()?.to_string();
            name.starts_with("hidraw").then(|| format!("/dev/{name}"))
        })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn host() -> ConsoleHost {
        ConsoleHost {
            card_node: "/dev/dri/card1".into(),
            render_node: Some("/dev/dri/renderD129".into()),
            sound: true,
            hidraw_nodes: Vec::new(),
            hidraw_major: None,
        }
    }

    fn pairs(args: &[String], flag: &str) -> Vec<String> {
        args.windows(2)
            .filter(|w| w[0] == flag)
            .map(|w| w[1].clone())
            .collect()
    }

    fn output(id: &str, connected: bool) -> crate::messages::DrmOutputCapability {
        let (card, connector) = id.split_once(':').unwrap();
        crate::messages::DrmOutputCapability {
            id: id.into(),
            card: card.into(),
            render_node: None,
            connector: connector.into(),
            connected,
            active_mode: None,
            modes: Vec::new(),
        }
    }

    #[test]
    fn only_direct_console_apps_and_no_other_topology_are_accepted() {
        use crate::messages::VideoTopology::*;
        assert_eq!(topology_refusal(StreamOnly, false), None);
        assert_eq!(topology_refusal(StreamOnly, true), None);
        assert_eq!(topology_refusal(LocalOnly, true), None);
        assert!(topology_refusal(LocalOnly, false)
            .unwrap()
            .contains("direct_display"));
        assert!(topology_refusal(Unsupported, true)
            .unwrap()
            .contains("stream_only and local_only"));
    }

    #[test]
    fn the_host_sound_answer_is_the_actors_when_it_gave_one() {
        assert!(host_sound(Some("1"), false));
        assert!(!host_sound(Some("0"), true));
        assert!(host_sound(None, true));
        assert!(!host_sound(None, false));
        assert!(host_sound(Some("garbage"), true));
    }

    #[test]
    fn input_devices_auto_absent_or_a_list() {
        use serde_json::json;
        assert_eq!(input_grant(&json!(null)), Ok(InputGrant::All));
        assert_eq!(input_grant(&json!("auto")), Ok(InputGrant::All));
        assert_eq!(
            input_grant(&json!([
                "/dev/input/event3",
                "/dev/input/event3",
                "/dev/input/event5"
            ])),
            Ok(InputGrant::Nodes(vec![
                "/dev/input/event3".into(),
                "/dev/input/event5".into()
            ]))
        );
        for bad in [
            json!("all"),
            json!(3),
            json!(["/dev/sda"]),
            json!([7]),
            json!(["/dev/input/"]),
            json!(["/dev/input/."]),
            json!(["/dev/input/by-id"]),
            json!(["/dev/input/event"]),
            json!(["/dev/input/event3/../../sda"]),
            json!(["/dev/input/mouse0"]),
        ] {
            assert!(input_grant(&bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn auto_takes_the_first_connected_output() {
        let outputs = [
            output("card0:DP-1", false),
            output("card0:DP-2", true),
            output("card1:HDMI-A-1", true),
        ];
        for id in [None, Some("auto")] {
            assert_eq!(console_output(id, &outputs).unwrap().id, "card0:DP-2");
        }
    }

    #[test]
    fn a_named_output_must_exist_and_have_a_monitor() {
        let outputs = [output("card0:DP-1", false), output("card1:HDMI-A-1", true)];
        assert_eq!(
            console_output(Some("card1:HDMI-A-1"), &outputs).unwrap().id,
            "card1:HDMI-A-1"
        );
        assert!(console_output(Some("card0:DP-1"), &outputs)
            .unwrap_err()
            .contains("no monitor"));
        assert!(console_output(Some("card2:DP-1"), &outputs)
            .unwrap_err()
            .contains("not found"));
        assert!(console_output(None, &[output("card0:DP-1", false)])
            .unwrap_err()
            .contains("no monitor"));
    }

    #[test]
    fn all_input_binds_the_directory_with_the_hotplug_rule() {
        let args = console_args(&host(), &InputGrant::All);
        assert_eq!(
            pairs(&args, "--device"),
            ["/dev/dri/card1", "/dev/dri/renderD129"]
        );
        assert_eq!(
            pairs(&args, "--device-cgroup-rule"),
            ["c 13:* rwm", "c 116:* rwm"]
        );
        assert_eq!(
            pairs(&args, "--mount"),
            [
                "type=bind,src=/dev/input,dst=/dev/input,readonly",
                "type=bind,src=/dev/snd,dst=/dev/snd,readonly",
                "type=bind,src=/run/udev/data,dst=/run/udev/data,readonly",
            ]
        );
        assert_eq!(pairs(&args, "--network"), ["host"]);
        assert_eq!(pairs(&args, "-e"), ["QUASAR_DIRECT_DISPLAY=1"]);
    }

    #[test]
    fn an_allowlist_passes_only_its_nodes_and_no_hotplug_rule() {
        let input = InputGrant::Nodes(vec!["/dev/input/event3".into(), "/dev/input/event7".into()]);
        let args = console_args(&host(), &input);
        assert_eq!(
            pairs(&args, "--device"),
            [
                "/dev/dri/card1",
                "/dev/dri/renderD129",
                "/dev/input/event3",
                "/dev/input/event7",
            ]
        );
        // No evdev/hidraw hotplug rule for an allowlist; sound still gets its own.
        assert_eq!(pairs(&args, "--device-cgroup-rule"), ["c 116:* rwm"]);
        assert!(!pairs(&args, "--mount")
            .iter()
            .any(|m| m.contains("dst=/dev/input")));
        // udev still tells the desktop what the allowed devices are.
        assert!(pairs(&args, "--mount")
            .iter()
            .any(|m| m.contains("dst=/run/udev/data")));
    }

    #[test]
    fn missing_render_node_and_sound_are_left_out() {
        let bare = ConsoleHost {
            render_node: None,
            sound: false,
            ..host()
        };
        let args = console_args(&bare, &InputGrant::All);
        assert_eq!(pairs(&args, "--device"), ["/dev/dri/card1"]);
        assert_eq!(pairs(&args, "--device-cgroup-rule"), ["c 13:* rwm"]);
        assert!(!pairs(&args, "--mount")
            .iter()
            .any(|m| m.contains("/dev/snd")));
    }

    #[test]
    fn all_input_also_grants_every_hidraw_node_with_the_major_rule() {
        let with_hidraw = ConsoleHost {
            hidraw_nodes: vec!["/dev/hidraw0".into(), "/dev/hidraw3".into()],
            hidraw_major: Some(242),
            ..host()
        };
        let args = console_args(&with_hidraw, &InputGrant::All);
        assert_eq!(
            pairs(&args, "--device"),
            [
                "/dev/dri/card1",
                "/dev/dri/renderD129",
                "/dev/hidraw0",
                "/dev/hidraw3",
            ]
        );
        assert_eq!(
            pairs(&args, "--device-cgroup-rule"),
            ["c 13:* rwm", "c 242:* rwm", "c 116:* rwm"]
        );
    }

    #[test]
    fn a_host_with_no_hidraw_major_grants_no_hidraw_rule() {
        // Absent hidraw entirely: no nodes, no major (the `host()` fixture default).
        // Behaves exactly as it did before #462.
        let args = console_args(&host(), &InputGrant::All);
        assert!(!pairs(&args, "--device")
            .iter()
            .any(|d| d.contains("hidraw")));
        assert_eq!(
            pairs(&args, "--device-cgroup-rule"),
            ["c 13:* rwm", "c 116:* rwm"]
        );
    }

    #[test]
    fn sound_is_a_read_only_directory_bind_and_never_a_device_node() {
        // Rootless Podman would list a `--device /dev/snd` node as a regular file (#460).
        for input in [
            InputGrant::All,
            InputGrant::Nodes(vec!["/dev/input/event3".into()]),
        ] {
            let args = console_args(&host(), &input);
            assert!(!pairs(&args, "--device").iter().any(|d| d.contains("snd")));
            assert!(pairs(&args, "--mount")
                .contains(&"type=bind,src=/dev/snd,dst=/dev/snd,readonly".to_string()));
            assert!(pairs(&args, "--device-cgroup-rule").contains(&"c 116:* rwm".to_string()));
        }
    }

    #[test]
    fn an_allowlisted_events_resolved_hidraw_sibling_is_granted_with_no_rule() {
        // The caller already resolved event11's sibling through sysfs before building the
        // plan; `console_args` itself does no I/O.
        let resolved = ConsoleHost {
            hidraw_nodes: vec!["/dev/hidraw5".into()],
            ..host()
        };
        let input = InputGrant::Nodes(vec!["/dev/input/event11".into()]);
        let args = console_args(&resolved, &input);
        assert_eq!(
            pairs(&args, "--device"),
            [
                "/dev/dri/card1",
                "/dev/dri/renderD129",
                "/dev/hidraw5",
                "/dev/input/event11",
            ]
        );
        // Only the sound rule: no evdev or hidraw hotplug rule for an allowlist.
        assert_eq!(pairs(&args, "--device-cgroup-rule"), ["c 116:* rwm"]);
    }

    #[test]
    fn an_event_with_no_hidraw_sibling_grants_nothing_extra() {
        // The caller found no sibling for this event (e.g. a sound card's jack-sense input,
        // or the power button: real evdev devices with no HID backing at all), so
        // hidraw_nodes stays empty and only the allowlisted event itself is passed.
        let input = InputGrant::Nodes(vec!["/dev/input/event0".into()]);
        let args = console_args(&host(), &input);
        assert_eq!(
            pairs(&args, "--device"),
            ["/dev/dri/card1", "/dev/dri/renderD129", "/dev/input/event0",]
        );
    }

    /// `<root>/class/input/event<N>/device` symlinked to `<root>/devices/<hid>/input/input<M>`,
    /// mimicking the real sysfs layout (verified by reading a live console host's
    /// `/sys/class/input/event*/device` structure for #462): an evdev device's `device`
    /// symlink resolves to the HID device's own `input/inputM` subdirectory, so `hidraw`
    /// is a sibling of the HID device two levels up from there, not one.
    fn fake_sysfs_hid_event(
        root: &std::path::Path,
        event_name: &str,
        hid_name: &str,
        input_name: &str,
    ) -> std::path::PathBuf {
        let hid_dir = root.join("devices").join(hid_name);
        let input_dir = hid_dir.join("input").join(input_name);
        std::fs::create_dir_all(&input_dir).unwrap();
        let event_dir = root.join("class/input").join(event_name);
        std::fs::create_dir_all(&event_dir).unwrap();
        std::os::unix::fs::symlink(&input_dir, event_dir.join("device")).unwrap();
        hid_dir
    }

    #[test]
    fn hidraw_sibling_at_resolves_through_the_hid_devices_sysfs_layout() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        let hid_dir = fake_sysfs_hid_event(root, "event11", "0003:342D:E3E7.0023", "input485");
        std::fs::create_dir_all(hid_dir.join("hidraw/hidraw5")).unwrap();

        assert_eq!(
            hidraw_sibling_at(root, "/dev/input/event11"),
            Some("/dev/hidraw5".to_string())
        );
    }

    #[test]
    fn host_hidraw_nodes_are_listed_from_sysfs_as_host_paths() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        assert!(
            host_hidraw_nodes_at(root).is_empty(),
            "no class/hidraw at all"
        );
        let class = root.join("class/hidraw");
        for name in [
            "hidraw10", "hidraw2", "hidraw0", "hidrawx", "hidraw", "other",
        ] {
            std::fs::create_dir_all(class.join(name)).unwrap();
        }
        assert_eq!(
            host_hidraw_nodes_at(root),
            vec!["/dev/hidraw0", "/dev/hidraw2", "/dev/hidraw10"]
        );
    }

    #[test]
    fn hidraw_sibling_at_is_none_with_no_hidraw_sibling() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        // A real evdev device with no HID backing at all (e.g. the power button): its HID
        // device directory exists but never grows a `hidraw/` subdirectory.
        fake_sysfs_hid_event(root, "event1", "LNXPWRBN:00", "input19");

        assert_eq!(hidraw_sibling_at(root, "/dev/input/event1"), None);
    }

    #[test]
    fn hidraw_sibling_at_is_none_for_an_unresolvable_event() {
        let dir = tempfile::tempdir().unwrap();
        assert_eq!(hidraw_sibling_at(dir.path(), "/dev/input/event99"), None);
    }

    #[test]
    fn hidraw_major_from_proc_devices_listing() {
        let listing = "Character devices:\n  1 mem\n  4 /dev/vc/0\n 13 input\n242 hidraw\n\n\
                        Block devices:\n  8 sd\n";
        assert_eq!(hidraw_major_from(listing), Some(242));
        assert_eq!(
            hidraw_major_from("Character devices:\n  1 mem\n\nBlock devices:\n  8 sd\n"),
            None
        );
    }

    #[test]
    fn the_plan_round_trips_through_the_runtime_request() {
        let with_hidraw = ConsoleHost {
            hidraw_nodes: vec!["/dev/hidraw0".into()],
            hidraw_major: Some(242),
            ..host()
        };
        let mut args: Vec<String> = ["run", "--name", "quasar-sess-x"]
            .map(String::from)
            .to_vec();
        args.extend(console_args(&with_hidraw, &InputGrant::All));
        args.push("image".into());
        let request = super::super::container::application_request_for_test(&args);
        assert_eq!(request.network, "host");
        assert_eq!(
            request.device_cgroup_rules,
            ["c 13:* rwm", "c 242:* rwm", "c 116:* rwm"]
        );
        assert!(request.devices.contains(&"/dev/dri/card1".to_string()));
        assert!(!request.devices.iter().any(|d| d.contains("snd")));
        assert!(request.is_valid());
        assert!(request.devices.contains(&"/dev/hidraw0".to_string()));
        assert!(request
            .environment
            .contains(&"QUASAR_DIRECT_DISPLAY=1".to_string()));
    }
}
