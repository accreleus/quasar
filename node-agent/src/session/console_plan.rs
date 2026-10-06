//! What a console session's container is granted so its desktop can drive the display
//! itself (#453, ADR 0009). Pure: the host facts come in, the `docker run` fragment goes
//! out, and `ContainerRuntime::run` appends it in place of the nested-display grants.

/// Set on a console container; the image's launcher starts its desktop on the DRM backend
/// instead of nested. Contract with quasar-images' KDE and Steam launchers.
pub const DIRECT_DISPLAY_ENV: &str = "QUASAR_DIRECT_DISPLAY";

/// evdev's character major. Bind-mounting `/dev/input` alone is not enough: a device
/// plugged in after start is a new node the device cgroup has never allowed.
const INPUT_CGROUP_RULE: &str = "c 13:* rwm";

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
                    .filter(|path| path.starts_with("/dev/input/") && !path.contains(".."))
                    .ok_or_else(|| {
                        format!("input_devices entry {item} is not a /dev/input node")
                    })?;
                if !nodes.iter().any(|n| n == node) {
                    nodes.push(node.to_string());
                }
            }
            Ok(InputGrant::Nodes(nodes))
        }
        other => Err(format!(
            "input_devices must be \"auto\" or a list of /dev/input nodes, not {other}"
        )),
    }
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
    if host.sound {
        device("/dev/snd");
    }
    let mut bind = |src: &str, read_only: bool| {
        args.push("--mount".to_string());
        args.push(format!(
            "type=bind,src={src},dst={src}{}",
            if read_only { ",readonly" } else { "" }
        ));
    };
    match input {
        InputGrant::All => bind("/dev/input", false),
        InputGrant::Nodes(_) => {}
    }
    // libudev reads device properties from the data dir and takes the control socket's
    // presence to mean udev is running; the hotplug events themselves arrive over netlink,
    // which is why the container shares the host's network namespace.
    bind("/run/udev/data", true);
    bind("/run/udev/control", true);
    match input {
        InputGrant::All => args.extend(["--device-cgroup-rule".into(), INPUT_CGROUP_RULE.into()]),
        InputGrant::Nodes(nodes) => {
            for node in nodes {
                args.extend(["--device".into(), node.clone()]);
            }
        }
    }
    args.extend([
        "--network".into(),
        "host".into(),
        "-e".into(),
        format!("{DIRECT_DISPLAY_ENV}=1"),
    ]);
    args
}

#[cfg(test)]
mod tests {
    use super::*;

    fn host() -> ConsoleHost {
        ConsoleHost {
            card_node: "/dev/dri/card1".into(),
            render_node: Some("/dev/dri/renderD129".into()),
            sound: true,
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
        for bad in [json!("all"), json!(3), json!(["/dev/sda"]), json!([7])] {
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
            ["/dev/dri/card1", "/dev/dri/renderD129", "/dev/snd"]
        );
        assert_eq!(pairs(&args, "--device-cgroup-rule"), ["c 13:* rwm"]);
        assert_eq!(
            pairs(&args, "--mount"),
            [
                "type=bind,src=/dev/input,dst=/dev/input",
                "type=bind,src=/run/udev/data,dst=/run/udev/data,readonly",
                "type=bind,src=/run/udev/control,dst=/run/udev/control,readonly",
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
                "/dev/snd",
                "/dev/input/event3",
                "/dev/input/event7",
            ]
        );
        assert!(pairs(&args, "--device-cgroup-rule").is_empty());
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
    }

    #[test]
    fn the_plan_round_trips_through_the_runtime_request() {
        let mut args: Vec<String> = ["run", "--name", "quasar-sess-x"]
            .map(String::from)
            .to_vec();
        args.extend(console_args(&host(), &InputGrant::All));
        args.push("image".into());
        let request = super::super::container::application_request_for_test(&args);
        assert_eq!(request.network, "host");
        assert_eq!(request.device_cgroup_rules, ["c 13:* rwm"]);
        assert!(request.devices.contains(&"/dev/dri/card1".to_string()));
        assert!(request
            .environment
            .contains(&"QUASAR_DIRECT_DISPLAY=1".to_string()));
    }
}
