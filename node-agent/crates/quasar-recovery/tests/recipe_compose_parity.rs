//! Compose parity (ADR 0008 consequences; architecture §5.3): the rendered node-agent
//! specification must equal the `quasar-node-agent` service of `deploy/docker-compose.yml`
//! (plus `deploy/docker-compose.nvidia.yml` on NVIDIA), evaluated with a `.env` holding the
//! same machine inputs, except for the differences listed in `ALLOWED`, each with its
//! reason. A device, mount or environment entry added to the Compose files and not to the
//! recipe (or the reverse) fails here. In the spirit of the control plane's
//! `TestEnrollHostComposeMatchesBase`.

use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;

use quasar_recovery::recipe::{
    names, render, secrets, ContainerSpec, GpuFacts, GpuVendor, HostDevices, ImageRef, Inputs,
    Role, SecretMounts,
};
use serde_yaml::Value;

const AGENT_IMAGE: &str = "registry.example.invalid/quasar/quasar-node-agent@sha256:bb22000000000000000000000000000000000000000000000000000000000000";
const SERVICE: &str = "quasar-node-agent";

/// `(kind, item, vendor, reason)`. `kind` is `-env`/`+env` (removed from/added to
/// Compose's environment), `-bind`/`+bind`, `-device`, or `-key` (a Compose-only service
/// key). `vendor` scopes a difference to one GPU vendor's machine (`none`: no GPU), or `*`.
const ALLOWED: &[(&str, &str, &str, &str)] = &[
    ("-env", "CONTROL_PLANE_URL", "*", "the enrollment string carries the control-plane URL, as in enroll-host.sh's agent-only stack"),
    ("-env", "ENROLLMENT_TOKEN", "*", "secrets reach containers only as read-only files (D5) and the static token retires (D10)"),
    ("+env", "QUASAR_ENROLLMENT_FILE", "*", "the enrollment string, delivered as a file in the agent's secrets volume (D5)"),
    ("+env", "QUASAR_RECOVERY_SOCKET", "*", "the agent socket: how an owned agent reaches its recovery actor (D6(a))"),
    ("-bind", "quasar-updater-run:/run/quasar-updater", "*", "the Go updater has no place on an owned machine: the recovery actor replaces it"),
    ("+bind", "quasar-node-agent-secrets:/run/quasar-secrets:ro", "*", "the agent's per-service secrets volume, read-only (D5)"),
    ("+bind", "quasar-recovery-agent:/run/quasar-recovery:ro", "*", "the agent-socket volume, read-only in the agent (D6(a))"),
    ("-key", "depends_on", "*", "Compose-only start ordering; a GPU host has no local control plane"),
    ("-device", "/dev/dri", "none", "the engine refuses a container naming a device the host lacks; readiness reports the missing GPU"),
];

fn deploy(file: &str) -> Value {
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../deploy")
        .join(file);
    serde_yaml::from_str(&std::fs::read_to_string(&path).unwrap())
        .unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

/// Compose variable interpolation: `${V}`, `${V:-d}`, `${V-d}`, `${V:+a}`, `${V+a}`,
/// `${V:?m}`, `${V?m}`, nested in defaults. A required variable that is missing evaluates
/// to a marker the comparison will show.
fn interpolate(s: &str, env: &BTreeMap<&str, String>) -> String {
    let mut out = String::new();
    let bytes = s.as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'$' && bytes.get(i + 1) == Some(&b'{') {
            let (mut depth, mut j) = (0, i);
            loop {
                if bytes[j] == b'$' && bytes.get(j + 1) == Some(&b'{') {
                    depth += 1;
                    j += 2;
                    continue;
                }
                if bytes[j] == b'}' {
                    depth -= 1;
                    if depth == 0 {
                        break;
                    }
                }
                j += 1;
            }
            let inner = &s[i + 2..j];
            let name_end = inner
                .find(|c: char| !(c.is_ascii_alphanumeric() || c == '_'))
                .unwrap_or(inner.len());
            let (name, rest) = inner.split_at(name_end);
            let value = env.get(name);
            let set = value.is_some();
            let non_empty = value.is_some_and(|v| !v.is_empty());
            let own = || value.cloned().unwrap_or_default();
            out += &match rest {
                "" => own(),
                r if r.starts_with(":-") => {
                    if non_empty {
                        own()
                    } else {
                        interpolate(&r[2..], env)
                    }
                }
                r if r.starts_with('-') => {
                    if set {
                        own()
                    } else {
                        interpolate(&r[1..], env)
                    }
                }
                r if r.starts_with(":+") => {
                    if non_empty {
                        interpolate(&r[2..], env)
                    } else {
                        String::new()
                    }
                }
                r if r.starts_with('+') => {
                    if set {
                        interpolate(&r[1..], env)
                    } else {
                        String::new()
                    }
                }
                r if r.starts_with(":?") || r.starts_with('?') => {
                    if non_empty {
                        own()
                    } else {
                        format!("<required {name}>")
                    }
                }
                r => panic!("unsupported interpolation {r:?} in {s:?}"),
            };
            i = j + 1;
        } else {
            out.push(bytes[i] as char);
            i += 1;
        }
    }
    out
}

#[derive(Debug, Default, PartialEq)]
struct Shape {
    image: String,
    entrypoint: Option<Vec<String>>,
    network_mode: Option<String>,
    cap_add: BTreeSet<String>,
    init: bool,
    env: BTreeMap<String, String>,
    binds: BTreeSet<String>,
    devices: BTreeSet<String>,
    device_cgroup_rules: BTreeSet<String>,
    restart: String,
    gpus_all: bool,
    keys: BTreeSet<String>,
}

fn strings(v: &Value, env: &BTreeMap<&str, String>) -> Vec<String> {
    v.as_sequence()
        .unwrap()
        .iter()
        .map(|x| interpolate(x.as_str().unwrap(), env))
        .collect()
}

fn bind_of(short: &str) -> String {
    match short.splitn(3, ':').collect::<Vec<_>>()[..] {
        [s, t] => format!("{s}:{t}"),
        [s, t, "ro"] => format!("{s}:{t}:ro"),
        [s, t, "rw"] => format!("{s}:{t}"),
        _ => panic!("volume {short:?}"),
    }
}

fn device_of(short: &str) -> String {
    match short.split(':').collect::<Vec<_>>()[..] {
        [h] => format!("{h}:{h}:rwm"),
        [h, c] => format!("{h}:{c}:rwm"),
        [h, c, p] => format!("{h}:{c}:{p}"),
        _ => panic!("device {short:?}"),
    }
}

/// Merge the service definitions as Compose does for `-f base -f overlay`.
fn compose_shape(files: &[Value], env: &BTreeMap<&str, String>) -> Shape {
    let mut shape = Shape::default();
    let mut binds: BTreeMap<String, String> = BTreeMap::new();
    for doc in files {
        let svc = &doc["services"][SERVICE];
        for (key, value) in svc.as_mapping().unwrap() {
            let key = key.as_str().unwrap();
            shape.keys.insert(key.to_owned());
            match key {
                "image" => shape.image = interpolate(value.as_str().unwrap(), env),
                "entrypoint" => shape.entrypoint = Some(strings(value, env)),
                "network_mode" => shape.network_mode = Some(value.as_str().unwrap().into()),
                "cap_add" => shape.cap_add.extend(strings(value, env)),
                "init" => shape.init = value.as_bool().unwrap(),
                "restart" => shape.restart = value.as_str().unwrap().into(),
                "gpus" => shape.gpus_all = value.as_str() == Some("all"),
                "device_cgroup_rules" => shape.device_cgroup_rules.extend(strings(value, env)),
                "devices" => shape.devices.extend(strings(value, env).iter().map(|d| device_of(d))),
                "volumes" => {
                    for v in strings(value, env) {
                        let b = bind_of(&v);
                        let target = b.split(':').nth(1).unwrap().to_owned();
                        binds.insert(target, b);
                    }
                }
                "environment" => {
                    for (k, v) in value.as_mapping().unwrap() {
                        let k = k.as_str().unwrap().to_owned();
                        match v {
                            // A bare key passes the shell's value, and the .env sets none.
                            Value::Null => {
                                shape.env.remove(&k);
                            }
                            Value::String(s) => {
                                shape.env.insert(k, interpolate(s, env));
                            }
                            other => {
                                shape.env.insert(k, other.as_str().map(str::to_owned).unwrap_or_else(|| serde_yaml::to_string(other).unwrap().trim().to_owned()));
                            }
                        }
                    }
                }
                "depends_on" => {}
                other => panic!("the parity test does not understand Compose key {other:?}: teach it, or allowlist it"),
            }
        }
    }
    shape.binds = binds.into_values().collect();
    shape
}

fn spec_shape(spec: &ContainerSpec) -> Shape {
    Shape {
        image: spec.image.clone(),
        entrypoint: spec.entrypoint.clone(),
        network_mode: spec.network_mode.clone(),
        cap_add: spec.cap_add.iter().cloned().collect(),
        init: spec.init,
        env: spec.env.clone(),
        binds: spec.binds.iter().map(|b| b.to_engine()).collect(),
        devices: spec
            .devices
            .iter()
            .map(|d| format!("{}:{}:{}", d.host, d.container, d.permissions))
            .collect(),
        device_cgroup_rules: spec.device_cgroup_rules.iter().cloned().collect(),
        restart: serde_json::to_value(spec.restart)
            .unwrap()
            .as_str()
            .unwrap()
            .into(),
        gpus_all: spec.gpus.len() == 1
            && spec.gpus[0].count == -1
            && spec.gpus[0].capabilities == vec![vec!["gpu".to_string()]],
        keys: BTreeSet::new(),
    }
}

fn inputs(vendor: Option<GpuVendor>) -> Inputs {
    let render_node = match vendor {
        Some(GpuVendor::Amd) => Some("/dev/dri/renderD129"),
        Some(_) => Some("/dev/dri/renderD128"),
        None => None,
    };
    Inputs {
        installation_id: "5f0c1e0e-0c5a-4d1b-9a2f-3e4d5c6b7a89".into(),
        node_name: "gpu-host-01".into(),
        home_root: "/srv/quasar/homes".into(),
        template_root: "/srv/quasar/templates".into(),
        docker_socket: "/var/run/docker.sock".into(),
        gpu: GpuFacts {
            vendor,
            render_node: render_node.map(str::to_owned),
            gpus_served: vendor == Some(GpuVendor::Nvidia),
        },
        devices: HostDevices {
            dri: vendor.is_some(),
            uinput: true,
            kmsg: true,
        },
    }
}

/// The `.env` an operator would write for the same machine: the machine inputs under the
/// names Compose reads them by.
fn dotenv(i: &Inputs) -> BTreeMap<&'static str, String> {
    let mut env = BTreeMap::from([
        ("NODE_NAME", i.node_name.clone()),
        ("QUASAR_HOME_ROOT", i.home_root.clone()),
        ("QUASAR_TEMPLATE_ROOT", i.template_root.clone()),
        ("QUASAR_AGENT_IMAGE", AGENT_IMAGE.to_string()),
    ]);
    if let Some(node) = &i.gpu.render_node {
        env.insert("QUASAR_RENDER_NODE", node.clone());
    }
    env
}

fn differences(compose: &Shape, rendered: &Shape) -> BTreeSet<(String, String)> {
    let mut out = BTreeSet::new();
    let mut scalar = |what: &str, a: String, b: String| {
        if a != b {
            out.insert((format!("~{what}"), format!("{a} != {b}")));
        }
    };
    scalar("image", compose.image.clone(), rendered.image.clone());
    scalar(
        "entrypoint",
        format!("{:?}", compose.entrypoint),
        format!("{:?}", rendered.entrypoint),
    );
    scalar(
        "network_mode",
        format!("{:?}", compose.network_mode),
        format!("{:?}", rendered.network_mode),
    );
    scalar("init", compose.init.to_string(), rendered.init.to_string());
    scalar("restart", compose.restart.clone(), rendered.restart.clone());
    scalar(
        "gpus",
        compose.gpus_all.to_string(),
        rendered.gpus_all.to_string(),
    );
    scalar(
        "cap_add",
        format!("{:?}", compose.cap_add),
        format!("{:?}", rendered.cap_add),
    );
    scalar(
        "device_cgroup_rules",
        format!("{:?}", compose.device_cgroup_rules),
        format!("{:?}", rendered.device_cgroup_rules),
    );
    for (k, v) in &compose.env {
        match rendered.env.get(k) {
            None => {
                out.insert(("-env".into(), k.clone()));
            }
            Some(r) if r != v => {
                out.insert(("~env".into(), format!("{k}: compose {v:?}, recipe {r:?}")));
            }
            _ => {}
        }
    }
    for k in rendered
        .env
        .keys()
        .filter(|k| !compose.env.contains_key(*k))
    {
        out.insert(("+env".into(), k.clone()));
    }
    for b in compose.binds.difference(&rendered.binds) {
        out.insert(("-bind".into(), b.clone()));
    }
    for b in rendered.binds.difference(&compose.binds) {
        out.insert(("+bind".into(), b.clone()));
    }
    for d in compose.devices.difference(&rendered.devices) {
        out.insert(("-device".into(), d.split(':').next().unwrap().to_owned()));
    }
    for d in rendered.devices.difference(&compose.devices) {
        out.insert(("+device".into(), d.clone()));
    }
    if compose.keys.contains("depends_on") {
        out.insert(("-key".into(), "depends_on".into()));
    }
    out
}

#[test]
fn the_rendered_node_agent_matches_the_compose_definitions_except_the_listed_differences() {
    let base = deploy("docker-compose.yml");
    let nvidia = deploy("docker-compose.nvidia.yml");
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let secrets = SecretMounts {
        volume: Some(names::NODE_AGENT_SECRETS_VOLUME.into()),
        files: BTreeSet::from([secrets::ENROLLMENT.to_string()]),
    };
    let allowed_for = |vendor: &str| -> BTreeSet<(String, String)> {
        ALLOWED
            .iter()
            .filter(|(_, _, v, _)| *v == "*" || *v == vendor)
            .map(|(k, i, _, _)| (k.to_string(), i.to_string()))
            .collect()
    };
    let mut used = BTreeSet::new();

    for vendor in [
        Some(GpuVendor::Nvidia),
        Some(GpuVendor::Amd),
        Some(GpuVendor::Intel),
        None,
    ] {
        let inputs = inputs(vendor);
        let files = if vendor == Some(GpuVendor::Nvidia) {
            vec![base.clone(), nvidia.clone()]
        } else {
            vec![base.clone()]
        };
        let compose = compose_shape(&files, &dotenv(&inputs));
        let rendered = spec_shape(&render(Role::NodeAgent, 1, &inputs, &image, &secrets).unwrap());
        let found = differences(&compose, &rendered);
        let name = vendor.map_or("none".to_string(), |v| format!("{v:?}").to_lowercase());
        let allowed = allowed_for(&name);
        let unexplained: Vec<_> = found.difference(&allowed).collect();
        assert!(
            unexplained.is_empty(),
            "{vendor:?}: differences from Compose with no reason in ALLOWED: {unexplained:#?}"
        );
        used.extend(found);
    }
    let stale: Vec<_> = allowed_for("*")
        .union(&allowed_for("none"))
        .filter(|a| !used.contains(*a))
        .cloned()
        .collect();
    assert!(
        stale.is_empty(),
        "ALLOWED lists differences that no longer occur: {stale:?}"
    );
}
