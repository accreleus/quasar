//! Each engine's reporting, from what real engines returned for the same requested
//! container (Docker 29 and Podman 5.8.4, rootful and rootless; #397). The rule under
//! test is the same for every engine: exactly what Quasar asked for passes, and anything
//! it did not ask for is refused. Rootful Docker keeps today's exact comparison.

use super::*;

const REQUESTED_CAPS: [&str; 8] = [
    "CHOWN",
    "DAC_OVERRIDE",
    "FOWNER",
    "SETGID",
    "SETUID",
    "SETPCAP",
    "KILL",
    "SYS_NICE",
];

fn caps(values: &[&str]) -> Vec<String> {
    values.iter().map(|v| v.to_string()).collect()
}

fn podman(effective: &[&str], runtime: &str) -> PodmanFacts {
    let set: Vec<String> = effective.iter().map(|c| format!("CAP_{c}")).collect();
    PodmanFacts {
        effective_caps: Some(set.clone()),
        bounding_caps: Some(set),
        oci_runtime: Some(runtime.into()),
        mount_propagations: vec!["rprivate".into()],
        uid_map: Vec::new(),
        gid_map: Vec::new(),
    }
}

fn device(path: &str, permissions: &str) -> DeviceMapping {
    DeviceMapping {
        path_on_host: Some(path.into()),
        path_in_container: Some(path.into()),
        cgroup_permissions: Some(permissions.into()),
    }
}

#[test]
fn an_unknown_engine_is_held_to_dockers_exact_rules() {
    assert_eq!(Dialect::of(EngineKind::Unknown), Dialect::Docker);
    assert_eq!(Dialect::of(EngineKind::Docker), Dialect::Docker);
    assert_eq!(Dialect::of(EngineKind::Podman), Dialect::Podman);
}

#[test]
fn capabilities_pass_exactly_as_requested_on_each_engine() {
    let requested = caps(&REQUESTED_CAPS);
    // Docker echoes the request.
    assert!(Dialect::Docker.capabilities_ok(
        Some(&requested),
        Some(&caps(&["ALL"])),
        None,
        &requested,
        true
    ));
    // Podman's compatible inspect as measured: a delta from its default set. Its native
    // EffectiveCaps are what is compared.
    let podman_add = caps(&["SYS_NICE"]);
    let podman_drop = caps(&["FSETID", "NET_BIND_SERVICE", "SETFCAP", "SYS_CHROOT"]);
    assert!(Dialect::Podman.capabilities_ok(
        Some(&podman_add),
        Some(&podman_drop),
        Some(&podman(&REQUESTED_CAPS, "crun")),
        &requested,
        true
    ));
}

#[test]
fn a_capability_quasar_did_not_ask_for_is_refused_on_each_engine() {
    let requested = caps(&REQUESTED_CAPS);
    let mut more = requested.clone();
    more.push("SYS_ADMIN".into());
    assert!(!Dialect::Docker.capabilities_ok(
        Some(&more),
        Some(&caps(&["ALL"])),
        None,
        &requested,
        true
    ));
    let mut effective: Vec<&str> = REQUESTED_CAPS.to_vec();
    effective.push("SYS_ADMIN");
    assert!(!Dialect::Podman.capabilities_ok(
        None,
        None,
        Some(&podman(&effective, "crun")),
        &requested,
        true
    ));
    // Podman without its native answer cannot be proven: refused, not assumed.
    assert!(!Dialect::Podman.capabilities_ok(None, None, None, &requested, true));
    // Docker's rule is unchanged: the compatible fields must echo the request.
    assert!(!Dialect::Docker.capabilities_ok(Some(&requested), None, None, &requested, true));
}

#[test]
fn runtimes_accepted_per_engine() {
    assert!(Dialect::Docker.runtime_ok(Some("runc"), None, false));
    assert!(Dialect::Docker.runtime_ok(None, None, false));
    assert!(
        !Dialect::Docker.runtime_ok(Some("crun"), None, false),
        "rootful Docker unchanged"
    );
    assert!(!Dialect::Docker.runtime_ok(Some("nvidia"), None, false));
    assert!(Dialect::Docker.runtime_ok(Some("nvidia"), None, true));
    assert!(Dialect::Podman.runtime_ok(Some("oci"), Some(&podman(&[], "crun")), false));
    assert!(Dialect::Podman.runtime_ok(Some("oci"), Some(&podman(&[], "runc")), false));
    assert!(!Dialect::Podman.runtime_ok(Some("oci"), Some(&podman(&[], "krun")), false));
    assert!(!Dialect::Podman.runtime_ok(Some("oci"), None, false));
    assert!(!Dialect::Podman.runtime_ok(Some("kata"), Some(&podman(&[], "crun")), false));
}

#[test]
fn default_namespaces_per_engine_and_the_host_never() {
    for ns in [Namespace::Pid, Namespace::Ipc, Namespace::Uts] {
        for dialect in [Dialect::Docker, Dialect::Podman] {
            assert!(dialect.default_namespace(ns, None));
            assert!(
                !dialect.default_namespace(ns, Some("host")),
                "{dialect:?} {ns:?}"
            );
            assert!(
                !dialect.default_namespace(ns, Some("container:abc")),
                "{dialect:?} {ns:?}"
            );
        }
    }
    // As measured on Podman: pid private, ipc shareable, uts private.
    assert!(Dialect::Podman.default_namespace(Namespace::Pid, Some("private")));
    assert!(Dialect::Podman.default_namespace(Namespace::Ipc, Some("shareable")));
    assert!(Dialect::Podman.default_namespace(Namespace::Uts, Some("private")));
    // Docker's rules are today's.
    assert!(!Dialect::Docker.default_namespace(Namespace::Ipc, Some("shareable")));
    assert!(!Dialect::Docker.default_namespace(Namespace::Uts, Some("private")));
}

#[test]
fn security_options_compare_as_one_set() {
    let docker = vec![
        "seccomp=unconfined".to_string(),
        "no-new-privileges:true".to_string(),
    ];
    let podman = vec![
        "no-new-privileges".to_string(),
        "seccomp=unconfined".to_string(),
    ];
    assert_eq!(
        Dialect::Docker.security_options(Some(&docker)),
        Dialect::Podman.security_options(Some(&podman))
    );
    // Docker's own spelling is not rewritten.
    assert_ne!(
        Dialect::Docker.security_options(Some(&vec!["no-new-privileges".into()])),
        Dialect::Docker.security_options(Some(&vec!["no-new-privileges:true".into()]))
    );
}

#[test]
fn devices_docker_exact_podman_only_what_was_asked_for() {
    let requested = vec!["/dev/dri/renderD128".to_string()];
    assert!(Dialect::Docker.devices_ok(&[device("/dev/dri/renderD128", "rwm")], &requested, false));
    assert!(
        !Dialect::Docker.devices_ok(&[], &requested, false),
        "Docker must report it"
    );
    assert!(!Dialect::Docker.devices_ok(&[device("/dev/dri/renderD128", "")], &requested, false));
    // Rootless Podman lists no plain device at all: not reported, not granted.
    assert!(Dialect::Podman.devices_ok(&[], &requested, false));
    // Rootful Podman lists it with empty permissions.
    assert!(Dialect::Podman.devices_ok(&[device("/dev/dri/renderD128", "")], &requested, false));
    // A device nobody asked for is refused on every engine.
    for dialect in [Dialect::Docker, Dialect::Podman] {
        assert!(
            !dialect.devices_ok(&[device("/dev/sda", "")], &requested, false),
            "{dialect:?}"
        );
        assert!(
            !dialect.devices_ok(&[device("/dev/nvidia0", "")], &requested, false),
            "{dialect:?}"
        );
    }
    // A requested GPU expands to its NVIDIA and DRM nodes, and to nothing else.
    let expanded = [
        device("/dev/nvidia0", ""),
        device("/dev/nvidiactl", ""),
        device("/dev/nvidia-uvm", ""),
        device("/dev/dri/card1", ""),
        device("/dev/dri/renderD128", ""),
    ];
    assert!(Dialect::Podman.devices_ok(&expanded, &[], true));
    assert!(!Dialect::Podman.devices_ok(
        &[device("/dev/nvidia0", ""), device("/dev/kmsg", "")],
        &[],
        true
    ));
    assert!(!Dialect::Podman.devices_ok(&[device("/dev/nvidia0", "/dev/other")], &[], true));
}

#[test]
fn device_requests_docker_echoes_podman_reports_none_and_never_an_unasked_one() {
    let nvidia = DeviceRequest {
        driver: Some("nvidia".into()),
        count: Some(-1),
        capabilities: Some(vec![vec!["gpu".into()]]),
        ..Default::default()
    };
    let is_nvidia = |r: &DeviceRequest| r.driver.as_deref() == Some("nvidia");
    assert!(Dialect::Docker.device_requests_ok(Some(&vec![nvidia.clone()]), Some(&is_nvidia)));
    assert!(!Dialect::Docker.device_requests_ok(None, Some(&is_nvidia)));
    assert!(Dialect::Podman.device_requests_ok(None, Some(&is_nvidia)));
    for dialect in [Dialect::Docker, Dialect::Podman] {
        assert!(dialect.device_requests_ok(None, None));
        assert!(
            !dialect.device_requests_ok(Some(&vec![nvidia.clone()]), None),
            "{dialect:?}"
        );
    }
}

#[test]
fn only_docker_is_held_to_echoed_mounts_image_and_masked_paths() {
    assert!(Dialect::Docker.echoes_mount_requests());
    assert!(Dialect::Docker.echoes_config_image());
    assert!(Dialect::Docker.reports_masked_paths());
    assert!(!Dialect::Podman.echoes_mount_requests());
    assert!(!Dialect::Podman.echoes_config_image());
    assert!(!Dialect::Podman.reports_masked_paths());
}

/// Without drop-all both engines grant their own defaults plus the additions, and
/// Podman's compatible delta then reads exactly as Docker's does.
#[test]
fn without_drop_all_both_engines_compare_the_compatible_fields() {
    let add = caps(&["SYS_NICE"]);
    for dialect in [Dialect::Docker, Dialect::Podman] {
        assert!(
            dialect.capabilities_ok(Some(&add), None, None, &add, false),
            "{dialect:?}"
        );
        assert!(
            dialect.capabilities_ok(None, None, None, &[], false),
            "{dialect:?}"
        );
        assert!(
            !dialect.capabilities_ok(Some(&caps(&["SYS_ADMIN"])), None, None, &[], false),
            "{dialect:?}"
        );
    }
}

#[test]
fn podmans_null_capabilities_are_none_and_a_missing_field_cannot_be_proven() {
    let none = PodmanFacts::from_inspect(
        r#"{"EffectiveCaps":null,"BoundingCaps":null,"OCIRuntime":"crun","Mounts":[]}"#,
    )
    .unwrap();
    assert_eq!(none.effective_caps, Some(Vec::new()));
    assert!(Dialect::Podman.capabilities_ok(None, None, Some(&none), &[], true));
    let missing = PodmanFacts::from_inspect(r#"{"OCIRuntime":"crun"}"#).unwrap();
    assert_eq!(missing.effective_caps, None);
    assert!(!Dialect::Podman.capabilities_ok(None, None, Some(&missing), &[], true));
    assert!(PodmanFacts::from_inspect("not json").is_err());
}

/// Security review of #397: a drop-all never applied could leave the bounding set wide.
#[test]
fn podman_capabilities_need_the_bounding_set_too() {
    let requested = caps(&REQUESTED_CAPS);
    let mut facts = podman(&REQUESTED_CAPS, "crun");
    let mut wide = facts.bounding_caps.clone().unwrap();
    wide.push("CAP_SYS_ADMIN".into());
    facts.bounding_caps = Some(wide);
    assert!(!Dialect::Podman.capabilities_ok(None, None, Some(&facts), &requested, true));
    facts.bounding_caps = None;
    assert!(!Dialect::Podman.capabilities_ok(None, None, Some(&facts), &requested, true));
}

/// Security review of #397: a GPU expands to NVIDIA and DRM nodes exactly, and a path
/// that walks out of them is refused however it is spelled.
#[test]
fn gpu_expansion_is_exact_and_refuses_traversal() {
    for ok in [
        "/dev/nvidia0",
        "/dev/nvidia12",
        "/dev/nvidiactl",
        "/dev/nvidia-uvm",
        "/dev/nvidia-uvm-tools",
        "/dev/nvidia-modeset",
        "/dev/nvidia-caps/nvidia-cap1",
        "/dev/dri/card1",
        "/dev/dri/renderD128",
    ] {
        assert!(
            Dialect::Podman.devices_ok(&[device(ok, "")], &[], true),
            "{ok}"
        );
    }
    for bad in [
        "/dev/dri/../sda",
        "/dev/dri/./card1",
        "/dev/dri/by-path/x",
        "/dev/nvidia-evil",
        "/dev/sda",
        "/dev/kmsg",
        "/dev/nvidia0/../sda",
        "dev/nvidia0",
        "/dev/dri/card",
        "/dev/nvidiactl2",
    ] {
        assert!(
            !Dialect::Podman.devices_ok(&[device(bad, "")], &[], true),
            "{bad}"
        );
    }
    // A requested device spelled with traversal is refused too.
    assert!(!Dialect::Podman.devices_ok(
        &[device("/dev/dri/../sda", "")],
        &["/dev/dri/../sda".to_string()],
        false
    ));
}

#[test]
fn podman_mounts_must_be_private() {
    let mut facts = podman(&[], "crun");
    assert!(Dialect::Podman.mount_propagation_ok(Some(&facts)));
    facts.mount_propagations = vec!["rprivate".into(), "rshared".into()];
    assert!(!Dialect::Podman.mount_propagation_ok(Some(&facts)));
    assert!(!Dialect::Podman.mount_propagation_ok(None));
    assert!(Dialect::Docker.mount_propagation_ok(None));
    let parsed = PodmanFacts::from_inspect(
        r#"{"EffectiveCaps":[],"BoundingCaps":[],"OCIRuntime":"crun","Mounts":[{"Propagation":"rprivate"},{"Propagation":"rslave"}]}"#,
    )
    .unwrap();
    assert!(!Dialect::Podman.mount_propagation_ok(Some(&parsed)));
}

#[test]
fn each_injection_reads_back_only_its_own_request() {
    use crate::runtime::GpuInjection;
    for injection in [GpuInjection::Cdi, GpuInjection::DeviceRequest] {
        assert!(is_nvidia_request(
            injection,
            &nvidia_device_request(injection)
        ));
    }
    assert!(!is_nvidia_request(
        GpuInjection::Cdi,
        &nvidia_device_request(GpuInjection::DeviceRequest)
    ));
    assert!(!is_nvidia_request(
        GpuInjection::DeviceRequest,
        &nvidia_device_request(GpuInjection::Cdi)
    ));
    // Docker echoes a CDI request with Count 0; a wider device set is never ours.
    let mut echoed = nvidia_device_request(GpuInjection::Cdi);
    echoed.count = Some(0);
    assert!(is_nvidia_request(GpuInjection::Cdi, &echoed));
    echoed.device_ids = Some(vec!["nvidia.com/gpu=0".into(), "vendor.com/x=all".into()]);
    assert!(!is_nvidia_request(GpuInjection::Cdi, &echoed));
}

/// Live on rootless Podman: a requested `/dev/dri` is reported as its nodes, and a CDI GPU
/// as the nodes its specification lists (the DRM ones included).
#[test]
fn podman_reports_a_requested_directory_as_its_drm_nodes_and_nothing_else() {
    let requested = vec!["/dev/dri".to_string()];
    let expanded = [
        device("/dev/dri/card1", ""),
        device("/dev/dri/renderD128", ""),
    ];
    assert!(Dialect::Podman.devices_ok(&expanded, &requested, false));
    for foreign in ["/dev/dri/by-path", "/dev/sda", "/dev/nvidia0"] {
        assert!(
            !Dialect::Podman.devices_ok(&[device(foreign, "")], &requested, false),
            "{foreign}"
        );
    }
    let cdi = [
        device("/dev/nvidiactl", ""),
        device("/dev/nvidia0", ""),
        device("/dev/dri/card1", ""),
    ];
    assert!(Dialect::Podman.devices_ok(&cdi, &requested, true));
    assert!(!Dialect::Docker.devices_ok(&expanded, &requested, false));
}

/// Captured live on rootless Podman 5.8.4 (`keep-id:uid=1000,gid=1000`).
#[test]
fn keep_id_is_proven_by_the_one_range_mapped_onto_the_engine_user() {
    let body = r#"{"HostConfig":{"IDMappings":{"UidMap":["0:1:1000","1000:0:1","1001:1001:64536"],"GidMap":["0:1:1000","1000:0:1","1001:1001:64536"]}}}"#;
    let facts = PodmanFacts::from_inspect(body).unwrap();
    assert!(keep_id_ok(Some(&facts), 1000, 1000));
    assert!(!keep_id_ok(Some(&facts), 1001, 1000));
    // The rootless default maps container root onto the engine user: not keep-id.
    let default = PodmanFacts::from_inspect(
        r#"{"HostConfig":{"IDMappings":{"UidMap":["0:0:1","1:1:65536"],"GidMap":["0:0:1","1:1:65536"]}}}"#,
    )
    .unwrap();
    assert!(!keep_id_ok(Some(&default), 1000, 1000));
    assert!(!keep_id_ok(None, 1000, 1000));
}
