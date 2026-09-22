//! Container-runtime readiness (#254): what the engine really is, observed only. The facts
//! come from one bounded engine inspection per probe ([`RuntimeView::live`]); the verdicts
//! are the `check_*` functions, pure over the view. Nothing here mutates the engine, and
//! GPU injection (device request + driver volume) is untouched: CDI is reported, not used.

use crate::messages::{ReadinessBlocks, ReadinessCheck};
use crate::runtime::{EngineFacts, ErrorKind, RuntimeError};

pub const ENDPOINT_ID: &str = "runtime_endpoint";
pub const API_VERSION_ID: &str = "runtime_api_version";
pub const CAPABILITIES_ID: &str = "runtime_capabilities";
pub const CDI_ID: &str = "runtime_cdi";

/// The lowest engine API this agent speaks (docker.rs discovery floor).
pub const API_FLOOR: &str = "1.40";

/// Why the engine could not be inspected, folded from the runtime layer's error kinds so
/// the verdict reads the failure class, not the transport detail.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RuntimeFault {
    /// Nothing answered: socket missing, connection refused or reset, or the request timed
    /// out. A timeout reads as unreachable: an engine that cannot answer within the budget
    /// cannot run a session either.
    Unreachable(String),
    /// The socket exists but this agent may not use it.
    PermissionDenied(String),
    /// The engine answered but its API range does not include a version this agent speaks.
    IncompatibleApi(String),
    /// The endpoint configuration itself is invalid (docs/configuration.md, DOCKER_HOST).
    Unconfigured(String),
    /// The runtime client could not ask this refresh (busy, cancelled) or the reply made no
    /// sense; the verdict warns and the next refresh tries again.
    Inconclusive(String),
}

impl From<RuntimeError> for RuntimeFault {
    fn from(error: RuntimeError) -> Self {
        match error.kind {
            ErrorKind::Unavailable => RuntimeFault::Unreachable(
                "no engine answered at the socket (missing, refused or reset)".into(),
            ),
            ErrorKind::Timeout => RuntimeFault::Unreachable(
                "the engine did not answer within the inspection budget".into(),
            ),
            ErrorKind::PermissionDenied => {
                RuntimeFault::PermissionDenied("the socket refused this agent's identity".into())
            }
            ErrorKind::IncompatibleApi => RuntimeFault::IncompatibleApi(
                "the engine's API range does not include a version this agent speaks".into(),
            ),
            ErrorKind::InvalidConfiguration => RuntimeFault::Unconfigured(
                "the endpoint configuration was refused (DOCKER_HOST must be a unix:// socket; \
                 DOCKER_CONTEXT, DOCKER_TLS*, DOCKER_API_VERSION must be unset)"
                    .into(),
            ),
            ErrorKind::Busy | ErrorKind::Cancelled => {
                RuntimeFault::Inconclusive("the runtime client was busy this refresh".into())
            }
            _ => RuntimeFault::Inconclusive(error.to_string()),
        }
    }
}

/// The runtime facts one probe sees. One value per probe, never held in bulk, so the
/// variant size gap buys nothing to box.
#[derive(Debug, Clone, PartialEq, Eq)]
#[allow(clippy::large_enum_variant)]
pub enum RuntimeView {
    /// The engine was not asked (test fixtures; never in production).
    NotObserved,
    /// One inspection of `endpoint` and what it returned.
    Observed {
        endpoint: String,
        outcome: Result<EngineFacts, RuntimeFault>,
    },
}

impl RuntimeView {
    /// Production: one bounded `inspect_engine` on the configured client. The bound is
    /// [`crate::runtime::ENGINE_INSPECTION_BUDGET`]; a daemon that accepted the connection
    /// and will not answer only reveals itself when it expires (#274).
    pub fn live() -> Self {
        match crate::runtime::configured() {
            Ok(client) => RuntimeView::observe(client),
            Err(error) => RuntimeView::Observed {
                endpoint: std::env::var("DOCKER_HOST")
                    .ok()
                    .filter(|s| !s.is_empty())
                    .unwrap_or_else(|| "unix:///var/run/docker.sock".into()),
                outcome: Err(RuntimeFault::from(error)),
            },
        }
    }

    /// One bounded inspection of `client`. [`Self::live`] is this on the configured
    /// client; tests drive it with a stub engine on a real socket.
    pub fn observe(client: &crate::runtime::RuntimeClient) -> Self {
        RuntimeView::Observed {
            endpoint: client.endpoint(),
            outcome: client.inspect_engine().wait().map_err(RuntimeFault::from),
        }
    }

    /// Did the engine answer this refresh? `false` only for a **definitive** fault: nothing
    /// answered, the socket refused this agent, or the endpoint configuration is invalid.
    ///
    /// #274: a hung daemon (socket accepts, nothing replies) makes every other engine call
    /// in the same refresh spend its own full client deadline to reach the same verdict —
    /// measured at ~100 s serially, which pushed the failing `runtime_endpoint` past the
    /// control plane's readiness staleness window. Those collectors already degrade to
    /// exactly what they report on an engine error, so skipping them changes no verdict; it
    /// only stops the refresh paying for them. An incompatible API, a busy client or an
    /// unparseable reply all mean the engine is there, so they keep the full refresh.
    pub fn engine_answered(&self) -> bool {
        match self {
            // Fixtures never carry engine faults, and must not disable the other collectors.
            RuntimeView::NotObserved => true,
            RuntimeView::Observed { outcome, .. } => !matches!(
                outcome,
                Err(RuntimeFault::Unreachable(_)
                    | RuntimeFault::PermissionDenied(_)
                    | RuntimeFault::Unconfigured(_))
            ),
        }
    }
}

pub fn check_runtime_endpoint(view: &RuntimeView) -> ReadinessCheck {
    check_runtime_endpoint_inner(view)
        .with_source("runtime")
        // Agent-enforced: the agent refuses these launches itself, and no override lifts it.
        .with_blocks(ReadinessBlocks::host("agent"))
}

fn check_runtime_endpoint_inner(view: &RuntimeView) -> ReadinessCheck {
    let RuntimeView::Observed { endpoint, outcome } = view else {
        return super::skip(ENDPOINT_ID, "The container engine was not asked");
    };
    match outcome {
        Ok(facts) => super::pass(
            ENDPOINT_ID,
            format!(
                "{} {} at {endpoint} answers",
                facts.info.name, facts.info.version
            ),
        ),
        Err(RuntimeFault::Unreachable(reason)) => super::fail(
            ENDPOINT_ID,
            format!("the container runtime at {endpoint} is unreachable: {reason}"),
            "Check that Docker is running on the host and that its socket \
             (/var/run/docker.sock by default, or the DOCKER_HOST unix:// path) is mounted \
             into the agent container; recreate the agent after changing the mount."
                .into(),
        ),
        Err(RuntimeFault::PermissionDenied(reason)) => super::fail(
            ENDPOINT_ID,
            format!("the container runtime at {endpoint} refused this agent: {reason}"),
            "Grant the agent permission on the engine socket: run it as root or add its user \
             to the group owning /var/run/docker.sock, then recreate the agent."
                .into(),
        ),
        Err(RuntimeFault::IncompatibleApi(_)) => super::pass(
            ENDPOINT_ID,
            format!(
                "the container runtime at {endpoint} answered; its API is incompatible \
                 (see runtime_api_version)"
            ),
        ),
        Err(RuntimeFault::Unconfigured(reason)) => super::fail(
            ENDPOINT_ID,
            format!("the container runtime endpoint configuration is invalid: {reason}"),
            "Set DOCKER_HOST to a unix:// socket, or leave it unset for /var/run/docker.sock, \
             and unset DOCKER_CONTEXT, DOCKER_TLS, DOCKER_TLS_VERIFY and DOCKER_API_VERSION; \
             the agent speaks to one explicit Unix endpoint (docs/configuration.md)."
                .into(),
        ),
        Err(RuntimeFault::Inconclusive(reason)) => super::warn_check(
            ENDPOINT_ID,
            format!(
                "the container runtime at {endpoint} could not be inspected this refresh: {reason}"
            ),
            "The agent retries on the next refresh; check its logs if this persists.".into(),
        ),
    }
}

pub fn check_runtime_api_version(view: &RuntimeView) -> ReadinessCheck {
    check_runtime_api_version_inner(view).with_source("runtime")
}

fn check_runtime_api_version_inner(view: &RuntimeView) -> ReadinessCheck {
    let RuntimeView::Observed { outcome, .. } = view else {
        return super::skip(API_VERSION_ID, "The container engine was not asked");
    };
    match outcome {
        Ok(facts) => super::pass(
            API_VERSION_ID,
            format!(
                "API {} negotiated with {} {} (the engine offers {}-{}; this agent needs at \
                 least {API_FLOOR})",
                facts.info.api_version,
                facts.info.name,
                facts.info.version,
                facts.info.server_min_api,
                facts.info.server_max_api,
            ),
        ),
        Err(RuntimeFault::IncompatibleApi(reason)) => super::fail(
            API_VERSION_ID,
            format!("{reason} (this agent needs at least API {API_FLOOR})"),
            format!(
                "Upgrade the container engine to a release offering API {API_FLOOR} or newer \
                 (Docker Engine 19.03 or later)."
            ),
        ),
        Err(_) => super::skip(
            API_VERSION_ID,
            "No API was negotiated: the engine could not be inspected",
        ),
    }
}

pub fn check_runtime_capabilities(view: &RuntimeView) -> ReadinessCheck {
    check_runtime_capabilities_inner(view).with_source("runtime")
}

fn check_runtime_capabilities_inner(view: &RuntimeView) -> ReadinessCheck {
    let RuntimeView::Observed { outcome, .. } = view else {
        return super::skip(CAPABILITIES_ID, "The container engine was not asked");
    };
    match outcome {
        Ok(facts) => {
            let os = facts.operating_system.as_deref().unwrap_or("unknown");
            let arch = facts.architecture.as_deref().unwrap_or("unknown");
            let cgroup = facts
                .cgroup_version
                .as_deref()
                .map(|v| format!("cgroup v{v}"))
                .unwrap_or_else(|| "cgroup unknown".into());
            let security = if facts.security_options.is_empty() {
                "none reported".to_string()
            } else {
                facts.security_options.join(", ")
            };
            let runtimes = if facts.runtimes.is_empty() {
                "none reported".to_string()
            } else {
                facts.runtimes.join(", ")
            };
            let default = facts.default_runtime.as_deref().unwrap_or("unknown");
            super::pass(
                CAPABILITIES_ID,
                format!(
                    "{} {} on {os} ({arch}), {cgroup}; security options: {security}; \
                     runtimes: {runtimes} (default {default}); as stated by the engine, not exercised",
                    facts.info.name, facts.info.version,
                ),
            )
        }
        Err(_) => super::skip(
            CAPABILITIES_ID,
            "The engine's capabilities are unknown: it could not be inspected",
        ),
    }
}

pub fn check_runtime_cdi(view: &RuntimeView) -> ReadinessCheck {
    check_runtime_cdi_inner(view).with_source("runtime")
}

fn check_runtime_cdi_inner(view: &RuntimeView) -> ReadinessCheck {
    let RuntimeView::Observed { outcome, .. } = view else {
        return super::skip(CDI_ID, "The container engine was not asked");
    };
    match outcome {
        Ok(facts) => match &facts.cdi {
            None => super::skip(CDI_ID, "This engine does not report CDI"),
            Some(cdi) if cdi.spec_dirs.is_empty() => super::pass(
                CDI_ID,
                "CDI injection is disabled on this engine; Quasar injects GPUs with a device \
                 request and the driver volume, not CDI"
                    .into(),
            ),
            Some(cdi) if cdi.devices.is_empty() => super::pass(
                CDI_ID,
                format!(
                    "CDI is enabled (spec dirs: {}) and the engine discovered no devices; GPU \
                     injection does not use CDI",
                    cdi.spec_dirs.join(", ")
                ),
            ),
            Some(cdi) => super::pass(
                CDI_ID,
                format!(
                    "CDI is enabled (spec dirs: {}); devices discovered: {}; GPU injection does \
                     not use CDI",
                    cdi.spec_dirs.join(", "),
                    cdi.devices.join(", ")
                ),
            ),
        },
        Err(_) => super::skip(CDI_ID, "CDI is unknown: the engine could not be inspected"),
    }
}
