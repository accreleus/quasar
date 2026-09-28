//! Runtime and CDI readiness (#254) at the fake-root boundary: engine facts injected as a
//! `RuntimeView`, verdicts read back through `probe`. Observation only: no check here may
//! imply a mutation, and an engine that does not answer is reported as unreachable, never
//! as an engine with missing containers.

use super::super::runtime_facts::*;
use super::super::*;
use super::{get, FakeRoot};
use crate::messages::ReadinessBlocks;
use crate::runtime::{
    ApiVersion, CdiFacts, EngineFacts, EngineInfo, EngineKind, EngineMode, ErrorKind, RuntimeError,
    API_FLOOR,
};

const ENDPOINT: &str = "unix:///var/run/docker.sock";

fn api(major: usize, minor: usize) -> ApiVersion {
    ApiVersion { major, minor }
}

fn facts(cdi: Option<CdiFacts>) -> EngineFacts {
    EngineFacts {
        info: EngineInfo {
            kind: EngineKind::Docker,
            name: "Docker Engine - Community".into(),
            version: "28.0.0".into(),
            api_version: api(1, 48),
            server_min_api: api(1, 24),
            server_max_api: api(1, 48),
        },
        mode: EngineMode::Rootful,
        operating_system: Some("Ubuntu 24.04".into()),
        architecture: Some("x86_64".into()),
        cgroup_version: Some("2".into()),
        cgroup_driver: Some("systemd".into()),
        security_options: vec![
            "name=seccomp,profile=builtin".into(),
            "name=cgroupns".into(),
        ],
        runtimes: vec!["nvidia".into(), "runc".into()],
        default_runtime: Some("runc".into()),
        cdi,
    }
}

fn observed(root: &FakeRoot, outcome: Result<EngineFacts, RuntimeFault>) -> ProbeEnv {
    ProbeEnv {
        runtime: RuntimeView::Observed {
            endpoint: ENDPOINT.into(),
            outcome,
        },
        ..root.env(false, "")
    }
}

fn cdi_enabled_with_gpu() -> CdiFacts {
    CdiFacts {
        spec_dirs: vec!["/etc/cdi".into(), "/var/run/cdi".into()],
        devices: vec![
            "nvidia.com/gpu=0 (cdi)".into(),
            "nvidia.com/gpu=all (cdi)".into(),
        ],
    }
}

#[test]
fn a_reachable_engine_passes_endpoint_api_and_capabilities_with_source_wording() {
    let root = FakeRoot::new("runtime-reachable");
    let checks = probe(&observed(&root, Ok(facts(Some(cdi_enabled_with_gpu())))));

    let endpoint = get(&checks, ENDPOINT_ID);
    assert_eq!(endpoint.status, PASS, "{endpoint:?}");
    assert!(
        endpoint.summary.contains(ENDPOINT),
        "names the endpoint: {endpoint:?}"
    );
    assert!(
        endpoint.summary.contains("28.0.0"),
        "names the engine: {endpoint:?}"
    );

    let api = get(&checks, API_VERSION_ID);
    assert_eq!(api.status, PASS, "{api:?}");
    assert!(
        api.summary.contains("1.48"),
        "the negotiated version: {api:?}"
    );
    assert!(api.summary.contains("1.24"), "the engine's range: {api:?}");

    let caps = get(&checks, CAPABILITIES_ID);
    assert_eq!(caps.status, PASS, "{caps:?}");
    for word in [
        "Ubuntu 24.04",
        "x86_64",
        "cgroup v2",
        "seccomp",
        "nvidia",
        "runc",
    ] {
        assert!(caps.summary.contains(word), "{word} in {caps:?}");
    }
    assert!(caps.remediation.is_empty());
}

#[test]
fn an_unreachable_engine_fails_the_endpoint_and_skips_the_rest() {
    let root = FakeRoot::new("runtime-unreachable");
    let checks = probe(&observed(
        &root,
        Err(RuntimeFault::Unreachable("connection refused".into())),
    ));

    let endpoint = get(&checks, ENDPOINT_ID);
    assert_eq!(endpoint.status, FAIL, "{endpoint:?}");
    assert!(endpoint.summary.contains("unreachable"), "{endpoint:?}");
    assert!(
        endpoint.summary.contains("connection refused"),
        "{endpoint:?}"
    );
    assert!(endpoint.summary.contains(ENDPOINT), "{endpoint:?}");
    assert!(
        endpoint.remediation.contains("docker.sock")
            || endpoint.remediation.contains("DOCKER_HOST"),
        "{endpoint:?}"
    );
    for id in [API_VERSION_ID, CAPABILITIES_ID, CDI_ID] {
        let c = get(&checks, id);
        assert_eq!(c.status, SKIP, "{c:?}");
    }
    // Never a claim about what is or is not running on an engine nobody could ask.
    for c in checks.iter().filter(|c| c.id.starts_with("runtime_")) {
        let text = format!("{} {}", c.summary, c.remediation).to_lowercase();
        assert!(
            !text.contains("missing container") && !text.contains("no container"),
            "{c:?}"
        );
    }
}

/// #274: a HUNG engine — a socket that accepts the connection and then never answers, the
/// shape a SIGSTOPped dockerd has — must fail `runtime_endpoint` inside the inspection
/// budget, with the same wording and the same remediation an absent engine gets. Driven at
/// the runtime-client seam on a real silent socket, with the client deadline shrunk so the
/// test costs milliseconds; the budget itself is guarded by
/// `the_engine_budget_beats_the_control_planes_staleness_window`.
#[test]
fn a_hung_engine_fails_the_endpoint_within_the_inspection_budget() {
    use crate::runtime::{RuntimeClient, RuntimeConfig};
    use std::os::unix::net::UnixListener;
    use std::time::{Duration, Instant};

    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("hung.sock");
    // Bound but never accepted: the connect succeeds from the kernel backlog and the
    // request is never answered. Connection failure would be a different defect.
    let _listener = UnixListener::bind(&socket).unwrap();
    let mut config = RuntimeConfig::unix(socket);
    config.deadline = Duration::from_millis(200);
    let client = RuntimeClient::new(config).unwrap();

    let started = Instant::now();
    let view = RuntimeView::observe(&client);
    let elapsed = started.elapsed();
    assert!(
        elapsed < Duration::from_secs(2),
        "the inspection was bounded, not left on the hung engine: {elapsed:?}"
    );

    assert!(
        !view.engine_answered(),
        "a hung engine did not answer, so the refresh must skip the other engine calls: {view:?}"
    );

    let check = check_runtime_endpoint(&view);
    assert_eq!(check.status, FAIL, "{check:?}");
    assert!(
        check.summary.contains("unreachable")
            && check
                .summary
                .contains("did not answer within the inspection budget"),
        "the summary says the engine did not answer in time: {check:?}"
    );
    assert!(
        check.remediation.contains("docker.sock") || check.remediation.contains("DOCKER_HOST"),
        "the existing remediation is unchanged: {check:?}"
    );
}

/// The bound is only useful if a failing report reaches the control plane before it stops
/// trusting the last one. Two cases, and the slower one is what the window must survive.
///
/// **Refresh starts after the freeze.** Its engine view is the first thing it builds and
/// comes back failing within one budget; every other collector is then skipped. Cost: up
/// to one refresh interval of tick latency, plus one budget.
///
/// **Refresh straddles the freeze.** Its engine view ran before the daemon stopped
/// answering, so it saw a healthy engine, does NOT skip its collectors, and carries a
/// stale `pass` when it finishes. Single-flight (`readiness_busy`) holds the next refresh
/// behind it. The failing report is therefore the one after: the straddling refresh's own
/// remaining engine calls, then a tick, then a fast refresh. Four budgeted calls bound the
/// straddler — the agent's own mount inspection, the EGL probe's image lookup, the image
/// root (two calls), and the firewall's network-mode read — which is what
/// `readiness_engine_budget_convention.rs` enforces file by file.
#[test]
fn the_engine_budget_beats_the_control_planes_staleness_window() {
    use crate::agent::READINESS_REFRESH_INTERVAL;
    use crate::runtime::ENGINE_INSPECTION_BUDGET;
    use std::time::Duration;

    /// `QUASAR_READINESS_STALE_SECS`' default in the control plane's readiness gate.
    const STALE_DEFAULT: Duration = Duration::from_secs(60);
    /// Budgeted engine calls a refresh that did NOT skip its collectors still makes.
    const STRADDLER_ENGINE_CALLS: u32 = 5;

    let fresh_refresh = READINESS_REFRESH_INTERVAL + ENGINE_INSPECTION_BUDGET;
    assert!(
        fresh_refresh <= Duration::from_secs(20),
        "a hang that lands between refreshes must be reported within about 15 s: \
         {fresh_refresh:?}"
    );

    let straddling = ENGINE_INSPECTION_BUDGET * STRADDLER_ENGINE_CALLS + fresh_refresh;
    assert!(
        straddling < STALE_DEFAULT,
        "even a refresh that straddled the freeze must get the failing report out before \
         the gate stops trusting the last one: {straddling:?} vs {STALE_DEFAULT:?}"
    );
}

/// Which faults let the refresh skip the rest of its engine calls. Only a definitive
/// "the engine is not usable" answer does; anything that proves an engine is there keeps
/// the full refresh, because those collectors still have real work to do.
#[test]
fn only_a_definitive_engine_fault_skips_the_other_engine_calls() {
    let observed = |outcome| RuntimeView::Observed {
        endpoint: ENDPOINT.into(),
        outcome,
    };
    for fault in [
        RuntimeFault::Unreachable("no engine answered".into()),
        RuntimeFault::PermissionDenied("refused".into()),
        RuntimeFault::Unconfigured("bad DOCKER_HOST".into()),
    ] {
        assert!(
            !observed(Err(fault.clone())).engine_answered(),
            "{fault:?} is definitive"
        );
    }
    for fault in [
        RuntimeFault::IncompatibleApi("too old".into()),
        RuntimeFault::Indeterminate("busy".into()),
    ] {
        assert!(
            observed(Err(fault.clone())).engine_answered(),
            "{fault:?} still proves an engine is there"
        );
    }
    assert!(observed(Ok(facts(None))).engine_answered());
    assert!(
        RuntimeView::NotObserved.engine_answered(),
        "fixtures must not disable the other collectors"
    );
}

#[test]
fn a_timeout_reads_as_unreachable() {
    let fault = RuntimeFault::from(RuntimeError::from(ErrorKind::Timeout));
    assert!(matches!(fault, RuntimeFault::Unreachable(_)), "{fault:?}");
    let fault = RuntimeFault::from(RuntimeError::from(ErrorKind::Unavailable));
    assert!(matches!(fault, RuntimeFault::Unreachable(_)), "{fault:?}");
    assert!(matches!(
        RuntimeFault::from(RuntimeError::from(ErrorKind::PermissionDenied)),
        RuntimeFault::PermissionDenied(_)
    ));
    assert!(matches!(
        RuntimeFault::from(RuntimeError::from(ErrorKind::IncompatibleApi)),
        RuntimeFault::IncompatibleApi(_)
    ));
    assert!(matches!(
        RuntimeFault::from(RuntimeError::from(ErrorKind::InvalidConfiguration)),
        RuntimeFault::Unconfigured(_)
    ));
    // A busy client is not a broken engine.
    assert!(matches!(
        RuntimeFault::from(RuntimeError::from(ErrorKind::Busy)),
        RuntimeFault::Indeterminate(_)
    ));
}

#[test]
fn an_incompatible_api_fails_the_api_check_with_the_floor_and_keeps_the_endpoint_reachable() {
    let root = FakeRoot::new("runtime-incompatible");
    let checks = probe(&observed(
        &root,
        Err(RuntimeFault::IncompatibleApi(
            "engine offers 1.20-1.38".into(),
        )),
    ));

    let endpoint = get(&checks, ENDPOINT_ID);
    assert_eq!(endpoint.status, PASS, "the engine answered: {endpoint:?}");
    let api = get(&checks, API_VERSION_ID);
    assert_eq!(api.status, FAIL, "{api:?}");
    assert!(api.summary.contains(&API_FLOOR.to_string()), "{api:?}");
    assert!(
        api.remediation.to_lowercase().contains("upgrade"),
        "{api:?}"
    );
    assert_eq!(get(&checks, CAPABILITIES_ID).status, SKIP);
    assert_eq!(get(&checks, CDI_ID).status, SKIP);
}

/// #266: the floor lives once, in the runtime module that enforces it. Every arm of
/// `runtime_api_version` that names a floor must therefore name THAT one — a bump in
/// discovery cannot leave the operator reading the retired number.
#[test]
fn the_api_version_wording_quotes_the_floor_discovery_enforces() {
    let floor = API_FLOOR.to_string();
    let root = FakeRoot::new("runtime-floor-wording");

    let incompatible = probe(&observed(
        &root,
        Err(RuntimeFault::IncompatibleApi(
            "engine offers 1.20-1.38".into(),
        )),
    ));
    let failing = get(&incompatible, API_VERSION_ID);
    assert!(failing.summary.contains(&floor), "{failing:?}");
    assert!(failing.remediation.contains(&floor), "{failing:?}");

    let negotiated = probe(&observed(&root, Ok(facts(None))));
    let passing = get(&negotiated, API_VERSION_ID);
    assert!(passing.summary.contains(&floor), "{passing:?}");

    // And the one the agent really refuses below, spelled out so a bump has to come here.
    assert_eq!(API_FLOOR, api(1, 40));
}

#[test]
fn a_denied_socket_fails_the_endpoint_with_a_permission_fix() {
    let root = FakeRoot::new("runtime-denied");
    let checks = probe(&observed(
        &root,
        Err(RuntimeFault::PermissionDenied("permission denied".into())),
    ));
    let endpoint = get(&checks, ENDPOINT_ID);
    assert_eq!(endpoint.status, FAIL, "{endpoint:?}");
    assert!(
        endpoint.remediation.to_lowercase().contains("permission")
            || endpoint.remediation.contains("group"),
        "{endpoint:?}"
    );
}

#[test]
fn an_indeterminate_inspection_warns_and_never_fails() {
    let root = FakeRoot::new("runtime-busy");
    let checks = probe(&observed(
        &root,
        Err(RuntimeFault::Indeterminate("busy".into())),
    ));
    let endpoint = get(&checks, ENDPOINT_ID);
    assert_eq!(endpoint.status, WARN, "{endpoint:?}");
    assert!(endpoint.summary.contains("busy"), "{endpoint:?}");
}

fn nvidia_env(root: &FakeRoot, facts: EngineFacts, gpus: Vec<(i32, bool)>) -> ProbeEnv {
    ProbeEnv {
        runtime: RuntimeView::Observed {
            endpoint: ENDPOINT.into(),
            outcome: Ok(facts),
        },
        gpus,
        ..root.env(true, "")
    }
}

fn rootless(mut f: EngineFacts) -> EngineFacts {
    f.mode = EngineMode::Rootless;
    f
}

fn no_cdi() -> Option<CdiFacts> {
    Some(CdiFacts {
        spec_dirs: vec!["/etc/cdi".into()],
        devices: vec![],
    })
}

#[test]
fn a_host_without_nvidia_passes_and_blocks_nothing() {
    let root = FakeRoot::new("runtime-cdi-amd");
    let checks = probe(&observed(&root, Ok(facts(no_cdi()))));
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, PASS, "{c:?}");
    assert!(c.summary.contains("No NVIDIA GPU"), "{c:?}");
    assert!(c.blocks.is_none(), "{c:?}");
}

#[test]
fn nvidia_on_an_engine_with_an_nvidia_cdi_device_goes_by_cdi_and_blocks_the_host() {
    let root = FakeRoot::new("runtime-cdi-nvidia");
    let checks = probe(&nvidia_env(
        &root,
        rootless(facts(Some(cdi_enabled_with_gpu()))),
        vec![(0, true)],
    ));
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, PASS, "{c:?}");
    assert!(c.summary.contains("by CDI (nvidia.com/gpu=all)"), "{c:?}");
    assert_eq!(c.blocks, Some(ReadinessBlocks::host("control_plane")));
}

#[test]
fn nvidia_on_rootful_docker_without_cdi_goes_by_device_request() {
    let root = FakeRoot::new("runtime-cdi-gpus");
    let checks = probe(&nvidia_env(&root, facts(no_cdi()), vec![(0, true)]));
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, PASS, "{c:?}");
    assert!(c.summary.contains("--gpus"), "{c:?}");
}

/// Only the device Quasar requests counts: a specification listing per-index devices alone
/// cannot serve `nvidia.com/gpu=all`.
#[test]
fn a_cdi_spec_without_the_all_device_is_not_used() {
    let root = FakeRoot::new("runtime-cdi-index-only");
    let per_index = Some(CdiFacts {
        spec_dirs: vec!["/etc/cdi".into()],
        devices: vec!["nvidia.com/gpu=0 (cdi)".into()],
    });
    let checks = probe(&nvidia_env(
        &root,
        rootless(facts(per_index)),
        vec![(0, true)],
    ));
    assert_eq!(get(&checks, CDI_ID).status, FAIL);
}

/// Podman's `/info` lists no CDI devices, so its evidence is the agent's own container:
/// the NVIDIA nodes CDI put there, or their absence.
#[test]
fn podman_goes_by_cdi_with_the_agents_own_nvidia_nodes_as_evidence() {
    let root = FakeRoot::new("runtime-cdi-podman");
    let mut f = rootless(facts(None));
    f.info.kind = EngineKind::Podman;
    let checks = probe(&nvidia_env(&root, f.clone(), vec![(0, true)]));
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, FAIL, "{c:?}");
    assert!(c.remediation.contains("prepare-host.sh"), "{c:?}");
    assert_eq!(c.blocks, Some(ReadinessBlocks::host("control_plane")));

    root.file("dev/nvidiactl", "");
    let checks = probe(&nvidia_env(&root, f, vec![(0, true)]));
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, PASS, "{c:?}");
    assert!(c.summary.contains("by CDI"), "{c:?}");
}

/// Amendment 17: an NVIDIA host whose engine can inject the GPU by neither CDI nor
/// `--gpus` fails, names host preparation, and never suggests running as root.
#[test]
fn nvidia_on_rootless_docker_without_cdi_fails_naming_host_preparation() {
    let root = FakeRoot::new("runtime-cdi-none");
    let checks = probe(&nvidia_env(
        &root,
        rootless(facts(no_cdi())),
        vec![(0, true)],
    ));
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, FAIL, "{c:?}");
    assert!(c.remediation.contains("prepare-host.sh"), "{c:?}");
    assert!(c.remediation.contains("Never run Quasar as root"), "{c:?}");
    assert_eq!(c.blocks, Some(ReadinessBlocks::host("control_plane")));
    assert!(!checks.iter().any(|c| c.id.starts_with("runtime_cdi_gpu")));
}

#[test]
fn a_mixed_host_blocks_each_nvidia_gpu_not_the_host() {
    let root = FakeRoot::new("runtime-cdi-mixed");
    let checks = probe(&nvidia_env(
        &root,
        rootless(facts(no_cdi())),
        vec![(0, false), (1, true)],
    ));
    let host = get(&checks, CDI_ID);
    assert_eq!(host.status, FAIL, "{host:?}");
    assert!(host.blocks.is_none(), "{host:?}");
    let gpu = get(&checks, "runtime_cdi_gpu1");
    assert_eq!(gpu.status, FAIL, "{gpu:?}");
    assert_eq!(gpu.blocks, Some(ReadinessBlocks::gpu(1, "control_plane")));
    assert!(!checks.iter().any(|c| c.id == "runtime_cdi_gpu0"));
}

#[test]
fn an_engine_that_could_not_be_inspected_skips_cdi_without_blocking() {
    let root = FakeRoot::new("runtime-cdi-unknown");
    let checks = probe(&ProbeEnv {
        runtime: RuntimeView::Observed {
            endpoint: ENDPOINT.into(),
            outcome: Err(RuntimeFault::Indeterminate("busy".into())),
        },
        gpus: vec![(0, true)],
        ..root.env(true, "")
    });
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, SKIP, "{c:?}");
    assert!(c.blocks.is_none(), "{c:?}");
}

#[test]
fn an_unobserved_runtime_skips_every_runtime_check() {
    let root = FakeRoot::new("runtime-unobserved");
    let checks = probe(&ProbeEnv {
        runtime: RuntimeView::NotObserved,
        ..root.env(false, "")
    });
    for id in [ENDPOINT_ID, API_VERSION_ID, CAPABILITIES_ID, CDI_ID] {
        assert_eq!(get(&checks, id).status, SKIP, "{id}");
    }
}

#[test]
fn an_invalid_endpoint_configuration_fails_the_endpoint_naming_the_knob() {
    let root = FakeRoot::new("runtime-unconfigured");
    let checks = probe(&observed(
        &root,
        Err(RuntimeFault::Unconfigured("DOCKER_CONTEXT is set".into())),
    ));
    let endpoint = get(&checks, ENDPOINT_ID);
    assert_eq!(endpoint.status, FAIL, "{endpoint:?}");
    assert!(endpoint.remediation.contains("DOCKER_HOST"), "{endpoint:?}");
}

// ── RH-07 #396: the engine, its mode, and remediation that names them ───────

fn engine(kind: EngineKind, version: &str, mode: EngineMode, os: &str) -> EngineFacts {
    let mut f = facts(None);
    f.info.kind = kind;
    f.info.version = version.into();
    f.mode = mode;
    f.operating_system = Some(os.into());
    f
}

#[test]
fn the_engine_check_names_the_engine_its_version_and_mode() {
    let root = FakeRoot::new("runtime-engine-docker");
    let checks = probe(&observed(
        &root,
        Ok(engine(
            EngineKind::Docker,
            "29.7.2",
            EngineMode::Rootful,
            "Fedora Linux 43",
        )),
    ));
    let c = get(&checks, ENGINE_ID);
    assert_eq!(c.status, PASS, "{c:?}");
    assert!(c.summary.contains("Docker 29.7.2, rootful"), "{c:?}");
    assert_eq!(c.source.as_deref(), Some("runtime"));
    assert!(c.blocks.is_none(), "{c:?}");
}

/// Until the RH-07 acceptance map proves them, the new profiles read as experimental:
/// a combination with no evidence is never implied supported (CONTEXT.md "Engine profile").
#[test]
fn a_profile_without_evidence_warns_as_experimental_and_blocks_nothing() {
    let root = FakeRoot::new("runtime-engine-podman");
    for (kind, mode) in [
        (EngineKind::Podman, EngineMode::Rootless),
        (EngineKind::Docker, EngineMode::Rootless),
        (EngineKind::Podman, EngineMode::Rootful),
    ] {
        let checks = probe(&observed(&root, Ok(engine(kind, "5.8.4", mode, "fedora"))));
        let c = get(&checks, ENGINE_ID);
        assert_eq!(c.status, WARN, "{c:?}");
        assert!(c.summary.contains("experimental"), "{c:?}");
        assert!(c.summary.contains(mode.wire()), "{c:?}");
        assert!(c.blocks.is_none(), "{c:?}");
    }
    let checks = probe(&observed(
        &root,
        Ok(engine(
            EngineKind::Podman,
            "5.8.4",
            EngineMode::Rootless,
            "Debian GNU/Linux 13",
        )),
    ));
    let c = get(&checks, ENGINE_ID);
    assert_eq!(c.status, WARN, "{c:?}");
    assert!(c.summary.contains("unsupported"), "{c:?}");
    assert!(
        c.remediation.contains("Docker rootful"),
        "names the alternative: {c:?}"
    );
}

#[test]
fn an_engine_that_cannot_be_named_is_unsupported_not_docker() {
    let root = FakeRoot::new("runtime-engine-unknown");
    let checks = probe(&observed(
        &root,
        Ok(engine(
            EngineKind::Unknown,
            "1.0",
            EngineMode::Rootful,
            "fedora",
        )),
    ));
    let c = get(&checks, ENGINE_ID);
    assert_eq!(c.status, WARN, "{c:?}");
    assert!(c.summary.contains("unsupported"), "{c:?}");
    assert!(!c.summary.contains("Docker 1.0"), "{c:?}");
}

/// The endpoint checks name the socket and both engines, and never tell an operator to
/// run Quasar as root (#396 acceptance).
#[test]
fn no_endpoint_remediation_names_only_docker_or_suggests_root() {
    let root = FakeRoot::new("runtime-remediation");
    for fault in [
        RuntimeFault::Unreachable("connection refused".into()),
        RuntimeFault::PermissionDenied("permission denied".into()),
        RuntimeFault::Unconfigured("DOCKER_CONTEXT is set".into()),
        RuntimeFault::Ambiguous("two endpoints".into()),
    ] {
        let checks = probe(&observed(&root, Err(fault.clone())));
        let c = get(&checks, ENDPOINT_ID);
        assert_eq!(c.status, FAIL, "{c:?}");
        let text = c.remediation.to_lowercase();
        assert!(
            !text.contains("as root") && !text.contains("run it as root"),
            "{c:?}"
        );
        assert!(text.contains("podman"), "names Podman too: {c:?}");
    }
    let checks = probe(&observed(
        &root,
        Err(RuntimeFault::Unreachable("refused".into())),
    ));
    assert!(get(&checks, ENDPOINT_ID)
        .remediation
        .contains("/var/run/docker.sock"));
}

#[test]
fn an_ambiguous_endpoint_is_refused_by_name_and_names_both_variables() {
    let root = FakeRoot::new("runtime-ambiguous");
    let fault = RuntimeFault::from(RuntimeError::from(ErrorKind::AmbiguousEndpoint));
    assert!(matches!(fault, RuntimeFault::Ambiguous(_)), "{fault:?}");
    let checks = probe(&observed(&root, Err(fault)));
    let c = get(&checks, ENDPOINT_ID);
    assert_eq!(c.status, FAIL, "{c:?}");
    assert!(
        c.summary.contains("DOCKER_HOST") && c.summary.contains("CONTAINER_HOST"),
        "{c:?}"
    );
    assert!(
        c.blocks.is_some(),
        "an unusable endpoint keeps its agent-enforced block"
    );
}

// ── RH-07 #405: can this engine run container health checks? ────────────────

#[test]
fn podman_with_systemd_runs_health_checks_and_docker_skips() {
    let root = FakeRoot::new("runtime-healthchecks");
    let podman = engine(EngineKind::Podman, "5.8.4", EngineMode::Rootless, "fedora");
    let checks = probe(&observed(&root, Ok(podman)));
    let c = get(&checks, HEALTHCHECKS_ID);
    assert_eq!(c.status, PASS, "{c:?}");
    assert!(c.blocks.is_none(), "a proxy check never blocks: {c:?}");
    let docker = engine(EngineKind::Docker, "29.7.2", EngineMode::Rootful, "fedora");
    let checks = probe(&observed(&root, Ok(docker)));
    assert_eq!(get(&checks, HEALTHCHECKS_ID).status, SKIP);
}

/// A rootless Podman without a systemd user session never runs health checks, and
/// installs wait on them: named, with the fix, instead of a timeout nobody can explain.
#[test]
fn podman_without_systemd_fails_health_checks_naming_the_fix() {
    let root = FakeRoot::new("runtime-healthchecks-cgroupfs");
    let mut podman = engine(EngineKind::Podman, "5.8.4", EngineMode::Rootless, "fedora");
    podman.cgroup_driver = Some("cgroupfs".into());
    let checks = probe(&observed(&root, Ok(podman)));
    let c = get(&checks, HEALTHCHECKS_ID);
    assert_eq!(c.status, FAIL, "{c:?}");
    assert!(c.summary.contains("cgroupfs"), "{c:?}");
    assert!(c.remediation.contains("linger"), "{c:?}");
    // Rootful Podman has no user session to linger: its fix is systemd as init.
    let mut rootful = engine(EngineKind::Podman, "5.8.4", EngineMode::Rootful, "fedora");
    rootful.cgroup_driver = Some("cgroupfs".into());
    let checks = probe(&observed(&root, Ok(rootful)));
    let c = get(&checks, HEALTHCHECKS_ID);
    assert!(
        !c.remediation.contains("linger") && c.remediation.contains("systemd"),
        "{c:?}"
    );
    assert!(!c.remediation.to_lowercase().contains("as root"), "{c:?}");
    assert!(c.blocks.is_none(), "{c:?}");
}

/// Live on rootless Podman: the engine injected no GPU, so capacity dropped it and reported
/// no NVIDIA. The agent still runs NVIDIA sessions, and `runtime_cdi` must say they cannot.
#[test]
fn an_nvidia_agent_whose_gpu_capacity_dropped_still_fails_cdi() {
    let root = FakeRoot::new("runtime-cdi-dropped");
    let mut f = rootless(facts(None));
    f.info.kind = EngineKind::Podman;
    let checks = probe(&ProbeEnv {
        runtime: RuntimeView::Observed {
            endpoint: ENDPOINT.into(),
            outcome: Ok(f),
        },
        nvidia_runtime: true,
        ..root.env(false, "")
    });
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, FAIL, "{c:?}");
    assert_eq!(c.blocks, Some(ReadinessBlocks::host("control_plane")));
}
