//! Runtime and CDI readiness (#254) at the fake-root boundary: engine facts injected as a
//! `RuntimeView`, verdicts read back through `probe`. Observation only: no check here may
//! imply a mutation, and an engine that does not answer is reported as unreachable, never
//! as an engine with missing containers.

use super::super::runtime_facts::*;
use super::super::*;
use super::{get, FakeRoot};
use crate::runtime::{
    ApiVersion, CdiFacts, EngineFacts, EngineInfo, ErrorKind, RuntimeError, API_FLOOR,
};

const ENDPOINT: &str = "unix:///var/run/docker.sock";

fn api(major: usize, minor: usize) -> ApiVersion {
    ApiVersion { major, minor }
}

fn facts(cdi: Option<CdiFacts>) -> EngineFacts {
    EngineFacts {
        info: EngineInfo {
            name: "Docker Engine - Community".into(),
            version: "28.0.0".into(),
            api_version: api(1, 48),
            server_min_api: api(1, 24),
            server_max_api: api(1, 48),
        },
        operating_system: Some("Ubuntu 24.04".into()),
        architecture: Some("x86_64".into()),
        cgroup_version: Some("2".into()),
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
        devices: vec!["nvidia.com/gpu=0 (cdi)".into()],
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

#[test]
fn cdi_enabled_with_devices_reports_dirs_and_devices() {
    let root = FakeRoot::new("runtime-cdi-devices");
    let checks = probe(&observed(&root, Ok(facts(Some(cdi_enabled_with_gpu())))));
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, PASS, "{c:?}");
    assert!(c.summary.contains("enabled"), "{c:?}");
    assert!(c.summary.contains("/etc/cdi"), "{c:?}");
    assert!(c.summary.contains("nvidia.com/gpu=0"), "{c:?}");
    assert!(
        c.remediation.is_empty(),
        "observed only, nothing to fix: {c:?}"
    );
}

#[test]
fn cdi_enabled_with_no_devices_says_so_without_a_verdict() {
    let root = FakeRoot::new("runtime-cdi-none");
    let checks = probe(&observed(
        &root,
        Ok(facts(Some(CdiFacts {
            spec_dirs: vec!["/etc/cdi".into()],
            devices: vec![],
        }))),
    ));
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, PASS, "{c:?}");
    assert!(c.summary.contains("enabled"), "{c:?}");
    assert!(c.summary.contains("no devices"), "{c:?}");
    assert!(c.remediation.is_empty(), "{c:?}");
}

#[test]
fn cdi_disabled_is_reported_and_names_how_gpus_are_really_injected() {
    let root = FakeRoot::new("runtime-cdi-disabled");
    let checks = probe(&observed(
        &root,
        Ok(facts(Some(CdiFacts {
            spec_dirs: vec![],
            devices: vec![],
        }))),
    ));
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, PASS, "{c:?}");
    assert!(c.summary.contains("disabled"), "{c:?}");
    assert!(c.summary.contains("device request"), "{c:?}");
    assert!(c.remediation.is_empty(), "{c:?}");
}

#[test]
fn cdi_not_reported_by_the_engine_skips() {
    let root = FakeRoot::new("runtime-cdi-unreported");
    let checks = probe(&observed(&root, Ok(facts(None))));
    let c = get(&checks, CDI_ID);
    assert_eq!(c.status, SKIP, "{c:?}");
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
