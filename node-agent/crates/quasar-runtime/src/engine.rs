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

/// Discovery facts are not a claim that GPU/rootless capabilities were tested.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EngineInfo {
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
    pub operating_system: Option<String>,
    pub architecture: Option<String>,
    pub cgroup_version: Option<String>,
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
