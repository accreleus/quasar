//! Golden rendered specifications per role, revision and GPU vendor (ADR 0008). A change
//! to a rendered shape must show up as a reviewed diff of these files; regenerate with
//! `QUASAR_UPDATE_GOLDEN=1 cargo test -p quasar-recovery --test recipe_golden`.

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};

use quasar_recovery::recipe::{
    names, render, secrets, Book, ControlInputs, DatabaseInputs, GpuFacts, GpuVendor, HostDevices,
    ImageRef, Inputs, RenderError, Role, SecretMounts,
};

const AGENT_IMAGE: &str = "registry.example.invalid/quasar/quasar-node-agent@sha256:bb22000000000000000000000000000000000000000000000000000000000000";
const ACTOR_IMAGE: &str = "registry.example.invalid/quasar/quasar-recovery@sha256:cc33000000000000000000000000000000000000000000000000000000000000";

fn golden_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/recovery/recipes")
}

pub fn inputs(vendor: Option<GpuVendor>) -> Inputs {
    let (render_node, gpus_served) = match vendor {
        Some(GpuVendor::Nvidia) => (Some("/dev/dri/renderD128"), true),
        Some(GpuVendor::Amd) => (Some("/dev/dri/renderD129"), false),
        Some(GpuVendor::Intel) => (Some("/dev/dri/renderD128"), false),
        None => (None, false),
    };
    Inputs {
        unknown: Default::default(),
        installation_id: "5f0c1e0e-0c5a-4d1b-9a2f-3e4d5c6b7a89".into(),
        node_name: "gpu-host-01".into(),
        home_root: "/srv/quasar/homes".into(),
        template_root: "/var/lib/quasar/templates".into(),
        docker_socket: "/var/run/docker.sock".into(),
        gpu: GpuFacts {
            cdi: false,
            unknown: Default::default(),
            vendor,
            render_node: render_node.map(str::to_owned),
            gpus_served,
            fallback: None,
        },
        devices: HostDevices {
            unknown: Default::default(),
            kernel_log: false,
            engine_rootless: false,
            host_sysfs: false,
            dri_nodes: vec![],
            sound: false,
            i2c: Vec::new(),
            logind: false,
            console_audio: false,
            console_vt: false,
            udev_data: None,
            host_sound: None,
            fuse: false,
            dri: vendor.is_some(),
            uinput: true,
            kmsg: true,
        },
        control: None,
        socket_dir: None,
        trust: Default::default(),
        enroll: Default::default(),
        app: Default::default(),
        console: false,
        console_vt_kept: false,
        agent_variables: Default::default(),
    }
}

fn agent_secrets() -> SecretMounts {
    SecretMounts {
        volume: Some(names::NODE_AGENT_SECRETS_VOLUME.into()),
        files: BTreeSet::from([secrets::ENROLLMENT.to_string()]),
    }
}

fn check(name: &str, spec: &impl serde::Serialize) {
    let path = golden_dir().join(name);
    let rendered = serde_json::to_string_pretty(spec).unwrap() + "\n";
    if std::env::var_os("QUASAR_UPDATE_GOLDEN").is_some() {
        std::fs::create_dir_all(golden_dir()).unwrap();
        std::fs::write(&path, &rendered).unwrap();
    }
    let golden = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("{}: {e} (QUASAR_UPDATE_GOLDEN=1 writes it)", path.display()));
    assert_eq!(rendered, golden, "{name} differs from its golden file");
}

#[test]
fn node_agent_revision_1_renders_its_golden_specification_per_vendor() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    for (vendor, file) in [
        (Some(GpuVendor::Nvidia), "node-agent-r1-nvidia.json"),
        (Some(GpuVendor::Amd), "node-agent-r1-amd.json"),
        (Some(GpuVendor::Intel), "node-agent-r1-intel.json"),
        (None, "node-agent-r1-none.json"),
    ] {
        let spec = render(
            Role::NodeAgent,
            1,
            &inputs(vendor),
            &image,
            &agent_secrets(),
        )
        .unwrap();
        check(file, &spec);
    }
}

#[test]
fn node_agent_revision_3_renders_its_golden_specification_per_vendor() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    for (vendor, file) in [
        (Some(GpuVendor::Nvidia), "node-agent-r3-nvidia.json"),
        (Some(GpuVendor::Amd), "node-agent-r3-amd.json"),
        (Some(GpuVendor::Intel), "node-agent-r3-intel.json"),
        (None, "node-agent-r3-none.json"),
    ] {
        let spec = render(
            Role::NodeAgent,
            3,
            &inputs(vendor),
            &image,
            &agent_secrets(),
        )
        .unwrap();
        check(file, &spec);
    }
}

/// RH-07 #402 (D1, P1): revision 3 is one least-privilege recipe for every engine mode.
/// The agent holds none of the host's /dev, NET_ADMIN, SYSLOG or /dev/kmsg, and carries
/// label=disable because it mounts the engine socket. Kernel-log access, an optional
/// diagnostic, comes back only when the host allows unprivileged reads of it.
#[test]
fn node_agent_revision_3_holds_none_of_the_removed_access() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    for vendor in [Some(GpuVendor::Nvidia), Some(GpuVendor::Amd), None] {
        let spec = render(
            Role::NodeAgent,
            3,
            &inputs(vendor),
            &image,
            &agent_secrets(),
        )
        .unwrap();
        assert!(
            !spec
                .binds
                .iter()
                .any(|b| b.source == "/dev" || b.target == "/host/dev"),
            "{vendor:?}: host /dev is mounted"
        );
        assert!(spec.cap_add.is_empty(), "{vendor:?}: {:?}", spec.cap_add);
        assert!(
            spec.security_opt.iter().any(|o| o == "label=disable"),
            "{vendor:?}: the engine socket needs label=disable on SELinux hosts"
        );
        assert!(
            !spec.devices.iter().any(|d| d.host == "/dev/kmsg"),
            "{vendor:?}: /dev/kmsg without the host allowing it"
        );
    }
    let mut allowed = inputs(Some(GpuVendor::Nvidia));
    allowed.devices.kernel_log = true;
    let spec = render(Role::NodeAgent, 3, &allowed, &image, &agent_secrets()).unwrap();
    let kmsg = spec
        .devices
        .iter()
        .find(|d| d.host == "/dev/kmsg")
        .expect("kmsg when allowed");
    assert_eq!(kmsg.permissions, "r");
    assert!(
        spec.cap_add.is_empty(),
        "never SYSLOG: the host setting is what allows the read"
    );
    // On a rootless engine, which refuses device-cgroup rules, revision 3 carries none.
    let mut rootless = inputs(Some(GpuVendor::Nvidia));
    rootless.devices.engine_rootless = true;
    let spec = render(Role::NodeAgent, 3, &rootless, &image, &agent_secrets()).unwrap();
    assert!(
        spec.device_cgroup_rules.is_empty(),
        "{:?}",
        spec.device_cgroup_rules
    );
    check("node-agent-r3-nvidia-rootless.json", &spec);
    // Where the engine served the GPU through CDI, revision 3 asks by CDI.
    let mut cdi = rootless.clone();
    cdi.gpu.cdi = true;
    let spec = render(Role::NodeAgent, 3, &cdi, &image, &agent_secrets()).unwrap();
    assert_eq!(spec.gpus.len(), 1);
    assert_eq!(spec.gpus[0].driver.as_deref(), Some("cdi"));
    assert_eq!(
        spec.gpus[0].device_ids,
        vec!["nvidia.com/gpu=all".to_string()]
    );
    check("node-agent-r3-nvidia-rootless-cdi.json", &spec);
    // Rootless Docker mounts no sysfs for a host-network container: the host's, read-only.
    let mut docker_rootless = cdi.clone();
    docker_rootless.devices.host_sysfs = true;
    let spec = render(
        Role::NodeAgent,
        3,
        &docker_rootless,
        &image,
        &agent_secrets(),
    )
    .unwrap();
    check("node-agent-r3-nvidia-docker-rootless.json", &spec);
    // Revisions 1 and 2 never ask by CDI.
    let r2 = render(Role::NodeAgent, 2, &cdi, &image, &agent_secrets()).unwrap();
    assert!(r2.gpus.iter().all(|g| g.device_ids.is_empty()));
    // Revisions 1 and 2 render exactly as released.
    let r1 = render(
        Role::NodeAgent,
        1,
        &inputs(Some(GpuVendor::Nvidia)),
        &image,
        &agent_secrets(),
    )
    .unwrap();
    assert!(r1.cap_add.contains(&"NET_ADMIN".to_string()));
}

/// RH-07 #395: console mode adds exactly the console additions to revision 3, on a rootful
/// engine with the host's /dev/dri, and nothing else changes.
#[test]
fn node_agent_revision_3_with_console_mode_adds_only_the_console_additions() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let owned = combined(DatabaseInputs::Owned);
    for (plain, secrets, file) in [
        (
            inputs(Some(GpuVendor::Nvidia)),
            agent_secrets(),
            "node-agent-r3-nvidia-console.json",
        ),
        (
            inputs(Some(GpuVendor::Amd)),
            agent_secrets(),
            "node-agent-r3-amd-console.json",
        ),
        (
            owned,
            local_agent_secrets(),
            "node-agent-r3-combined-amd-console.json",
        ),
    ] {
        let mut console = plain.clone();
        console.console = true;
        console.devices.sound = true;
        let without = render(Role::NodeAgent, 3, &plain, &image, &secrets).unwrap();
        let with = render(Role::NodeAgent, 3, &console, &image, &secrets).unwrap();
        check(file, &with);

        assert_eq!(with.cap_add, vec!["SYS_ADMIN".to_string()], "{file}");
        let added = |a: Vec<String>, b: Vec<String>| -> Vec<String> {
            b.into_iter().filter(|x| !a.contains(x)).collect()
        };
        let binds = |s: &quasar_recovery::recipe::ContainerSpec| -> Vec<String> {
            s.binds.iter().map(|b| b.to_engine()).collect()
        };
        assert_eq!(
            added(binds(&without), binds(&with)),
            vec![
                "/dev/snd:/dev/snd".to_string(),
                "/proc/asound:/host-proc/asound:ro".to_string()
            ],
            "{file}"
        );
        assert!(
            added(binds(&with), binds(&without)).is_empty(),
            "{file}: a bind was dropped"
        );
        assert_eq!(
            added(
                without.device_cgroup_rules.clone(),
                with.device_cgroup_rules.clone()
            ),
            vec!["c 116:* rw".to_string(), "c 89:* rmw".to_string()],
            "{file}"
        );
        let mut env = with.env.clone();
        assert_eq!(
            env.remove("QUASAR_CONSOLE_ACCESS").as_deref(),
            Some("1"),
            "{file}"
        );
        assert_eq!(env, without.env, "{file}");
        assert_eq!(with.devices, without.devices, "{file}");
        assert_eq!(with.security_opt, without.security_opt, "{file}");
        assert_eq!(with.gpus, without.gpus, "{file}");
        assert_ne!(
            with.labels["io.quasar.spec"], without.labels["io.quasar.spec"],
            "{file}"
        );
        assert!(!without.env.contains_key("QUASAR_CONSOLE_ACCESS"), "{file}");

        // A host without sound: console mode without the sound devices, and nothing the
        // engine would have to create.
        let mut quiet = console.clone();
        quiet.devices.sound = false;
        let quiet = render(Role::NodeAgent, 3, &quiet, &image, &secrets).unwrap();
        assert_eq!(quiet.cap_add, vec!["SYS_ADMIN".to_string()], "{file}");
        assert_eq!(binds(&quiet), binds(&without), "{file}");
        assert_eq!(
            added(
                without.device_cgroup_rules.clone(),
                quiet.device_cgroup_rules
            ),
            vec!["c 89:* rmw".to_string()],
            "{file}"
        );
        assert_eq!(
            quiet.env.get("QUASAR_CONSOLE_ACCESS").map(String::as_str),
            Some("1"),
            "{file}"
        );
    }
}

#[test]
fn node_agent_revision_4_renders_its_golden_specification_per_vendor() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    for (vendor, file) in [
        (Some(GpuVendor::Nvidia), "node-agent-r4-nvidia.json"),
        (Some(GpuVendor::Amd), "node-agent-r4-amd.json"),
        (Some(GpuVendor::Intel), "node-agent-r4-intel.json"),
        (None, "node-agent-r4-none.json"),
    ] {
        let r4 = render(
            Role::NodeAgent,
            4,
            &inputs(vendor),
            &image,
            &agent_secrets(),
        )
        .unwrap();
        check(file, &r4);
        // Without console mode revision 4 is revision 3: only the console grants moved.
        let mut r3 = render(
            Role::NodeAgent,
            3,
            &inputs(vendor),
            &image,
            &agent_secrets(),
        )
        .unwrap();
        r3.labels = r4.labels.clone();
        assert_eq!(r3, r4, "{file}");
    }
}

/// ADR 0009: from revision 4 the console session's container, not the agent, holds
/// the screen, the input and the sound device. Console mode gives the agent only what it
/// launches and watches a console desktop with: logind's state to name a display holder,
/// the i2c nodes for DDC (a rootful `c 89` rule, or each node on a rootless engine), the
/// console VT, and the host facts it cannot read (udev data, sound). No capability, no
/// sound device, no ALSA rule, no PipeWire socket, whatever the host has.
#[test]
fn node_agent_revision_4_with_console_mode_gives_the_agent_only_its_own_console_grants() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let rootless = |vendor, host_sysfs: bool, cdi: bool| {
        let mut i = inputs(Some(vendor));
        i.devices.engine_rootless = true;
        i.devices.host_sysfs = host_sysfs;
        i.gpu.cdi = cdi;
        i
    };
    let owned = combined(DatabaseInputs::Owned);
    for (plain, secrets, file) in [
        (
            inputs(Some(GpuVendor::Nvidia)),
            agent_secrets(),
            "node-agent-r4-nvidia-console.json",
        ),
        (
            inputs(Some(GpuVendor::Amd)),
            agent_secrets(),
            "node-agent-r4-amd-console.json",
        ),
        (
            owned,
            local_agent_secrets(),
            "node-agent-r4-combined-amd-console.json",
        ),
        (
            rootless(GpuVendor::Nvidia, false, true),
            agent_secrets(),
            "node-agent-r4-nvidia-rootless-console.json",
        ),
        (
            rootless(GpuVendor::Nvidia, true, true),
            agent_secrets(),
            "node-agent-r4-nvidia-docker-rootless-console.json",
        ),
        (
            rootless(GpuVendor::Amd, false, false),
            agent_secrets(),
            "node-agent-r4-amd-rootless-console.json",
        ),
    ] {
        let rootless = plain.devices.engine_rootless;
        let mut console = plain.clone();
        console.console = true;
        // Everything a host can have; revision 3 would grant the agent all of it.
        console.devices.sound = true;
        console.devices.console_audio = true;
        console.devices.logind = true;
        console.devices.console_vt = true;
        console.devices.udev_data = Some(true);
        console.devices.host_sound = Some(true);
        console.devices.i2c = vec![3, 12];
        let without = render(Role::NodeAgent, 4, &plain, &image, &secrets).unwrap();
        let with = render(Role::NodeAgent, 4, &console, &image, &secrets).unwrap();
        check(file, &with);

        assert_eq!(with.cap_add, without.cap_add, "{file}: no capability");
        let binds = |s: &quasar_recovery::recipe::ContainerSpec| -> Vec<String> {
            s.binds.iter().map(|b| b.to_engine()).collect()
        };
        let added: Vec<String> = binds(&with)
            .into_iter()
            .filter(|b| !binds(&without).contains(b))
            .collect();
        assert_eq!(
            added,
            vec![
                "/run/systemd/seats:/host/run/systemd/seats:ro".to_string(),
                "/run/systemd/sessions:/host/run/systemd/sessions:ro".to_string(),
            ],
            "{file}"
        );
        assert!(
            binds(&without).iter().all(|b| binds(&with).contains(b)),
            "{file}: a bind was dropped"
        );
        let new_devices: Vec<String> = with
            .devices
            .iter()
            .filter(|d| !without.devices.contains(d))
            .map(|d| format!("{}:{}:{}", d.host, d.container, d.permissions))
            .collect();
        let new_rules: Vec<String> = with
            .device_cgroup_rules
            .iter()
            .filter(|r| !without.device_cgroup_rules.contains(r))
            .cloned()
            .collect();
        if rootless {
            assert_eq!(
                new_devices,
                vec![
                    "/dev/i2c-3:/dev/i2c-3:rw".to_string(),
                    "/dev/i2c-12:/dev/i2c-12:rw".to_string(),
                    "/dev/tty8:/dev/tty8:rw".to_string(),
                ],
                "{file}"
            );
            assert!(with.device_cgroup_rules.is_empty(), "{file}");
        } else {
            assert_eq!(
                new_devices,
                vec!["/dev/tty8:/dev/tty8:rw".to_string()],
                "{file}"
            );
            assert_eq!(new_rules, vec!["c 89:* rmw".to_string()], "{file}");
        }
        let mut env = with.env.clone();
        for (key, value) in [
            ("QUASAR_CONSOLE_ACCESS", "1"),
            ("QUASAR_HOST_SOUND", "1"),
            ("QUASAR_HOST_UDEV_DATA", "1"),
        ] {
            assert_eq!(env.remove(key).as_deref(), Some(value), "{file}: {key}");
        }
        assert_eq!(env, without.env, "{file}");
        assert_eq!(with.security_opt, without.security_opt, "{file}");
        assert_eq!(with.gpus, without.gpus, "{file}");

        // A host with none of them, read: told there is no sound device, and nothing the
        // engine would have to find on the host.
        let mut bare = console.clone();
        bare.devices.sound = false;
        bare.devices.host_sound = Some(false);
        bare.devices.console_audio = false;
        bare.devices.logind = false;
        bare.devices.console_vt = false;
        bare.devices.udev_data = None;
        bare.devices.i2c.clear();
        let quiet = render(Role::NodeAgent, 4, &bare, &image, &secrets).unwrap();
        assert_eq!(binds(&quiet), binds(&without), "{file}");
        assert_eq!(quiet.devices, without.devices, "{file}");
        assert_eq!(quiet.cap_add, without.cap_add, "{file}");
        assert_eq!(
            quiet.env.get("QUASAR_HOST_SOUND").map(String::as_str),
            Some("0"),
            "{file}"
        );
        assert!(!quiet.env.contains_key("QUASAR_HOST_UDEV_DATA"), "{file}");
        // Not read yet (machine state from before revision 4): no answer rendered at all.
        bare.devices.host_sound = None;
        let unread = render(Role::NodeAgent, 4, &bare, &image, &secrets).unwrap();
        assert!(!unread.env.contains_key("QUASAR_HOST_SOUND"), "{file}");
    }
}

/// Machine state written with console mode off reads and writes back without the field, so
/// every existing machine and golden stays as it was.
#[test]
fn console_mode_off_is_not_written() {
    let plain = inputs(Some(GpuVendor::Amd));
    let json = serde_json::to_value(&plain).unwrap();
    assert!(json.get("console").is_none(), "{json}");
    let read: Inputs = serde_json::from_value(json).unwrap();
    assert!(!read.console);
    let mut on = plain;
    on.console = true;
    assert_eq!(serde_json::to_value(&on).unwrap()["console"], true);
}

/// RH-07 #407: console mode on a rootless engine. No capability (SYS_ADMIN never reaches the
/// initial user namespace the kernel checks, and a free display needs none) and no
/// device-cgroup rule (a rootless engine refuses them); the i2c nodes the host has are
/// passed as devices, since the agent cannot mknod them; sound and logind's state are bound
/// only where the host has them.
#[test]
fn node_agent_revision_3_with_console_mode_on_a_rootless_engine() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let rootless = |vendor, host_sysfs: bool, cdi: bool| {
        let mut i = inputs(Some(vendor));
        i.devices.engine_rootless = true;
        i.devices.host_sysfs = host_sysfs;
        i.gpu.cdi = cdi;
        i
    };
    for (plain, file) in [
        (
            rootless(GpuVendor::Nvidia, false, true),
            "node-agent-r3-nvidia-rootless-console.json",
        ),
        (
            rootless(GpuVendor::Nvidia, true, true),
            "node-agent-r3-nvidia-docker-rootless-console.json",
        ),
        (
            rootless(GpuVendor::Amd, false, false),
            "node-agent-r3-amd-rootless-console.json",
        ),
    ] {
        let mut console = plain.clone();
        console.console = true;
        console.devices.sound = true;
        console.devices.logind = true;
        console.devices.i2c = vec![3, 12];
        let without = render(Role::NodeAgent, 3, &plain, &image, &agent_secrets()).unwrap();
        let with = render(Role::NodeAgent, 3, &console, &image, &agent_secrets()).unwrap();
        check(file, &with);

        assert!(with.cap_add.is_empty(), "{file}: {:?}", with.cap_add);
        assert!(
            with.device_cgroup_rules.is_empty(),
            "{file}: {:?}",
            with.device_cgroup_rules
        );
        let binds = |s: &quasar_recovery::recipe::ContainerSpec| -> Vec<String> {
            s.binds.iter().map(|b| b.to_engine()).collect()
        };
        let added: Vec<String> = binds(&with)
            .into_iter()
            .filter(|b| !binds(&without).contains(b))
            .collect();
        assert_eq!(
            added,
            vec![
                "/dev/snd:/dev/snd".to_string(),
                "/proc/asound:/host-proc/asound:ro".to_string(),
                "/run/systemd/seats:/host/run/systemd/seats:ro".to_string(),
                "/run/systemd/sessions:/host/run/systemd/sessions:ro".to_string(),
            ],
            "{file}"
        );
        assert!(
            binds(&without).iter().all(|b| binds(&with).contains(b)),
            "{file}: a bind was dropped"
        );
        let new_devices: Vec<(String, String, String)> = with
            .devices
            .iter()
            .filter(|d| !without.devices.contains(d))
            .map(|d| (d.host.clone(), d.container.clone(), d.permissions.clone()))
            .collect();
        assert_eq!(
            new_devices,
            [3, 12]
                .map(|n| (
                    format!("/dev/i2c-{n}"),
                    format!("/dev/i2c-{n}"),
                    "rw".into()
                ))
                .to_vec(),
            "{file}"
        );
        let mut env = with.env.clone();
        assert_eq!(env.remove("QUASAR_CONSOLE_ACCESS").as_deref(), Some("1"));
        assert_eq!(env, without.env, "{file}");
        assert_eq!(with.security_opt, without.security_opt, "{file}");
        assert_eq!(with.gpus, without.gpus, "{file}");

        // A host with none of them: the console marker alone, nothing the engine would
        // have to find on the host.
        let mut bare = console.clone();
        bare.devices.sound = false;
        bare.devices.logind = false;
        bare.devices.i2c.clear();
        let bare = render(Role::NodeAgent, 3, &bare, &image, &agent_secrets()).unwrap();
        assert_eq!(binds(&bare), binds(&without), "{file}");
        assert_eq!(bare.devices, without.devices, "{file}");
        assert!(bare.cap_add.is_empty() && bare.device_cgroup_rules.is_empty());
        assert_eq!(
            bare.env.get("QUASAR_CONSOLE_ACCESS").map(String::as_str),
            Some("1")
        );
    }

    // Rootful keeps SYS_ADMIN and its mknod path for i2c (until owner decision 1 is proven
    // on hardware), and names a display holder from logind the same way.
    let mut rootful = inputs(Some(GpuVendor::Amd));
    rootful.console = true;
    rootful.devices.logind = true;
    rootful.devices.i2c = vec![3];
    let spec = render(Role::NodeAgent, 3, &rootful, &image, &agent_secrets()).unwrap();
    assert_eq!(spec.cap_add, vec!["SYS_ADMIN".to_string()]);
    assert!(spec.device_cgroup_rules.contains(&"c 89:* rmw".to_string()));
    assert!(!spec.devices.iter().any(|d| d.host.starts_with("/dev/i2c")));
    assert!(spec
        .binds
        .iter()
        .any(|b| b.target == "/host/run/systemd/seats" && b.read_only));
}

/// RH-07 #407: a host with VTs gets console mode's VT as one read/write device on either
/// engine, and nothing else changes: no capability, no device-cgroup rule, no bind. A host
/// without it gets nothing the engine would have to find.
#[test]
fn node_agent_revision_3_with_console_mode_passes_the_console_vt() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let rootless = || {
        let mut i = inputs(Some(GpuVendor::Amd));
        i.devices.engine_rootless = true;
        i
    };
    for (plain, file) in [
        (
            inputs(Some(GpuVendor::Amd)),
            "node-agent-r3-amd-console-vt.json",
        ),
        (rootless(), "node-agent-r3-amd-rootless-console-vt.json"),
    ] {
        let mut console = plain.clone();
        console.console = true;
        let without = render(Role::NodeAgent, 3, &console, &image, &agent_secrets()).unwrap();
        console.devices.console_vt = true;
        let with = render(Role::NodeAgent, 3, &console, &image, &agent_secrets()).unwrap();
        check(file, &with);

        let added: Vec<(String, String, String)> = with
            .devices
            .iter()
            .filter(|d| !without.devices.contains(d))
            .map(|d| (d.host.clone(), d.container.clone(), d.permissions.clone()))
            .collect();
        assert_eq!(
            added,
            vec![("/dev/tty8".into(), "/dev/tty8".into(), "rw".into())],
            "{file}"
        );
        assert_eq!(with.devices.len(), without.devices.len() + 1, "{file}");
        assert_eq!(with.cap_add, without.cap_add, "{file}");
        assert_eq!(
            with.device_cgroup_rules, without.device_cgroup_rules,
            "{file}"
        );
        assert_eq!(with.binds, without.binds, "{file}");
        assert_eq!(with.env, without.env, "{file}");
        assert_eq!(with.security_opt, without.security_opt, "{file}");

        // Not before console mode has been on here.
        let mut off = plain.clone();
        off.devices.console_vt = true;
        let never = render(Role::NodeAgent, 3, &off, &image, &agent_secrets()).unwrap();
        assert!(
            !never.devices.iter().any(|d| d.host == "/dev/tty8"),
            "{file}"
        );

        // Kept once it has been: the VT alone, nothing else of console mode.
        off.console_vt_kept = true;
        let kept = render(Role::NodeAgent, 3, &off, &image, &agent_secrets()).unwrap();
        check(&file.replace(".json", "-kept.json"), &kept);
        let mut devices = kept.devices.clone();
        devices.retain(|d| d.host != "/dev/tty8");
        assert_eq!(devices, never.devices, "{file}");
        assert_eq!(kept.devices.len(), never.devices.len() + 1, "{file}");
        assert_eq!(kept.cap_add, never.cap_add, "{file}");
        assert_eq!(
            kept.device_cgroup_rules, never.device_cgroup_rules,
            "{file}"
        );
        assert_eq!(kept.binds, never.binds, "{file}");
        assert_eq!(kept.env, never.env, "{file}");
        assert!(!kept.env.contains_key("QUASAR_CONSOLE_ACCESS"), "{file}");
    }
    let mut plain = inputs(Some(GpuVendor::Amd));
    let json = serde_json::to_value(&plain).unwrap();
    assert!(json["devices"].get("console_vt").is_none(), "{json}");
    assert!(json.get("console_vt_kept").is_none(), "{json}");
    plain.devices.console_vt = true;
    assert_eq!(
        serde_json::to_value(&plain).unwrap()["devices"]["console_vt"],
        true
    );
    // Kept only with console mode on and the VT seen.
    plain.keep_console_vt();
    assert!(!plain.console_vt_kept);
    plain.console = true;
    plain.keep_console_vt();
    assert!(plain.console_vt_kept);
    plain.console = false;
    plain.keep_console_vt();
    assert!(plain.console_vt_kept, "turning console mode off keeps it");
}

/// RH-07 #407 (D13): a host prepared with `--console-audio-user` has the desktop user's
/// Quasar-only PipeWire socket directory; console mode binds it read-write at the same path
/// on either engine, and a host without it gets nothing the engine would have to create.
#[test]
fn node_agent_revision_3_with_console_mode_binds_the_pipewire_socket_directory() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let audio_bind = "/run/quasar-console-audio:/run/quasar-console-audio".to_string();
    let binds = |s: &quasar_recovery::recipe::ContainerSpec| -> Vec<String> {
        s.binds.iter().map(|b| b.to_engine()).collect()
    };

    let mut rootless = inputs(Some(GpuVendor::Nvidia));
    rootless.devices.engine_rootless = true;
    rootless.gpu.cdi = true;
    rootless.console = true;
    rootless.devices.sound = true;
    rootless.devices.logind = true;
    rootless.devices.i2c = vec![3, 12];
    let without = render(Role::NodeAgent, 3, &rootless, &image, &agent_secrets()).unwrap();
    rootless.devices.console_audio = true;
    let with = render(Role::NodeAgent, 3, &rootless, &image, &agent_secrets()).unwrap();
    check("node-agent-r3-nvidia-rootless-console-pipewire.json", &with);
    let added: Vec<String> = binds(&with)
        .into_iter()
        .filter(|b| !binds(&without).contains(b))
        .collect();
    assert_eq!(added, vec![audio_bind.clone()]);
    assert!(!binds(&without).contains(&audio_bind));
    assert!(with.cap_add.is_empty() && with.device_cgroup_rules.is_empty());
    assert_eq!(with.devices, without.devices);

    let mut rootful = inputs(Some(GpuVendor::Amd));
    rootful.console = true;
    rootful.devices.console_audio = true;
    let spec = render(Role::NodeAgent, 3, &rootful, &image, &agent_secrets()).unwrap();
    assert!(binds(&spec).contains(&audio_bind), "{:?}", binds(&spec));

    // Console mode off: never bound, whatever the host has.
    let mut off = rootful.clone();
    off.console = false;
    let spec = render(Role::NodeAgent, 3, &off, &image, &agent_secrets()).unwrap();
    assert!(!binds(&spec).contains(&audio_bind));

    // Written only when true.
    let json = serde_json::to_value(inputs(Some(GpuVendor::Amd))).unwrap();
    assert!(json["devices"].get("console_audio").is_none(), "{json}");
    assert_eq!(
        serde_json::to_value(&rootful).unwrap()["devices"]["console_audio"],
        true
    );
}

/// Machine state gains the console devices only where the host has them, so every existing
/// machine and golden stays as it was.
#[test]
fn console_devices_absent_are_not_written() {
    let plain = inputs(Some(GpuVendor::Amd));
    let json = serde_json::to_value(&plain).unwrap();
    assert!(json["devices"].get("i2c").is_none(), "{json}");
    assert!(json["devices"].get("logind").is_none(), "{json}");
    let mut on = plain;
    on.devices.i2c = vec![4];
    on.devices.logind = true;
    let json = serde_json::to_value(&on).unwrap();
    assert_eq!(json["devices"]["i2c"], serde_json::json!([4]));
    assert_eq!(json["devices"]["logind"], true);
    assert_eq!(serde_json::from_value::<Inputs>(json).unwrap(), on);
}

#[test]
fn console_mode_is_refused_where_the_recipe_cannot_grant_it() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let mut rootless = inputs(Some(GpuVendor::Nvidia));
    rootless.devices.engine_rootless = true;
    rootless.console = true;
    quasar_recovery::recipe::validate(&rootless).expect("#407: a rootless engine takes it");
    let mut no_dri = inputs(None);
    no_dri.console = true;
    let refused = quasar_recovery::recipe::validate(&no_dri).expect_err("no /dev/dri");
    assert!(refused.to_string().contains("/dev/dri"), "{refused}");
    assert!(matches!(
        render(Role::NodeAgent, 3, &no_dri, &image, &agent_secrets()),
        Err(RenderError::Invalid(_))
    ));
    let mut older = inputs(Some(GpuVendor::Amd));
    older.console = true;
    for revision in [1, 2] {
        assert!(
            matches!(
                render(Role::NodeAgent, revision, &older, &image, &agent_secrets()),
                Err(RenderError::Invalid(_))
            ),
            "revision {revision} would render without the console additions"
        );
    }
}

#[test]
fn recovery_actor_revision_1_renders_its_golden_specification() {
    let image = ImageRef::parse(ACTOR_IMAGE).unwrap();
    let spec = render(
        Role::RecoveryActor,
        1,
        &inputs(None),
        &image,
        &SecretMounts::default(),
    )
    .unwrap();
    check("recovery-actor-r1.json", &spec);
}

/// The operator socket (`restore`, `reconfigure`) may change machine inputs and replace
/// the database, so only a process inside the actor's own container may reach it: no
/// revision of the actor's recipe mounts anything over its directory.
#[test]
fn no_recovery_actor_revision_mounts_the_operator_socket() {
    let image = ImageRef::parse(ACTOR_IMAGE).unwrap();
    let dir = Path::new(quasar_recovery::operator::SOCKET)
        .parent()
        .unwrap();
    let mut rendered = 0;
    for revision in [1, 2] {
        for inputs in [
            inputs(None),
            combined(DatabaseInputs::Owned),
            combined(external()),
        ] {
            let Ok(spec) = render(
                Role::RecoveryActor,
                revision,
                &inputs,
                &image,
                &SecretMounts::default(),
            ) else {
                continue;
            };
            rendered += 1;
            for bind in &spec.binds {
                let target = Path::new(&bind.target);
                assert!(
                    !dir.starts_with(target) && !target.starts_with(dir),
                    "revision {revision} mounts {} over the operator socket's {}",
                    bind.target,
                    dir.display()
                );
            }
        }
    }
    assert!(rendered > 0, "no actor revision rendered");
}

#[test]
fn a_revision_the_book_does_not_carry_is_recipe_unsupported() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    for (role, revision) in [
        (Role::NodeAgent, 0),
        (Role::NodeAgent, 5),
        (Role::RecoveryActor, 2),
        (Role::ControlPlane, 0),
        (Role::ControlPlane, 3),
        (Role::Postgres, 2),
    ] {
        assert_eq!(
            render(
                role,
                revision,
                &inputs(None),
                &image,
                &SecretMounts::default()
            ),
            Err(RenderError::Unsupported { role, revision })
        );
        assert!(!Book::supports(role, revision));
    }
}

#[test]
fn inputs_that_could_widen_host_access_are_refused() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let mut bad = Vec::new();
    for home in ["/", "relative/homes", "/srv/../etc", "/srv/a:b", "/srv/a,b"] {
        let mut i = inputs(None);
        i.home_root = home.into();
        bad.push(i);
    }
    let mut i = inputs(None);
    i.template_root = i.home_root.clone();
    bad.push(i);
    let mut i = inputs(None);
    i.node_name = "gpu host; rm".into();
    bad.push(i);
    let mut i = inputs(Some(GpuVendor::Amd));
    i.gpu.render_node = Some("/dev/sda".into());
    bad.push(i);
    for i in bad {
        assert!(
            matches!(
                render(Role::NodeAgent, 1, &i, &image, &agent_secrets()),
                Err(RenderError::Invalid(_))
            ),
            "{i:?}"
        );
    }
    for reference in [
        "registry.example.invalid/quasar/quasar-node-agent:latest",
        "registry.example.invalid/quasar/quasar-node-agent@sha256:BB22",
        "registry.example.invalid/quasar/quasar-node-agent:dev@sha256:bb22000000000000000000000000000000000000000000000000000000000000",
    ] {
        assert!(ImageRef::parse(reference).is_err(), "{reference}");
    }
}

#[test]
fn the_same_inputs_always_render_the_same_specification_and_digest() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let a = render(
        Role::NodeAgent,
        1,
        &inputs(Some(GpuVendor::Amd)),
        &image,
        &agent_secrets(),
    )
    .unwrap();
    let b = render(
        Role::NodeAgent,
        1,
        &inputs(Some(GpuVendor::Amd)),
        &image,
        &agent_secrets(),
    )
    .unwrap();
    assert_eq!(a, b);
    let c = render(
        Role::NodeAgent,
        1,
        &inputs(Some(GpuVendor::Intel)),
        &image,
        &agent_secrets(),
    )
    .unwrap();
    assert_ne!(a.labels["io.quasar.spec"], c.labels["io.quasar.spec"]);
}

fn declared_revision(file: &Path) -> u32 {
    let text = std::fs::read_to_string(file).unwrap();
    let line = text
        .lines()
        .find(|l| l.starts_with("pub const RECIPE_REVISION: u32 = "))
        .unwrap_or_else(|| {
            panic!(
                "{}: no `pub const RECIPE_REVISION: u32 = N;`",
                file.display()
            )
        });
    line.trim_start_matches("pub const RECIPE_REVISION: u32 = ")
        .trim_end_matches(';')
        .parse()
        .unwrap()
}

/// The release-time rule in miniature (ADR 0008): the actor built from this tree carries
/// every revision the platform images built from this tree declare. The constants are the
/// ones `deploy/build-images.sh` stamps as `org.quasar.recipe`.
#[test]
fn every_revision_the_tree_declares_is_carried_by_the_book() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR"));
    let agent = declared_revision(&root.join("../../src/recipe.rs"));
    assert!(
        Book::supports(Role::NodeAgent, agent),
        "node-agent revision {agent}"
    );
    let actor = declared_revision(&root.join("src/recipe/revision.rs"));
    assert!(
        Book::supports(Role::RecoveryActor, actor),
        "recovery-actor revision {actor}"
    );
    let go =
        std::fs::read_to_string(root.join("../../../control-plane/internal/buildinfo/recipe.go"))
            .unwrap();
    let control: u32 = go
        .lines()
        .find_map(|l| l.strip_prefix("const RecipeRevision = "))
        .expect("control-plane/internal/buildinfo/recipe.go: const RecipeRevision = N")
        .trim()
        .parse()
        .unwrap();
    assert!(
        Book::supports(Role::ControlPlane, control),
        "control-plane revision {control}"
    );
}

const CONTROL_IMAGE: &str = "registry.example.invalid/quasar/quasar-control-plane@sha256:aa11000000000000000000000000000000000000000000000000000000000000";
const POSTGRES_IMAGE: &str = "docker.io/library/postgres@sha256:dd44000000000000000000000000000000000000000000000000000000000000";
const SOCKET_DIR: &str = "/var/lib/docker/volumes/quasar-recovery-agent/_data";

pub fn combined(database: DatabaseInputs) -> Inputs {
    let mut i = inputs(Some(GpuVendor::Amd));
    i.node_name = "living-room-pc".into();
    i.control = Some(ControlInputs {
        unknown: Default::default(),
        machine_role: quasar_recovery::recipe::ControlRole::Combined,
        trusted_proxies: None,
        http_port: 8080,
        tls_port: 8443,
        public_host: Some("quasar.example.invalid".into()),
        tls_hosts: None,
        database,
    });
    i.socket_dir = Some(SOCKET_DIR.into());
    i
}

fn external() -> DatabaseInputs {
    DatabaseInputs::External {
        unknown: Default::default(),
        host: "db.example.invalid".into(),
        port: 5433,
        user: "quasar_app".into(),
        name: "quasar_prod".into(),
        sslmode: "require".into(),
    }
}

fn control_secrets(local: bool) -> SecretMounts {
    let mut files = BTreeSet::from([
        secrets::DATABASE_PASSWORD.to_string(),
        secrets::SECRET_KEY.to_string(),
    ]);
    if local {
        files.insert(secrets::LOCAL_ENROLLMENT.to_string());
    }
    SecretMounts {
        volume: Some(names::CONTROL_PLANE_SECRETS_VOLUME.into()),
        files,
    }
}

fn local_agent_secrets() -> SecretMounts {
    SecretMounts {
        volume: Some(names::NODE_AGENT_SECRETS_VOLUME.into()),
        files: BTreeSet::from([secrets::LOCAL_ENROLLMENT.to_string()]),
    }
}

#[test]
fn the_combined_and_control_only_recipes_render_their_golden_specifications() {
    let cp = ImageRef::parse(CONTROL_IMAGE).unwrap();
    let pg = ImageRef::parse(POSTGRES_IMAGE).unwrap();
    let agent = ImageRef::parse(AGENT_IMAGE).unwrap();
    let pg_secrets = SecretMounts {
        volume: Some(names::POSTGRES_SECRETS_VOLUME.into()),
        files: BTreeSet::from([secrets::DATABASE_PASSWORD.to_string()]),
    };
    let owned = combined(DatabaseInputs::Owned);
    let spec = render(Role::Postgres, 1, &owned, &pg, &pg_secrets).unwrap();
    check("postgres-r1.json", &spec);
    let spec = render(Role::ControlPlane, 1, &owned, &cp, &control_secrets(true)).unwrap();
    check("control-plane-r1-combined.json", &spec);
    let mut control_only = combined(external());
    control_only.home_root = String::new();
    control_only.template_root = quasar_recovery::recipe::default_template_root();
    control_only.node_name = "attic-server".into();
    control_only.control.as_mut().unwrap().machine_role =
        quasar_recovery::recipe::ControlRole::ControlOnly;
    let spec = render(
        Role::ControlPlane,
        1,
        &control_only,
        &cp,
        &control_secrets(false),
    )
    .unwrap();
    check("control-plane-r1-control-only-external.json", &spec);
    let spec = render(Role::NodeAgent, 2, &owned, &agent, &local_agent_secrets()).unwrap();
    check("node-agent-r2-combined-amd.json", &spec);
    // Revision 3 on a combined host (RH-07 #402).
    let spec = render(Role::NodeAgent, 3, &owned, &agent, &local_agent_secrets()).unwrap();
    check("node-agent-r3-combined-amd.json", &spec);

    // Revision 2 (#365): Add host's install-time images are fallbacks below the installed
    // release, and only the operator's overrides reach QUASAR_ENROLL_*.
    let mut enrolling = owned.clone();
    enrolling.enroll = quasar_recovery::recipe::EnrollImages {
        seed: Some(ImageRef::parse(ACTOR_IMAGE).unwrap()),
        agent: Some(agent.clone()),
        agent_override: Some(ImageRef::parse(OVERRIDE_AGENT_IMAGE).unwrap()),
        ..Default::default()
    };
    let spec = render(
        Role::ControlPlane,
        2,
        &enrolling,
        &cp,
        &control_secrets(true),
    )
    .unwrap();
    check("control-plane-r2-combined.json", &spec);
    assert_eq!(spec.env["QUASAR_ENROLL_SEED_IMAGE"], "");
    assert_eq!(spec.env["QUASAR_ENROLL_AGENT_IMAGE"], OVERRIDE_AGENT_IMAGE);
    assert_eq!(spec.env["QUASAR_ENROLL_FALLBACK_SEED_IMAGE"], ACTOR_IMAGE);
    assert_eq!(spec.env["QUASAR_ENROLL_FALLBACK_AGENT_IMAGE"], AGENT_IMAGE);
    let spec = render(
        Role::ControlPlane,
        2,
        &control_only,
        &cp,
        &control_secrets(false),
    )
    .unwrap();
    check("control-plane-r2-control-only-external.json", &spec);

    // Revision 1 has no fallback variables: an override, else the install-time image.
    let spec = render(
        Role::ControlPlane,
        1,
        &enrolling,
        &cp,
        &control_secrets(true),
    )
    .unwrap();
    assert_eq!(spec.env["QUASAR_ENROLL_SEED_IMAGE"], ACTOR_IMAGE);
    assert_eq!(spec.env["QUASAR_ENROLL_AGENT_IMAGE"], OVERRIDE_AGENT_IMAGE);
    assert!(!spec.env.contains_key("QUASAR_ENROLL_FALLBACK_SEED_IMAGE"));
}

const OVERRIDE_AGENT_IMAGE: &str =
    "registry.example.invalid/dev/quasar-node-agent@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee";

#[test]
fn a_gpu_hosts_agent_has_the_same_shape_at_revisions_1_and_2() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    for vendor in [Some(GpuVendor::Nvidia), Some(GpuVendor::Amd), None] {
        let mut one = render(
            Role::NodeAgent,
            1,
            &inputs(vendor),
            &image,
            &agent_secrets(),
        )
        .unwrap();
        let mut two = render(
            Role::NodeAgent,
            2,
            &inputs(vendor),
            &image,
            &agent_secrets(),
        )
        .unwrap();
        for spec in [&mut one, &mut two] {
            spec.labels.remove("io.quasar.recipe");
            spec.labels.remove("io.quasar.spec");
        }
        assert_eq!(one, two, "{vendor:?}");
    }
}

#[test]
fn a_combined_hosts_agent_needs_revision_2_and_is_given_only_its_own_socket() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let owned = combined(DatabaseInputs::Owned);
    assert_eq!(
        render(Role::NodeAgent, 1, &owned, &image, &local_agent_secrets()),
        Err(RenderError::Unsupported {
            role: Role::NodeAgent,
            revision: 1
        })
    );
    let spec = render(Role::NodeAgent, 2, &owned, &image, &local_agent_secrets()).unwrap();
    assert_eq!(spec.env["CONTROL_PLANE_URL"], "ws://127.0.0.1:8080");
    assert_eq!(
        spec.env["ENROLLMENT_TOKEN_FILE"],
        "/run/quasar-secrets/local-enrollment"
    );
    assert!(!spec.env.contains_key("ENROLLMENT_TOKEN"));
    assert!(!spec.env.contains_key("QUASAR_ENROLLMENT_FILE"));
    let sockets: Vec<_> = spec
        .binds
        .iter()
        .filter(|b| b.target == "/run/quasar-recovery")
        .collect();
    assert_eq!(sockets.len(), 1);
    assert_eq!(sockets[0].source, format!("{SOCKET_DIR}/agent"));
    assert!(sockets[0].read_only);
    let cp = render(
        Role::ControlPlane,
        1,
        &owned,
        &ImageRef::parse(CONTROL_IMAGE).unwrap(),
        &control_secrets(true),
    )
    .unwrap();
    // Neither is given the other's socket, nor the actor's whole volume.
    for s in [&spec, &cp] {
        assert!(s
            .binds
            .iter()
            .all(|b| b.source != names::AGENT_SOCKET_VOLUME && b.source != names::MACHINE_VOLUME));
    }
    let cp_socket = cp
        .binds
        .iter()
        .find(|b| b.target == "/run/quasar-recovery")
        .unwrap();
    assert_eq!(cp_socket.source, format!("{SOCKET_DIR}/control"));
}

#[test]
fn the_control_plane_gets_its_secrets_as_files_never_as_values() {
    let owned = combined(DatabaseInputs::Owned);
    let cp = ImageRef::parse(CONTROL_IMAGE).unwrap();
    let spec = render(Role::ControlPlane, 1, &owned, &cp, &control_secrets(true)).unwrap();
    for key in [
        "DATABASE_URL",
        "QUASAR_DATABASE_PASSWORD",
        "QUASAR_SECRET_KEY",
        "ENROLLMENT_TOKEN",
        "POSTGRES_PASSWORD",
    ] {
        assert!(!spec.env.contains_key(key), "{key}");
    }
    assert_eq!(
        spec.env["QUASAR_DATABASE_PASSWORD_FILE"],
        "/run/quasar-secrets/database-password"
    );
    assert_eq!(
        spec.env["QUASAR_SECRET_KEY_FILE"],
        "/run/quasar-secrets/secret-key"
    );
    assert_eq!(
        spec.env["QUASAR_LOCAL_ENROLLMENT_NODE_NAME"],
        "living-room-pc"
    );
    // Its TLS pair lives in a named volume, which outlives every replacement.
    assert!(spec
        .binds
        .iter()
        .any(|b| b.source == names::CONTROL_DATA_VOLUME && b.target == "/var/lib/quasar-control"));
    let mut missing = control_secrets(true);
    missing.files.remove(secrets::SECRET_KEY);
    assert!(matches!(
        render(Role::ControlPlane, 1, &owned, &cp, &missing),
        Err(RenderError::Invalid(_))
    ));
}

#[test]
fn an_external_database_gets_no_postgres_and_its_settings_reach_the_control_plane() {
    let mut i = combined(external());
    i.home_root = String::new();
    let pg = ImageRef::parse(POSTGRES_IMAGE).unwrap();
    let pg_secrets = SecretMounts {
        volume: Some(names::POSTGRES_SECRETS_VOLUME.into()),
        files: BTreeSet::from([secrets::DATABASE_PASSWORD.to_string()]),
    };
    assert!(matches!(
        render(Role::Postgres, 1, &i, &pg, &pg_secrets),
        Err(RenderError::Invalid(_))
    ));
    let spec = render(
        Role::ControlPlane,
        1,
        &i,
        &ImageRef::parse(CONTROL_IMAGE).unwrap(),
        &control_secrets(false),
    )
    .unwrap();
    assert_eq!(spec.env["QUASAR_DATABASE_HOST"], "db.example.invalid");
    assert_eq!(spec.env["QUASAR_DATABASE_PORT"], "5433");
    assert_eq!(spec.env["QUASAR_DATABASE_SSLMODE"], "require");
    assert!(!spec.env.contains_key("QUASAR_HOME_ROOT"));
    assert!(!spec.env.contains_key("QUASAR_LOCAL_ENROLLMENT_FILE"));
}

#[test]
fn control_inputs_that_could_inject_are_refused() {
    let cp = ImageRef::parse(CONTROL_IMAGE).unwrap();
    let mut bad = Vec::new();
    for host in ["db host", "db;rm", "a,b", ""] {
        let mut i = combined(external());
        if let Some(c) = i.control.as_mut() {
            c.database = DatabaseInputs::External {
                unknown: Default::default(),
                host: host.into(),
                port: 5432,
                user: "quasar".into(),
                name: "quasar".into(),
                sslmode: "disable".into(),
            };
        }
        bad.push(i);
    }
    let mut i = combined(DatabaseInputs::Owned);
    i.control.as_mut().unwrap().public_host = Some("evil host".into());
    bad.push(i);
    let mut i = combined(DatabaseInputs::Owned);
    i.control.as_mut().unwrap().tls_port = 8080;
    bad.push(i);
    let mut i = combined(DatabaseInputs::Owned);
    i.socket_dir = None;
    bad.push(i);
    let mut i = combined(DatabaseInputs::Owned);
    i.socket_dir = Some("relative".into());
    bad.push(i);
    for i in bad {
        assert!(
            matches!(
                render(Role::ControlPlane, 1, &i, &cp, &control_secrets(true)),
                Err(RenderError::Invalid(_))
            ),
            "{i:?}"
        );
    }
}

/// RH-07 #407, found live: rootless Podman refused the `/dev/dri` directory as a device
/// ("no devices found in /dev/dri") on a host where each node alone was accepted. On a
/// rootless engine the agent gets each DRM node the probe listed instead; with no list
/// recorded (a machine installed before the list existed), the directory as before.
#[test]
fn a_rootless_agent_gets_each_drm_node_rather_than_the_directory() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let devices = |i: &Inputs| -> Vec<String> {
        render(Role::NodeAgent, 3, i, &image, &agent_secrets())
            .unwrap()
            .devices
            .iter()
            .map(|d| d.host.clone())
            .collect()
    };
    let mut rootless = inputs(Some(GpuVendor::Nvidia));
    rootless.devices.engine_rootless = true;
    rootless.devices.dri_nodes = vec!["/dev/dri/card0".into(), "/dev/dri/renderD128".into()];
    let got = devices(&rootless);
    assert!(got.contains(&"/dev/dri/card0".to_string()), "{got:?}");
    assert!(got.contains(&"/dev/dri/renderD128".to_string()), "{got:?}");
    assert!(!got.contains(&"/dev/dri".to_string()), "{got:?}");

    let mut unlisted = rootless.clone();
    unlisted.devices.dri_nodes.clear();
    assert!(devices(&unlisted).contains(&"/dev/dri".to_string()));

    let mut rootful = rootless.clone();
    rootful.devices.engine_rootless = false;
    let got = devices(&rootful);
    assert!(got.contains(&"/dev/dri".to_string()), "{got:?}");
    assert!(!got.contains(&"/dev/dri/card0".to_string()), "{got:?}");
}

#[test]
fn agent_variables_render_over_the_agent_defaults_and_none_render_nothing() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    for vendor in [Some(GpuVendor::Nvidia), Some(GpuVendor::Amd), None] {
        let plain = inputs(vendor);
        assert!(serde_json::to_value(&plain)
            .unwrap()
            .get("agent_variables")
            .is_none());
        let before = render(Role::NodeAgent, 3, &plain, &image, &agent_secrets()).unwrap();

        let mut with = plain.clone();
        with.agent_variables.extend([
            (
                "QUASAR_APP_MOUNT_ALLOW".to_string(),
                "/mnt/games:rw".to_string(),
            ),
            ("QUASAR_CUDA_DEVICE".to_string(), "1".to_string()),
            ("QUASAR_ABR_MODE".to_string(), "protective".to_string()),
        ]);
        let after = render(Role::NodeAgent, 3, &with, &image, &agent_secrets()).unwrap();
        assert_eq!(after.env["QUASAR_APP_MOUNT_ALLOW"], "/mnt/games:rw");
        assert_eq!(
            after.env["QUASAR_CUDA_DEVICE"], "1",
            "over the NVIDIA default"
        );
        assert_eq!(after.env["QUASAR_ABR_MODE"], "protective");
        let mut rest = after.env.clone();
        let mut expected = before.env.clone();
        for k in with.agent_variables.keys() {
            rest.remove(k);
            expected.remove(k);
        }
        assert_eq!(rest, expected, "nothing else moves");
        assert_eq!(after.binds, before.binds);
        assert_ne!(after.labels, before.labels, "a new specification");
    }

    let mut owned = inputs(None);
    owned
        .agent_variables
        .insert("QUASAR_RENDER_NODE".into(), "/dev/dri/renderD129".into());
    assert!(matches!(
        render(Role::NodeAgent, 3, &owned, &image, &agent_secrets()),
        Err(RenderError::Invalid(_))
    ));
    let mut broken = inputs(None);
    broken
        .agent_variables
        .insert("QUASAR_APP_MOUNT_ALLOW".into(), "/a\n/b".into());
    assert!(render(Role::NodeAgent, 3, &broken, &image, &agent_secrets()).is_err());
}

#[test]
fn agent_variables_are_documented() {
    let docs = std::fs::read_to_string(
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../docs/configuration.md"),
    )
    .unwrap();
    for name in quasar_recovery::recipe::AGENT_VARIABLES {
        assert!(
            docs.contains(&format!("| `{name}`")) || docs.contains(&format!("`{name}` /")),
            "{name} has no row in docs/configuration.md"
        );
    }
}

/// #460: whether the host has udev's database reaches a console agent as an environment
/// input, only once console mode has read it; a machine that never read it renders as before.
#[test]
fn a_console_agent_is_told_whether_the_host_has_udev_data_once_it_was_read() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let mut console = inputs(Some(GpuVendor::Amd));
    console.console = true;
    let unread = render(Role::NodeAgent, 3, &console, &image, &agent_secrets()).unwrap();
    assert!(!unread.env.contains_key("QUASAR_HOST_UDEV_DATA"));
    for (fact, value) in [(true, "1"), (false, "0")] {
        let mut read = console.clone();
        read.devices.udev_data = Some(fact);
        let spec = render(Role::NodeAgent, 3, &read, &image, &agent_secrets()).unwrap();
        assert_eq!(
            spec.env.get("QUASAR_HOST_UDEV_DATA").map(String::as_str),
            Some(value)
        );
        assert_eq!(spec.binds, unread.binds, "an input, never a mount");
        // Console mode off: no console additions, so no such input either.
        read.console = false;
        let off = render(Role::NodeAgent, 3, &read, &image, &agent_secrets()).unwrap();
        assert!(!off.env.contains_key("QUASAR_HOST_UDEV_DATA"));
    }
}
