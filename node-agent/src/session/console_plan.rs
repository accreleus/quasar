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
