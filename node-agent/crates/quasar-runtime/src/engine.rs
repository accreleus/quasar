//! What discovery learns about the engine, and the API floor it enforces.

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub struct ApiVersion {
    pub major: usize,
    pub minor: usize,
}
impl std::fmt::Display for ApiVersion {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}.{}", self.major, self.minor)
    }
}

/// The lowest engine API this agent speaks, and the single place it is written down
/// (#266): discovery refuses anything below it and the `runtime_api_version` readiness
/// wording renders from it, so the two can never quote different numbers. Higher
/// capability floors must be established by the caller migrations that need them.
pub const API_FLOOR: ApiVersion = ApiVersion {
    major: 1,
    minor: 40,
};

/// Which container engine answers on the socket (RH-07 #396, amendment 17 `engine`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum EngineKind {
    Docker,
    Podman,
    /// An engine this runtime cannot name. Never guessed to be Docker, and never
    /// reported on the wire.
    Unknown,
}
impl EngineKind {
    /// The amendment-17 `engine` token; `None` for an engine this runtime cannot name.
    pub fn wire(self) -> Option<&'static str> {
        match self {
            EngineKind::Docker => Some("docker"),
            EngineKind::Podman => Some("podman"),
            EngineKind::Unknown => None,
        }
    }
    /// How an operator reads it.
    pub fn label(self) -> &'static str {
        match self {
            EngineKind::Docker => "Docker",
            EngineKind::Podman => "Podman",
            EngineKind::Unknown => "an unrecognised container engine",
        }
    }
    /// From `/version`: Podman lists a `Podman Engine` component and Docker an `Engine`
    /// one (rootful moby reports an empty platform name, so components come first). An
    /// engine that lists no components is named by its platform (older Docker releases).
    pub(crate) fn from_version(platform: Option<&str>, components: &[String]) -> Self {
        if components.iter().any(|c| c == "Podman Engine") {
            return EngineKind::Podman;
        }
        if components.iter().any(|c| c == "Engine") {
            return EngineKind::Docker;
        }
        match platform {
            _ if !components.is_empty() => EngineKind::Unknown,
            Some(name) if name.contains("Podman") => EngineKind::Podman,
            Some(name) if name.contains("Docker") => EngineKind::Docker,
            _ => EngineKind::Unknown,
        }
    }
}

/// Whether the engine runs as root on its host (amendment 17 `engine_mode`). Describes
/// the engine, as it reports itself, not the uid of this process.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum EngineMode {
    Rootful,
    Rootless,
}
impl EngineMode {
    pub fn wire(self) -> &'static str {
        match self {
            EngineMode::Rootful => "rootful",
            EngineMode::Rootless => "rootless",
        }
    }
    /// From `/info`: Docker and Podman both list `name=rootless` among their security
    /// options when rootless.
    pub(crate) fn from_security_options(options: &[String]) -> Self {
        if options
            .iter()
            .any(|o| o.split(',').any(|part| part == "name=rootless"))
        {
            EngineMode::Rootless
        } else {
            EngineMode::Rootful
        }
    }
}

/// Discovery facts are not a claim that GPU/rootless capabilities were tested.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EngineInfo {
    /// Docker, Podman, or unknown: from the engine's `/version`.
    pub kind: EngineKind,
    pub name: String,
    pub version: String,
    pub api_version: ApiVersion,
    pub server_min_api: ApiVersion,
    pub server_max_api: ApiVersion,
}

/// What one engine inspection reports beyond [`EngineInfo`]: the engine's own statements
/// about itself, observed and passed through. None of it is a claim that a capability was
/// exercised (#254).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EngineFacts {
    pub info: EngineInfo,
    /// Rootful or rootless, as the engine states it.
    pub mode: EngineMode,
    pub operating_system: Option<String>,
    pub architecture: Option<String>,
    pub cgroup_version: Option<String>,
    /// `systemd` or `cgroupfs`. Podman schedules container health checks through systemd
    /// timers, so a rootless Podman with no systemd user session falls back to `cgroupfs`
    /// and never runs them (RH-07 #405).
    pub cgroup_driver: Option<String>,
    pub security_options: Vec<String>,
    /// Configured OCI runtimes by name, sorted.
    pub runtimes: Vec<String>,
    pub default_runtime: Option<String>,
    /// `None` when the engine does not report CDI at all (pre-CDI API).
    pub cdi: Option<CdiFacts>,
}

/// The engine's CDI statement. An empty `spec_dirs` means CDI injection is disabled.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CdiFacts {
    pub spec_dirs: Vec<String>,
    /// `"<id> (<source>)"` per discovered device.
    pub devices: Vec<String>,
}

/// How an NVIDIA GPU reaches a container on this engine (RH-07 #399, decision D10). One
/// decision for every container Quasar creates, from what the engine reports about itself.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GpuInjection {
    /// A CDI device request (`nvidia.com/gpu=all`): the one mechanism in every engine mode.
    Cdi,
    /// Docker's `--gpus` device request: only a rootful Docker whose engine resolves no
    /// NVIDIA CDI device.
    DeviceRequest,
}

/// The CDI device every NVIDIA consumer requests.
pub const NVIDIA_CDI_DEVICE: &str = "nvidia.com/gpu=all";

impl GpuInjection {
    /// Podman resolves CDI whenever a specification exists (it does not list them in its
    /// compatible `/info`), so it always asks by CDI. Docker asks by CDI when it discovered an
    /// NVIDIA CDI device, and otherwise only a rootful Docker may fall back to `--gpus`. `None`:
    /// this engine cannot be given an NVIDIA GPU at all, which is a readiness failure naming
    /// the host preparation, never a privileged fallback.
    pub fn for_engine(kind: EngineKind, mode: EngineMode, cdi: Option<&CdiFacts>) -> Option<Self> {
        let nvidia_cdi =
            cdi.is_some_and(|c| c.devices.iter().any(|d| d.starts_with("nvidia.com/gpu")));
        match (kind, mode) {
            (EngineKind::Podman, _) => Some(GpuInjection::Cdi),
            _ if nvidia_cdi => Some(GpuInjection::Cdi),
            (_, EngineMode::Rootful) => Some(GpuInjection::DeviceRequest),
            (_, EngineMode::Rootless) => None,
        }
    }
}
